package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/store"
)

// Route patterns the ingress handler answers, Go 1.22 mux wildcards. The test
// pattern is more specific, so a POST to /v1/hooks/test/{token} routes there
// rather than matching the production {token} single segment.
const (
	prodRoute = "POST /v1/hooks/{token}"
	testRoute = "POST /v1/hooks/test/{token}"
)

// tokenHexLen is the exact character length of a valid hex path token: 32 bytes
// of entropy render to 64 hex chars. The handler rejects any other length
// before a lookup or a body read, so garbage floods never reach the store
// (security amendment 3).
const tokenHexLen = tokenBytes * 2

// Trigger labels stamped on webhook-started runs; store.Run.Trigger records
// them as opaque strings.
const (
	triggerWebhook     = "webhook"
	triggerWebhookTest = "webhook_test"
)

// HMAC header forms. GitHub sends X-Hub-Signature-256 (sha256=<hex>) over the
// body alone; the native sender uses X-Cairnos-Signature-256 (bare hex) over
// timestamp + nonce + body so the signature itself carries freshness (security
// amendment 1). Header lookups canonicalize, so the exact casing here is
// cosmetic.
const (
	headerGitHubSig      = "X-Hub-Signature-256"
	headerGitHubDelivery = "X-Github-Delivery"
	headerCairnosSig     = "X-Cairnos-Signature-256"
	headerCairnosTS      = "X-Cairnos-Timestamp"
	headerCairnosNonce   = "X-Cairnos-Nonce"

	githubSigPrefix  = "sha256="
	signaturePartSep = "\n"
)

// envelopeStatusError is the fixed status discriminator in error bodies; the
// HTTP status code, never this field, decides success versus failure.
const (
	envelopeStatusError = "error"
	envelopeStatusOK    = "ok"
)

// workflowCodePrefix marks canonical validation errors the engine surfaces at
// StartRun (workflow.invalid and friends). The handler passes them through as a
// 400 so the caller sees the offending keys.
const workflowCodePrefix = "workflow."

// Default timings applied by NewHandler when a config leaves them zero.
const (
	defaultTestWait      = 30 * time.Second
	defaultSignatureSkew = 5 * time.Minute
)

// StartedRun is one started run as the ingress handler sees it: the run id to
// echo and a channel that delivers the terminal outcome exactly once.
type StartedRun struct {
	Outcome <-chan RunResult
	RunID   string
}

// RunResult is a started run's terminal state, delivered on StartedRun.Outcome.
// Run is the finished run record the test endpoint marshals into its response;
// Err is set when the run failed after it was accepted.
type RunResult struct {
	Run any
	Err error
}

// RunStarter starts one run and hands back its id plus a terminal-outcome
// channel. The serve wiring implements it as a closure over the run manager, so
// this package never imports internal/api.
type RunStarter interface {
	StartRun(ctx context.Context, workflowPath string, inputs map[string]any, trigger string) (StartedRun, error)
}

// PauseChecker answers the pause gate before a run starts, the same seam the
// scheduler's FireRunner reads.
type PauseChecker interface {
	IsPaused(ctx context.Context, workflowName string) (bool, error)
}

// SecretResolver resolves an HMAC secret by ref name. The env-backed
// secrets.Source satisfies it: the raw key rides CAIRNOS_SECRET_<NAME>, never
// the registrations table.
type SecretResolver interface {
	Get(ctx context.Context, name string) (string, error)
}

