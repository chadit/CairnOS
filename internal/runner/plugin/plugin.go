// Package plugin hosts out-of-process runners. Each configured plugin runs
// as a child process serving the cairnos.plugin.v1.RunnerPlugin service over
// a private unix socket; this package is the engine-side half that spawns the
// process, speaks the protocol, and translates between proto and pkg/runner
// types (.agents/spec/03-runner-interface.md, "Custom runners and plugins",
// path 2). Crashes stay isolated: a dead or unresponsive plugin surfaces as
// a transient <name>.plugin_down error, never a wedged engine.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/plugin/v1"
	sdk "github.com/chadit/CairnOS/pkg/plugin"
	"github.com/chadit/CairnOS/pkg/runner"
)

const (
	// connectTimeout bounds spawn-to-handshake: the window between starting
	// the child process and a successful Describe over the socket.
	connectTimeout = 10 * time.Second

	// validateTimeout bounds the Validate round trip, which carries no caller
	// context by the runner.Validate contract.
	validateTimeout = 10 * time.Second

	// socketFileName is the socket's name inside the per-plugin temp dir.
	socketFileName = "plugin.sock"
)

// Config configures one plugin-backed runner.
type Config struct {
	// Command is the argv that launches the plugin process.
	Command []string

	// Name overrides the runner name used in Name() and error codes. Empty
	// means the name the plugin reports in its Describe handshake.
	Name string

	// EnvPassthrough names host environment variables to forward to the plugin
	// process on top of the minimal base env (spec L2). Empty hands the child
	// only PATH and the socket variable, so a plugin never receives the host's
	// tokens, DSNs, or signing keys unless the operator listed the variable.
	EnvPassthrough []string
}

// Plugin is one out-of-process runner: a child process this type spawned at
// construction and talks to over gRPC. Safe for concurrent use; the engine
// may call Execute from multiple goroutines at once and gRPC multiplexes the
// calls over the one connection.
type Plugin struct {
	toolSpec  runner.ToolSpec
	client    pluginv1.RunnerPluginClient
	cmd       *exec.Cmd
	conn      *grpc.ClientConn
	exited    chan struct{}
	name      string
	socketDir string
	tree      processTree
}

// New spawns the plugin process, connects over its private unix socket, and
// runs the Describe handshake. ctx bounds construction only; the returned
// Plugin lives until Close.
func New(ctx context.Context, cfg Config) (*Plugin, error) {
	if len(cfg.Command) == 0 {
		return nil, ErrEmptyCommand
	}

	socketDir, err := os.MkdirTemp("", "cairnos-plugin-")
	if err != nil {
		return nil, fmt.Errorf("create plugin socket directory: %w", err)
	}

	socketPath := filepath.Join(socketDir, socketFileName)

	// WithoutCancel keeps the construction context's values while detaching
	// its cancellation: the plugin's lifetime belongs to Close, not to
	// whatever deadline the caller built the runner under.
	//
	// #nosec G204 -- the argv comes from the operator's own [runner.<name>]
	// config block, not from workflow params or network input.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), cfg.Command[0], cfg.Command[1:]...)

	cmd.Env = pluginEnv(socketPath, cfg.EnvPassthrough)
	// The plugin's stderr shares the host's, so crash output lands in the
	// operator's log instead of vanishing with the child.
	cmd.Stderr = os.Stderr

	plug := &Plugin{
		name:      cfg.Name,
		socketDir: socketDir,
		cmd:       cmd,
		exited:    make(chan struct{}),
	}

	plug.configureProcessTree()

	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(socketDir)

		return nil, fmt.Errorf("start plugin %q: %w", cfg.Command[0], err)
	}

	go func() {
		// Wait reaps the child no matter how it ends; the closed channel is
		// the one exit signal every other goroutine watches.
		_ = cmd.Wait()

		close(plug.exited)
	}()

	if err := plug.adoptProcessTree(); err != nil {
		_ = plug.Close()

		return nil, err
	}

	if err := plug.connect(ctx, socketPath); err != nil {
		_ = plug.Close()

		return nil, err
	}

	return plug, nil
}

// pluginEnv builds the child process environment from a minimal base, never the
// host's full os.Environ (spec L2). The base is the socket variable plus PATH,
// so the child can find its own interpreter. Everything else arrives only when
// the operator named it, by exact name, in env_passthrough.
func pluginEnv(socketPath string, passthrough []string) []string {
	// Cap holds the passthrough names plus the two fixed base entries (the
	// socket variable and PATH), so the slice never reallocates.
	const baseEnvEntries = 2

	env := make([]string, 0, len(passthrough)+baseEnvEntries)
	env = append(env, sdk.SocketEnv+"="+socketPath)

	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+path)
	}

	for _, name := range passthrough {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}

	return env
}

