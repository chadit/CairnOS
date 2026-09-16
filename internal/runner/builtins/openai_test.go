package builtins_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture names for the LLM API runner tests.
const (
	modelKey        = "model"
	baseURLKey      = "base_url"
	priceInKey      = "price_in"
	priceOutKey     = "price_out"
	testModel       = "test-model"
	testAPIKey      = "sk-test" // #nosec G101 -- fixture value for httptest servers, not a credential
	actionLLM       = "llm"
	unresolved      = "${{ pick.output }}"
	outputFormatKey = "output_format"
	formatJSON      = "json"
	jsonCountField  = "count"
	systemKey       = "system"
	systemBrief     = "be brief"
)

// Case names the provider classification tables share.
const (
	case401           = "401 is permanent auth"
	case429           = "429 honors retry-after"
	case500           = "500 is transient"
	caseMissingPrompt = "missing prompt"
)

// openaiSuccessBody is the golden non-streaming chat-completions response.
// 500k input at $2/M plus 250k output at $4/M prices to exactly 2.0 USD,
// keeping the float comparison exact.
const openaiSuccessBody = `{
	"model": "test-model-1",
	"choices": [
		{"message": {"role": "assistant", "content": "Hello there"}, "finish_reason": "stop"}
	],
	"usage": {
		"prompt_tokens": 500000,
		"completion_tokens": 250000,
		"prompt_tokens_details": {"cached_tokens": 128}
	}
}`

// openaiJSONContentBody is the golden response when output_format=json: the
// model's content is itself a JSON object, which the runner parses into Output.
const openaiJSONContentBody = `{
	"model": "test-model-1",
	"choices": [
		{"message": {"role": "assistant", "content": "{\"count\": 3, \"ok\": true}"}, "finish_reason": "stop"}
	],
	"usage": {
		"prompt_tokens": 500000,
		"completion_tokens": 250000,
		"prompt_tokens_details": {"cached_tokens": 128}
	}
}`

// openaiProseContentBody is the golden response when output_format=json but the
// model returned prose, which must fail with the bad_json code.
const openaiProseContentBody = `{
	"model": "test-model-1",
	"choices": [
		{"message": {"role": "assistant", "content": "Sorry, here is some prose."}, "finish_reason": "stop"}
	],
	"usage": {"prompt_tokens": 1, "completion_tokens": 1}
}`

// openaiStreamBody is the golden SSE stream: two content deltas, a finish
// chunk, the include_usage summary chunk, and the DONE sentinel.
const openaiStreamBody = `data: {"model":"test-model-1","choices":[{"delta":{"role":"assistant","content":"Hel"}}]}

data: {"choices":[{"delta":{"content":"lo"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":500000,"completion_tokens":250000,"prompt_tokens_details":{"cached_tokens":128}}}

data: [DONE]

`

func llmTask(params map[string]any) *runner.Task {
	return &runner.Task{RunID: testRunID, ActionName: actionLLM, Params: params, Resolved: true}
}

func pricedConfig(url string) map[string]any {
	return map[string]any{
		modelKey:    testModel,
		baseURLKey:  url,
		priceInKey:  2.0,
		priceOutKey: 4.0,
	}
}

// assertLLMUsage checks the token and cost mapping every golden body above
// is built around.
func assertLLMUsage(t *testing.T, usage runner.Usage) {
	t.Helper()

	if usage.InputTokens != 500_000 || usage.OutputTokens != 250_000 || usage.CachedTokens != 128 {
		t.Fatalf("usage tokens = %+v", usage)
	}

	if usage.CostUSD != 2.0 {
		t.Fatalf("cost = %v, want 2.0", usage.CostUSD)
	}
}

