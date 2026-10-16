package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the public Bot API host and the only base URL the
	// production path uses: New passes it to NewClientOver whenever Config.API
	// is nil. NewClient also takes the base URL as a parameter so the client's own
	// tests can point at an httptest server; it is never read from config or an
	// env var (doc.go, token-exfiltration rationale).
	DefaultBaseURL = "https://api.telegram.org"

	// redactedToken replaces the bot token wherever a string might carry it.
	redactedToken = "[redacted]"

	methodGetUpdates          = "getUpdates"
	methodSendMessage         = "sendMessage"
	methodEditMessageText     = "editMessageText"
	methodAnswerCallbackQuery = "answerCallbackQuery"
)

// Update is one Bot API update: either a message or a callback-query tap. The
// adapter reads only the fields it routes on.
type Update struct {
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	UpdateID      int64          `json:"update_id"`
}

// Message is a Telegram chat message.
type Message struct {
	From      *User  `json:"from,omitempty"`
	Text      string `json:"text,omitempty"`
	Chat      Chat   `json:"chat"`
	MessageID int64  `json:"message_id"`
}

// Chat identifies the conversation a message belongs to.
type Chat struct {
	ID int64 `json:"id"`
}

// User identifies a Telegram sender; its ID is the allowlist key.
type User struct {
	ID int64 `json:"id"`
}

// CallbackQuery is an inline-keyboard tap.
type CallbackQuery struct {
	From    *User    `json:"from,omitempty"`
	Message *Message `json:"message,omitempty"`
	ID      string   `json:"id"`
	Data    string   `json:"data,omitempty"`
}

// InlineKeyboardMarkup is the reply markup for an Approve/Deny keyboard. The
// outbound adapter builds one; the client sends it verbatim.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// InlineKeyboardButton is one keyboard button carrying its callback_data.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// apiResponse is the Bot API envelope: an ok flag plus a raw result the caller
// decodes into the shape a given method returns.
type apiResponse struct {
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	OK          bool            `json:"ok"`
}

// BotAPI is the Bot API surface the adapter drives. *Client is the only
// production implementation, and the adapter holds it through this interface so
// it depends on the behavior, not the concrete HTTP client. A fake injects its
// own recorder through Config.API, which is what lets every adapter suite run as
// an external package with no white-box access.
type BotAPI interface {
	// GetUpdates long-polls for the next batch of updates starting at offset.
	GetUpdates(ctx context.Context, offset int64, timeoutSecs int) ([]Update, error)
	// SendMessage posts one message and returns the sent message id so the caller
	// can later edit it in place.
	SendMessage(ctx context.Context, chatID int64, text string, keyboard *InlineKeyboardMarkup) (int64, error)
	// EditMessageText rewrites a previously sent message.
	EditMessageText(ctx context.Context, chatID, messageID int64, text string) error
	// AnswerCallbackQuery clears a tapped button's spinner and optionally toasts.
	AnswerCallbackQuery(ctx context.Context, callbackID, text string) error
}

// Client is the Bot API client and the sole production BotAPI. token is the
// credential embedded in every URL path; baseURL is DefaultBaseURL on every
// production call and varied only by the client's own suite (doc.go). The
// client never logs and never returns a raw transport error: every failure is
// wrapped with the method name and a token-redacted cause.
type Client struct {
	http    *http.Client
	token   string
	baseURL string
	timeout time.Duration
}

// NewClient builds the Bot API client. baseURL is DefaultBaseURL on every
// production call; it is a parameter only so the client's own suite can point
// somewhere else (doc.go). sendTimeout bounds each non-poll call via its
// context, while the http.Client's Timeout is the long-poll ceiling that stops a
// hung connection.
func NewClient(token, baseURL string, sendTimeout, pollTimeout time.Duration) *Client {
	return NewClientOver(token, baseURL, sendTimeout, pollTimeout, nil)
}

// NewClientOver is NewClient over a caller-supplied round tripper. A nil
// transport takes the shared default, which is every production call. The
// suite passes a handler-backed tripper so its round trips open no socket: a
// loopback dial can land on an ephemeral port whose conntrack entries from an
// earlier listener have not aged out, and the SYN comes back EHOSTUNREACH.
func NewClientOver(
	token, baseURL string,
	sendTimeout, pollTimeout time.Duration,
	transport http.RoundTripper,
) *Client {
	return &Client{
		http:    &http.Client{Timeout: pollTimeout + pollDeadlineMargin, Transport: transport},
		token:   token,
		baseURL: baseURL,
		timeout: sendTimeout,
	}
}