// HandlerConfig wires one Handler. NewHandler applies safe defaults for the
// durations and rates a caller leaves zero.
type HandlerConfig struct {
	Store   RegistrationReader
	Runs    RunStarter
	Pauses  PauseChecker
	Secrets SecretResolver

	// Now injects the clock the limiters and the replay cache read, so tests
	// drive rate and freshness windows without sleeping. Nil means time.Now.
	Now func() time.Time

	Logger *slog.Logger

	// MaxBodyBytes caps each request body (spec cfg.API.MaxBodyBytes). Extension
	// routes bypass the gateway's limitBody wrapper, so the handler must cap its
	// own body or it is an unbounded-body hole. Zero leaves the body uncapped.
	MaxBodyBytes int64

	// RatePerMinute is the default per-registration delivery cap; a registration
	// may override it.
	RatePerMinute int

	// UnknownRatePerMinute caps unknown-token requests process-wide (security
	// amendment 3).
	UnknownRatePerMinute int

	// TestWait bounds how long the test endpoint holds a connection for the
	// terminal run before answering 202.
	TestWait time.Duration

	// SignatureSkew is the native-signature timestamp window and the replay-cache
	// retention (security amendment 1).
	SignatureSkew time.Duration
}

// Handler serves the two webhook ingress routes. It is safe for concurrent use:
// the limiters and replay cache serialize their own writes and every other field
// is immutable after construction.
type Handler struct {
	store    RegistrationReader
	runs     RunStarter
	pauses   PauseChecker
	secrets  SecretResolver
	limiter  *registrationLimiter
	unknown  *globalLimiter
	replays  *replayCache
	now      func() time.Time
	logger   *slog.Logger
	maxBody  int64
	testWait time.Duration
	skew     time.Duration
}

// NewHandler builds a handler over its config, defaulting the clock, logger,
// wait budget, and skew window when unset.
func NewHandler(cfg *HandlerConfig) *Handler {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	testWait := cfg.TestWait
	if testWait <= 0 {
		testWait = defaultTestWait
	}

	skew := cfg.SignatureSkew
	if skew <= 0 {
		skew = defaultSignatureSkew
	}

	return &Handler{
		store:    cfg.Store,
		runs:     cfg.Runs,
		pauses:   cfg.Pauses,
		secrets:  cfg.Secrets,
		limiter:  newRegistrationLimiter(cfg.RatePerMinute, now),
		unknown:  newGlobalLimiter(cfg.UnknownRatePerMinute, now),
		replays:  newReplayCache(skew, now),
		maxBody:  cfg.MaxBodyBytes,
		testWait: testWait,
		skew:     skew,
		now:      now,
		logger:   logger,
	}
}

// Routes returns the two ingress handlers for the gateway's root mux, outside
// the gRPC interceptor. A webhook POST starts a run with no scope check and no
// tenant claim, so the path token is a write:workflow-equivalent capability for
// one workflow and the run executes under store.DefaultWorkspace. The ingress
// path provides no tenant isolation (security amendment 5).
func (h *Handler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		prodRoute: func(w http.ResponseWriter, r *http.Request) { h.serve(w, r, EndpointProd) },
		testRoute: func(w http.ResponseWriter, r *http.Request) { h.serve(w, r, EndpointTest) },
	}
}

// serve runs the ordered ingress gate for one request: token shape, lookup,
// body cap, HMAC (before the rate bucket), rate cap, payload mapping, pause
// gate, then the run start. Each failure answers in the canonical envelope and
// starts no run.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, endpoint Endpoint) {
	token := r.PathValue("token")

	// Shape gate before any lookup or body read: a token that is not exactly 64
	// hex chars matches nothing, so it is unknown by definition and never touches
	// the store (security amendment 3).
	if !validTokenShape(token) {
		h.rejectUnknown(w, r, "")

		return
	}

	hash := HashToken(token)

	reg, matched, err := h.store.GetByTokenHash(r.Context(), hash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			h.rejectUnknown(w, r, hash)

			return
		}

		h.logger.ErrorContext(r.Context(), "webhook token lookup", "hash_prefix", hashPrefix(hash), "error", err)
		h.writeError(w, r, http.StatusInternalServerError, CodeInternal, msgInternal, nil)

		return
	}

	// A token minted for one endpoint used on the other route is not a valid
	// capability for this path; treat it as unknown so a leaked test token never
	// opens the production path.
	if matched != endpoint {
		h.rejectUnknown(w, r, hash)

		return
	}

	body, gotBody := h.readBody(w, r, reg)
	if !gotBody {
		return
	}

	// HMAC verifies before the execution bucket is charged, so a leaked path
	// token without the signing secret cannot drain a registration's bucket with
	// bad signatures and starve real deliveries (security amendment 4).
	if reg.HMACSecretRef != "" && !h.verifyHMAC(w, r, reg, body) {
		return
	}

	if !h.limiter.allow(reg.ID, reg.RatePerMinute) {
		h.reject(w, r, reg, http.StatusTooManyRequests, CodeRateLimited, "rate limit exceeded")

		return
	}

	inputs, ok := h.buildInputs(w, r, reg, body)
	if !ok {
		return
	}

	if h.paused(r, reg) {
		h.reject(w, r, reg, http.StatusConflict, CodePaused, "workflow is paused")

		return
	}

	h.start(w, r, reg, endpoint, inputs)
}