func TestOpenAISuccessReportsUsageAndCost(t *testing.T) {
	t.Setenv(builtins.OpenAIKeyEnv, testAPIKey)

	requests := make(chan map[string]any, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil &&
			r.URL.Path == "/chat/completions" &&
			r.Header.Get("Authorization") == "Bearer "+testAPIKey {
			requests <- decoded
		}

		_, _ = io.WriteString(w, openaiSuccessBody)
	}))
	defer server.Close()

	llm, err := builtins.NewOpenAI(pricedConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("new openai: %v", err)
	}

	result, err := llm.Execute(t.Context(), llmTask(map[string]any{promptKey: testPrompt, systemKey: systemBrief}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != llmHelloText {
		t.Fatalf("output = %#v", result.Output)
	}

	assertLLMUsage(t, result.Usage)

	if result.Meta[modelKey] != "test-model-1" || result.Meta["stop_reason"] != "stop" {
		t.Fatalf("meta = %v", result.Meta)
	}

	select {
	case request := <-requests:
		messages, _ := request["messages"].([]any)
		if request[modelKey] != testModel || len(messages) != 2 {
			t.Fatalf("request = %v", request)
		}
	default:
		t.Fatal("server saw no authorized chat-completions request")
	}
}

func TestOpenAIJSONOutputParsesObjectAndSetsResponseFormat(t *testing.T) {
	t.Setenv(builtins.OpenAIKeyEnv, testAPIKey)

	requests := make(chan map[string]any, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil {
			requests <- decoded
		}

		_, _ = io.WriteString(w, openaiJSONContentBody)
	}))
	defer server.Close()

	llm, err := builtins.NewOpenAI(pricedConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("new openai: %v", err)
	}

	result, err := llm.Execute(t.Context(), llmTask(map[string]any{
		promptKey: testPrompt, outputFormatKey: formatJSON,
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isMap := result.Output.(map[string]any)
	if !isMap {
		t.Fatalf("output is not a map: %#v", result.Output)
	}

	// The count field must be reachable for ${{ action.output.count }} > 0 to
	// work downstream, the whole point of json mode. JSON numbers decode to
	// float64.
	if output[jsonCountField] != float64(3) || output["ok"] != true {
		t.Fatalf("parsed output = %#v", output)
	}

	select {
	case request := <-requests:
		format, _ := request["response_format"].(map[string]any)
		if format["type"] != "json_object" {
			t.Fatalf("response_format = %v, want json_object", request["response_format"])
		}
	default:
		t.Fatal("server saw no decodable request")
	}
}

func TestOpenAIJSONOutputRejectsProse(t *testing.T) {
	t.Setenv(builtins.OpenAIKeyEnv, testAPIKey)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, openaiProseContentBody)
	}))
	defer server.Close()

	llm, err := builtins.NewOpenAI(pricedConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("new openai: %v", err)
	}

	_, err = llm.Execute(t.Context(), llmTask(map[string]any{
		promptKey: testPrompt, outputFormatKey: formatJSON,
	}))

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	if classified.Class != runner.ClassPermanent || classified.Code != "openai.bad_json" {
		t.Fatalf("class/code = %v/%q, want permanent/openai.bad_json", classified.Class, classified.Code)
	}
}

func TestOpenAIDefaultOutputStaysPlainText(t *testing.T) {
	t.Setenv(builtins.OpenAIKeyEnv, testAPIKey)

	requests := make(chan map[string]any, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil {
			requests <- decoded
		}

		_, _ = io.WriteString(w, openaiSuccessBody)
	}))
	defer server.Close()

	llm, err := builtins.NewOpenAI(pricedConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("new openai: %v", err)
	}

	result, err := llm.Execute(t.Context(), llmTask(map[string]any{promptKey: testPrompt}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != llmHelloText {
		t.Fatalf("output = %#v, want plain text", result.Output)
	}

	select {
	case request := <-requests:
		if _, present := request["response_format"]; present {
			t.Fatalf("response_format leaked into a plain-text request: %v", request)
		}
	default:
		t.Fatal("server saw no decodable request")
	}
}

func TestOpenAIClassifiesStatusCodes(t *testing.T) {
	cases := []struct {
		name       string
		retryAfter string
		wantCode   string
		status     int
		wantClass  runner.ErrorClass
		wantRetry  time.Duration
	}{
		{name: case401, status: http.StatusUnauthorized, wantClass: runner.ClassPermanent, wantCode: "openai.auth"},
		{name: case429, status: http.StatusTooManyRequests, retryAfter: "3", wantClass: runner.ClassRateLimited, wantCode: "openai.rate_limited", wantRetry: 3 * time.Second},
		{name: case500, status: http.StatusInternalServerError, wantClass: runner.ClassTransient, wantCode: "openai.status"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(builtins.OpenAIKeyEnv, testAPIKey)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}

				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
			}))
			defer server.Close()

			llm, err := builtins.NewOpenAI(pricedConfig(server.URL), server.Client())
			if err != nil {
				t.Fatalf("new openai: %v", err)
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

func TestOpenAIStreamsChunksThenUsage(t *testing.T) {
	t.Setenv(builtins.OpenAIKeyEnv, testAPIKey)

	requests := make(chan map[string]any, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil {
			requests <- decoded
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openaiStreamBody)
	}))
	defer server.Close()

	llm, err := builtins.NewOpenAI(pricedConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("new openai: %v", err)
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

	select {
	case request := <-requests:
		options, _ := request["stream_options"].(map[string]any)
		if request["stream"] != true || options["include_usage"] != true {
			t.Fatalf("request = %v", request)
		}
	default:
		t.Fatal("server saw no stream request")
	}
}

func TestOpenAIValidate(t *testing.T) {
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
		{
			name: "non-string system", cfg: map[string]any{modelKey: testModel},
			params: map[string]any{promptKey: testPrompt, systemKey: int64(1)}, key: testAPIKey, resolved: true, wantErr: true,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(builtins.OpenAIKeyEnv, tt.key)

			llm, err := builtins.NewOpenAI(tt.cfg, &http.Client{Timeout: time.Second})
			if err != nil {
				t.Fatalf("new openai: %v", err)
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
