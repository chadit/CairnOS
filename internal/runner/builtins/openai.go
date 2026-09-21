package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/pkg/runner"
)

// OpenAIName is the registry name of the OpenAI chat-completions runner.
const OpenAIName = "openai"

// OpenAIKeyEnv is the API-key variable an openai-type block falls back to. A
// block reads CAIRNOS_RUNNER_<NAME>_API_KEY first and this one second, so a
// host that sets only this keeps working however its blocks are named. The
// config file never carries the key, matching the repo-wide rule that secrets
// live in the environment.
//
// It is exported so the secrets pull warning can name what a runner reads.
const OpenAIKeyEnv = "CAIRNOS_RUNNER_OPENAI_API_KEY" // #nosec G101 -- environment variable name, not a credential

// defaultOpenAIBaseURL targets the hosted API; base_url in config points the
// runner at compatible gateways and proxies instead.
const defaultOpenAIBaseURL = "https://api.openai.com/v1"

// chatCompletionsPath is the chat-completions endpoint under the base URL.
const chatCompletionsPath = "/chat/completions"

// openaiStreamDone is the sentinel data payload that ends an OpenAI SSE
// stream.
const openaiStreamDone = "[DONE]"

// OpenAI calls the OpenAI chat-completions API, one of the Phase 1a API
// runners in the built-ins table (.agents/spec/03-runner-interface.md).
//
// Config: model (required to run), base_url, max_tokens, temperature, and
// price_in/price_out in USD per million tokens for cost reporting. Params:
// prompt (required), system, and model (overrides config). The API key is read
// at call time from the block's own CAIRNOS_RUNNER_<NAME>_API_KEY, falling back
// to OpenAIKeyEnv (see config.RunnerKeyEnvs).
type OpenAI struct {
	client *http.Client

	// keyEnvs are the API-key variables this block reads, most specific first.
	keyEnvs []string

	cfg llmConfig
}

// NewOpenAI builds the runner for a block named after the type, over an
// injected HTTP client so tests can point it at httptest servers and an
// injected lookup so they can supply a key without setting one process-wide.
func NewOpenAI(cfg map[string]any, client *http.Client, lookup environ.Lookup) (*OpenAI, error) {
	return NewOpenAIFor(OpenAIName, cfg, client, lookup)
}

// NewOpenAIFor builds the runner for a block under any name. The name decides
// which API key it reads first, so two openai-type blocks pointed at different
// vendors do not have to share one credential.
func NewOpenAIFor(block string, cfg map[string]any, client *http.Client, lookup environ.Lookup) (*OpenAI, error) {
	decoded, err := decodeLLMConfig(cfg, OpenAIName, defaultOpenAIBaseURL, 0, lookup)
	if err != nil {
		return nil, err
	}

	return &OpenAI{
		cfg:     decoded,
		client:  client,
		keyEnvs: config.RunnerKeyEnvs(block, OpenAIKeyEnv),
	}, nil
}

// OpenAIFactory adapts NewOpenAI to the registry contract with a
// backstopped HTTP client.
func OpenAIFactory(lookup environ.Lookup) runner.Factory {
	return OpenAIFactoryFor(OpenAIName, lookup)
}

// OpenAIFactoryFor is the registration for a `[runner.<name>]` block of this
// type under another name, which is what makes CAIRNOS_RUNNER_<NAME>_API_KEY
// reach the right runner.
func OpenAIFactoryFor(block string, lookup environ.Lookup) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		return NewOpenAIFor(block, cfg, &http.Client{Timeout: defaultLLMTimeout}, lookup)
	}
}

// Name implements runner.Runner.
func (*OpenAI) Name() string {
	return OpenAIName
}

// Validate implements runner.Runner: params must carry a string prompt, a
// model must come from params or config, and the API key must be set.
func (o *OpenAI) Validate(task *runner.Task) error {
	return o.cfg.validateTask(task, OpenAIName, o.keyEnvs)
}

// Execute implements runner.Runner.
func (o *OpenAI) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	return o.run(ctx, task, nil)
}

// ExecuteStream implements runner.Streamer over the API's SSE mode, sending
// content deltas as chunks while the full text still lands in Result.Output.
func (o *OpenAI) ExecuteStream(ctx context.Context, task *runner.Task, out chan<- runner.Chunk) (runner.Result, error) {
	return o.run(ctx, task, out)
}

