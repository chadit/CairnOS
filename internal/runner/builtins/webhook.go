package builtins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// WebhookName is the registry name of the HTTP notification runner.
const WebhookName = "webhook"

// paramURL doubles as the config key: a workflow param overrides the
// configured default target.
const paramURL = "url"

// defaultWebhookTimeout backstops the factory-built client. Per-task limits
// still come from ctx and the task deadline, which the engine sets tighter;
// this only catches an endpoint that hangs when no deadline is configured.
const defaultWebhookTimeout = 30 * time.Second

// Webhook posts a workflow payload to an HTTP endpoint, the second notification
// sink in the built-ins table.
//
// Config: url, the default target. Params: body (required; strings post
// verbatim, anything else JSON-encodes) and url (overrides the config
// target). Timeouts come from ctx and the task deadline, not the client.
type Webhook struct {
	client *http.Client
	url    string
}

// NewWebhook builds the runner over an injected HTTP client so tests can
// point it at httptest servers.
func NewWebhook(cfg map[string]any, client *http.Client) (*Webhook, error) {
	url, err := configValue(cfg, paramURL, "")
	if err != nil {
		return nil, &configError{runner: WebhookName, err: err}
	}

	return &Webhook{url: url, client: client}, nil
}

// WebhookFactory adapts guardedWebhook to the registry contract.
func WebhookFactory() runner.Factory {
	return asFactory(guardedWebhook)
}

// guardedWebhook builds the runner with the shared egress guard wrapping the
// client. allow_private_targets opts out of the IP guard for operators that
// post to internal endpoints on purpose.
func guardedWebhook(cfg map[string]any) (*Webhook, error) {
	allowPrivate, err := configValue(cfg, cfgKeyAllowPrivateTargets, false)
	if err != nil {
		return nil, &configError{runner: WebhookName, err: err}
	}

	return NewWebhook(cfg, safeHTTPClient(WebhookName, defaultWebhookTimeout, allowPrivate))
}

// Name implements runner.Runner.
func (*Webhook) Name() string {
	return WebhookName
}

// Validate implements runner.Runner: params must carry body, and a target
// URL must come from either params or config.
func (w *Webhook) Validate(task *runner.Task) error {
	if _, exists := task.Params[paramBody]; !exists {
		return taskError(runner.ClassPermanent, WebhookName+".missing_body", "params must include body")
	}

	if err := optionalStringParam(task, paramURL, WebhookName); err != nil {
		return err
	}

	target, _ := task.Params[paramURL].(string)
	if target == "" && w.url == "" {
		return taskError(runner.ClassPermanent, WebhookName+".missing_url",
			"set url in params or in the runner config")
	}

	return nil
}

// Execute implements runner.Runner.
func (w *Webhook) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := w.Validate(task); err != nil {
		return runner.Result{}, err
	}

	target, _ := task.Params[paramURL].(string)
	if target == "" {
		target = w.url
	}

	if err := guardTarget(WebhookName, target); err != nil {
		return runner.Result{}, err
	}

	payload, contentType, err := encodeBody(task.Params[paramBody])
	if err != nil {
		return runner.Result{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return runner.Result{}, &runner.Error{
			Class: runner.ClassPermanent, Code: WebhookName + ".bad_url",
			Message: "build request: " + err.Error(), Cause: err,
		}
	}

	request.Header.Set("Content-Type", contentType)

	start := time.Now()

	response, err := w.client.Do(request)
	if err != nil {
		if blocked := classifyBlockedDial(WebhookName, err); blocked != nil {
			return runner.Result{}, blocked
		}

		return runner.Result{}, classifyTransport(ctx, err)
	}

	defer func() { _ = response.Body.Close() }()

	// The cap matches captured CLI output: enough to be useful downstream,
	// bounded so a misbehaving endpoint cannot balloon run storage.
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCapturedOutput))
	if err != nil {
		return runner.Result{}, &runner.Error{
			Class: runner.ClassTransient, Code: WebhookName + ".read",
			Message: "read response: " + err.Error(), Cause: err,
		}
	}

	if err := classifyStatus(response); err != nil {
		return runner.Result{}, err
	}

	return runner.Result{
		Output: string(body),
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   map[string]string{"status_code": strconv.Itoa(response.StatusCode)},
	}, nil
}

// encodeBody passes strings through verbatim and JSON-encodes everything
// else, so workflows can post structured action outputs without stringifying
// them first.
func encodeBody(value any) ([]byte, string, error) {
	if text, isString := value.(string); isString {
		return []byte(text), contentTypeText, nil
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, "", &runner.Error{
			Class: runner.ClassPermanent, Code: WebhookName + ".bad_body",
			Message: "encode body: " + err.Error(), Cause: err,
		}
	}

	return encoded, contentTypeJSON, nil
}

// classifyTransport maps a failed round trip. Cancellation and deadline both
// surface as the timeout code; every other transport failure stays transient
// because connection problems are the retryable case notifications care
// about.
func classifyTransport(ctx context.Context, err error) error {
	code := WebhookName + ".network"
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		code = WebhookName + ".timeout"
	}

	return &runner.Error{
		Class: runner.ClassTransient, Code: code,
		Message: "post webhook: " + err.Error(), Cause: err,
	}
}

// classifyStatus turns a non-2xx response into a classified error: 429 is
// rate limited with the Retry-After hint, 408 and 5xx are transient, every
// other 4xx is permanent.
func classifyStatus(response *http.Response) error {
	status := response.StatusCode
	message := "webhook returned " + response.Status

	switch {
	case status >= http.StatusOK && status < http.StatusMultipleChoices:
		return nil
	case status == http.StatusTooManyRequests:
		return &runner.Error{
			Class:      runner.ClassRateLimited,
			Code:       WebhookName + ".rate_limited",
			Message:    message,
			RetryAfter: retryAfterHint(response.Header.Get("Retry-After")),
		}
	case status == http.StatusRequestTimeout || status >= http.StatusInternalServerError:
		return &runner.Error{Class: runner.ClassTransient, Code: WebhookName + ".status", Message: message}
	default:
		return &runner.Error{Class: runner.ClassPermanent, Code: WebhookName + ".status", Message: message}
	}
}

// retryAfterHint parses a Retry-After header, which is either delta seconds
// or an HTTP date. Zero means no usable hint, letting the engine fall back
// to computed backoff.
func retryAfterHint(header string) time.Duration {
	if header == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}

	if when, err := http.ParseTime(header); err == nil {
		return time.Until(when)
	}

	return 0
}
