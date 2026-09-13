package builtins_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// localModelName is the model string the [runner.local] block names, passed
// through to the server verbatim.
const localModelName = "gemma-4-e4b"

// localBaseURL is the loopback endpoint `cairnos model setup` writes into the
// [runner.local] config block.
const localBaseURL = "http://127.0.0.1:8089/v1"

// TestOpenAIValidateKeyRequirementFollowsTheAddress proves the credential is
// waived for a server on our own side of the network and demanded everywhere
// else. The tailnet row is the one that would be easy to get wrong: a Mac
// running CairnOS reaches llama-server on another box at a 100.64.0.0/10
// address, which is neither loopback nor RFC 1918. The typo row is why the
// rule is about the address and not about differing from the vendor default.
func TestOpenAIValidateKeyRequirementFollowsTheAddress(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		wantKey bool
	}{
		{"loopback", localBaseURL, false},
		{"localhost by name", "http://localhost:8089/v1", false},
		{"loopback ipv6", "http://[::1]:8089/v1", false},
		{"private class a", "http://10.0.4.7:8089/v1", false},
		{"private class c", "http://192.168.1.40:8089/v1", false},
		{"tailnet carrier-grade nat", "http://100.64.0.10:8089/v1", false},
		{"hosted endpoint", "https://api.example.com/v1", true},
		{"typo in a hosted endpoint", "https://api.openai.con/v1", true},
		{"public address", "https://93.184.216.34/v1", true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(openaiKeyEnv, "")

			llm, err := builtins.NewOpenAI(map[string]any{
				modelKey:   localModelName,
				baseURLKey: testCase.baseURL,
			}, &http.Client{Timeout: time.Second})
			if err != nil {
				t.Fatalf("new openai: %v", err)
			}

			err = llm.Validate(&runner.Task{
				RunID: testRunID, ActionName: actionLLM,
				Params: map[string]any{promptKey: testPrompt}, Resolved: true,
			})

			if gotKey := err != nil; gotKey != testCase.wantKey {
				t.Fatalf("validate error = %v, want an API key demanded: %t", err, testCase.wantKey)
			}
		})
	}
}

// TestOpenAIValidateStillRequiresAPIKeyForTheHostedAPI proves the check did not
// simply go away: a block with no base_url still reaches api.openai.com, and a
// call there without a key is a permanent failure worth catching early.
func TestOpenAIValidateStillRequiresAPIKeyForTheHostedAPI(t *testing.T) {
	t.Setenv(openaiKeyEnv, "")

	llm, err := builtins.NewOpenAI(map[string]any{modelKey: testModel}, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatalf("new openai: %v", err)
	}

	task := &runner.Task{
		RunID: testRunID, ActionName: actionLLM,
		Params: map[string]any{promptKey: testPrompt}, Resolved: true,
	}

	if err := llm.Validate(task); err == nil {
		t.Fatal("validate accepted a hosted call with no API key")
	}
}

// TestOpenAISendsTheConfiguredModelToALocalServer proves the [runner.local]
// block's model string reaches the server as the request model, which is what
// lets one llama-server name the model it loaded.
func TestOpenAISendsTheConfiguredModelToALocalServer(t *testing.T) {
	t.Setenv(openaiKeyEnv, "")

	var gotModel string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotModel = decodeRequestModel(t, r)

		_, _ = w.Write([]byte(openaiSuccessBody))
	}))
	defer server.Close()

	llm, err := builtins.NewOpenAI(map[string]any{
		modelKey:   localModelName,
		baseURLKey: server.URL,
	}, server.Client())
	if err != nil {
		t.Fatalf("new openai: %v", err)
	}

	_, err = llm.Execute(t.Context(), &runner.Task{
		RunID: testRunID, ActionName: actionLLM,
		Params: map[string]any{promptKey: testPrompt}, Resolved: true,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if gotModel != localModelName {
		t.Errorf("request model = %q, want the configured block model", gotModel)
	}
}

// decodeRequestModel reads the model field out of a chat-completions request.
func decodeRequestModel(t *testing.T, r *http.Request) string {
	t.Helper()

	var decoded struct {
		Model string `json:"model"`
	}

	if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode request: %v", err)
	}

	return decoded.Model
}
