package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"

	pluginv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/plugin/v1"
	"github.com/chadit/CairnOS/pkg/runner"
)

// chunkBuffer sizes the channel between a streaming runner and the gRPC
// send loop, so a bursty runner is not gated on the wire per chunk.
const chunkBuffer = 16

// service adapts a runner.Runner onto the RunnerPlugin gRPC surface.
type service struct {
	pluginv1.UnimplementedRunnerPluginServer

	impl runner.Runner
}

// Describe implements the startup handshake: the runner's name, plus its
// tool descriptor when the implementation satisfies runner.Describer.
func (s *service) Describe(ctx context.Context, _ *pluginv1.DescribeRequest) (*pluginv1.DescribeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("describe canceled: %w", err)
	}

	resp := &pluginv1.DescribeResponse{Name: s.impl.Name()}

	describer, isDescriber := s.impl.(runner.Describer)
	if !isDescriber {
		return resp, nil
	}

	spec := describer.Describe()

	var schemaJSON string

	if spec.InputSchema != nil {
		encoded, err := json.Marshal(spec.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("encode tool input schema: %w", err)
		}

		schemaJSON = string(encoded)
	}

	resp.ToolSpec = &pluginv1.ToolSpec{
		Name:            spec.Name,
		Description:     spec.Description,
		InputSchemaJson: schemaJSON,
	}

	return resp, nil
}

// Validate decodes the task and reports the runner's verdict in the response
// body; transport errors stay reserved for the connection itself.
func (s *service) Validate(ctx context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("validate canceled: %w", err)
	}

	task, err := taskFromProto(req.GetTask())
	if err != nil {
		return &pluginv1.ValidateResponse{Error: errorToProto(err, s.impl.Name())}, nil
	}

	if err := s.impl.Validate(task); err != nil {
		return &pluginv1.ValidateResponse{Error: errorToProto(err, s.impl.Name())}, nil
	}

	return &pluginv1.ValidateResponse{}, nil
}

// Execute runs one task, forwarding chunks while the runner works and
// closing the stream with exactly one result or classified error event.
func (s *service) Execute(req *pluginv1.ExecuteRequest, stream grpc.ServerStreamingServer[pluginv1.ExecuteEvent]) error {
	ctx := stream.Context()

	task, err := taskFromProto(req.GetTask())
	if err != nil {
		return sendEvent(stream, errorEvent(err, s.impl.Name()))
	}

	result, execErr := s.run(ctx, task, stream)
	if execErr != nil {
		return sendEvent(stream, errorEvent(execErr, s.impl.Name()))
	}

	protoResult, err := resultToProto(&result)
	if err != nil {
		return sendEvent(stream, errorEvent(err, s.impl.Name()))
	}

	return sendEvent(stream, &pluginv1.ExecuteEvent{Event: &pluginv1.ExecuteEvent_Result{Result: protoResult}})
}

// run picks the richer streaming path when the implementation offers it.
func (s *service) run(ctx context.Context, task *runner.Task, stream grpc.ServerStreamingServer[pluginv1.ExecuteEvent]) (runner.Result, error) {
	streamer, isStreamer := s.impl.(runner.Streamer)
	if !isStreamer {
		return s.impl.Execute(ctx, task) //nolint:wrapcheck // the runner's classified error must reach errorToProto verbatim; wrapping here would bury the class
	}

	return runStreaming(ctx, task, streamer, stream)
}

// runStreaming pumps the runner's chunk channel onto the gRPC stream from
// this goroutine, because a gRPC server stream is not safe for concurrent
// sends and the terminal event has to go out last.
func runStreaming(ctx context.Context, task *runner.Task, streamer runner.Streamer, stream grpc.ServerStreamingServer[pluginv1.ExecuteEvent]) (runner.Result, error) {
	type outcome struct {
		err    error
		result runner.Result
	}

	chunks := make(chan runner.Chunk, chunkBuffer)
	done := make(chan outcome, 1)

	go func() {
		result, err := streamer.ExecuteStream(ctx, task, chunks)
		done <- outcome{result: result, err: err}
	}()

	forwarding := true

	for {
		select {
		case chunk := <-chunks:
			forwarding = sendChunk(stream, &chunk, forwarding)
		case final := <-done:
			// The runner finished but may have buffered chunks racing the
			// outcome; drain them before the terminal event.
			for {
				select {
				case chunk := <-chunks:
					forwarding = sendChunk(stream, &chunk, forwarding)
				default:
					return final.result, final.err
				}
			}
		}
	}
}

