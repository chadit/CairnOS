package builtins_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/runner/builtins"
)

func TestOpenAIEmbedderPostsAndDecodes(t *testing.T) {
	t.Parallel()

	var gotPath, gotModel, gotInput string

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		var req struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode embeddings request: %v", err)
		}

		gotModel, gotInput = req.Model, req.Input

		w.Header().Set("Content-Type", mediaJSON)
		writeBody(t, w, `{"data":[{"embedding":[0.1,0.2,0.3]}]}`)
	}))

	// An empty model falls back to the default, proving the fallback is wired.
	embedder := builtins.NewOpenAIEmbedder("", server.URL, server.Client(), openAIKey(testAPIKey))

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
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeBody(t, w, `{"data":[]}`)
	}))

	embedder := builtins.NewOpenAIEmbedder("text-embedding-3-large", server.URL, server.Client(), openAIKey(testAPIKey))

	if _, err := embedder.Embed(t.Context(), "x"); err == nil {
		t.Fatal("an embeddings response with no vector must be an error")
	}
}
