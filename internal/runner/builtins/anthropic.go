package builtins

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/pkg/runner"
)

// AnthropicName is the registry name of the Anthropic Messages API runner.
const AnthropicName = "anthropic"

// AnthropicKeyEnv is the API-key variable an anthropic-type block falls back
// to, behind its own CAIRNOS_RUNNER_<NAME>_API_KEY. Exported for the same
// reason as OpenAIKeyEnv.
const AnthropicKeyEnv = "CAIRNOS_RUNNER_ANTHROPIC_API_KEY" // #nosec G101 -- environment variable name, not a credential

// defaultAnthropicBaseURL targets the hosted API; base_url in config points
// the runner at gateways and proxies instead.
const defaultAnthropicBaseURL = "https://api.anthropic.com/v1"

// messagesPath is the Messages endpoint under the base URL.
const messagesPath = "/messages"

// anthropicVersion pins the API revision every request declares. The
// Messages API requires the header; bumping it is a deliberate change, not a
// drift.
const anthropicVersion = "2023-06-01"

// defaultAnthropicMaxTokens backstops the required max_tokens request field
// when the config block does not set one.
const defaultAnthropicMaxTokens = 4096

// Event types in the Messages streaming protocol the runner acts on.
const (
	anthropicEventMessageStart = "message_start"
	anthropicEventBlockDelta   = "content_block_delta"
	anthropicEventMessageDelta = "message_delta"
	anthropicEventMessageStop  = "message_stop"
	anthropicEventError        = "error"
)

// anthropicBlockToolUse is the content-block type carrying a tool call.
const anthropicBlockToolUse = "tool_use"

// Anthropic calls the Messages API, a Phase 1a API runner
// (.agents/spec/03-runner-interface.md).
//
// Config: model (required to run), base_url, max_tokens (the API requires it;
// default 4096), temperature, and price_in/price_out in USD per million
// tokens. Params: prompt (required), system, and model (overrides config). The
// API key is read at call time from the block's own
// CAIRNOS_RUNNER_<NAME>_API_KEY, falling back to AnthropicKeyEnv (see
// config.RunnerKeyEnvs).
type Anthropic struct {
	client *http.Client

	// keyEnvs are the API-key variables this block reads, most specific first.
	keyEnvs []string

	cfg llmConfig
}

// NewAnthropic builds the runner for a block named after the type, over an
// injected HTTP client so tests can point it at httptest servers and an
// injected lookup so they can supply a key without setting one process-wide.
func NewAnthropic(cfg map[string]any, client *http.Client, lookup environ.Lookup) (*Anthropic, error) {
	return NewAnthropicFor(AnthropicName, cfg, client, lookup)
}

// NewAnthropicFor builds the runner for a block under any name. The name
// decides which API key it reads first, so two anthropic-type blocks pointed at
// different gateways do not have to share one credential.
func NewAnthropicFor(
	block string, cfg map[string]any, client *http.Client, lookup environ.Lookup,
) (*Anthropic, error) {
	decoded, err := decodeLLMConfig(cfg, AnthropicName, defaultAnthropicBaseURL, defaultAnthropicMaxTokens, lookup)
	if err != nil {
		return nil, err
	}

	return &Anthropic{
		cfg:     decoded,
		client:  client,
		keyEnvs: config.RunnerKeyEnvs(block, AnthropicKeyEnv),
	}, nil
}

// AnthropicFactory adapts NewAnthropic to the registry contract with a
// backstopped HTTP client. A nil transport keeps http.DefaultTransport; a test
// passes one that answers in the process instead of over a socket.
func AnthropicFactory(lookup environ.Lookup, transport http.RoundTripper) runner.Factory {
	return AnthropicFactoryFor(AnthropicName, lookup, transport)
}

// AnthropicFactoryFor is the registration for a `[runner.<name>]` block of this
// type under another name.
func AnthropicFactoryFor(block string, lookup environ.Lookup, transport http.RoundTripper) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		return NewAnthropicFor(block, cfg, &http.Client{Timeout: defaultLLMTimeout, Transport: transport}, lookup)
	}
}

// Name implements runner.Runner.
func (*Anthropic) Name() string {
	return AnthropicName
}

// Validate implements runner.Runner: params must carry a string prompt, a
// model must come from params or config, and the API key must be set.
func (a *Anthropic) Validate(task *runner.Task) error {
	return a.cfg.validateTask(task, AnthropicName, a.keyEnvs)
}

