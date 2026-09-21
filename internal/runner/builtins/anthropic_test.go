package builtins_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// anthropicSuccessBody is the golden Messages response, priced like the
// OpenAI golden body so assertLLMUsage covers both providers.
const anthropicSuccessBody = `{
	"model": "test-model-1",
	"content": [
		{"type": "text", "text": "Hello "},
		{"type": "text", "text": "there"}
	],
	"stop_reason": "end_turn",
	"usage": {
		"input_tokens": 500000,
		"output_tokens": 250000,
		"cache_read_input_tokens": 128
	}
}`

// anthropicJSONContentBody is the golden Messages response when
// output_format=json: the joined text blocks form one JSON object the runner
// parses into Output.
const anthropicJSONContentBody = `{
	"model": "test-model-1",
	"content": [
		{"type": "text", "text": "{\"count\": 3,"},
		{"type": "text", "text": " \"ok\": true}"}
	],
	"stop_reason": "end_turn",
	"usage": {"input_tokens": 1, "output_tokens": 1, "cache_read_input_tokens": 0}
}`

// anthropicProseContentBody is the golden response when output_format=json but
// the model returned prose, which must fail with the bad_json code.
const anthropicProseContentBody = `{
	"model": "test-model-1",
	"content": [{"type": "text", "text": "Sorry, here is some prose."}],
	"stop_reason": "end_turn",
	"usage": {"input_tokens": 1, "output_tokens": 1, "cache_read_input_tokens": 0}
}`

