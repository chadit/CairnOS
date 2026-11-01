package builtins_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/nettest"
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
// else. The tailnet row is easy to get wrong: a 100.64.0.0/10 address is
// neither loopback nor RFC 1918. The typo row is why the rule keys on the
// address rather than on differing from the vendor default.
func TestOpenAIValidateKeyRequirementFollowsTheAddress(t *testing.T) {
	t.Parallel()

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
		{"public address", "https://203.0.113.10/v1", true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			llm, err := builtins.NewOpenAI(map[string]any{
				modelKey:   localModelName,
				baseURLKey: testCase.baseURL,
			}, &http.Client{Timeout: time.Second}, openAIKey(""))
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
	t.Parallel()

	llm, err := builtins.NewOpenAI(map[string]any{modelKey: testModel}, &http.Client{Timeout: time.Second}, openAIKey(""))
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
	t.Parallel()

	var gotModel string

	// The runner drops the API-key requirement only for a loopback base_url, so
	// the block names one. Nothing dials it: the fixture client answers from
	// this handler.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotModel = decodeRequestModel(t, r)

		writeBody(t, w, openaiSuccessBody)
	})

	llm, err := builtins.NewOpenAI(map[string]any{
		modelKey:   localModelName,
		baseURLKey: localBaseURL,
	}, nettest.Client(handler), openAIKey(""))
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
