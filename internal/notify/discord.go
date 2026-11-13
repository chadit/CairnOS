package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// notifyTimeout bounds one webhook POST so a slow or hung endpoint never blocks
// the worker goroutine that holds a re-auth job past its lease.
const notifyTimeout = 10 * time.Second

// ReauthAlert is the operator-facing summary of a held job: which CLI tool's
// credential expired, which account it was, and the command that fixes it.
// Account is best-effort and may be empty until the resume probe
// reads it from the tool's credential file.
type ReauthAlert struct {
	Tool        string
	Account     string
	Remediation string
	DedupKey    string
}

// DiscordNotifier posts re-auth alerts to a Discord channel webhook. The webhook
// URL grants posting rights, so it is sourced from the environment, never a
// config file.
type DiscordNotifier struct {
	http       *http.Client
	webhookURL string
}

// NewDiscordNotifier builds a notifier that posts to webhookURL with a bounded
// per-request timeout.
func NewDiscordNotifier(webhookURL string) *DiscordNotifier {
	return NewDiscordNotifierOver(webhookURL, nil)
}

// NewDiscordNotifierOver is NewDiscordNotifier over a caller-supplied round
// tripper. A nil transport keeps the default one.
func NewDiscordNotifierOver(webhookURL string, transport http.RoundTripper) *DiscordNotifier {
	return &DiscordNotifier{
		webhookURL: webhookURL,
		http:       &http.Client{Timeout: notifyTimeout, Transport: transport},
	}
}

// discordPayload is the minimal Discord webhook body: a plain content string.
type discordPayload struct {
	Content string `json:"content"`
}

// Post sends content as one Discord webhook message. It is the generic sink the
// event fan-out drains one line at a time, and the primitive Notify renders an
// alert through. A non-2xx response is an error so the caller can log a failed
// delivery.
func (d *DiscordNotifier) Post(ctx context.Context, content string) error {
	if d.webhookURL == "" {
		return ErrEmptyWebhook
	}

	body, err := json.Marshal(discordPayload{Content: content})
	if err != nil {
		return fmt.Errorf("encode discord payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build discord request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("post discord webhook: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%w: %d", ErrNotifyStatus, resp.StatusCode)
	}

	return nil
}

// Notify posts the alert. It renders the alert to its content string and
// delegates to Post, so the direct re-auth alert and the fan-out share one
// webhook path. The caller treats notification as best-effort and never lets a
// failed delivery change the held job's fate.
func (d *DiscordNotifier) Notify(ctx context.Context, alert ReauthAlert) error {
	return d.Post(ctx, alert.Message())
}

// Message renders the alert as the Discord content string, leading with the tool
// and the fix so the line is actionable straight from a phone notification.
// Exported so the serve fan-out can post a held alert as one line through the
// same webhook, keeping the default ["held"] set byte-identical to the direct
// Notify path.
func (a ReauthAlert) Message() string {
	var builder strings.Builder

	builder.WriteString("CairnOS: a scheduled job is held, re-authentication needed.\n")

	writeField(&builder, "Tool", a.Tool)
	writeField(&builder, "Account", a.Account)
	writeField(&builder, "Fix", a.Remediation)
	writeField(&builder, "Job", a.DedupKey)

	return strings.TrimRight(builder.String(), "\n")
}

// writeField appends a "Label: value" line when value is non-empty, so an alert
// omits a field it has no data for rather than printing a blank label.
func writeField(builder *strings.Builder, label, value string) {
	if value == "" {
		return
	}

	builder.WriteString(label)
	builder.WriteString(": ")
	builder.WriteString(value)
	builder.WriteString("\n")
}
