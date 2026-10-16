// Command echo is the reference out-of-process CairnOS runner: a separate
// binary the engine spawns and talks to over the RunnerPlugin gRPC service
// on a private unix socket. It shows everything a plugin author writes: a
// plain pkg/runner implementation handed to pkg/plugin.Serve.
//
// Build it:
//
//	$ go build -o ~/.cairnos/plugins/echo ./examples/plugins/echo
//
// Wire it into config.toml (the block name is the runner name workflows
// reference):
//
//	[runner.echo]
//	type = "plugin"
//	command = ["/home/you/.cairnos/plugins/echo"]
//
// Then any workflow action can use it:
//
//	[action.shout]
//	runner = "echo"
//	params = { message = "hello" }
//
// The runner echoes its params back as the action output. Test-oriented
// params, mirroring the built-in mock runner:
//
//	fail_class      string  "transient", "permanent", or "rate_limited":
//	                        fail with that classified error class
//	retry_after_ms  int     RetryAfter hint attached to rate_limited failures
//	chunks          array   strings emitted as stream chunks before the result
//	crash           bool    kill this process mid-Execute (crash-isolation
//	                        testing; the engine sees <name>.plugin_down)
//	bad_param       any     presence makes Validate reject the task
package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strconv"
	"time"

	"github.com/chadit/CairnOS/pkg/plugin"
	"github.com/chadit/CairnOS/pkg/runner"
)

// runnerName is what Describe reports; the host config block may override it.
const runnerName = "echo"

// echoRunner is the plain runner implementation. It satisfies runner.Runner,
// runner.Streamer, and runner.Describer; pkg/plugin handles the wire.
type echoRunner struct{}

// Name implements runner.Runner.
func (echoRunner) Name() string {
	return runnerName
}

// Validate implements runner.Runner. The bad_param rejection exists so hosts
// can exercise the Validate translation path end to end.
func (echoRunner) Validate(task *runner.Task) error {
	if _, present := task.Params["bad_param"]; present {
		return runner.NewError(runner.ClassPermanent, runnerName+".bad_param", "params must not include bad_param")
	}

	return nil
}

// Execute implements runner.Runner: configured failures first, then the
// params echoed back as the output.
func (e echoRunner) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	return e.ExecuteStream(ctx, task, nil)
}

// ExecuteStream implements runner.Streamer, emitting the optional chunks
// param before the terminal result. Execute funnels here with a nil stream.
func (echoRunner) ExecuteStream(ctx context.Context, task *runner.Task, out chan<- runner.Chunk) (runner.Result, error) {
	start := time.Now()

	if shouldCrash(task) {
		// An uncatchable kill of this process simulates a hard plugin crash
		// mid-task; the host turns it into a transient plugin_down. A kill
		// that cannot be sent fails the task instead of passing as a result.
		if err := killSelf(); err != nil {
			return runner.Result{}, runner.NewError(runner.ClassPermanent, runnerName+".crash_failed", err.Error())
		}
	}

	if err := configuredFailure(task); err != nil {
		return runner.Result{}, err
	}

	if !emitChunks(ctx, task, out) {
		return runner.Result{}, runner.NewError(runner.ClassTransient, runnerName+".canceled", "canceled while streaming")
	}

	return runner.Result{
		Output: echoOutput(task),
		Usage: runner.Usage{
			InputTokens: int64(len(task.Params)),
			Latency:     time.Since(start),
		},
		Meta: map[string]string{
			"pid": strconv.Itoa(os.Getpid()),
		},
	}, nil
}

// echoOutput returns the params echoed back, plus the value of the host env
// variable named by the echo_env param when one is given. It lets a host test
// the least-privilege child environment: the variable reads empty unless it
// was forwarded through the plugin's env_passthrough allowlist.
func echoOutput(task *runner.Task) map[string]any {
	output := make(map[string]any, len(task.Params)+1)
	maps.Copy(output, task.Params)

	if name, ok := task.Params["echo_env"].(string); ok && name != "" {
		output["env_value"] = os.Getenv(name)
	}

	return output
}

// Describe implements runner.Describer so hosts can test the ToolSpec
// translation path.
func (echoRunner) Describe() runner.ToolSpec {
	return runner.ToolSpec{
		Name:        runnerName,
		Description: "Echoes the call arguments back unchanged. Use it to inspect what a tool call would receive.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": true,
		},
	}
}

// shouldCrash reads the crash param.
func shouldCrash(task *runner.Task) bool {
	crash, isBool := task.Params["crash"].(bool)

	return isBool && crash
}

// killSelf sends this process the uncatchable kill the crash param asks for.
func killSelf() error {
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		return fmt.Errorf("find own process: %w", err)
	}

	if err := self.Kill(); err != nil {
		return fmt.Errorf("kill own process: %w", err)
	}

	return nil
}

// configuredFailure maps the fail_class param onto a classified error, the
// same vocabulary the built-in mock runner uses.
func configuredFailure(task *runner.Task) error {
	class, present := task.Params["fail_class"].(string)
	if !present {
		return nil
	}

	switch class {
	case "transient":
		return runner.NewError(runner.ClassTransient, runnerName+".transient", "configured transient failure")
	case "rate_limited":
		failure := runner.NewError(runner.ClassRateLimited, runnerName+".rate_limited", "configured rate limit")
		failure.RetryAfter = retryAfter(task)

		return failure
	default:
		return runner.NewError(runner.ClassPermanent, runnerName+".permanent", fmt.Sprintf("configured failure %v", class))
	}
}

// retryAfter reads the retry_after_ms param, tolerating the numeric types
// JSON and TOML produce.
func retryAfter(task *runner.Task) time.Duration {
	switch value := task.Params["retry_after_ms"].(type) {
	case float64:
		return time.Duration(value) * time.Millisecond
	case int64:
		return time.Duration(value) * time.Millisecond
	default:
		return 0
	}
}

// emitChunks streams each entry of the chunks param, reporting false when
// the context died first.
func emitChunks(ctx context.Context, task *runner.Task, out chan<- runner.Chunk) bool {
	if out == nil {
		return true
	}

	items, isList := task.Params["chunks"].([]any)
	if !isList {
		return true
	}

	for _, item := range items {
		text, isString := item.(string)
		if !isString {
			continue
		}

		select {
		case out <- runner.Chunk{Data: text, Time: time.Now()}:
		case <-ctx.Done():
			return false
		}
	}

	return true
}

func main() {
	os.Exit(serve())
}

// serve hands the runner to the SDK, which owns the socket, the gRPC server,
// and the shutdown on SIGTERM, SIGINT, or a closed stdin pipe; nothing else
// is needed here.
func serve() int {
	if err := plugin.Serve(echoRunner{}); err != nil {
		slog.Error("echo plugin stopped", "error", err)

		return 1
	}

	return 0
}