// ExecuteWithTools implements runner.ToolCaller over the chat-completions
// tools mode, non-streaming: the ToolSpecs translate to function tools with
// tool_choice auto, and the response maps onto the agent-loop driver
// convention documented on pkg/runner.ToolCaller.
func (o *OpenAI) ExecuteWithTools(ctx context.Context, task *runner.Task, tools []runner.ToolSpec) (runner.Result, error) {
	if err := o.Validate(task); err != nil {
		return runner.Result{}, err
	}

	start := time.Now()

	payload := o.payload(task, false)
	payload.Tools = openaiToolDefs(tools)

	if len(payload.Tools) > 0 {
		// tool_choice without a tools array is a request error, so it rides
		// along only when the action offers tools.
		payload.ToolChoice = "auto"
	}

	response, err := sendJSON(ctx, o.client, OpenAIName,
		o.cfg.baseURL+chatCompletionsPath, o.authHeaders(), payload)
	if err != nil {
		return runner.Result{}, err
	}

	defer func() { _ = response.Body.Close() }()

	return o.decodeToolResponse(response.Body, start)
}

// run drives one chat-completions call. A nil stream means plain Execute; a
// non-nil stream flips the request into SSE mode.
func (o *OpenAI) run(ctx context.Context, task *runner.Task, stream chan<- runner.Chunk) (runner.Result, error) {
	if err := o.Validate(task); err != nil {
		return runner.Result{}, err
	}

	start := time.Now()

	response, err := sendJSON(ctx, o.client, OpenAIName,
		o.cfg.baseURL+chatCompletionsPath, o.authHeaders(), o.payload(task, stream != nil))
	if err != nil {
		return runner.Result{}, err
	}

	defer func() { _ = response.Body.Close() }()

	if stream == nil {
		return o.decodeResponse(response.Body, o.cfg.resolveOutputFormat(task), start)
	}

	return o.decodeStream(ctx, response.Body, stream, start)
}

// authHeaders builds the per-call auth header; the key reads at call time
// so a rotation needs no restart.
func (o *OpenAI) authHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + apiKey(o.cfg.env, o.keyEnvs)}
}

// payload assembles the request body for one task.
func (o *OpenAI) payload(task *runner.Task, streaming bool) openaiRequest {
	prompt, _ := task.Params[paramPrompt].(string)
	system, _ := task.Params[paramSystem].(string)

	messages := make([]openaiMessage, 0, 2)
	if system != "" {
		messages = append(messages, openaiMessage{Role: roleSystem, Content: system})
	}

	messages = append(messages, openaiMessage{Role: roleUser, Content: prompt})

	request := openaiRequest{
		Model:     o.cfg.resolveModel(task),
		Messages:  messages,
		MaxTokens: o.cfg.maxTokens,
	}

	if o.cfg.resolveOutputFormat(task) == outputFormatJSON {
		// The chat-completions JSON mode constrains the model to emit a single
		// valid JSON object, which decodeResponse then parses into Output.
		request.ResponseFormat = &openaiResponseFormat{Type: openaiResponseJSONObject}
	}

	if o.cfg.hasTemp {
		temperature := o.cfg.temperature
		request.Temperature = &temperature
	}

	if streaming {
		request.Stream = true
		// include_usage makes the final SSE chunk carry token counts, so
		// streamed calls report the same Usage as plain ones.
		request.StreamOptions = &openaiStreamOptions{IncludeUsage: true}
	}

	return request
}

// decodeResponse maps a non-streaming response body onto a Result, shaping
// Output per the resolved output format: text passes through, json parses the
// model's content into a map or array.
func (o *OpenAI) decodeResponse(body io.Reader, format string, start time.Time) (runner.Result, error) {
	decoded, err := decodeOpenAIBody(body)
	if err != nil {
		return runner.Result{}, err
	}

	output, err := formatModelOutput(OpenAIName, format, decoded.Choices[0].Message.Content)
	if err != nil {
		return runner.Result{}, err
	}

	return runner.Result{
		Output: output,
		Usage:  o.usage(decoded.Usage, start),
		Meta: map[string]string{
			keyModel:       decoded.Model,
			metaStopReason: decoded.Choices[0].FinishReason,
		},
	}, nil
}

