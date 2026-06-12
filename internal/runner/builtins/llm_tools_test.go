package builtins_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixtures for the ToolCaller tests; llmHelloText is the answer the
// provider golden success bodies carry.
const (
	toolNameMemory = "memory"
	stopReasonKey  = "stop_reason"
	llmHelloText   = "Hello there"
)

// memoryToolSpec is the descriptor offered in every ToolCaller test.
func memoryToolSpec() []runner.ToolSpec {
	return []runner.ToolSpec{{
		Name:        toolNameMemory,
		Description: "search memory",
		InputSchema: map[string]any{"type": "object"},
	}}
}

// wantMemorySearchCall is the convention-shaped output both providers must
// produce from their golden tool-call bodies.
func wantMemorySearchCall() map[string]any {
	return map[string]any{
		"tool":      toolNameMemory,
		"arguments": map[string]any{"op": opSearch, queryKey: "failures"},
	}
}

// openaiToolCallBody is the golden tools-mode response carrying one tool
// call, priced like the other golden bodies so assertLLMUsage covers it.
const openaiToolCallBody = `{
	"model": "test-model-1",
	"choices": [
		{"message": {"role": "assistant", "tool_calls": [
			{"id": "call_1", "type": "function",
			 "function": {"name": "memory", "arguments": "{\"op\":\"search\",\"query\":\"failures\"}"}}
		]}, "finish_reason": "tool_calls"}
	],
	"usage": {
		"prompt_tokens": 500000,
		"completion_tokens": 250000,
		"prompt_tokens_details": {"cached_tokens": 128}
	}
}`

// anthropicToolUseBody is the golden Messages response carrying one
// tool_use block alongside thinking text.
const anthropicToolUseBody = `{
	"model": "test-model-1",
	"content": [
		{"type": "text", "text": "Let me check."},
		{"type": "tool_use", "id": "tu_1", "name": "memory",
		 "input": {"op": "search", "query": "failures"}}
	],
	"stop_reason": "tool_use",
	"usage": {
		"input_tokens": 500000,
		"output_tokens": 250000,
		"cache_read_input_tokens": 128
	}
}`

func TestOpenAIExecuteWithToolsParsesToolCall(t *testing.T) {
	t.Setenv(openaiKeyEnv, testAPIKey)

	requests := make(chan map[string]any, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil {
			requests <- decoded
		}

		_, _ = io.WriteString(w, openaiToolCallBody)
	}))
	defer server.Close()

	llm, err := builtins.NewOpenAI(pricedConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("new openai: %v", err)
	}

	result, err := llm.ExecuteWithTools(t.Context(), llmTask(map[string]any{promptKey: testPrompt}), memoryToolSpec())
	if err != nil {
		t.Fatalf("execute with tools: %v", err)
	}

	if !reflect.DeepEqual(result.Output, wantMemorySearchCall()) {
		t.Fatalf("output = %#v, want the driver-convention tool call", result.Output)
	}

	if result.Meta[stopReasonKey] != runner.StopReasonToolUse {
		t.Fatalf("stop reason = %q, want %q", result.Meta[stopReasonKey], runner.StopReasonToolUse)
	}

	assertLLMUsage(t, result.Usage)

	select {
	case request := <-requests:
		assertOpenAIToolRequest(t, request)
	default:
		t.Fatal("server saw no chat-completions request")
	}
}

// assertOpenAIToolRequest checks the request carried the tools array and
// tool_choice auto.
func assertOpenAIToolRequest(t *testing.T, request map[string]any) {
	t.Helper()

	if request["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", request["tool_choice"])
	}

	tools, _ := request["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("request tools = %#v, want one function tool", request["tools"])
	}

	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)

	if tool["type"] != "function" || fn["name"] != toolNameMemory {
		t.Fatalf("tool def = %#v, want a function tool named %q", tool, toolNameMemory)
	}
}

func TestOpenAIExecuteWithToolsReturnsFinalText(t *testing.T) {
	t.Setenv(openaiKeyEnv, testAPIKey)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, openaiSuccessBody)
	}))
	defer server.Close()

	llm, err := builtins.NewOpenAI(pricedConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("new openai: %v", err)
	}

	result, err := llm.ExecuteWithTools(t.Context(), llmTask(map[string]any{promptKey: testPrompt}), memoryToolSpec())
	if err != nil {
		t.Fatalf("execute with tools: %v", err)
	}

	if result.Output != llmHelloText {
		t.Fatalf("output = %#v, want the final text", result.Output)
	}

	if result.Meta[stopReasonKey] != runner.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want %q", result.Meta[stopReasonKey], runner.StopReasonEndTurn)
	}

	assertLLMUsage(t, result.Usage)
}

func TestAnthropicExecuteWithToolsParsesToolUse(t *testing.T) {
	t.Setenv(anthropicKeyEnv, testAPIKey)

	requests := make(chan map[string]any, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil {
			requests <- decoded
		}

		_, _ = io.WriteString(w, anthropicToolUseBody)
	}))
	defer server.Close()

	llm, err := builtins.NewAnthropic(pricedConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("new anthropic: %v", err)
	}

	result, err := llm.ExecuteWithTools(t.Context(), llmTask(map[string]any{promptKey: testPrompt}), memoryToolSpec())
	if err != nil {
		t.Fatalf("execute with tools: %v", err)
	}

	if !reflect.DeepEqual(result.Output, wantMemorySearchCall()) {
		t.Fatalf("output = %#v, want the driver-convention tool call", result.Output)
	}

	if result.Meta[stopReasonKey] != runner.StopReasonToolUse {
		t.Fatalf("stop reason = %q, want %q", result.Meta[stopReasonKey], runner.StopReasonToolUse)
	}

	assertLLMUsage(t, result.Usage)

	select {
	case request := <-requests:
		assertAnthropicToolRequest(t, request)
	default:
		t.Fatal("server saw no messages request")
	}
}

// assertAnthropicToolRequest checks the request carried the tools array in
// the Messages shape.
func assertAnthropicToolRequest(t *testing.T, request map[string]any) {
	t.Helper()

	tools, _ := request["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("request tools = %#v, want one tool", request["tools"])
	}

	tool, _ := tools[0].(map[string]any)
	if tool["name"] != toolNameMemory || tool["input_schema"] == nil {
		t.Fatalf("tool def = %#v, want name %q with input_schema", tool, toolNameMemory)
	}
}

func TestAnthropicExecuteWithToolsReturnsFinalText(t *testing.T) {
	t.Setenv(anthropicKeyEnv, testAPIKey)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, anthropicSuccessBody)
	}))
	defer server.Close()

	llm, err := builtins.NewAnthropic(pricedConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("new anthropic: %v", err)
	}

	result, err := llm.ExecuteWithTools(t.Context(), llmTask(map[string]any{promptKey: testPrompt}), memoryToolSpec())
	if err != nil {
		t.Fatalf("execute with tools: %v", err)
	}

	if result.Output != llmHelloText {
		t.Fatalf("output = %#v, want the joined final text", result.Output)
	}

	if result.Meta[stopReasonKey] != runner.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want %q", result.Meta[stopReasonKey], runner.StopReasonEndTurn)
	}

	assertLLMUsage(t, result.Usage)
}