// validTokenShape reports whether token is exactly 64 lowercase-or-uppercase hex
// chars, the minted shape. Anything else is rejected before a lookup.
func validTokenShape(token string) bool {
	if len(token) != tokenHexLen {
		return false
	}

	_, err := hex.DecodeString(token)

	return err == nil
}

// hashPrefix returns the first 8 hex chars of a token hash, the only
// token-derived value any log line may carry, and only on the unknown path. It
// is the same prefix the DB stores, so it recovers nothing. An empty hash (a
// shape-rejected token, which is never hashed) yields "".
func hashPrefix(hash string) string {
	const prefixLen = 8
	if len(hash) < prefixLen {
		return ""
	}

	return hash[:prefixLen]
}

// readBody reads the request body under a MaxBytesReader at the configured cap.
// An over-cap body is 413 with the cap in details, so a sender can size its
// payload; any other read failure is a generic 500. Both write the response
// and report false.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, reg *Registration) ([]byte, bool) {
	reader := r.Body
	if h.maxBody > 0 {
		reader = http.MaxBytesReader(w, r.Body, h.maxBody)
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		if maxErr, isMax := errors.AsType[*http.MaxBytesError](err); isMax {
			details := map[string]any{"limit_bytes": maxErr.Limit}
			h.rejectDetails(w, r, reg, http.StatusRequestEntityTooLarge, CodeBodyTooLarge, "request body too large", details)

			return nil, false
		}

		h.logger.ErrorContext(r.Context(), "webhook read body", "registration_id", reg.ID, "error", err)
		h.reject(w, r, reg, http.StatusInternalServerError, CodeInternal, msgInternal)

		return nil, false
	}

	return body, true
}

// buildInputs turns the request body into workflow inputs. Raw mode delivers the
// whole body as a string in the one declared PayloadInput; default mode parses a
// JSON object mapped 1:1 onto declared inputs, with engine.validateInputs the
// final say at StartRun. A non-object body in default mode is 400.
func (h *Handler) buildInputs(w http.ResponseWriter, r *http.Request, reg *Registration, body []byte) (map[string]any, bool) {
	if reg.PayloadInput != "" {
		return map[string]any{reg.PayloadInput: string(body)}, true
	}

	var inputs map[string]any
	if err := json.Unmarshal(body, &inputs); err != nil || inputs == nil {
		h.reject(w, r, reg, http.StatusBadRequest, CodeBadPayload, "request body must be a JSON object")

		return nil, false
	}

	return inputs, true
}

// paused reports whether the workflow is paused. A pause-read failure logs and
// proceeds rather than dropping a delivery, mirroring the scheduler's
// FireRunner.skipPaused precedent this seam is drawn from.
func (h *Handler) paused(r *http.Request, reg *Registration) bool {
	paused, err := h.pauses.IsPaused(r.Context(), reg.WorkflowName)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "webhook pause check", "registration_id", reg.ID, "error", err)

		return false
	}

	return paused
}

