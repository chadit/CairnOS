// Package runner defines the contract between the CairnOS engine and the
// adapters that execute work. It is the only package external runner authors
// import, so its surface must stay stable across engine changes
// (.agents/spec/03-runner-interface.md).
package runner

import (
	"context"
	"time"
)

// Runner executes one action against a target: a CLI, an API, a model, or a
// local function. Implementations must be safe for concurrent use: the engine
// may call Execute from multiple goroutines for different actions at once.
type Runner interface {
	// Name returns the registered identifier that workflow files reference in
	// their runner field.
	Name() string

	// Execute runs the task and returns its result. A non-nil error means the
	// action failed; the engine decides retry based on the error's class and
	// the action's retry policy. Execute must respect ctx cancellation and
	// stop promptly when ctx is done.
	Execute(ctx context.Context, task *Task) (Result, error)

	// Validate checks task.Params and returns a typed error describing what is
	// wrong. The engine calls it twice: at workflow load time, when params may
	// still contain unresolved interpolation references (validate structure
	// only, and treat any unresolved reference as satisfying any type); and
	// again at action launch with fully resolved params. Implementations
	// detect the load-time case via Task.Resolved.
	Validate(task *Task) error
}

// Streamer lets a runner emit incremental output (tokens, log lines) while it
// runs. The engine forwards chunks to telemetry and to streaming API clients.
type Streamer interface {
	ExecuteStream(ctx context.Context, task *Task, out chan<- Chunk) (Result, error)
}

// Closer releases resources (processes, connections, file handles) when the
// engine shuts down. Called once at engine stop.
type Closer interface {
	Close() error
}

// Describer makes a runner usable as an agent-loop tool: the descriptor is
// what the driver model sees when deciding to call it. A runner without
// Describer cannot appear in an agent action's tools list
// (.agents/spec/15-agent-loop.md).
type Describer interface {
	Describe() ToolSpec
}

// ToolCaller marks an LLM runner that supports its provider's native tool-use
// mode, making it eligible as an agent-loop driver
// (.agents/spec/15-agent-loop.md). There is no JSON-in-text fallback for
// drivers without native tool calling (D8 in .agents/spec/14-open-decisions.md):
// a driver that does not implement ToolCaller fails agent validation with
// agent.driver_incapable.
//
// ExecuteWithTools runs one driver turn with the given tools offered. The
// returned Result follows the driver convention the agent loop parses:
//
//   - Tool call: Output is map[string]any{ToolCallName: <tool name>,
//     ToolCallArguments: map[string]any{...}} and Meta[MetaStopReason] is
//     StopReasonToolUse.
//   - Final answer: Output is any other value (normally the response text)
//     and Meta[MetaStopReason] is StopReasonEndTurn.
//
// Meta[MetaStopReason] settles the ambiguous case: a final answer that
// happens to be a map with a "tool" key reads as a tool call by shape, so
// the loop trusts the stop reason first. Implementations translate at most
// one tool call per turn and drop extra parallel calls from the provider.
type ToolCaller interface {
	ExecuteWithTools(ctx context.Context, task *Task, tools []ToolSpec) (Result, error)
}

// Driver-convention keys and values for ToolCaller results. The agent loop
// reads these to tell a tool call from a final answer.
const (
	// MetaStopReason is the Result.Meta key a ToolCaller sets to declare how
	// its turn ended: StopReasonToolUse or StopReasonEndTurn.
	MetaStopReason = "stop_reason"
	// StopReasonToolUse marks a turn that requested a tool call.
	StopReasonToolUse = "tool_use"
	// StopReasonEndTurn marks a turn that produced a final answer.
	StopReasonEndTurn = "end_turn"
	// ToolCallName and ToolCallArguments are the keys of the Output map a
	// ToolCaller returns for a tool call.
	ToolCallName      = "tool"
	ToolCallArguments = "arguments"
)

// Task is one action's worth of work.
type Task struct {
	// RunID and ActionName identify where this task sits in a workflow run.
	RunID      string
	ActionName string

	// Params are the arguments from the workflow file. When Resolved is true
	// all interpolation references are substituted and values are native Go
	// types (string, int64, float64, bool, []any, map[string]any).
	Params map[string]any

	// Inputs are the run-level inputs declared by the workflow.
	Inputs map[string]any

	// Idempotent mirrors the action's idempotent flag. A runner may read it
	// but the engine, not the runner, decides retries.
	Idempotent bool

	// Resolved is true once all interpolation references in Params are
	// substituted. False during load-time Validate, where unresolved
	// references remain and only structural checks are possible.
	Resolved bool

	// Deadline is the absolute time this task must finish by, derived from the
	// action and workflow timeouts. Zero means no deadline beyond ctx.
	Deadline time.Time
}

// Result is what a runner returns on success.
type Result struct {
	// Output is the action's result, readable downstream through output
	// references. Keep it JSON-serializable: the engine persists it and
	// passes it to other actions and to API clients.
	Output any

	// Usage reports resource consumption for telemetry and tuning. All fields
	// are optional; a runner sets what it knows.
	Usage Usage

	// Meta carries runner-specific detail that is not the primary output
	// (model name, CLI exit code, request ID). Surfaced in telemetry.
	Meta map[string]string
}

// Usage is the per-action accounting the metric catalog reads.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64
	CostUSD      float64 // estimated, runner-computed
	Latency      time.Duration
	Retries      int // set by the engine, not the runner
}

// Chunk is one piece of incremental output from a streaming runner.
type Chunk struct {
	// Data is the raw streamed text (a token batch, a log line).
	Data string
	// Time is when the runner produced the chunk.
	Time time.Time
}

// ToolSpec describes a runner to an agent-loop driver. Write Description for a
// model, not a human: when and why to call this tool.
type ToolSpec struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object for the tool's arguments.
	InputSchema map[string]any
}
