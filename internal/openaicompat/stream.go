package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
)

// objectCompletionChunk is the object value every streaming frame carries,
// telling a client an SSE delta apart from the non-streaming chat.completion.
const objectCompletionChunk = "chat.completion.chunk"

// sseDone is the terminating SSE frame an OpenAI stream ends on; a client stops
// reading once it arrives.
const sseDone = "data: [DONE]\n\n"

// streamChunk is one chat.completion.chunk frame. Every frame in a turn shares
// the id, model, and created stamp; only the choice delta changes.
type streamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
}

// streamChoice is the single choice each chunk carries. FinishReason is a
// pointer so an intermediate chunk renders JSON null while the closing chunk
// renders "stop", matching the OpenAI wire shape.
type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// streamDelta is the incremental piece a chunk adds: the role on the opening
// chunk, a content piece on each answer chunk, empty on the closer.
type streamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// streamState carries the per-turn chunk identity shared by every frame, so the
// chunk builder doesn't re-thread the completion id, model, and created stamp.
type streamState struct {
	id      string
	model   string
	created int64
}

// chunk assembles one frame with the shared identity plus the given delta and
// finish reason.
func (st streamState) chunk(delta streamDelta, finish *string) streamChunk {
	return streamChunk{
		ID:      st.id,
		Object:  objectCompletionChunk,
		Created: st.created,
		Model:   st.model,
		Choices: []streamChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
}

// streamCompletion runs one turn and writes it as an OpenAI SSE stream: a role
// chunk, one content chunk per assistant piece (a D9 gate renders as the
// confirmation text, since this surface has no confirm route), a closing stop
// chunk, then the terminating [DONE]. A turn error after the status line is
// already written can't become an HTTP status, so it surfaces as an SSE error
// payload before [DONE]. The request context drives the turn, so a dropped
// client cancels it (.agents/spec/parity/openai-compat.md, "Streaming").
func (s *Service) streamCompletion(ctx context.Context, w http.ResponseWriter, session *store.ChatSession, prompt string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	flusher, _ := w.(http.Flusher)

	state := streamState{
		id:      completionIDPrefix + session.ID,
		model:   session.Personality,
		created: time.Now().Unix(),
	}

	s.writeSSE(w, flusher, state.chunk(streamDelta{Role: roleAssistant}, nil))

	events := make(chan chat.Event, completionEventBuffer)
	callDone := make(chan error, 1)

	go func() {
		callDone <- s.agent.SendMessage(ctx, session.ID, prompt, events)
	}()

	for event := range events {
		switch event.Type {
		case chat.EventAssistantChunk:
			s.writeSSE(w, flusher, state.chunk(streamDelta{Content: event.Text}, nil))
		case chat.EventConfirmationRequired:
			s.writeSSE(w, flusher, state.chunk(streamDelta{Content: renderConfirmation(event.Proposal, session.ID)}, nil))
		case chat.EventTurnDone, chat.EventToolCall, chat.EventRunStarted, chat.EventRunFinished:
			// The stream carries only assistant text and the D9 gate. Turn-done is
			// the range's own terminator, and the tool/run events belong to the
			// native surface, so they pass without a frame.
		}
	}

	if err := <-callDone; err != nil {
		// A client that dropped mid-turn cancels the request context, so there is
		// no live connection left to receive an error frame; nothing is written.
		if ctx.Err() != nil {
			return
		}

		s.writeSSE(w, flusher, errorEnvelopeFrom(err))
		s.writeDone(w, flusher)

		return
	}

	stop := finishReasonStop
	s.writeSSE(w, flusher, state.chunk(streamDelta{}, &stop))
	s.writeDone(w, flusher)
}

// writeSSE marshals a payload and writes it as one SSE data frame, flushing so
// the client sees each event as it happens. A marshal or write failure lands
// after the status line, so it's logged rather than surfaced as a status.
func (s *Service) writeSSE(w http.ResponseWriter, flusher http.Flusher, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		s.logger.Error("openai-compat marshal stream chunk", "error", err)

		return
	}

	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		s.logger.Error("openai-compat write stream chunk", "error", err)

		return
	}

	if flusher != nil {
		flusher.Flush()
	}
}

// writeDone writes the terminating [DONE] frame and flushes, closing the stream.
func (s *Service) writeDone(w http.ResponseWriter, flusher http.Flusher) {
	if _, err := io.WriteString(w, sseDone); err != nil {
		s.logger.Error("openai-compat write stream done", "error", err)

		return
	}

	if flusher != nil {
		flusher.Flush()
	}
}

// errorEnvelopeFrom renders any turn error onto the OpenAI error object, the
// same shape writeError renders, so a mid-stream failure carries the canonical
// code a client would have seen as an HTTP body.
func errorEnvelopeFrom(err error) errorEnvelope {
	rendered := asHTTPError(err)

	return errorEnvelope{Error: errorBody{
		Message: rendered.Message,
		Type:    rendered.Type,
		Code:    rendered.Code,
	}}
}