// start stamps the default workspace, starts the run, and dispatches to the
// production or test response path. Stamping DefaultWorkspace makes the
// scope-bypass invariant explicit: the ingress path is outside the gRPC scope
// interceptor and claims no tenant, so the run executes under the default
// workspace by construction, not by a context that merely happens to carry none
// (security amendment 5).
func (h *Handler) start(w http.ResponseWriter, r *http.Request, reg *Registration, endpoint Endpoint, inputs map[string]any) {
	trigger := triggerWebhook
	if endpoint == EndpointTest {
		trigger = triggerWebhookTest
	}

	// WithoutCancel detaches the run from the request: a dropped client (or the
	// 202 response returning) must not cancel a run that is meant to continue.
	runCtx := store.ContextWithWorkspace(context.WithoutCancel(r.Context()), store.DefaultWorkspace)

	started, err := h.runs.StartRun(runCtx, reg.WorkflowPath, inputs, trigger)
	if err != nil {
		h.startError(w, r, reg, err)

		return
	}

	// One synchronous access line carrying the registration ID (never the token
	// or path), so a known-registration hit is diagnosable even before the
	// background drain logs the terminal state.
	h.logger.InfoContext(r.Context(), "webhook run started",
		"registration_id", reg.ID, "run_id", started.RunID, "trigger", trigger)

	if endpoint == EndpointTest {
		h.serveTest(w, r, reg, started)

		return
	}

	go h.drain(reg, started)

	h.writeAccepted(w, r, started.RunID)
}

// startError maps a StartRun failure. A canonical workflow.* error (from
// engine.validateInputs) passes through as a 400 naming the offending keys;
// anything else is a generic 500.
func (h *Handler) startError(w http.ResponseWriter, r *http.Request, reg *Registration, err error) {
	code, message, details, ok := canonicalStartError(err)
	if ok && strings.HasPrefix(code, workflowCodePrefix) {
		h.rejectDetails(w, r, reg, http.StatusBadRequest, code, message, details)

		return
	}

	h.logger.ErrorContext(r.Context(), "webhook start run", "registration_id", reg.ID, "error", err)
	h.reject(w, r, reg, http.StatusInternalServerError, CodeInternal, msgInternal)
}

// canonicalStartError extracts a canonical code, message, and details from a
// StartRun error, whether it arrives as a single cairnerr.Error or the
// cairnerr.Errors list validateInputs returns. The bool reports whether a
// canonical error was found.
func canonicalStartError(err error) (string, string, map[string]any, bool) {
	if single, ok := errors.AsType[*cairnerr.Error](err); ok {
		return single.Code, single.Message, single.Details, true
	}

	if list, ok := errors.AsType[cairnerr.Errors](err); ok && len(list) > 0 {
		return list[0].Code, list.Error(), list[0].Details, true
	}

	return "", "", nil, false
}

// serveTest holds the bounded synchronous wait; serveProd is folded into start.
// The shared http.Server has no WriteTimeout and extension routes cannot add
// one, so an unbounded block on Outcome would pin a connection for a whole run.
// Whichever lands first wins: Outcome answers 200, the timer answers 202 and a
// drain goroutine lets the run finish in the background.
func (h *Handler) serveTest(w http.ResponseWriter, r *http.Request, reg *Registration, started StartedRun) {
	timer := time.NewTimer(h.testWait)
	defer timer.Stop()

	select {
	case res := <-started.Outcome:
		if res.Run == nil {
			h.logger.ErrorContext(r.Context(), "webhook test run produced no record",
				"registration_id", reg.ID, "run_id", started.RunID, "error", res.Err)
			h.reject(w, r, reg, http.StatusInternalServerError, CodeInternal, msgInternal)

			return
		}

		h.logger.InfoContext(r.Context(), "webhook test run finished",
			"registration_id", reg.ID, "run_id", started.RunID)
		h.writeJSON(w, r, http.StatusOK, res.Run)

	case <-timer.C:
		go h.drain(reg, started)

		h.writeAccepted(w, r, started.RunID)
	}
}

