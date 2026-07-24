package openaicompat_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/openaicompat"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Streaming fixture values; goconst applies to tests in this repo's lint policy.
const (
	streamUserBody = `{"model":"assistant","messages":[{"role":"user","content":"hi"}],"stream":true}`
	objectChunk    = "chat.completion.chunk"
	sseDoneMarker  = "[DONE]"
)

// streamChunkBody is one decoded chat.completion.chunk frame. FinishReason is a
// pointer so a test tells an intermediate null apart from the closing "stop".
type streamChunkBody struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// TestStreamMultiChunkDeltas proves a streaming turn writes the OpenAI SSE
// shape: text/event-stream with no-cache, a role chunk, one content chunk per
// assistant piece, a closing stop chunk, and a terminating [DONE].
func TestStreamMultiChunkDeltas(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{
		sessionID:   defaultSessionID,
		personality: personalityAssistant,
		events: []chat.Event{
			chunkEvent("Hello, "),
			chunkEvent("world"),
			doneEvent(runner.Usage{}),
		},
	}
	fix := startFixture(t, fixtureOptions{agent: agent})

	status, header, body := fix.postStream(t, "", streamUserBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}

	if ct := header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	if cc := header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}

	frames := sseFrames(t, body)
	if len(frames) != 5 {
		t.Fatalf("frames = %d %v, want 5", len(frames), frames)
	}

	if last := frames[len(frames)-1]; last != sseDoneMarker {
		t.Errorf("last frame = %q, want %s", last, sseDoneMarker)
	}

	role := decodeChunk(t, frames[0])
	if role.Object != objectChunk {
		t.Errorf("first chunk object = %q, want %q", role.Object, objectChunk)
	}

	if role.ID != completionIDPrefix+defaultSessionID {
		t.Errorf("chunk id = %q, want %s%s", role.ID, completionIDPrefix, defaultSessionID)
	}

	if len(role.Choices) != 1 || role.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first chunk choices = %+v, want one assistant role delta", role.Choices)
	}

	if role.Choices[0].FinishReason != nil {
		t.Errorf("role chunk finish_reason = %q, want null", *role.Choices[0].FinishReason)
	}

	if got := decodeChunk(t, frames[1]).Choices[0].Delta.Content; got != "Hello, " {
		t.Errorf("chunk 1 content = %q, want %q", got, "Hello, ")
	}

	if got := decodeChunk(t, frames[2]).Choices[0].Delta.Content; got != "world" {
		t.Errorf("chunk 2 content = %q, want %q", got, "world")
	}

	stop := decodeChunk(t, frames[3])
	if stop.Choices[0].FinishReason == nil || *stop.Choices[0].FinishReason != finishStop {
		t.Errorf("stop chunk finish_reason = %v, want %q", stop.Choices[0].FinishReason, finishStop)
	}

	if stop.Choices[0].Delta.Role != "" || stop.Choices[0].Delta.Content != "" {
		t.Errorf("stop chunk delta = %+v, want empty", stop.Choices[0].Delta)
	}
}

// TestStreamConfirmationRendersAsDelta proves the D9 gate surfaces over a stream
// as a content delta naming the pending proposal, and that this surface still
// exposes no confirm route, so a foreign client can never fire the workflow.
func TestStreamConfirmationRendersAsDelta(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{
		sessionID:   defaultSessionID,
		personality: personalityAssistant,
		events:      []chat.Event{confirmEvent(stagingProposal()), doneEvent(runner.Usage{})},
	}
	fix := startFixture(t, fixtureOptions{agent: agent})

	status, _, body := fix.postStream(t, "", streamUserBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}

	content := joinContent(t, sseFrames(t, body))
	for _, want := range confirmationWants(defaultSessionID) {
		if !strings.Contains(content, want) {
			t.Errorf("stream confirmation missing %q; got:\n%s", want, content)
		}
	}

	notFound := probe(t, fix.server.URL+"/v1/chat/sessions/"+defaultSessionID+"/confirm")
	if notFound != http.StatusNotFound {
		t.Errorf("confirm route status = %d, want 404 (no confirm route by design)", notFound)
	}
}

// TestStreamMidTurnErrorPayload proves a turn failure after the status line is
// written surfaces as an SSE error frame carrying the canonical chat.* code,
// emitted before [DONE], with the partial content that reached the client intact.
func TestStreamMidTurnErrorPayload(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{
		sessionID:   defaultSessionID,
		personality: personalityAssistant,
		events:      []chat.Event{chunkEvent("partial ")},
		sendErr:     cairnerr.New(chat.CodeDriverFailed, "chat driver failed"),
	}
	fix := startFixture(t, fixtureOptions{agent: agent})

	status, _, body := fix.postStream(t, "", streamUserBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a stream error rides an SSE frame, body: %s)", status, body)
	}

	frames := sseFrames(t, body)
	if len(frames) != 4 || frames[len(frames)-1] != sseDoneMarker {
		t.Fatalf("frames = %v, want role, content, error, then %s", frames, sseDoneMarker)
	}

	if got := decodeChunk(t, frames[1]).Choices[0].Delta.Content; got != "partial " {
		t.Errorf("pre-error content = %q, want the partial delta", got)
	}

	failure := decodeError(t, []byte(frames[2]))
	if failure.Error.Type != "server_error" || failure.Error.Code != chat.CodeDriverFailed {
		t.Errorf("error frame = %+v, want server_error / %s", failure.Error, chat.CodeDriverFailed)
	}
}