// Close shuts the plugin down: it closes the connection, takes the plugin's
// whole process tree down, and removes the socket directory. The registry
// calls it once at engine stop; it is also safe after a crash, when the
// process is already gone.
func (p *Plugin) Close() error {
	var errs []error

	if p.conn != nil {
		if err := p.conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close plugin connection: %w", err))
		}
	}

	if err := p.stopProcess(); err != nil {
		errs = append(errs, err)
	}

	if err := os.RemoveAll(p.socketDir); err != nil {
		errs = append(errs, fmt.Errorf("remove plugin socket directory: %w", err))
	}

	if len(errs) == 0 {
		return nil
	}

	return fmt.Errorf("close plugin %q: %w", p.name, errors.Join(errs...))
}

// Describe implements runner.Describer with the spec captured during the
// startup handshake. A plugin that reported no tool spec yields a zero
// ToolSpec, which the agent loop reads as not-a-tool.
func (p *Plugin) Describe() runner.ToolSpec {
	return p.toolSpec
}

// Execute implements runner.Runner over the Execute stream, discarding any
// chunk events the plugin emits.
func (p *Plugin) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	return p.run(ctx, task, nil)
}

// ExecuteStream implements runner.Streamer, forwarding the plugin's chunk
// events to out as they arrive.
func (p *Plugin) ExecuteStream(ctx context.Context, task *runner.Task, out chan<- runner.Chunk) (runner.Result, error) {
	return p.run(ctx, task, out)
}

// Name implements runner.Runner.
func (p *Plugin) Name() string {
	return p.name
}

// SocketDir reports the per-plugin temp directory holding the unix socket,
// for diagnostics and tests; Close removes it.
func (p *Plugin) SocketDir() string {
	return p.socketDir
}

// Validate implements runner.Runner by round-tripping the task to the
// plugin. The Validate contract carries no context, so the call runs under
// its own bounded one, mirroring the mcp_client Describer pattern.
func (p *Plugin) Validate(task *runner.Task) error {
	protoTask, err := taskToProto(task)
	if err != nil {
		return p.encodeError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), validateTimeout)
	defer cancel()

	resp, err := p.client.Validate(ctx, &pluginv1.ValidateRequest{Task: protoTask})
	if err != nil {
		return p.transportError(ctx, err)
	}

	if protoErr := resp.GetError(); protoErr != nil {
		return errorFromProto(protoErr)
	}

	return nil
}

// code builds a stable machine code under this plugin's runner name.
func (p *Plugin) code(suffix string) string {
	return p.name + "." + suffix
}

// connect dials the socket and performs the Describe handshake. WaitForReady
// holds the first RPC until the child finishes binding the socket; a child
// that dies first cancels the wait through the exit watcher instead of
// burning the whole timeout.
func (p *Plugin) connect(ctx context.Context, socketPath string) error {
	conn, err := grpc.NewClient("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial plugin socket: %w", err)
	}

	p.conn = conn
	p.client = pluginv1.NewRunnerPluginClient(conn)

	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	go func() {
		select {
		case <-p.exited:
			cancel()
		case <-dialCtx.Done():
		}
	}()

	resp, err := p.client.Describe(dialCtx, &pluginv1.DescribeRequest{}, grpc.WaitForReady(true))
	if err != nil {
		return fmt.Errorf("describe plugin: %w", err)
	}

	if p.name == "" {
		p.name = resp.GetName()
	}

	if p.name == "" {
		return ErrUnnamedPlugin
	}

	if spec := resp.GetToolSpec(); spec != nil {
		toolSpec, specErr := toolSpecFromProto(spec)
		if specErr != nil {
			return specErr
		}

		p.toolSpec = toolSpec
	}

	return nil
}

// encodeError classifies a host-side task encode failure as permanent: the
// same params will fail the same way on every retry.
func (p *Plugin) encodeError(cause error) *runner.Error {
	return &runner.Error{
		Class:   runner.ClassPermanent,
		Code:    p.code("encode_task"),
		Message: "encode task: " + cause.Error(),
		Cause:   cause,
	}
}

// recvResult carries one stream.Recv outcome from the receive goroutine back
// to the run loop, so the loop can select the process-exit watcher against it.
type recvResult struct {
	event *pluginv1.ExecuteEvent
	err   error
}

// run is the shared Execute/ExecuteStream body: one Execute RPC, chunks
// forwarded to any consumer, the terminal event translated back.
//
// Crash isolation: only a received result or classified-error event ends the
// call cleanly. Every other exit is a plugin_down: a broken stream, or the
// process-exit watcher firing, even while a chunk forward is parked on a
// consumer that stopped reading.
func (p *Plugin) run(ctx context.Context, task *runner.Task, out chan<- runner.Chunk) (runner.Result, error) {
	protoTask, err := taskToProto(task)
	if err != nil {
		return runner.Result{}, p.encodeError(err)
	}

	stream, err := p.client.Execute(ctx, &pluginv1.ExecuteRequest{Task: protoTask})
	if err != nil {
		return runner.Result{}, p.transportError(ctx, err)
	}

	// One receive goroutine owns stream.Recv for the stream's whole life, so
	// the loop can select it against the process-exit watcher without spawning
	// a goroutine per event. The buffer of one lets a Recv that is unblocking
	// after a process death finish its send and exit even when the loop has
	// already returned on p.exited, so nothing leaks.
	recv := make(chan recvResult, 1)
	go receiveLoop(stream, recv)

	for {
		select {
		case <-p.exited:
			// The process died before a terminal event arrived. Whatever was
			// on the wire is from a corpse; surface plugin_down.
			return runner.Result{}, p.processExitedError(ctx)
		case got := <-recv:
			if got.err != nil {
				// Covers io.EOF before any terminal event too: a stream that
				// just ends means the plugin died mid-task.
				return runner.Result{}, p.transportError(ctx, got.err)
			}

			outcome := p.handleEvent(ctx, got.event, out)
			if outcome.terminal {
				return outcome.value, outcome.err
			}

			out = outcome.out
		}
	}
}

