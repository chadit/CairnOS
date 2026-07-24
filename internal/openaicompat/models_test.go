package openaicompat_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/openaicompat"
)

// Shared model-listing fixtures; goconst applies to tests in this repo's policy.
const (
	modelsPath     = "/v1/models"
	ownedByCairnOS = "cairnos"
)

// modelListBody is the decoded GET /v1/models response.
type modelListBody struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// mountModels builds an adapter over the given personalities with auth off and
// mounts its Extension routes, so a models test drives GET /v1/models without a
// token. Auth enforcement has its own test through startFixture.
func mountModels(t *testing.T, personalities *chat.Personalities) *fixture {
	t.Helper()

	service, err := openaicompat.NewService(&openaicompat.Config{
		Agent:         answerAgent(),
		Store:         &fakeStore{},
		Personalities: personalities,
		Logger:        slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("build service: %v", err)
	}

	return mountService(t, service)
}

// modelIDs pulls the ordered model ids out of a GET /v1/models body.
func modelIDs(t *testing.T, body string) []string {
	t.Helper()

	var list modelListBody
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode model list %q: %v", body, err)
	}

	ids := make([]string, len(list.Data))
	for idx := range list.Data {
		ids[idx] = list.Data[idx].ID
	}

	return ids
}

// TestModelsListsBuiltinsAndConfigured proves an OpenAI-style client sees the
// built-in personalities plus any the config adds, sorted, each shaped as an
// OpenAI model object owned by cairnos.
func TestModelsListsBuiltinsAndConfigured(t *testing.T) {
	t.Parallel()

	personalities := chat.NewPersonalities(map[string]string{"support": "be helpful"}, "")
	fix := mountModels(t, personalities)

	body := getBody(t, fix.server.URL+modelsPath)

	var list modelListBody
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode model list %q: %v", body, err)
	}

	if list.Object != "list" {
		t.Errorf("object = %q, want list", list.Object)
	}

	want := []string{chat.PersonalityAssistant, chat.PersonalityOps, "support"}
	if got := modelIDs(t, body); !slices.Equal(got, want) {
		t.Errorf("model ids = %v, want %v", got, want)
	}

	for idx := range list.Data {
		if list.Data[idx].Object != "model" {
			t.Errorf("data[%d].object = %q, want model", idx, list.Data[idx].Object)
		}

		if list.Data[idx].OwnedBy != ownedByCairnOS {
			t.Errorf("data[%d].owned_by = %q, want %q", idx, list.Data[idx].OwnedBy, ownedByCairnOS)
		}
	}
}

// TestModelsOverrideNoDuplicate proves a config entry that overrides a built-in
// name yields a single entry for that name, not two.
func TestModelsOverrideNoDuplicate(t *testing.T) {
	t.Parallel()

	personalities := chat.NewPersonalities(map[string]string{chat.PersonalityAssistant: "reshaped"}, "")
	fix := mountModels(t, personalities)

	want := []string{chat.PersonalityAssistant, chat.PersonalityOps}
	if got := modelIDs(t, getBody(t, fix.server.URL+modelsPath)); !slices.Equal(got, want) {
		t.Errorf("model ids = %v, want %v", got, want)
	}
}

// TestModelsAuthorization proves GET /v1/models sits behind the same
// chat:session check as completions: missing and invalid tokens 401, a wrong
// scope 403, and a chat:session token 200.
func TestModelsAuthorization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		token      func(t *testing.T, fix *fixture) string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing_token",
			token:      func(_ *testing.T, _ *fixture) string { return "" },
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthenticated,
		},
		{
			name:       "invalid_token",
			token:      func(_ *testing.T, _ *fixture) string { return "not-a-real-token" },
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthenticated,
		},
		{
			name: "wrong_scope",
			token: func(t *testing.T, fix *fixture) string {
				t.Helper()

				return fix.token(t, auth.ScopeReadMemory)
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "auth.forbidden",
		},
		{
			name: "chat_session_scope",
			token: func(t *testing.T, fix *fixture) string {
				t.Helper()

				return fix.token(t, auth.ScopeChatSession)
			},
			wantStatus: http.StatusOK,
			wantCode:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fix := startFixture(t, fixtureOptions{authEnabled: true, agent: answerAgent()})

			status, data := fix.getModels(t, tt.token(t, fix))
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", status, tt.wantStatus, data)
			}

			if tt.wantCode != "" {
				if got := decodeError(t, data).Error.Code; got != tt.wantCode {
					t.Errorf("code = %q, want %q", got, tt.wantCode)
				}
			}
		})
	}
}

// getModels drives a GET /v1/models with an optional bearer token, returning the
// status code and body, so the models auth test mirrors the completions post
// helper. It closes the body here to keep callers simple.
func (f *fixture) getModels(t *testing.T, token string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+modelsPath, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: testRequestTimeout}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get models: %v", err)
	}

	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return resp.StatusCode, data
}
