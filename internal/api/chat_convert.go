package api

import (
	"errors"

	"google.golang.org/grpc/codes"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
)

// codeChatSessionNotFound is the API-layer code for a missing chat session.
const codeChatSessionNotFound = "api.chat_session_not_found"

// protoChatSession maps a persisted session onto the wire shape.
func protoChatSession(session *store.ChatSession) *cairnosv1.ChatSession {
	return &cairnosv1.ChatSession{
		Id:          session.ID,
		Personality: session.Personality,
		CreatedAt:   protoTime(session.CreatedAt),
		UpdatedAt:   protoTime(session.UpdatedAt),
	}
}

// protoChatMessage maps a persisted message onto the wire shape.
func protoChatMessage(message *store.ChatMessage) *cairnosv1.ChatMessage {
	return &cairnosv1.ChatMessage{
		Seq:       int32(message.Seq), // #nosec G115 -- sequence counts stay far below int32
		Role:      message.Role,
		Content:   message.Content,
		CreatedAt: protoTime(message.CreatedAt),
		SessionId: message.SessionID,
	}
}

// protoChatEvent maps one streamed turn event onto the wire shape, filling only
// the fields the event's type uses.
func protoChatEvent(event *chat.Event) *cairnosv1.ChatEvent {
	out := &cairnosv1.ChatEvent{
		Type:      protoChatEventType(event.Type),
		Text:      event.Text,
		Tool:      event.Tool,
		ToolInput: protoStruct(event.ToolInput),
		RunId:     event.RunID,
		RunStatus: event.RunStatus,
	}

	if event.Proposal != nil {
		out.Proposal = &cairnosv1.WorkflowProposal{
			WorkflowName:       event.Proposal.WorkflowName,
			Inputs:             protoStruct(event.Proposal.Inputs),
			EstimatedBudgetUsd: event.Proposal.EstimatedBudgetUSD,
		}
	}

	return out
}

// protoChatEventType maps the chat event names onto the proto enum.
func protoChatEventType(eventType chat.EventType) cairnosv1.ChatEventType {
	switch eventType {
	case chat.EventAssistantChunk:
		return cairnosv1.ChatEventType_CHAT_EVENT_TYPE_ASSISTANT_CHUNK
	case chat.EventToolCall:
		return cairnosv1.ChatEventType_CHAT_EVENT_TYPE_TOOL_CALL
	case chat.EventConfirmationRequired:
		return cairnosv1.ChatEventType_CHAT_EVENT_TYPE_CONFIRMATION_REQUIRED
	case chat.EventRunStarted:
		return cairnosv1.ChatEventType_CHAT_EVENT_TYPE_RUN_STARTED
	case chat.EventRunFinished:
		return cairnosv1.ChatEventType_CHAT_EVENT_TYPE_RUN_FINISHED
	case chat.EventTurnDone:
		return cairnosv1.ChatEventType_CHAT_EVENT_TYPE_TURN_DONE
	default:
		return cairnosv1.ChatEventType_CHAT_EVENT_TYPE_UNSPECIFIED
	}
}

// chatStatus classifies a chat agent or store failure onto the canonical wire
// shape: a missing session maps to NotFound, an unknown personality to
// InvalidArgument, and the chat.* codes flow through the canonical mapping.
func chatStatus(err error) error {
	if errors.Is(err, store.ErrChatSessionNotFound) {
		return errorStatus(codes.NotFound, cairnerr.New(codeChatSessionNotFound, err.Error()))
	}

	if errors.Is(err, chat.ErrUnknownPersonality) {
		return errorStatus(codes.InvalidArgument, cairnerr.New(codeInvalidArgument, err.Error()))
	}

	return canonicalStatus(err)
}