// decodeToolResponse maps a tools-mode response onto the driver convention:
// the first tool call wins (extra parallel calls drop, per the ToolCaller
// contract), otherwise the text content is the final answer.
func (o *OpenAI) decodeToolResponse(body io.Reader, start time.Time) (runner.Result, error) {
	decoded, err := decodeOpenAIBody(body)
	if err != nil {
		return runner.Result{}, err
	}

	output, stopReason, err := openaiDriverOutput(&decoded.Choices[0].Message)
	if err != nil {
		return runner.Result{}, err
	}

	return runner.Result{
		Output: output,
		Usage:  o.usage(decoded.Usage, start),
		Meta: map[string]string{
			keyModel:       decoded.Model,
			metaStopReason: stopReason,
		},
	}, nil
}

// decodeOpenAIBody reads and unmarshals one non-streaming chat-completions
// response, enforcing the at-least-one-choice contract.
func decodeOpenAIBody(body io.Reader) (*openaiResponse, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxCapturedOutput))
	if err != nil {
		return nil, &runner.Error{
			Class: runner.ClassTransient, Code: OpenAIName + codeSuffixRead,
			Message: "read response: " + err.Error(), Cause: err,
		}
	}

	var decoded openaiResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, &runner.Error{
			Class: runner.ClassPermanent, Code: OpenAIName + codeSuffixBadResponse,
			Message: "decode response: " + err.Error(), Cause: err,
		}
	}

	if len(decoded.Choices) == 0 {
		return nil, runner.NewError(runner.ClassPermanent,
			OpenAIName+codeSuffixBadResponse, "response carried no choices")
	}

	return &decoded, nil
}

// openaiDriverOutput maps one assistant message onto the driver convention
// (pkg/runner.ToolCaller): a tool call becomes the convention map with its
// arguments decoded, plain content becomes the final answer.
func openaiDriverOutput(message *openaiMessage) (any, string, error) {
	if len(message.ToolCalls) == 0 {
		return message.Content, runner.StopReasonEndTurn, nil
	}

	call := message.ToolCalls[0]

	arguments := map[string]any{}

	if call.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil {
			return nil, "", &runner.Error{
				Class: runner.ClassPermanent, Code: OpenAIName + codeSuffixBadResponse,
				Message: "tool call arguments are not valid JSON: " + err.Error(), Cause: err,
			}
		}
	}

	return map[string]any{
		runner.ToolCallName:      call.Function.Name,
		runner.ToolCallArguments: arguments,
	}, runner.StopReasonToolUse, nil
}

// openaiToolDefs translates ToolSpecs into the function-tool wire shape.
func openaiToolDefs(tools []runner.ToolSpec) []openaiTool {
	defs := make([]openaiTool, 0, len(tools))

	for idx := range tools {
		defs = append(defs, openaiTool{
			Type: "function",
			Function: openaiToolFunction{
				Name:        tools[idx].Name,
				Description: tools[idx].Description,
				Parameters:  tools[idx].InputSchema,
			},
		})
	}

	return defs
}

// decodeStream walks the SSE body, forwarding content deltas and folding the
// final usage chunk into the Result.
func (o *OpenAI) decodeStream(ctx context.Context, body io.Reader, stream chan<- runner.Chunk, start time.Time) (runner.Result, error) {
	var (
		text         strings.Builder
		tokens       openaiUsage
		model        string
		finishReason string
	)

	err := readSSEEvents(body, func(event sseEvent) (bool, error) {
		if event.data == openaiStreamDone {
			return false, nil
		}

		var chunk openaiStreamChunk
		if err := json.Unmarshal([]byte(event.data), &chunk); err != nil {
			return false, &runner.Error{
				Class: runner.ClassPermanent, Code: OpenAIName + ".bad_stream",
				Message: "decode stream chunk: " + err.Error(), Cause: err,
			}
		}

		if chunk.Model != "" {
			model = chunk.Model
		}

		if chunk.Usage != nil {
			tokens = *chunk.Usage
		}

		if len(chunk.Choices) == 0 {
			return true, nil
		}

		if reason := chunk.Choices[0].FinishReason; reason != "" {
			finishReason = reason
		}

		if delta := chunk.Choices[0].Delta.Content; delta != "" {
			text.WriteString(delta)

			if stream != nil && !forwardChunk(ctx, stream, delta) {
				stream = nil
			}
		}

		return true, nil
	})
	if err != nil {
		return runner.Result{}, classifyStreamErr(ctx, OpenAIName, err)
	}

	return runner.Result{
		Output: text.String(),
		Usage:  o.usage(tokens, start),
		Meta:   map[string]string{keyModel: model, metaStopReason: finishReason},
	}, nil
}