// Execute implements runner.Runner.
func (a *Anthropic) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	return a.ExecuteStream(ctx, task, nil)
}

// ExecuteWithTools implements runner.ToolCaller over the Messages tools
// mode, non-streaming: ToolSpecs translate directly (the Messages tool
// shape matches ToolSpec field for field), and tool_use content blocks map
// onto the agent-loop driver convention documented on pkg/runner.ToolCaller.
func (a *Anthropic) ExecuteWithTools(ctx context.Context, task *runner.Task, tools []runner.ToolSpec) (runner.Result, error) {
	if err := a.Validate(task); err != nil {
		return runner.Result{}, err
	}

	start := time.Now()

	payload := a.payload(task, false)
	payload.Tools = anthropicToolDefs(tools)

	response, err := sendJSON(ctx, a.client, AnthropicName,
		a.cfg.baseURL+messagesPath, a.authHeaders(), payload)
	if err != nil {
		return runner.Result{}, err
	}

	defer func() { _ = response.Body.Close() }()

	return a.decodeToolResponse(response.Body, start)
}

// ExecuteStream implements runner.Streamer over the API's SSE mode, sending
// text deltas as chunks while the full text still lands in Result.Output. A
// nil stream, which Execute passes, keeps the Messages call out of SSE mode.
func (a *Anthropic) ExecuteStream(ctx context.Context, task *runner.Task, stream chan<- runner.Chunk) (runner.Result, error) {
	if err := a.Validate(task); err != nil {
		return runner.Result{}, err
	}

	start := time.Now()

	response, err := sendJSON(ctx, a.client, AnthropicName,
		a.cfg.baseURL+messagesPath, a.authHeaders(), a.payload(task, stream != nil))
	if err != nil {
		return runner.Result{}, err
	}

	defer func() { _ = response.Body.Close() }()

	if stream == nil {
		return a.decodeResponse(response.Body, a.cfg.resolveOutputFormat(task), start)
	}

	return a.decodeStream(ctx, response.Body, stream, start)
}

// authHeaders builds the per-call auth headers; the key reads at call time
// so a rotation needs no restart.
func (a *Anthropic) authHeaders() map[string]string {
	return map[string]string{
		"x-api-key":         apiKey(a.cfg.env, a.keyEnvs),
		"anthropic-version": anthropicVersion,
	}
}

// anthropicJSONInstruction is appended to the system prompt in json mode. The
// Messages API carries no response_format flag, so the JSON constraint rides
// the system prompt; decodeResponse still parses and fails loudly on prose.
const anthropicJSONInstruction = "Respond with a single valid JSON value and nothing else: no prose, no markdown fences."

// payload assembles the request body for one task. MaxTokens is always set
// because the Messages API rejects requests without it.
func (a *Anthropic) payload(task *runner.Task, streaming bool) anthropicRequest {
	prompt, _ := task.Params[paramPrompt].(string)
	system, _ := task.Params[paramSystem].(string)

	if a.cfg.resolveOutputFormat(task) == outputFormatJSON {
		system = appendJSONInstruction(system)
	}

	request := anthropicRequest{
		Model:     a.cfg.resolveModel(task),
		MaxTokens: a.cfg.maxTokens,
		Messages:  []anthropicMessage{{Role: roleUser, Content: prompt}},
		System:    system,
		Stream:    streaming,
	}

	if a.cfg.hasTemp {
		temperature := a.cfg.temperature
		request.Temperature = &temperature
	}

	return request
}

// appendJSONInstruction folds the JSON-mode constraint into the system prompt,
// keeping any operator-supplied system text ahead of it.
func appendJSONInstruction(system string) string {
	if system == "" {
		return anthropicJSONInstruction
	}

	return system + "\n\n" + anthropicJSONInstruction
}

// decodeResponse maps a non-streaming response body onto a Result, shaping
// Output per the resolved output format: text passes through, json parses the
// joined response text into a map or array.
func (a *Anthropic) decodeResponse(body io.Reader, format string, start time.Time) (runner.Result, error) {
	decoded, err := decodeAnthropicBody(body)
	if err != nil {
		return runner.Result{}, err
	}

	output, err := formatModelOutput(AnthropicName, format, joinAnthropicText(decoded.Content))
	if err != nil {
		return runner.Result{}, err
	}

	return runner.Result{
		Output: output,
		Usage:  a.usage(decoded.Usage, start),
		Meta: map[string]string{
			keyModel:       decoded.Model,
			metaStopReason: decoded.StopReason,
		},
	}, nil
}

