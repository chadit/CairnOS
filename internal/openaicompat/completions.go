package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Wire-shape constants the request and response share.
const (
	objectCompletion   = "chat.completion"
	completionIDPrefix = "chatcmpl-"
	finishReasonStop   = "stop"
	contentPartText    = "text"

	roleUser      = "user"
	roleAssistant = "assistant"
	roleSystem    = "system"
)

// completionEventBuffer bounds the per-turn event channel. A turn emits a
// handful of events, so a small buffer absorbs the burst while the handler
// drains it, matching internal/api's chatEventBuffer.
const completionEventBuffer = 32

// completionRequest is the subset of the OpenAI chat completions body the
// adapter reads. Tuning params (temperature, top_p, max_tokens, n, stop, tools,
// tool_choice, response_format, ...) are accepted and ignored; the driver owns
// its own settings (doc.go).
type completionRequest struct {
	Model    string           `json:"model"`
	Messages []requestMessage `json:"messages"`
	Stream   bool             `json:"stream"`
}

// requestMessage is one input message. Content is raw so it can be a string or
// an array of typed parts, decoded by parseContent.
type requestMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentPart is one element of an array-form content. Only text parts are
// accepted; any other type is a 400 unsupported_content_part.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// completionResponse is the non-streaming chat.completion body.
type completionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   usageBlock         `json:"usage"`
	Created int64              `json:"created"`
}

// completionChoice is one answer in the choices array. The adapter always
// returns a single choice.
type completionChoice struct {
	Message      responseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
	Index        int             `json:"index"`
}

// responseMessage is the assistant reply in a choice.
type responseMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// usageBlock reports the turn's real token spend, so clients see counts instead
// of fabricated zeros (.agents/spec/parity/openai-compat.md, "Real usage
// numbers").
type usageBlock struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// seededMessage is one prior-history row the adapter appends to the fresh
// session before the turn runs.
type seededMessage struct {
	role    string
	content string
}

// handleCompletions is POST /v1/chat/completions: authorize, decode under the
// body cap, map the model to a personality, seed prior history, run one turn,
// and answer the OpenAI chat.completion shape. Streaming lands in slice 2; until
// then stream:true is a 400.
func (s *Service) handleCompletions(w http.ResponseWriter, r *http.Request) {
	ctx, err := s.authorize(r.Context(), bearerFromHeader(r.Header))
	if err != nil {
		s.writeError(w, err)

		return
	}

	req, err := s.decodeRequest(w, r)
	if err != nil {
		s.writeError(w, err)

		return
	}

	prompt, seed, err := splitMessages(req.Messages)
	if err != nil {
		s.writeError(w, err)

		return
	}

	if req.Model != "" && !s.personalities.Known(req.Model) {
		s.writeError(w, errModelNotFound("unknown model: "+req.Model))

		return
	}

	session, err := s.agent.CreateSession(ctx, req.Model)
	if err != nil {
		s.writeError(w, createSessionError(err))

		return
	}

	if seedErr := s.seedHistory(ctx, session.ID, seed); seedErr != nil {
		s.writeError(w, seedErr)

		return
	}

	if req.Stream {
		s.streamCompletion(ctx, w, session, prompt)

		return
	}

	answer, usage, err := s.runTurn(ctx, session.ID, prompt)
	if err != nil {
		s.logger.Error("openai-compat turn failed", "session", session.ID, "error", err)
		s.writeError(w, err)

		return
	}

	s.writeJSON(w, http.StatusOK, completionResponse{
		ID:      completionIDPrefix + session.ID,
		Object:  objectCompletion,
		Created: time.Now().Unix(),
		Model:   session.Personality,
		Choices: []completionChoice{{
			Index:        0,
			Message:      responseMessage{Role: roleAssistant, Content: answer},
			FinishReason: finishReasonStop,
		}},
		Usage: usageFrom(usage),
	})
}

// decodeRequest reads the body under the configured cap and decodes the JSON. An
// over-limit body answers 413 request_too_large; malformed JSON answers 400
// invalid_body. The cap is applied here because extension routes bypass the
// gateway's limitBody wrapper (.agents/spec/parity/openai-compat.md, "Body cap").
func (s *Service) decodeRequest(w http.ResponseWriter, r *http.Request) (*completionRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)

	var req completionRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, errRequestTooLarge("request body exceeds the configured size limit")
		}

		return nil, errInvalidBody("request body is not valid JSON")
	}

	return &req, nil
}

// splitMessages validates the message list and splits it into the trailing user
// prompt and the prior history to seed. messages must be non-empty and the
// trailing message must be role user; content may be a string or an array of
// text parts, any other part type a 400 (.agents/spec/parity/openai-compat.md).
func splitMessages(messages []requestMessage) (string, []seededMessage, error) {
	if len(messages) == 0 {
		return "", nil, errMissingMessages("messages must contain at least one message")
	}

	last := len(messages) - 1
	if messages[last].Role != roleUser {
		return "", nil, errTrailingNotUser("the trailing message must have role user")
	}

	prompt, err := parseContent(messages[last].Content)
	if err != nil {
		return "", nil, err
	}

	seed := make([]seededMessage, 0, last)

	for idx := range last {
		content, contentErr := parseContent(messages[idx].Content)
		if contentErr != nil {
			return "", nil, contentErr
		}

		seed = append(seed, seededMessage{role: seedRole(messages[idx].Role), content: content})
	}

	return prompt, seed, nil
}

// parseContent reads a message's content, which OpenAI allows as either a string
// or an array of typed parts. A string passes through; an array concatenates its
// text parts in order and rejects any non-text part with a 400. Null or absent
// content reads as empty.
func parseContent(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}

	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errInvalidBody("message content must be a string or an array of content parts")
	}

	texts := make([]string, 0, len(parts))

	for idx := range parts {
		if parts[idx].Type != contentPartText {
			return "", errUnsupportedPart("unsupported content part type: " + parts[idx].Type)
		}

		texts = append(texts, parts[idx].Text)
	}

	return strings.Join(texts, ""), nil
}

