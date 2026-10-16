package api

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
)

// chatEventBuffer bounds the per-turn event channel the agent streams into. A
// turn emits a handful of events, so a small buffer absorbs the burst while the
// gRPC send drains it.
const chatEventBuffer = 32

// ChatHistory reads what a workspace has said: its sessions and their messages.
type ChatHistory interface {
	GetSession(ctx context.Context, id string) (*store.ChatSession, error)
	ListSessions(ctx context.Context, limit int) ([]*store.ChatSession, error)
	ListMessages(ctx context.Context, sessionID string) ([]*store.ChatMessage, error)
	SearchMessages(ctx context.Context, query store.ChatSearchQuery) ([]*store.ChatMessage, error)
}

// ChatConversation opens a session and runs its turns, streaming each turn's
// events into out.
type ChatConversation interface {
	CreateSession(ctx context.Context, personality string) (*store.ChatSession, error)
	SendMessage(ctx context.Context, sessionID, userMessage string, out chan<- chat.Event) error
	ConfirmAction(ctx context.Context, sessionID string, approve bool, out chan<- chat.Event) error
}

// ChatAgent is the slice of the chat package the service drives; *chat.Agent
// satisfies it. Defined here so the service stays testable against a fake and
// the dependency direction is the api package depending on a narrow surface.
type ChatAgent interface {
	ChatHistory
	ChatConversation
}

// chatService implements cairnos.v1.ChatService over the chat agent.
type chatService struct {
	cairnosv1.UnimplementedChatServiceServer

	agent ChatAgent
}

// CreateSession opens a new conversation with the requested personality.
func (s *chatService) CreateSession(ctx context.Context, req *cairnosv1.CreateSessionRequest) (*cairnosv1.CreateSessionResponse, error) {
	session, err := s.agent.CreateSession(ctx, req.GetPersonality())
	if err != nil {
		return nil, chatStatus(err)
	}

	return &cairnosv1.CreateSessionResponse{Session: protoChatSession(session)}, nil
}

// GetSession fetches one session's metadata.
func (s *chatService) GetSession(ctx context.Context, req *cairnosv1.GetSessionRequest) (*cairnosv1.GetSessionResponse, error) {
	session, err := s.agent.GetSession(ctx, req.GetSessionId())
	if err != nil {
		return nil, chatStatus(err)
	}

	return &cairnosv1.GetSessionResponse{Session: protoChatSession(session)}, nil
}

// ListSessions lists the workspace's sessions, most-recently-updated first.
func (s *chatService) ListSessions(ctx context.Context, req *cairnosv1.ListSessionsRequest) (*cairnosv1.ListSessionsResponse, error) {
	sessions, err := s.agent.ListSessions(ctx, int(req.GetLimit()))
	if err != nil {
		return nil, chatStatus(err)
	}

	out := make([]*cairnosv1.ChatSession, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, protoChatSession(session))
	}

	return &cairnosv1.ListSessionsResponse{Sessions: out}, nil
}

// ListMessages returns one session's transcript in order.
func (s *chatService) ListMessages(ctx context.Context, req *cairnosv1.ListMessagesRequest) (*cairnosv1.ListMessagesResponse, error) {
	messages, err := s.agent.ListMessages(ctx, req.GetSessionId())
	if err != nil {
		return nil, chatStatus(err)
	}

	out := make([]*cairnosv1.ChatMessage, 0, len(messages))
	for _, message := range messages {
		out = append(out, protoChatMessage(message))
	}

	return &cairnosv1.ListMessagesResponse{Messages: out}, nil
}

// SearchMessages runs full-text search over the workspace's transcripts. An
// empty query is rejected here so the store never sees it and gateway clients
// get the canonical InvalidArgument envelope.
func (s *chatService) SearchMessages(ctx context.Context, req *cairnosv1.SearchMessagesRequest) (*cairnosv1.SearchMessagesResponse, error) {
	if strings.TrimSpace(req.GetText()) == "" {
		return nil, errorStatus(codes.InvalidArgument,
			cairnerr.New(codeInvalidArgument, "text is required"))
	}

	messages, err := s.agent.SearchMessages(ctx, store.ChatSearchQuery{
		Text:      req.GetText(),
		SessionID: req.GetSessionId(),
		Limit:     int(req.GetLimit()),
	})
	if err != nil {
		return nil, chatStatus(err)
	}

	out := make([]*cairnosv1.ChatMessage, 0, len(messages))
	for _, message := range messages {
		out = append(out, protoChatMessage(message))
	}

	return &cairnosv1.SearchMessagesResponse{Messages: out}, nil
}

// SendMessage runs one conversation turn and streams its events. The chat agent
// runs the turn on this goroutine via a buffered channel a forwarder drains, so
// a dropped client stops the turn through the stream's context.
func (s *chatService) SendMessage(req *cairnosv1.SendMessageRequest, stream cairnosv1.ChatService_SendMessageServer) error {
	return s.streamTurn(stream, func(ctx context.Context, out chan<- chat.Event) error {
		return s.agent.SendMessage(ctx, req.GetSessionId(), req.GetMessage(), out)
	})
}

// ConfirmAction resolves a pending workflow proposal. An approval is the D9
// trigger, so it also requires write:workflow on the token, checked
// here from the stamped scopes since the method table grants one scope per
// method (.agents/spec/14-open-decisions.md, D9). A denial needs only
// chat:session, since nothing runs.
func (s *chatService) ConfirmAction(req *cairnosv1.ConfirmActionRequest, stream cairnosv1.ChatService_ConfirmActionServer) error {
	if req.GetApprove() && !hasScope(stream.Context(), auth.ScopeWriteWorkflow) {
		return errorStatus(codes.PermissionDenied, cairnerr.Newf(codeAuthForbidden,
			"confirming a workflow trigger requires the %q scope", auth.ScopeWriteWorkflow))
	}

	return s.streamTurn(stream, func(ctx context.Context, out chan<- chat.Event) error {
		return s.agent.ConfirmAction(ctx, req.GetSessionId(), req.GetApprove(), out)
	})
}

// chatStreamSender is the shared send side of SendMessage and ConfirmAction, so
// one streamTurn body serves both.
type chatStreamSender interface {
	Context() context.Context
	Send(event *cairnosv1.ChatEvent) error
}

// streamTurn runs one turn-producing call, forwarding each chat event to the
// gRPC stream as it arrives. The agent call runs on a goroutine and the
// forwarder ranges the channel on this one, so the stream sends in order and the
// call's terminal error surfaces after the last event.
func (*chatService) streamTurn(stream chatStreamSender, call func(context.Context, chan<- chat.Event) error) error {
	events := make(chan chat.Event, chatEventBuffer)
	callDone := make(chan error, 1)

	go func() {
		callDone <- call(stream.Context(), events)
	}()

	for event := range events {
		if err := stream.Send(protoChatEvent(&event)); err != nil {
			return fmt.Errorf("send chat event: %w", err)
		}
	}

	if err := <-callDone; err != nil {
		return chatStatus(err)
	}

	return nil
}