// TestStreamClientDisconnectCancelsTurn proves a dropped client cancels the turn
// through the request context: the agent blocks until its context ends, and the
// cause it records is the context cancellation.
func TestStreamClientDisconnectCancelsTurn(t *testing.T) {
	t.Parallel()

	agent := &blockingAgent{
		sessionID: defaultSessionID,
		started:   make(chan struct{}),
		finished:  make(chan struct{}),
	}

	service, err := openaicompat.NewService(&openaicompat.Config{
		Agent:         agent,
		Store:         &fakeStore{},
		Personalities: chat.NewPersonalities(nil, ""),
		Logger:        slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("build service: %v", err)
	}

	fix := mountService(t, service)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fix.server.URL+completionsPath, strings.NewReader(streamUserBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	clientDone := make(chan struct{})

	go func() {
		defer close(clientDone)

		resp, doErr := (&http.Client{}).Do(req)
		if doErr != nil {
			return
		}

		// Hold the stream open until the cancel below drops it; the copy then
		// returns and the connection tears down.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	select {
	case <-agent.started:
	case <-time.After(testRequestTimeout):
		t.Fatal("SendMessage never started")
	}

	cancel()
	<-clientDone

	select {
	case <-agent.finished:
	case <-time.After(testRequestTimeout):
		t.Fatal("turn never observed the client disconnect")
	}

	if got := agent.cancelCause(); !errors.Is(got, context.Canceled) {
		t.Errorf("turn cancel cause = %v, want context.Canceled", got)
	}
}

// postStream drives a streaming completions request and returns the status, the
// response headers, and the full SSE body. The fake agent emits a finite event
// sequence and closes, so the stream terminates and the body reads to EOF.
func (f *fixture) postStream(t *testing.T, token, body string) (int, http.Header, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+completionsPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: testRequestTimeout}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post stream: %v", err)
	}

	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		t.Fatalf("read stream body: %v", err)
	}

	return resp.StatusCode, resp.Header, data
}

// sseFrames splits an SSE body into the payload after each "data: " prefix, in
// order, so a test asserts the frame sequence a stream produced.
func sseFrames(t *testing.T, body []byte) []string {
	t.Helper()

	var frames []string

	for block := range strings.SplitSeq(string(body), "\n\n") {
		trimmed := strings.TrimSpace(block)
		if trimmed == "" {
			continue
		}

		payload, ok := strings.CutPrefix(trimmed, "data: ")
		if !ok {
			t.Fatalf("SSE frame missing data prefix: %q", trimmed)
		}

		frames = append(frames, payload)
	}

	return frames
}

// decodeChunk decodes one chat.completion.chunk frame, failing on malformed JSON.
func decodeChunk(t *testing.T, frame string) streamChunkBody {
	t.Helper()

	var chunk streamChunkBody
	if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
		t.Fatalf("decode stream chunk %q: %v", frame, err)
	}

	return chunk
}

// joinContent concatenates every content delta across the frames, skipping the
// [DONE] marker, so an assertion doesn't depend on chunk boundaries.
func joinContent(t *testing.T, frames []string) string {
	t.Helper()

	parts := make([]string, 0, len(frames))

	for _, frame := range frames {
		if frame == sseDoneMarker {
			continue
		}

		chunk := decodeChunk(t, frame)
		if len(chunk.Choices) == 1 {
			parts = append(parts, chunk.Choices[0].Delta.Content)
		}
	}

	return strings.Join(parts, "")
}

// blockingAgent is a ChatAgent whose SendMessage blocks until its context ends,
// then records the cancellation cause, so a disconnect test proves cancellation
// reaches the turn through the request context.
type blockingAgent struct {
	sessionID string
	started   chan struct{}
	finished  chan struct{}

	mu     sync.Mutex
	ctxErr error
}

// CreateSession returns a session stamped with the caller's workspace.
func (b *blockingAgent) CreateSession(ctx context.Context, _ string) (*store.ChatSession, error) {
	return &store.ChatSession{ID: b.sessionID, Personality: personalityAssistant, WorkspaceID: store.WorkspaceFromContext(ctx)}, nil
}

// SendMessage signals the turn started, blocks until the context ends, records
// the cancellation cause, and signals finished before closing the stream.
func (b *blockingAgent) SendMessage(ctx context.Context, _, _ string, out chan<- chat.Event) error {
	defer close(out)

	close(b.started)

	<-ctx.Done()

	b.mu.Lock()
	b.ctxErr = ctx.Err()
	b.mu.Unlock()

	close(b.finished)

	return ctx.Err() //nolint:wrapcheck // a fake surfaces the context error verbatim
}

// cancelCause reports the context error SendMessage recorded, under the lock.
func (b *blockingAgent) cancelCause() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.ctxErr
}
