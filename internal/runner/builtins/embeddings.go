package builtins

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/pkg/runner"
)

// embeddingsPath is the embeddings endpoint under the OpenAI base URL.
const embeddingsPath = "/embeddings"

// defaultEmbeddingModel is the embedding model the comparator uses when the
// operator names none: small, cheap, and good enough for similarity scoring.
const defaultEmbeddingModel = "text-embedding-3-small"

// embeddingsCodePrefix tags the embedder's classified errors so they read
// distinctly from the chat runner's in logs and evidence.
const embeddingsCodePrefix = OpenAIName + ".embeddings"

// OpenAIEmbedder vectorizes text for the tuning engine's embedding-similarity
// comparator (R1, .agents/spec/14-open-decisions.md). It satisfies
// tuning.Embedder structurally, so the CLI wires it in without the tuning
// package importing this one, and reuses the openai runner's base URL and API
// key.
//
// The base URL is operator-configured, so a plain client suffices without the
// SSRF egress guard.
type OpenAIEmbedder struct {
	client *http.Client

	// env answers the API-key variable, injected for the same reason the chat
	// runner injects it: a test supplies a key without setting one for the
	// whole process.
	env environ.Lookup

	model   string
	baseURL string
}

// NewOpenAIEmbedder builds an embedder over an injected HTTP client so tests
// point it at an httptest server. An empty model or base URL falls back to the
// hosted defaults; a nil client gets the shared LLM timeout.
func NewOpenAIEmbedder(model, baseURL string, client *http.Client, lookup environ.Lookup) *OpenAIEmbedder {
	if model == "" {
		model = defaultEmbeddingModel
	}

	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}

	if client == nil {
		client = &http.Client{Timeout: defaultLLMTimeout}
	}

	return &OpenAIEmbedder{model: model, baseURL: baseURL, client: client, env: lookup}
}

// Embed returns the embedding vector for text. The API key reads at call time
// from CAIRNOS_RUNNER_OPENAI_API_KEY, so a rotation needs no restart.
func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	response, err := sendJSON(ctx, e.client, embeddingsCodePrefix,
		e.baseURL+embeddingsPath, e.authHeaders(),
		openaiEmbeddingRequest{Model: e.model, Input: text})
	if err != nil {
		return nil, err
	}

	defer func() { _ = response.Body.Close() }()

	return decodeEmbedding(response.Body)
}

// authHeaders builds the per-call auth header from the same env var the openai
// chat runner reads.
func (e *OpenAIEmbedder) authHeaders() map[string]string {
	return map[string]string{headerAuthorization: "Bearer " + e.env.Get(OpenAIKeyEnv)}
}

// decodeEmbedding reads one embeddings response and returns the first vector,
// enforcing the at-least-one-vector contract.
func decodeEmbedding(body io.Reader) ([]float64, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxCapturedOutput))
	if err != nil {
		return nil, &runner.Error{
			Class: runner.ClassTransient, Code: embeddingsCodePrefix + codeSuffixRead,
			Message: "read embeddings response: " + err.Error(), Cause: err,
		}
	}

	var decoded openaiEmbeddingResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, &runner.Error{
			Class: runner.ClassPermanent, Code: embeddingsCodePrefix + codeSuffixBadResponse,
			Message: "decode embeddings response: " + err.Error(), Cause: err,
		}
	}

	if len(decoded.Data) == 0 || len(decoded.Data[0].Embedding) == 0 {
		return nil, runner.NewError(runner.ClassPermanent,
			embeddingsCodePrefix+codeSuffixBadResponse, "embeddings response carried no vector")
	}

	return decoded.Data[0].Embedding, nil
}

// openaiEmbeddingRequest is the embeddings request body. Input is one string;
// the comparator embeds one output at a time.
type openaiEmbeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// openaiEmbeddingResponse is the embeddings response body; the first datum's
// vector is the one the comparator scores.
type openaiEmbeddingResponse struct {
	Data []openaiEmbeddingDatum `json:"data"`
}

// openaiEmbeddingDatum is one embedded input's vector.
type openaiEmbeddingDatum struct {
	Embedding []float64 `json:"embedding"`
}