// anthropicStreamBody is the golden Messages SSE stream: input tokens on
// message_start, two text deltas, output tokens on message_delta, and the
// stop event.
const anthropicStreamBody = `event: message_start
data: {"type":"message_start","message":{"model":"test-model-1","usage":{"input_tokens":500000,"output_tokens":1,"cache_read_input_tokens":128}}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":250000}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicSuccessReportsUsageAndCost(t *testing.T) {
	t.Parallel()

	requests := make(chan map[string]any, 1)

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil &&
			r.URL.Path == "/messages" &&
			r.Header.Get("X-Api-Key") == testAPIKey &&
			r.Header.Get("Anthropic-Version") == "2023-06-01" {
			requests <- decoded
		}

		_, _ = io.WriteString(w, anthropicSuccessBody)
	}))

	llm, err := builtins.NewAnthropic(pricedConfig(server.URL), server.Client(), anthropicKey(testAPIKey))
	if err != nil {
		t.Fatalf("new anthropic: %v", err)
	}

	result, err := llm.Execute(t.Context(), llmTask(map[string]any{promptKey: testPrompt}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != llmHelloText {
		t.Fatalf("output = %#v", result.Output)
	}

	assertLLMUsage(t, result.Usage)

	if result.Meta["stop_reason"] != "end_turn" {
		t.Fatalf("meta = %v", result.Meta)
	}

	select {
	case request := <-requests:
		// The Messages API requires max_tokens, so the config default must
		// land in the request body even when the operator set nothing.
		if request[modelKey] != testModel || request["max_tokens"] != float64(4096) {
			t.Fatalf("request = %v", request)
		}
	default:
		t.Fatal("server saw no versioned, authenticated messages request")
	}
}

func TestAnthropicJSONOutputParsesObjectAndInstructsModel(t *testing.T) {
	t.Parallel()

	requests := make(chan map[string]any, 1)

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil {
			requests <- decoded
		}

		_, _ = io.WriteString(w, anthropicJSONContentBody)
	}))

	llm, err := builtins.NewAnthropic(pricedConfig(server.URL), server.Client(), anthropicKey(testAPIKey))
	if err != nil {
		t.Fatalf("new anthropic: %v", err)
	}

	result, err := llm.Execute(t.Context(), llmTask(map[string]any{
		promptKey: testPrompt, systemKey: systemBrief, outputFormatKey: formatJSON,
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isMap := result.Output.(map[string]any)
	if !isMap {
		t.Fatalf("output is not a map: %#v", result.Output)
	}

	if output[jsonCountField] != float64(3) || output["ok"] != true {
		t.Fatalf("parsed output = %#v", output)
	}

	select {
	case request := <-requests:
		// The Messages API has no response_format flag, so json mode rides the
		// system prompt: the operator's text must survive and the JSON
		// instruction must be appended.
		system, _ := request["system"].(string)
		if !strings.Contains(system, "be brief") || !strings.Contains(system, "valid JSON value") {
			t.Fatalf("system prompt = %q", system)
		}
	default:
		t.Fatal("server saw no decodable request")
	}
}

func TestAnthropicJSONOutputRejectsProse(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, anthropicProseContentBody)
	}))

	llm, err := builtins.NewAnthropic(pricedConfig(server.URL), server.Client(), anthropicKey(testAPIKey))
	if err != nil {
		t.Fatalf("new anthropic: %v", err)
	}

	_, err = llm.Execute(t.Context(), llmTask(map[string]any{
		promptKey: testPrompt, outputFormatKey: formatJSON,
	}))

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	if classified.Class != runner.ClassPermanent || classified.Code != "anthropic.bad_json" {
		t.Fatalf("class/code = %v/%q, want permanent/anthropic.bad_json", classified.Class, classified.Code)
	}
}

func TestAnthropicDefaultOutputStaysPlainText(t *testing.T) {
	t.Parallel()

	requests := make(chan map[string]any, 1)

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil {
			requests <- decoded
		}

		_, _ = io.WriteString(w, anthropicSuccessBody)
	}))

	llm, err := builtins.NewAnthropic(pricedConfig(server.URL), server.Client(), anthropicKey(testAPIKey))
	if err != nil {
		t.Fatalf("new anthropic: %v", err)
	}

	result, err := llm.Execute(t.Context(), llmTask(map[string]any{promptKey: testPrompt, systemKey: systemBrief}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != llmHelloText {
		t.Fatalf("output = %#v, want plain text", result.Output)
	}

	select {
	case request := <-requests:
		// Without json mode the system prompt is the operator's text verbatim,
		// no appended instruction.
		if request[systemKey] != systemBrief {
			t.Fatalf("system prompt = %q, want untouched", request["system"])
		}
	default:
		t.Fatal("server saw no decodable request")
	}
}

func TestAnthropicClassifiesStatusCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		retryAfter string
		wantCode   string
		status     int
		wantClass  runner.ErrorClass
		wantRetry  time.Duration
	}{
		{name: case401, status: http.StatusUnauthorized, wantClass: runner.ClassPermanent, wantCode: "anthropic.auth"},
		{name: case429, status: http.StatusTooManyRequests, retryAfter: "3", wantClass: runner.ClassRateLimited, wantCode: "anthropic.rate_limited", wantRetry: 3 * time.Second},
		{name: case500, status: http.StatusInternalServerError, wantClass: runner.ClassTransient, wantCode: "anthropic.status"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}

				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
			}))

			llm, err := builtins.NewAnthropic(pricedConfig(server.URL), server.Client(), anthropicKey(testAPIKey))
			if err != nil {
				t.Fatalf("new anthropic: %v", err)
			}

			_, err = llm.Execute(t.Context(), llmTask(map[string]any{promptKey: testPrompt}))

			classified, isClassified := errors.AsType[*runner.Error](err)
			if !isClassified {
				t.Fatalf("error %v is not a runner.Error", err)
			}

			if classified.Class != tt.wantClass || classified.Code != tt.wantCode {
				t.Fatalf("class/code = %v/%q, want %v/%q", classified.Class, classified.Code, tt.wantClass, tt.wantCode)
			}

			if classified.RetryAfter != tt.wantRetry {
				t.Fatalf("retry after = %v, want %v", classified.RetryAfter, tt.wantRetry)
			}
		})
	}
}

func TestAnthropicStreamsChunksThenUsage(t *testing.T) {
	t.Parallel()

	requests := make(chan map[string]any, 1)

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil {
			requests <- decoded
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicStreamBody)
	}))

	llm, err := builtins.NewAnthropic(pricedConfig(server.URL), server.Client(), anthropicKey(testAPIKey))
	if err != nil {
		t.Fatalf("new anthropic: %v", err)
	}

	out := make(chan runner.Chunk, 16)

	result, err := llm.ExecuteStream(t.Context(), llmTask(map[string]any{promptKey: testPrompt}), out)
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}

	close(out)

	var deltas []string

	for chunk := range out {
		deltas = append(deltas, chunk.Data)
	}

	if len(deltas) != 2 || deltas[0] != "Hel" || deltas[1] != "lo" {
		t.Fatalf("chunks = %v", deltas)
	}

	if result.Output != "Hello" {
		t.Fatalf("output = %#v", result.Output)
	}

	assertLLMUsage(t, result.Usage)

	if result.Meta["stop_reason"] != "end_turn" || result.Meta[modelKey] != "test-model-1" {
		t.Fatalf("meta = %v", result.Meta)
	}

	select {
	case request := <-requests:
		if request["stream"] != true {
			t.Fatalf("request = %v", request)
		}
	default:
		t.Fatal("server saw no stream request")
	}
}

func TestAnthropicValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		cfg      map[string]any
		params   map[string]any
		key      string
		resolved bool
		wantErr  bool
	}{
		{
			name: caseMissingPrompt, cfg: map[string]any{modelKey: testModel},
			params: map[string]any{}, key: testAPIKey, resolved: true, wantErr: true,
		},
		{
			name: "missing model everywhere", cfg: nil,
			params: map[string]any{promptKey: testPrompt}, key: testAPIKey, resolved: true, wantErr: true,
		},
		{
			name: "unresolved model reference defers", cfg: nil,
			params: map[string]any{promptKey: testPrompt, modelKey: unresolved}, key: testAPIKey, wantErr: false,
		},
		{
			name: "missing api key", cfg: map[string]any{modelKey: testModel},
			params: map[string]any{promptKey: testPrompt}, key: "", resolved: true, wantErr: true,
		},
		{
			name: "config model and key suffice", cfg: map[string]any{modelKey: testModel},
			params: map[string]any{promptKey: testPrompt}, key: testAPIKey, resolved: true, wantErr: false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			llm, err := builtins.NewAnthropic(tt.cfg, &http.Client{Timeout: time.Second}, anthropicKey(tt.key))
			if err != nil {
				t.Fatalf("new anthropic: %v", err)
			}

			err = llm.Validate(&runner.Task{
				RunID:      testRunID,
				ActionName: actionLLM,
				Params:     tt.params,
				Resolved:   tt.resolved,
			})

			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("validate error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

// anthropicKey answers the anthropic runner's key variable and nothing else.
func anthropicKey(value string) environ.Lookup {
	return keyEnv(builtins.AnthropicKeyEnv, value)
}