// seedRole maps an OpenAI message role onto a store chat role. The three roles a
// client seeds share their string with the store constants, so this names the
// mapping while passing an unrecognized role through unchanged.
func seedRole(clientRole string) string {
	switch clientRole {
	case roleUser:
		return store.ChatRoleUser
	case roleAssistant:
		return store.ChatRoleAssistant
	case roleSystem:
		return store.ChatRoleSystem
	default:
		return clientRole
	}
}

// seedHistory appends every prior-history message into the fresh session in
// order, so the turn's prompt replays the client's conversation. The client is
// stateless (it resends the whole thread each call) while the session is
// stateful, so the adapter re-persists the history each request, the O(n^2)
// write the spec accepts for v1 parity (.agents/spec/parity/openai-compat.md).
func (s *Service) seedHistory(ctx context.Context, sessionID string, seed []seededMessage) error {
	// The ChatStore has no batch append, so each prior message is one write.
	// This is the documented O(n^2) history cost of per-request sessions, not an
	// unbatched query that a join would fix.
	for idx := range seed {
		err := s.store.AppendMessage(ctx, &store.ChatMessage{
			SessionID: sessionID,
			Role:      seed[idx].role,
			Content:   seed[idx].content,
		})
		if err != nil {
			return fmt.Errorf("seed chat history: %w", err)
		}
	}

	return nil
}

// runTurn runs one conversation turn and collects its result, mirroring the
// streamTurn pattern in internal/api/chat_service.go: SendMessage runs on a
// goroutine, the handler ranges the event channel the agent closes, and the
// terminal error arrives on a one-slot done channel afterward. A D9
// confirmation-required event renders as the turn's assistant text, since this
// surface has no confirm route.
func (s *Service) runTurn(ctx context.Context, sessionID, prompt string) (string, runner.Usage, error) {
	events := make(chan chat.Event, completionEventBuffer)
	callDone := make(chan error, 1)

	go func() {
		callDone <- s.agent.SendMessage(ctx, sessionID, prompt, events)
	}()

	var (
		chunks   []string
		usage    runner.Usage
		proposal *chat.Proposal
	)

	for event := range events {
		switch event.Type {
		case chat.EventAssistantChunk:
			chunks = append(chunks, event.Text)
		case chat.EventConfirmationRequired:
			proposal = event.Proposal
		case chat.EventTurnDone:
			usage = event.Usage
		case chat.EventToolCall, chat.EventRunStarted, chat.EventRunFinished:
			// Intermediate events a SendMessage turn may emit (a read-tool call)
			// or that only a ConfirmAction turn emits (run started/finished). The
			// non-streaming completion carries only the final answer, so these are
			// collected past.
		}
	}

	if err := <-callDone; err != nil {
		return "", runner.Usage{}, err
	}

	if proposal != nil {
		return renderConfirmation(proposal, sessionID), usage, nil
	}

	return strings.Join(chunks, ""), usage, nil
}

// createSessionError maps a CreateSession failure onto the wire shape: an
// unknown personality is a 404 model_not_found, anything else stays a server
// error rendered by writeError.
func createSessionError(err error) error {
	if errors.Is(err, chat.ErrUnknownPersonality) {
		return errModelNotFound("unknown model")
	}

	return err
}

// confirmationTemplate is the assistant text a D9 gate renders as. It names the
// pending proposal in full and points at the native surfaces that can approve
// it, since this endpoint has no confirm route. The session id appears in the
// header, the CLI hint, and the confirm URL.
const confirmationTemplate = "I want to run a workflow, but this OpenAI-compatible endpoint can't approve workflow runs.\n\n" +
	"Pending proposal:\n" +
	"- workflow: %s\n" +
	"- inputs: %s\n" +
	"- estimated budget: $%.2f USD\n" +
	"- session: %s\n\n" +
	"To approve or deny, use a CairnOS surface: continue this session with `cairnos chat --session %s`, " +
	"or POST /v1/chat/sessions/%s/confirm with {\"approve\": true} using a token that carries write:workflow. " +
	"Nothing runs until then."

// renderConfirmation turns a D9 proposal into assistant text naming the pending
// proposal in full, so a human can act on a native surface. This endpoint has no
// confirm route by design, so a foreign client can never fire the proposal
// (.agents/spec/parity/openai-compat.md, "The D9 gate over a foreign client").
func renderConfirmation(proposal *chat.Proposal, sessionID string) string {
	return fmt.Sprintf(
		confirmationTemplate,
		proposal.WorkflowName,
		compactInputs(proposal.Inputs),
		proposal.EstimatedBudgetUSD,
		sessionID,
		sessionID,
		sessionID,
	)
}

// compactInputs renders proposal inputs as compact JSON, showing an empty object
// when there are none so the line always reads as JSON.
func compactInputs(inputs map[string]any) string {
	if len(inputs) == 0 {
		return "{}"
	}

	encoded, err := json.Marshal(inputs)
	if err != nil {
		return "{}"
	}

	return string(encoded)
}

// usageFrom builds the response usage block from the turn's token counts.
func usageFrom(usage runner.Usage) usageBlock {
	return usageBlock{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.InputTokens + usage.OutputTokens,
	}
}

// writeJSON writes a JSON payload with the given status. An encode failure lands
// after the header is written, so it can't change the status; it is logged
// rather than surfaced, since the client already sees a truncated body.
func (s *Service) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		s.logger.Error("openai-compat write response", "error", err)
	}
}
