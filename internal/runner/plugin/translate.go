package plugin

import (
	"encoding/json"
	"fmt"
	"time"

	pluginv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/plugin/v1"
	"github.com/chadit/CairnOS/pkg/runner"
)

// taskToProto encodes a runner.Task for the wire. Params and inputs travel
// as JSON strings, so the value semantics match the engine's encoding/json
// handling end to end (the rationale lives in plugin.proto).
func taskToProto(task *runner.Task) (*pluginv1.Task, error) {
	paramsJSON, err := json.Marshal(task.Params)
	if err != nil {
		return nil, fmt.Errorf("encode task params: %w", err)
	}

	inputsJSON, err := json.Marshal(task.Inputs)
	if err != nil {
		return nil, fmt.Errorf("encode task inputs: %w", err)
	}

	var deadlineMs int64
	if !task.Deadline.IsZero() {
		deadlineMs = task.Deadline.UnixMilli()
	}

	return &pluginv1.Task{
		RunId:          task.RunID,
		ActionName:     task.ActionName,
		ParamsJson:     string(paramsJSON),
		InputsJson:     string(inputsJSON),
		Idempotent:     task.Idempotent,
		Resolved:       task.Resolved,
		DeadlineUnixMs: deadlineMs,
	}, nil
}

// resultFromProto rebuilds a runner.Result, decoding the output JSON back
// into native Go values. decodeCode names the classified error used when the
// plugin sent undecodable output.
func resultFromProto(protoResult *pluginv1.Result, decodeCode string) (runner.Result, error) {
	var output any

	if text := protoResult.GetOutputJson(); text != "" {
		if err := json.Unmarshal([]byte(text), &output); err != nil {
			return runner.Result{}, &runner.Error{
				Class:   runner.ClassPermanent,
				Code:    decodeCode,
				Message: "decode plugin output: " + err.Error(),
				Cause:   err,
			}
		}
	}

	usage := protoResult.GetUsage()

	return runner.Result{
		Output: output,
		Usage: runner.Usage{
			InputTokens:  usage.GetInputTokens(),
			OutputTokens: usage.GetOutputTokens(),
			CachedTokens: usage.GetCachedTokens(),
			CostUSD:      usage.GetCostUsd(),
			Latency:      time.Duration(usage.GetLatencyMs()) * time.Millisecond,
		},
		Meta: protoResult.GetMeta(),
	}, nil
}

// errorFromProto rebuilds the classified error, so retry classification
// survives the wire intact.
func errorFromProto(protoErr *pluginv1.ClassifiedError) *runner.Error {
	return &runner.Error{
		Class:      classFromProto(protoErr.GetClass()),
		Code:       protoErr.GetCode(),
		Message:    protoErr.GetMessage(),
		RetryAfter: time.Duration(protoErr.GetRetryAfterMs()) * time.Millisecond,
	}
}

// classFromProto maps the wire enum back onto runner.ErrorClass. Unspecified
// reads as permanent: the engine must never retry a side effect the plugin
// did not declare safe.
func classFromProto(class pluginv1.ErrorClass) runner.ErrorClass {
	switch class {
	case pluginv1.ErrorClass_ERROR_CLASS_TRANSIENT:
		return runner.ClassTransient
	case pluginv1.ErrorClass_ERROR_CLASS_RATE_LIMITED:
		return runner.ClassRateLimited
	case pluginv1.ErrorClass_ERROR_CLASS_PERMANENT, pluginv1.ErrorClass_ERROR_CLASS_UNSPECIFIED:
		return runner.ClassPermanent
	default:
		return runner.ClassPermanent
	}
}

// toolSpecFromProto rebuilds the runner.ToolSpec captured in the handshake.
func toolSpecFromProto(spec *pluginv1.ToolSpec) (runner.ToolSpec, error) {
	out := runner.ToolSpec{Name: spec.GetName(), Description: spec.GetDescription()}

	if text := spec.GetInputSchemaJson(); text != "" {
		var schema map[string]any
		if err := json.Unmarshal([]byte(text), &schema); err != nil {
			return runner.ToolSpec{}, fmt.Errorf("decode tool input schema: %w", err)
		}

		out.InputSchema = schema
	}

	return out, nil
}