// drain reads a run's terminal outcome for logging and to keep the buffered
// Outcome channel from blocking after a 202. It carries the registration ID,
// never the token or path.
func (h *Handler) drain(reg *Registration, started StartedRun) {
	res := <-started.Outcome
	if res.Err != nil {
		h.logger.Warn("webhook run failed", "registration_id", reg.ID, "run_id", started.RunID, "error", res.Err)

		return
	}

	h.logger.Info("webhook run finished", "registration_id", reg.ID, "run_id", started.RunID)
}

// verifyHMAC resolves the registration's secret and verifies the request
// signature, folding replay resistance in (security amendment 1). An unresolvable
// secret ref fails closed with a 503 and a loud log naming the ref. It reports
// whether the request may proceed; a false result has already written the
// response.
func (h *Handler) verifyHMAC(w http.ResponseWriter, r *http.Request, reg *Registration, body []byte) bool {
	secret, err := h.secrets.Get(r.Context(), reg.HMACSecretRef)
	if err != nil {
		h.logger.WarnContext(r.Context(), "webhook hmac secret unresolved",
			"registration_id", reg.ID, "secret_ref", reg.HMACSecretRef, "error", err)
		h.writeError(w, r, http.StatusServiceUnavailable, CodeSecretUnavailable, "signature secret unavailable", nil)

		return false
	}

	if sig := r.Header.Get(headerGitHubSig); sig != "" {
		return h.verifyGitHub(w, r, reg, secret, body, sig)
	}

	if sig := r.Header.Get(headerCairnosSig); sig != "" {
		return h.verifyNative(w, r, reg, secret, body, sig)
	}

	h.reject(w, r, reg, http.StatusUnauthorized, CodeBadSignature, "missing signature header")

	return false
}

// verifyGitHub checks the GitHub-compatible X-Hub-Signature-256 (sha256=<hex>
// over the body alone) and dedupes the delivery id. The GitHub form carries no
// timestamp, so HMAC proves origin only; the delivery id is the sole freshness
// signal, so a missing one fails closed and a repeat is a replay.
func (h *Handler) verifyGitHub(w http.ResponseWriter, r *http.Request, reg *Registration, secret string, body []byte, sig string) bool {
	hexSig, hadPrefix := strings.CutPrefix(sig, githubSigPrefix)
	if !hadPrefix {
		h.reject(w, r, reg, http.StatusUnauthorized, CodeBadSignature, "malformed signature")

		return false
	}

	if !validMAC(secret, body, hexSig) {
		h.reject(w, r, reg, http.StatusUnauthorized, CodeBadSignature, msgSignatureMismatch)

		return false
	}

	delivery := r.Header.Get(headerGitHubDelivery)
	if delivery == "" {
		h.reject(w, r, reg, http.StatusUnauthorized, CodeBadSignature, "missing delivery id")

		return false
	}

	if !h.replays.admitDelivery(reg.ID, delivery) {
		h.reject(w, r, reg, http.StatusUnauthorized, CodeReplayed, "duplicate delivery")

		return false
	}

	return true
}

// verifyNative checks the native X-Cairnos-Signature-256 (bare hex over
// timestamp + nonce + body) with full replay resistance: a timestamp outside the
// skew window is rejected first, then the signature, then a reused nonce. Only a
// validly signed, in-window request reaches the nonce cache, so bad signatures
// cannot pollute it.
func (h *Handler) verifyNative(w http.ResponseWriter, r *http.Request, reg *Registration, secret string, body []byte, sig string) bool {
	timestamp := r.Header.Get(headerCairnosTS)
	nonce := r.Header.Get(headerCairnosNonce)

	if timestamp == "" || nonce == "" {
		h.reject(w, r, reg, http.StatusUnauthorized, CodeBadSignature, "missing signature headers")

		return false
	}

	signedAt, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		h.reject(w, r, reg, http.StatusUnauthorized, CodeBadSignature, "malformed timestamp")

		return false
	}

	if absDuration(h.now().Sub(time.Unix(signedAt, 0))) > h.skew {
		h.reject(w, r, reg, http.StatusUnauthorized, CodeReplayed, "timestamp outside skew window")

		return false
	}

	message := timestamp + signaturePartSep + nonce + signaturePartSep + string(body)
	if !validMAC(secret, []byte(message), sig) {
		h.reject(w, r, reg, http.StatusUnauthorized, CodeBadSignature, msgSignatureMismatch)

		return false
	}

	if !h.replays.admitNonce(reg.ID, nonce) {
		h.reject(w, r, reg, http.StatusUnauthorized, CodeReplayed, "nonce already used")

		return false
	}

	return true
}

