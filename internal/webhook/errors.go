package webhook

import "errors"

// Canonical envelope codes the ingress handler emits
// (.agents/spec/07-api-protocols.md, "HTTP response (error)"). Each failure in
// the request path answers with one of these in the canonical error body. The
// two replay codes (CodeReplayed) cover the freshness rejections the security
// council added on top of the original error table: a reused native nonce, a
// timestamp outside the skew window, or a duplicate GitHub delivery id.
const (
	CodeUnknownToken      = "webhook.unknown_token"
	CodeRateLimited       = "webhook.rate_limited"
	CodeBodyTooLarge      = "webhook.body_too_large"
	CodeBadSignature      = "webhook.bad_signature"
	CodeReplayed          = "webhook.replayed"
	CodeSecretUnavailable = "webhook.secret_unavailable"
	CodeBadPayload        = "webhook.bad_payload"
	CodePaused            = "webhook.paused"
	// CodeInternal is the generic 500 for a lookup, body-read, or run-start
	// failure the caller cannot act on. It carries no detail so a probe learns
	// nothing.
	CodeInternal = "webhook.internal"
)

// Sentinel errors for the webhook package. Callers match with errors.Is; the
// backends wrap these with %w so the driver detail rides along for logs.
var (
	// ErrNotFound reports a lookup (by token hash or by workflow name) that
	// matches no registration. The ingress handler maps it to a 404 in the
	// canonical envelope, before any body read.
	ErrNotFound = errors.New("webhook registration not found")

	// ErrDuplicate reports an Add for a workflow that already has a
	// registration; one registration per workflow is the invariant.
	ErrDuplicate = errors.New("webhook registration already exists")

	// ErrUnknownEndpoint reports an Endpoint value outside EndpointProd or
	// EndpointTest, which only a corrupt caller can produce.
	ErrUnknownEndpoint = errors.New("unknown webhook endpoint")

	// ErrNilPool reports a Postgres backend built without a pool; a store that
	// cannot reach a database is a wiring bug, caught before first use.
	ErrNilPool = errors.New("no database pool configured")
)
