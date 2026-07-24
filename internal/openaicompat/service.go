package openaicompat

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
)

// defaultMaxBodyBytes caps a completions body when the config passes none, the
// same 1 MiB default the gRPC gateway uses (.agents/spec/parity/openai-compat.md,
// "Body cap").
const defaultMaxBodyBytes = 1 << 20

// bearerScheme is the accepted Authorization scheme, compared case-insensitively
// per RFC 9110, the same parse internal/mcp and the gRPC interceptor make.
const bearerScheme = "bearer "

// ChatAgent is the slice of *chat.Agent the adapter drives: open a fresh session
// per request and run one turn over it.
type ChatAgent interface {
	CreateSession(ctx context.Context, personality string) (*store.ChatSession, error)
	SendMessage(ctx context.Context, sessionID, userMessage string, out chan<- chat.Event) error
}

// MessageAppender is the slice of store.ChatStore the adapter seeds prior
// history through.
type MessageAppender interface {
	AppendMessage(ctx context.Context, message *store.ChatMessage) error
}

// Authenticator matches internal/mcp's; *auth.Authenticator satisfies it. A nil
// Authenticator means auth is disabled: every call passes on the default
// workspace, matching the rest of the API.
type Authenticator interface {
	Verify(ctx context.Context, token string) (*auth.Claims, error)
}

// Config wires one Service.
type Config struct {
	// Agent runs the turn. *chat.Agent satisfies ChatAgent.
	Agent ChatAgent

	// Store seeds prior history into the fresh session. store.ChatStore
	// satisfies MessageAppender.
	Store MessageAppender

	// Personalities resolves the model name to a known personality; it also
	// backs GET /v1/models (slice 3).
	Personalities *chat.Personalities

	// Auth turns on token enforcement. Nil disables auth.
	Auth Authenticator

	// MaxBodyBytes caps the request body; zero falls back to defaultMaxBodyBytes.
	MaxBodyBytes int64

	// Logger receives operational events; nil means slog.Default.
	Logger *slog.Logger
}

// Service is the OpenAI-compatible HTTP surface over the shared chat seams.
type Service struct {
	agent         ChatAgent
	store         MessageAppender
	personalities *chat.Personalities
	authenticator Authenticator
	maxBodyBytes  int64
	logger        *slog.Logger
}

// NewService validates the wiring and returns the Service, erroring on a nil
// Agent, Store, or Personalities so a node never mounts an adapter it cannot
// back. A nil authenticator is allowed and means auth is disabled.
func NewService(cfg *Config) (*Service, error) {
	if cfg.Agent == nil {
		return nil, ErrNoAgent
	}

	if cfg.Store == nil {
		return nil, ErrNoStore
	}

	if cfg.Personalities == nil {
		return nil, ErrNoPersonalities
	}

	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		agent:         cfg.Agent,
		store:         cfg.Store,
		personalities: cfg.Personalities,
		authenticator: cfg.Auth,
		maxBodyBytes:  maxBody,
		logger:        logger,
	}, nil
}

// authorize runs the same verify-then-scope path the gRPC interceptor and
// internal/mcp run (.agents/spec/08-auth-and-access.md, "Scopes"): with auth
// disabled it passes on the default workspace, otherwise the token must verify
// and carry chat:session. On success it returns a context stamped with the
// caller's workspace, so the session and every seeded message land in the right
// tenant, the same stamping the gRPC interceptor gives ChatService.
func (s *Service) authorize(ctx context.Context, token string) (context.Context, error) {
	if s.authenticator == nil {
		return store.ContextWithWorkspace(ctx, store.DefaultWorkspace), nil
	}

	if token == "" {
		return ctx, errUnauthenticated("missing bearer token: present Authorization: Bearer <token>")
	}

	claims, err := s.authenticator.Verify(ctx, token)
	if err != nil {
		return ctx, verifyError(err)
	}

	if !auth.HasScope(claims.Scopes, auth.ScopeChatSession) {
		return ctx, errForbidden("this endpoint requires the " + auth.ScopeChatSession + " scope")
	}

	return store.ContextWithWorkspace(ctx, claims.Workspace), nil
}

// verifyError maps a token verification failure onto the OpenAI shape, the same
// split internal/mcp and the gRPC guard make: expiry gets its own code so the
// client re-logs in, every other failure collapses to one opaque
// auth.unauthenticated that tells an attacker nothing about which check tripped.
func verifyError(err error) error {
	if errors.Is(err, auth.ErrTokenExpired) {
		return errTokenExpired("token expired: exchange your access key for a fresh one")
	}

	return errUnauthenticated("invalid token")
}

// bearerFromHeader pulls the token out of an Authorization header, returning the
// empty string when the header is missing or not a bearer token, so authorize
// answers auth.unauthenticated uniformly rather than branching on "no header"
// versus "bad scheme".
func bearerFromHeader(header http.Header) string {
	value := header.Get("Authorization")
	if len(value) > len(bearerScheme) && strings.EqualFold(value[:len(bearerScheme)], bearerScheme) {
		return value[len(bearerScheme):]
	}

	return ""
}

// Compile-time proof the real authenticator satisfies the narrow interface, so
// a signature drift in internal/auth surfaces here rather than at the serve
// wiring call site.
var _ Authenticator = (*auth.Authenticator)(nil)