// decodeToolResponse maps a tools-mode response onto the driver convention:
// the first tool_use block wins (extra parallel calls drop, per the
// ToolCaller contract), otherwise the joined text is the final answer.
func (a *Anthropic) decodeToolResponse(body io.Reader, start time.Time) (runner.Result, error) {
	decoded, err := decodeAnthropicBody(body)
	if err != nil {
		return runner.Result{}, err
	}

	output, stopReason := anthropicDriverOutput(decoded.Content)

	return runner.Result{
		Output: output,
		Usage:  a.usage(decoded.Usage, start),
		Meta: map[string]string{
			keyModel:       decoded.Model,
			metaStopReason: stopReason,
		},
	}, nil
}

// decodeAnthropicBody reads and unmarshals one non-streaming Messages
// response, enforcing the at-least-one-block contract.
func decodeAnthropicBody(body io.Reader) (*anthropicResponse, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxCapturedOutput))
	if err != nil {
		return nil, &runner.Error{
			Class: runner.ClassTransient, Code: AnthropicName + codeSuffixRead,
			Message: "read response: " + err.Error(), Cause: err,
		}
	}

	var decoded anthropicResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, &runner.Error{
			Class: runner.ClassPermanent, Code: AnthropicName + codeSuffixBadResponse,
			Message: "decode response: " + err.Error(), Cause: err,
		}
	}

	if len(decoded.Content) == 0 {
		return nil, taskError(runner.ClassPermanent,
			AnthropicName+codeSuffixBadResponse, "response carried no content")
	}

	return &decoded, nil
}

// anthropicDriverOutput maps response content blocks onto the driver
// convention (pkg/runner.ToolCaller).
func anthropicDriverOutput(blocks []anthropicContent) (any, string) {
	for idx := range blocks {
		if blocks[idx].Type != anthropicBlockToolUse {
			continue
		}

		arguments := blocks[idx].Input
		if arguments == nil {
			arguments = map[string]any{}
		}

		return map[string]any{
			runner.ToolCallName:      blocks[idx].Name,
			runner.ToolCallArguments: arguments,
		}, runner.StopReasonToolUse
	}

	return joinAnthropicText(blocks), runner.StopReasonEndTurn
}

// anthropicToolDefs translates ToolSpecs into the Messages tools array. The
// API requires input_schema on every tool, so a Describer that returned
// none gets the permissive object schema.
func anthropicToolDefs(tools []runner.ToolSpec) []anthropicTool {
	defs := make([]anthropicTool, 0, len(tools))

	for idx := range tools {
		schema := tools[idx].InputSchema
		if schema == nil {
			schema = map[string]any{schemaType: schemaObject}
		}

		defs = append(defs, anthropicTool{
			Name:        tools[idx].Name,
			Description: tools[idx].Description,
			InputSchema: schema,
		})
	}

	return defs
}

// decodeStream walks the Messages SSE protocol: message_start carries input
// tokens, content_block_delta carries text, message_delta carries output
// tokens and the stop reason, message_stop ends the stream.
func (a *Anthropic) decodeStream(ctx context.Context, body io.Reader, stream chan<- runner.Chunk, start time.Time) (runner.Result, error) {
	var (
		text       strings.Builder
		tokens     anthropicUsage
		model      string
		stopReason string
	)

	err := readSSEEvents(body, func(event sseEvent) (bool, error) {
		var decoded anthropicStreamEvent
		if err := json.Unmarshal([]byte(event.data), &decoded); err != nil {
			return false, &runner.Error{
				Class: runner.ClassPermanent, Code: AnthropicName + ".bad_stream",
				Message: "decode stream event: " + err.Error(), Cause: err,
			}
		}

		switch decoded.Type {
		case anthropicEventMessageStart:
			if decoded.Message != nil {
				model = decoded.Message.Model
				tokens.InputTokens = decoded.Message.Usage.InputTokens
				tokens.CacheReadInputTokens = decoded.Message.Usage.CacheReadInputTokens
			}
		case anthropicEventBlockDelta:
			if decoded.Delta == nil || decoded.Delta.Text == "" {
				return true, nil
			}

			text.WriteString(decoded.Delta.Text)

			if stream != nil && !forwardChunk(ctx, stream, decoded.Delta.Text) {
				stream = nil
			}
		case anthropicEventMessageDelta:
			if decoded.Usage != nil {
				tokens.OutputTokens = decoded.Usage.OutputTokens
			}

			if decoded.Delta != nil && decoded.Delta.StopReason != "" {
				stopReason = decoded.Delta.StopReason
			}
		case anthropicEventMessageStop:
			return false, nil
		case anthropicEventError:
			return false, classifyAnthropicStreamError(decoded.Error)
		}

		return true, nil
	})
	if err != nil {
		return runner.Result{}, classifyStreamErr(ctx, AnthropicName, err)
	}

	return runner.Result{
		Output: text.String(),
		Usage:  a.usage(tokens, start),
		Meta:   map[string]string{keyModel: model, metaStopReason: stopReason},
	}, nil
}