// sendChunk forwards one chunk unless forwarding already failed; a host that
// went away cancels the stream context, so the runner unwinds on its own and
// the only job left here is to stop calling Send.
func sendChunk(stream grpc.ServerStreamingServer[pluginv1.ExecuteEvent], chunk *runner.Chunk, forwarding bool) bool {
	if !forwarding {
		return false
	}

	err := stream.Send(&pluginv1.ExecuteEvent{
		Event: &pluginv1.ExecuteEvent_Chunk{
			Chunk: &pluginv1.Chunk{Data: chunk.Data, TimeUnixMs: chunk.Time.UnixMilli()},
		},
	})

	return err == nil
}

// sendEvent writes one stream event, wrapping the transport error so the
// failure names the operation.
func sendEvent(stream grpc.ServerStreamingServer[pluginv1.ExecuteEvent], event *pluginv1.ExecuteEvent) error {
	if err := stream.Send(event); err != nil {
		return fmt.Errorf("send execute event: %w", err)
	}

	return nil
}

// errorEvent packages a failure as the stream's terminal event.
func errorEvent(err error, name string) *pluginv1.ExecuteEvent {
	return &pluginv1.ExecuteEvent{Event: &pluginv1.ExecuteEvent_Error{Error: errorToProto(err, name)}}
}

// taskFromProto rebuilds a runner.Task from the wire shape, decoding the
// JSON-encoded open-typed fields with the engine's own encoding/json
// semantics.
func taskFromProto(protoTask *pluginv1.Task) (*runner.Task, error) {
	params, err := decodeJSONMap(protoTask.GetParamsJson())
	if err != nil {
		return nil, fmt.Errorf("decode task params: %w", err)
	}

	inputs, err := decodeJSONMap(protoTask.GetInputsJson())
	if err != nil {
		return nil, fmt.Errorf("decode task inputs: %w", err)
	}

	var deadline time.Time
	if ms := protoTask.GetDeadlineUnixMs(); ms != 0 {
		deadline = time.UnixMilli(ms)
	}

	return &runner.Task{
		RunID:      protoTask.GetRunId(),
		ActionName: protoTask.GetActionName(),
		Params:     params,
		Inputs:     inputs,
		Idempotent: protoTask.GetIdempotent(),
		Resolved:   protoTask.GetResolved(),
		Deadline:   deadline,
	}, nil
}

// decodeJSONMap unmarshals one JSON object field. An empty wire value decodes
// as an empty map, so runners can range and index params without nil checks.
func decodeJSONMap(text string) (map[string]any, error) {
	decoded := map[string]any{}

	if text == "" {
		return decoded, nil
	}

	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil, fmt.Errorf("unmarshal JSON object: %w", err)
	}

	return decoded, nil
}

// resultToProto encodes a runner.Result for the wire.
func resultToProto(result *runner.Result) (*pluginv1.Result, error) {
	var outputJSON string

	if result.Output != nil {
		encoded, err := json.Marshal(result.Output)
		if err != nil {
			return nil, fmt.Errorf("encode result output: %w", err)
		}

		outputJSON = string(encoded)
	}

	return &pluginv1.Result{
		OutputJson: outputJSON,
		Usage: &pluginv1.Usage{
			InputTokens:  result.Usage.InputTokens,
			OutputTokens: result.Usage.OutputTokens,
			CachedTokens: result.Usage.CachedTokens,
			CostUsd:      result.Usage.CostUSD,
			LatencyMs:    result.Usage.Latency.Milliseconds(),
		},
		Meta: result.Meta,
	}, nil
}

// errorToProto encodes a failure for the wire. A plain unclassified error
// maps to permanent, matching the engine's never-retry-undeclared rule.
func errorToProto(err error, name string) *pluginv1.ClassifiedError {
	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		return &pluginv1.ClassifiedError{
			Class:   pluginv1.ErrorClass_ERROR_CLASS_PERMANENT,
			Code:    name + ".error",
			Message: err.Error(),
		}
	}

	return &pluginv1.ClassifiedError{
		Class:        classToProto(classified.Class),
		Code:         classified.Code,
		Message:      classified.Message,
		RetryAfterMs: classified.RetryAfter.Milliseconds(),
	}
}

// classToProto maps the runner error class onto the wire enum.
func classToProto(class runner.ErrorClass) pluginv1.ErrorClass {
	switch class {
	case runner.ClassTransient:
		return pluginv1.ErrorClass_ERROR_CLASS_TRANSIENT
	case runner.ClassPermanent:
		return pluginv1.ErrorClass_ERROR_CLASS_PERMANENT
	case runner.ClassRateLimited:
		return pluginv1.ErrorClass_ERROR_CLASS_RATE_LIMITED
	case runner.ClassNeedsReauth:
		// The plugin wire enum has no reauth value: re-auth is a built-in
		// CLI-tool concern (codex, qwen), not an external plugin one, so a
		// plugin that somehow reports it degrades to permanent on the wire.
		return pluginv1.ErrorClass_ERROR_CLASS_PERMANENT
	default:
		return pluginv1.ErrorClass_ERROR_CLASS_PERMANENT
	}
}
