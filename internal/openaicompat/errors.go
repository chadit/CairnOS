package openaicompat

import (
	"errors"
	"net/http"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// Wiring errors NewService returns when a required seam is missing, so a
// misconfigured node fails fast instead of serving an adapter it cannot back.
var (
	ErrNoAgent         = errors.New("openaicompat: agent is required")
	ErrNoStore         = errors.New("openaicompat: store is required")
	ErrNoPersonalities = errors.New("openaicompat: personalities are required")
)

// OpenAI error type values, the coarse class a client branches on alongside the
// HTTP status (.agents/spec/parity/openai-compat.md, "Errors").
const (
	typeAuthentication = "authentication_error"
	typePermission     = "permission_error"
	typeInvalidRequest = "invalid_request_error"
	typeServer         = "server_error"
)

// Canonical code strings the adapter answers with. The auth codes match the
// gRPC interceptor and internal/mcp exactly, so a log line here traces to the
// same code the native surface would have answered with; the request codes are
// the adapter's own.
const (
	codeAuthUnauthenticated = "auth.unauthenticated"
	codeAuthForbidden       = "auth.forbidden"
	codeAuthTokenExpired    = "auth.token_expired" // #nosec G101 -- canonical error code name, not a credential

	codeModelNotFound   = "model_not_found"
	codeInvalidBody     = "invalid_body"
	codeMissingMessages = "missing_messages"
	codeTrailingNotUser = "trailing_message_not_user"
	codeUnsupportedPart = "unsupported_content_part"
	codeRequestTooLarge = "request_too_large"
	codeInternalError   = "internal_error"
)

// httpError is one adapter failure carrying the HTTP status plus the OpenAI
// error triple. writeError renders it; the constructors below build one per
// class so a handler returns a typed value instead of assembling the shape at
// each call site, the way internal/chat keeps its codes typed per package.
type httpError struct {
	Type    string
	Code    string
	Message string
	Status  int
}

// Error renders "code: message" so a caller that only logs the error string
// still sees the canonical code.
func (e *httpError) Error() string {
	return e.Code + ": " + e.Message
}

func errUnauthenticated(message string) *httpError {
	return &httpError{Status: http.StatusUnauthorized, Type: typeAuthentication, Code: codeAuthUnauthenticated, Message: message}
}

func errTokenExpired(message string) *httpError {
	return &httpError{Status: http.StatusUnauthorized, Type: typeAuthentication, Code: codeAuthTokenExpired, Message: message}
}

func errForbidden(message string) *httpError {
	return &httpError{Status: http.StatusForbidden, Type: typePermission, Code: codeAuthForbidden, Message: message}
}

func errModelNotFound(message string) *httpError {
	return &httpError{Status: http.StatusNotFound, Type: typeInvalidRequest, Code: codeModelNotFound, Message: message}
}

func errInvalidBody(message string) *httpError {
	return &httpError{Status: http.StatusBadRequest, Type: typeInvalidRequest, Code: codeInvalidBody, Message: message}
}

func errMissingMessages(message string) *httpError {
	return &httpError{Status: http.StatusBadRequest, Type: typeInvalidRequest, Code: codeMissingMessages, Message: message}
}

func errTrailingNotUser(message string) *httpError {
	return &httpError{Status: http.StatusBadRequest, Type: typeInvalidRequest, Code: codeTrailingNotUser, Message: message}
}

func errUnsupportedPart(message string) *httpError {
	return &httpError{Status: http.StatusBadRequest, Type: typeInvalidRequest, Code: codeUnsupportedPart, Message: message}
}

func errRequestTooLarge(message string) *httpError {
	return &httpError{Status: http.StatusRequestEntityTooLarge, Type: typeInvalidRequest, Code: codeRequestTooLarge, Message: message}
}

// errorEnvelope is the OpenAI error wire shape: {"error": {...}}.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// errorBody is the inner error object clients read message, type, and code from.
type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// writeError renders any error onto the OpenAI shape: an *httpError as-is, a
// *cairnerr.Error (the chat.* turn failures) as 500 server_error carrying the
// canonical chat code, and anything else as 500 server_error / internal_error.
func (s *Service) writeError(w http.ResponseWriter, err error) {
	rendered := asHTTPError(err)

	s.writeJSON(w, rendered.Status, errorEnvelope{Error: errorBody{
		Message: rendered.Message,
		Type:    rendered.Type,
		Code:    rendered.Code,
	}})
}

// asHTTPError classifies an error into the HTTP-shaped triple writeError
// renders, so a chat.* turn failure keeps its canonical code on the wire.
func asHTTPError(err error) *httpError {
	if typed, ok := errors.AsType[*httpError](err); ok {
		return typed
	}

	if chatErr, ok := errors.AsType[*cairnerr.Error](err); ok {
		return &httpError{
			Status:  http.StatusInternalServerError,
			Type:    typeServer,
			Code:    chatErr.Code,
			Message: chatErr.Message,
		}
	}

	return &httpError{
		Status:  http.StatusInternalServerError,
		Type:    typeServer,
		Code:    codeInternalError,
		Message: "internal error",
	}
}