// validMAC reports whether hexSig is the HMAC-SHA256 of message under secret.
// The compare is hmac.Equal over the decoded digests, constant-time, and the
// checkable requirement the acceptance criteria pin: no ==/bytes.Equal on MAC
// values anywhere in this package.
func validMAC(secret string, message []byte, hexSig string) bool {
	want, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(message)

	return hmac.Equal(mac.Sum(nil), want)
}

// absDuration returns the magnitude of d, so a timestamp in the near future or
// the near past both measure against the skew window.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}

	return d
}

// rejectUnknown answers an unknown-token request. It charges the process-wide
// unknown-token bucket first (security amendment 3): an empty bucket is a
// generic 429, otherwise a 404. No body is read and no run starts. hash is ""
// when the token failed the shape gate, so the log never carries the token or
// path, only an 8-char prefix when one exists.
func (h *Handler) rejectUnknown(w http.ResponseWriter, r *http.Request, hash string) {
	if !h.unknown.allow() {
		h.logger.InfoContext(r.Context(), "webhook unknown-token rate limited", "hash_prefix", hashPrefix(hash))
		h.writeError(w, r, http.StatusTooManyRequests, CodeRateLimited, "rate limit exceeded", nil)

		return
	}

	h.logger.InfoContext(r.Context(), "webhook unknown token", "hash_prefix", hashPrefix(hash))
	h.writeError(w, r, http.StatusNotFound, CodeUnknownToken, "unknown webhook token", nil)
}

// reject writes an error envelope for a known registration and logs the
// rejection with the registration ID, never the token or path.
func (h *Handler) reject(w http.ResponseWriter, r *http.Request, reg *Registration, status int, code, message string) {
	h.rejectDetails(w, r, reg, status, code, message, nil)
}

// rejectDetails is reject with a details map the error envelope carries to
// the sender.
func (h *Handler) rejectDetails(w http.ResponseWriter, r *http.Request, reg *Registration, status int, code, message string, details map[string]any) {
	h.logger.InfoContext(r.Context(), "webhook rejected", "registration_id", reg.ID, "code", code, "status", status)
	h.writeError(w, r, status, code, message, details)
}

// writeAccepted answers 202 with the shared {"status":"ok","run_id":...}
// envelope both the production path and the timed-out test path return.
func (h *Handler) writeAccepted(w http.ResponseWriter, r *http.Request, runID string) {
	h.writeJSON(w, r, http.StatusAccepted, acceptedBody{Status: envelopeStatusOK, RunID: runID})
}

// writeError writes the canonical HTTP error body. A nil details map serializes
// as {}.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}

	h.writeJSON(w, r, status, errorBody{
		Status: envelopeStatusError,
		Error:  errorDetail{Code: code, Message: message, Details: details},
	})
}

// writeJSON sets the content type, writes the status, and encodes body. An
// encode failure only logs, since the status is already committed.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.WarnContext(r.Context(), "write webhook response", "error", err)
	}
}

// msgInternal and msgSignatureMismatch are the two messages the handler emits
// from more than one path.
const (
	msgInternal          = "internal error"
	msgSignatureMismatch = "signature mismatch"
)

// acceptedBody is the 202 success envelope: an ack plus the run id to poll.
type acceptedBody struct {
	Status string `json:"status"`
	RunID  string `json:"run_id"`
}

// errorBody is the canonical HTTP error envelope.
type errorBody struct {
	Error  errorDetail `json:"error"`
	Status string      `json:"status"`
}

// errorDetail mirrors cairnerr.Error on the wire.
type errorDetail struct {
	Details map[string]any `json:"details"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
}