// usage maps the provider's token accounting onto the engine's Usage shape.
func (o *OpenAI) usage(tokens openaiUsage, start time.Time) runner.Usage {
	return runner.Usage{
		InputTokens:  tokens.PromptTokens,
		OutputTokens: tokens.CompletionTokens,
		CachedTokens: tokens.PromptTokensDetails.CachedTokens,
		CostUSD:      o.cfg.cost(tokens.PromptTokens, tokens.CompletionTokens),
		Latency:      time.Since(start),
	}
}

// classifyStreamErr keeps an already-classified stream failure intact and
// marks anything else (a scanner error mid-stream) by transport rules, since
// a stream that died is a connection problem.
func classifyStreamErr(ctx context.Context, name string, err error) error {
	if _, classified := errors.AsType[*runner.Error](err); classified {
		return err
	}

	return classifyAPITransport(ctx, name, err)
}

// openaiRequest is the chat-completions request body.
type openaiRequest struct {
	Temperature    *float64              `json:"temperature,omitempty"`
	StreamOptions  *openaiStreamOptions  `json:"stream_options,omitempty"`
	ResponseFormat *openaiResponseFormat `json:"response_format,omitempty"`
	Model          string                `json:"model"`
	ToolChoice     string                `json:"tool_choice,omitempty"`
	Messages       []openaiMessage       `json:"messages"`
	Tools          []openaiTool          `json:"tools,omitempty"`
	MaxTokens      int64                 `json:"max_tokens,omitempty"`
	Stream         bool                  `json:"stream,omitempty"`
}

// openaiResponseJSONObject is the response_format type that flips the API into
// JSON mode, constraining the model to one valid JSON object.
const openaiResponseJSONObject = "json_object"

// openaiResponseFormat carries the chat-completions response_format request
// field; only the JSON-object mode is wired today.
type openaiResponseFormat struct {
	Type string `json:"type"`
}

// openaiMessage is one chat turn; it doubles as the delta shape in stream
// chunks because both carry role and content.
type openaiMessage struct {
	Role      string           `json:"role,omitempty"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
}

// openaiTool is one function tool in a tools-mode request.
type openaiTool struct {
	Type     string             `json:"type"`
	Function openaiToolFunction `json:"function"`
}

// openaiToolFunction carries a tool's schema both ways: Parameters rides
// requests, Arguments (a JSON-encoded string in this API) rides responses.
type openaiToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Arguments   string         `json:"arguments,omitempty"`
}

// openaiToolCall is one tool invocation in a response message.
type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openaiToolFunction `json:"function"`
}

// openaiStreamOptions tunes SSE mode.
type openaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// openaiResponse is the non-streaming chat-completions response body.
type openaiResponse struct {
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage"`
}

// openaiChoice is one completion candidate; Message is set on plain
// responses, Delta on stream chunks.
type openaiChoice struct {
	FinishReason string        `json:"finish_reason"`
	Message      openaiMessage `json:"message"`
	Delta        openaiMessage `json:"delta"`
}

// openaiUsage is the provider's token accounting.
type openaiUsage struct {
	PromptTokens        int64               `json:"prompt_tokens"`
	CompletionTokens    int64               `json:"completion_tokens"`
	PromptTokensDetails openaiPromptDetails `json:"prompt_tokens_details"`
}

// openaiPromptDetails breaks down prompt tokens; cached_tokens feeds the
// CachedTokens usage field.
type openaiPromptDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// openaiStreamChunk is one SSE data payload in streaming mode. Usage is a
// pointer because only the final chunk carries it.
type openaiStreamChunk struct {
	Usage   *openaiUsage   `json:"usage"`
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
}
