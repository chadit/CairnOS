package mattermost

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

// Configuration sentinels, raised by NewClient before any request is built.
var (
	// ErrNoURL reports an identity with no server URL.
	ErrNoURL = errors.New("mattermost: server url is required")

	// ErrBadURL reports a server URL that is not absolute. A bare host such as
	// "mm.example.test" parses as a path, and the resulting client would send
	// every request to nowhere.
	ErrBadURL = errors.New("mattermost: server url must be absolute, with a scheme and a host")

	// ErrInsecureURL reports a plain-http server URL without the explicit
	// insecure_http opt-in. The token rides a header, so http would put it on
	// the wire in the clear.
	ErrInsecureURL = errors.New("mattermost: server url must be https unless insecure_http is set")

	// ErrNoToken reports an identity with no bearer token.
	ErrNoToken = errors.New("mattermost: bot token is required")

	// ErrBadURLQuery reports a server URL carrying a query or a fragment. The API
	// path is joined onto the end of it, so a query would swallow the path and
	// send every call to "/" while the WebSocket URL still came out right.
	ErrBadURLQuery = errors.New("mattermost: server url must not carry a query or a fragment")

	// ErrNotConnected reports a call that needs the identity's own user id
	// before Connect has learned it.
	ErrNotConnected = errors.New("mattermost: client is not connected")

	// ErrSocketDial marks a Listen failure that never got past the WebSocket
	// upgrade. The reconnect loop resets its backoff only when the socket carried
	// something: a proxy that passes REST but refuses the upgrade would otherwise
	// reset the delay on every cycle and redial once a second forever (FR7).
	ErrSocketDial = errors.New("mattermost: websocket upgrade failed")
)

// Event-parsing sentinels. errNotPosted is the ordinary case: hello, typing and
// status frames arrive constantly, so a frame the bridge does not act on is
// routine traffic rather than something worth logging. The other three mark a
// posted frame whose shape the parser did not recognize, which is a server
// change worth seeing.
var (
	errNotPosted = errors.New("mattermost: not a posted event")

	errBadEnvelope = errors.New("mattermost: malformed event envelope")

	errBadPostedData = errors.New("mattermost: malformed posted payload")

	errBadPost = errors.New("mattermost: malformed post")
)

// ErrorClass tells the caller whether retrying can help. It mirrors the webhook
// runner's classification (internal/runner/builtins/webhook.go) so the whole
// codebase agrees on what a 429 or a 5xx means, with NotFound split out because
// the send path answers a missing channel differently from a bad token.
type ErrorClass int

const (
	// ClassTransient marks a failure worth retrying after backoff: a network
	// blip, a 5xx, a request timeout.
	ClassTransient ErrorClass = iota
	// ClassPermanent marks a failure retrying cannot fix: a revoked token, a
	// missing permission, a malformed request.
	ClassPermanent
	// ClassNotFound marks a channel, user, or post the server does not have.
	ClassNotFound
	// ClassRateLimited marks a 429; RetryAfter carries the server's own hint.
	ClassRateLimited
)

// APIError is a classified Mattermost API failure. It names the client method
// that failed and never the request: headers carry the bearer token, so keeping
// them out of the message keeps the token out of every log that prints it
// (FR43).
type APIError struct {
	Cause      error
	Method     string
	Message    string
	RetryAfter time.Duration
	Status     int
	Class      ErrorClass
}

func (e *APIError) Error() string {
	if e.Method == "" {
		return e.Message
	}

	return "mattermost " + e.Method + ": " + e.Message
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *APIError) Unwrap() error {
	return e.Cause
}

// maxRateLimitWait caps how long a 429 may park the shared bucket. The server
// sends seconds-until-reset and its own bucket refills at ten per second, so a
// larger value means a proxy or a misconfigured header, not a real wait.
const maxRateLimitWait = time.Minute

// defaultRateLimitWait is the park a 429 carrying no usable reset header gets.
// The server's bucket refills at ten per second, so a second clears a burst, and
// parking nothing spends the next token straight back into the limit.
const defaultRateLimitWait = time.Second

// rateLimitResetHeader carries seconds until the server's bucket refills.
const rateLimitResetHeader = "X-Ratelimit-Reset"

// classifyStatus maps one response onto a class, returning nil for 2xx.
func classifyStatus(method string, resp *http.Response) error {
	status := resp.StatusCode
	failure := &APIError{
		Method:  method,
		Message: "server returned " + resp.Status,
		Status:  status,
	}

	switch {
	case status >= http.StatusOK && status < http.StatusMultipleChoices:
		return nil
	case status == http.StatusTooManyRequests:
		failure.Class = ClassRateLimited
		failure.RetryAfter = rateLimitHint(resp.Header.Get(rateLimitResetHeader))
	case status == http.StatusNotFound:
		failure.Class = ClassNotFound
	case status == http.StatusRequestTimeout || status >= http.StatusInternalServerError:
		failure.Class = ClassTransient
	default:
		failure.Class = ClassPermanent
	}

	return failure
}

// rateLimitHint reads the reset header as whole seconds. A missing or unreadable
// value takes the default park rather than zero, because a zero park lets the
// retry spend the next token straight back into the limit it just hit.
func rateLimitHint(header string) time.Duration {
	seconds, err := strconv.Atoi(header)
	if err != nil || seconds <= 0 {
		return defaultRateLimitWait
	}

	wait := time.Duration(seconds) * time.Second
	if wait > maxRateLimitWait {
		return maxRateLimitWait
	}

	return wait
}
