package builtins_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
)

func TestOpenAIEmbedderPostsAndDecodes(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it.
	t.Setenv(builtins.OpenAIKeyEnv, testAPIKey)

	var gotPath, gotModel, gotInput string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		var req struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}

		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		gotModel, gotInput = req.Model, req.Input

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer server.Close()

	// An empty model falls back to the default, proving the fallback is wired.
	embedder := builtins.NewOpenAIEmbedder("", server.URL, server.Client())

	vec, err := embedder.Embed(t.Context(), "vectorize me")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}

	if gotPath != "/embeddings" {
		t.Fatalf("path = %q, want /embeddings", gotPath)
	}

	if gotModel != "text-embedding-3-small" {
		t.Fatalf("model = %q, want the default", gotModel)
	}

	if gotInput != "vectorize me" {
		t.Fatalf("input = %q, want the text passed to Embed", gotInput)
	}

	want := []float64{0.1, 0.2, 0.3}
	if len(vec) != len(want) {
		t.Fatalf("vector = %v, want %v", vec, want)
	}

	for idx := range want {
		if vec[idx] != want[idx] {
			t.Fatalf("vector[%d] = %v, want %v", idx, vec[idx], want[idx])
		}
	}
}

func TestOpenAIEmbedderEmptyVectorIsError(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it.
	t.Setenv(builtins.OpenAIKeyEnv, testAPIKey)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	embedder := builtins.NewOpenAIEmbedder("text-embedding-3-large", server.URL, server.Client())

	if _, err := embedder.Embed(t.Context(), "x"); err == nil {
		t.Fatal("an embeddings response with no vector must be an error")
	}
}