// receiveLoop pumps stream events to recv until the stream ends or errors,
// then sends the terminating error and stops. It runs as the single owner of
// stream.Recv so the run loop never calls Recv concurrently.
func receiveLoop(stream grpc.ServerStreamingClient[pluginv1.ExecuteEvent], recv chan<- recvResult) {
	for {
		event, err := stream.Recv()

		recv <- recvResult{event: event, err: err}

		if err != nil {
			return
		}
	}
}

// eventOutcome is one decoded ExecuteEvent. terminal marks the result and
// classified-error events that end the call; the non-terminal chunk and
// unknown events carry forward the possibly-cleared chunk consumer in out.
type eventOutcome struct {
	err      error
	out      chan<- runner.Chunk
	value    runner.Result
	terminal bool
}

// handleEvent translates one ExecuteEvent, marking result and classified-error
// events terminal so the run loop stops. A chunk forwards inside an exit-aware
// select, so a plugin that dies while the consumer stalls surfaces as
// plugin_down. An unknown oneof is skipped: a newer plugin's event kinds must
// not wedge an older host, and an empty event must never read as success.
func (p *Plugin) handleEvent(ctx context.Context, event *pluginv1.ExecuteEvent, out chan<- runner.Chunk) eventOutcome {
	switch payload := event.GetEvent().(type) {
	case *pluginv1.ExecuteEvent_Chunk:
		return p.forwardChunk(ctx, out, payload.Chunk)
	case *pluginv1.ExecuteEvent_Result:
		value, decodeErr := resultFromProto(payload.Result, p.code("decode_output"))

		return eventOutcome{value: value, terminal: true, err: decodeErr}
	case *pluginv1.ExecuteEvent_Error:
		return eventOutcome{terminal: true, err: errorFromProto(payload.Error)}
	default:
		return eventOutcome{out: out}
	}
}

// forwardChunk delivers one chunk to the consumer while watching for the plugin
// process dying, which makes the call a plugin_down. A canceled call context
// drops the consumer (out cleared to nil) without ending the call, so the rest
// of the stream still drains. A nil consumer is a plain Execute.
func (p *Plugin) forwardChunk(ctx context.Context, out chan<- runner.Chunk, chunk *pluginv1.Chunk) eventOutcome {
	if out == nil {
		return eventOutcome{}
	}

	select {
	case out <- runner.Chunk{Data: chunk.GetData(), Time: time.UnixMilli(chunk.GetTimeUnixMs())}:
		return eventOutcome{out: out}
	case <-p.exited:
		return eventOutcome{terminal: true, err: p.processExitedError(ctx)}
	case <-ctx.Done():
		return eventOutcome{out: nil}
	}
}

// processExitedError reports the plugin process exiting before a terminal
// event arrived. A caller that canceled the call still reads as a timeout;
// otherwise the process died on its own, surfaced as the same transient
// plugin_down the engine handles for any dead-plugin call.
func (p *Plugin) processExitedError(ctx context.Context) *runner.Error {
	if ctx.Err() != nil {
		return &runner.Error{
			Class:   runner.ClassTransient,
			Code:    p.code("timeout"),
			Message: "plugin call canceled or timed out",
			Cause:   ctx.Err(),
		}
	}

	return &runner.Error{
		Class:   runner.ClassTransient,
		Code:    p.code("plugin_down"),
		Message: "plugin process exited before returning a result",
		Cause:   ErrProcessExited,
	}
}

// transportError maps a connection-level failure onto the crash-isolation
// contract. A canceled or expired call context reads as a timeout; anything
// else means the plugin process is dead or unresponsive, surfaced as a
// transient <name>.plugin_down the engine can retry or circuit-break on
// instead of wedging.
func (p *Plugin) transportError(ctx context.Context, cause error) *runner.Error {
	if ctx.Err() != nil {
		return &runner.Error{
			Class:   runner.ClassTransient,
			Code:    p.code("timeout"),
			Message: "plugin call canceled or timed out",
			Cause:   cause,
		}
	}

	return &runner.Error{
		Class:   runner.ClassTransient,
		Code:    p.code("plugin_down"),
		Message: "plugin process is dead or unresponsive: " + cause.Error(),
		Cause:   cause,
	}
}