// getUpdatesRequest is the long-poll request body.
type getUpdatesRequest struct {
	Offset  int64 `json:"offset"`
	Timeout int   `json:"timeout"`
}

// GetUpdates long-polls for the next batch of updates starting at offset. It
// carries a deadline of the long-poll timeout plus a margin so a hung
// connection cannot wedge the poll loop forever.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSecs int) ([]Update, error) {
	deadline := time.Duration(timeoutSecs)*time.Second + pollDeadlineMargin

	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	var updates []Update

	if err := c.call(ctx, methodGetUpdates, &getUpdatesRequest{Offset: offset, Timeout: timeoutSecs}, &updates); err != nil {
		return nil, err
	}

	return updates, nil
}

// sendMessageRequest posts one outbound message, optionally with an inline
// keyboard.
type sendMessageRequest struct {
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
	Text        string                `json:"text"`
	ChatID      int64                 `json:"chat_id"`
}

// SendMessage posts one message and returns the sent message id so the caller
// can later edit it (the outbound adapter edits an expired proposal in place).
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, keyboard *InlineKeyboardMarkup) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var sent Message

	req := &sendMessageRequest{ChatID: chatID, Text: text, ReplyMarkup: keyboard}
	if err := c.call(ctx, methodSendMessage, req, &sent); err != nil {
		return 0, err
	}

	return sent.MessageID, nil
}

// editMessageTextRequest rewrites the text of a message already sent, used to
// mark an expired proposal in place so the stale keyboard reads as settled.
type editMessageTextRequest struct {
	Text      string `json:"text"`
	ChatID    int64  `json:"chat_id"`
	MessageID int64  `json:"message_id"`
}

// EditMessageText rewrites a previously sent message. The outbound adapter edits
// an expired proposal so the operator sees it lapsed rather than a live keyboard.
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req := &editMessageTextRequest{ChatID: chatID, MessageID: messageID, Text: text}

	return c.call(ctx, methodEditMessageText, req, nil)
}

// answerCallbackQueryRequest clears the client spinner on a tapped button and
// optionally shows a toast.
type answerCallbackQueryRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
}

// AnswerCallbackQuery clears the tapped button's spinner right away, so the
// Telegram client responds no matter how busy the target conversation's worker
// is.
func (c *Client) AnswerCallbackQuery(ctx context.Context, callbackID, text string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req := &answerCallbackQueryRequest{CallbackQueryID: callbackID, Text: text}

	return c.call(ctx, methodAnswerCallbackQuery, req, nil)
}

// redact removes the bot token from a string so no error or log line carries
// it. The Bot API embeds the token in the URL path and net/http returns a
// *url.Error carrying the full URL, so this runs on every returned cause and
// every emitted log field.
func (c *Client) redact(text string) string {
	if c.token == "" {
		return text
	}

	return strings.ReplaceAll(text, c.token, redactedToken)
}

// call posts one Bot API method as JSON and decodes the result into out (a nil
// out discards it). Every error path wraps with the method name and a
// token-redacted cause, so a caller or a log never sees the raw URL.
func (c *Client) call(ctx context.Context, method string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return errf(CodeTelegramAPI, "telegram %s: marshal request: %s", method, c.redact(err.Error()))
	}

	endpoint := c.baseURL + "/bot" + c.token + "/" + method

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return errf(CodeTelegramAPI, "telegram %s: build request: %s", method, c.redact(err.Error()))
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return errf(CodeTelegramAPI, "telegram %s: %s", method, c.redact(err.Error()))
	}

	defer func() { _ = resp.Body.Close() }()

	return c.decode(method, resp, out)
}

// decode reads one Bot API response, turning a non-200 status or an ok=false
// envelope into a token-redacted error and unmarshaling result into out.
func (c *Client) decode(method string, resp *http.Response, out any) error {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return errf(CodeTelegramAPI, "telegram %s: read response: %s", method, c.redact(err.Error()))
	}

	if resp.StatusCode != http.StatusOK {
		return errf(CodeTelegramAPI, "telegram %s: http %d", method, resp.StatusCode)
	}

	var envelope apiResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return errf(CodeTelegramAPI, "telegram %s: decode response: %s", method, c.redact(err.Error()))
	}

	if !envelope.OK {
		return errf(CodeTelegramAPI, "telegram %s: %s", method, c.redact(envelope.Description))
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return errf(CodeTelegramAPI, "telegram %s: decode result: %s", method, c.redact(err.Error()))
	}

	return nil
}