// usage maps the provider's token accounting onto the engine's Usage shape.
func (a *Anthropic) usage(tokens anthropicUsage, start time.Time) runner.Usage {
	return runner.Usage{
		InputTokens:  tokens.InputTokens,
		OutputTokens: tokens.OutputTokens,
		CachedTokens: tokens.CacheReadInputTokens,
		CostUSD:      a.cfg.cost(tokens.InputTokens, tokens.OutputTokens),
		Latency:      time.Since(start),
	}
}

// classifyAnthropicStreamError maps an in-stream error event: overloaded
// servers are transient, in-stream rate limits keep their class, everything
// else is permanent because re-sending the same request changes nothing.
func classifyAnthropicStreamError(detail *anthropicErrorDetail) error {
	var kind string

	message := "stream error"

	if detail != nil {
		message = detail.Message
		kind = detail.Type
	}

	switch kind {
	case "overloaded_error":
		return &runner.Error{Class: runner.ClassTransient, Code: AnthropicName + codeSuffixStatus, Message: message}
	case "rate_limit_error":
		return &runner.Error{Class: runner.ClassRateLimited, Code: AnthropicName + codeSuffixRateLimited, Message: message}
	default:
		return &runner.Error{Class: runner.ClassPermanent, Code: AnthropicName + codeSuffixStatus, Message: message}
	}
}

// joinAnthropicText concatenates the text blocks of a response. Blocks are
// contiguous spans of one message, so no separator is inserted.
func joinAnthropicText(blocks []anthropicContent) string {
	var (
		text  strings.Builder
		total int
	)

	for _, block := range blocks {
		total += len(block.Text)
	}

	text.Grow(total)

	for _, block := range blocks {
		text.WriteString(block.Text)
	}

	return text.String()
}

// anthropicRequest is the Messages request body.
type anthropicRequest struct {
	Temperature *float64           `json:"temperature,omitempty"`
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	MaxTokens   int64              `json:"max_tokens"`
	Stream      bool               `json:"stream,omitempty"`
}

// anthropicTool is one entry of the Messages tools array; it matches
// runner.ToolSpec field for field.
type anthropicTool struct {
	InputSchema map[string]any `json:"input_schema"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
}

// anthropicMessage is one chat turn.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicResponse is the non-streaming Messages response body.
type anthropicResponse struct {
	Model      string             `json:"model"`
	StopReason string             `json:"stop_reason"`
	Content    []anthropicContent `json:"content"`
	Usage      anthropicUsage     `json:"usage"`
}

// anthropicContent is one response content block: text blocks feed plain
// output, tool_use blocks carry a tool call's name and input.
type anthropicContent struct {
	Input map[string]any `json:"input,omitempty"`
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name,omitempty"`
}

// anthropicUsage is the provider's token accounting.
type anthropicUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
}

// anthropicStreamEvent is the union shape of the Messages SSE payloads; each
// event type fills a different subset of the pointers.
type anthropicStreamEvent struct {
	Message *anthropicStreamStart `json:"message"`
	Delta   *anthropicStreamDelta `json:"delta"`
	Usage   *anthropicUsage       `json:"usage"`
	Error   *anthropicErrorDetail `json:"error"`
	Type    string                `json:"type"`
}

// anthropicStreamStart is the message envelope inside message_start.
type anthropicStreamStart struct {
	Model string         `json:"model"`
	Usage anthropicUsage `json:"usage"`
}

// anthropicStreamDelta carries either a text fragment (content_block_delta)
// or the closing stop reason (message_delta).
type anthropicStreamDelta struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	StopReason string `json:"stop_reason"`
}

// anthropicErrorDetail is the error envelope of an in-stream error event.
type anthropicErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
