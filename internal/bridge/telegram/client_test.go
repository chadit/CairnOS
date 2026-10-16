package telegram_test

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge/telegram"
	"github.com/chadit/CairnOS/internal/nettest"
)

// botEnvelope is the Bot API response wrapper the fake server writes and the
// client decodes: an ok flag plus the method's raw result, or a description on a
// failure. It mirrors the wire contract without touching the adapter's unexported
// envelope type.
type botEnvelope struct {
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	OK          bool            `json:"ok"`
}

// botHost is the base URL the suite's clients carry. Nothing dials it, but the
// client embeds it in every request, so a redaction assertion still sees the
// token sitting in a real URL.
const botHost = nettest.BaseURL

// botServer is the fake Bot API: it serves scripted getUpdates and records the
// sendMessage text so the round-trip test can assert real marshaling and
// decoding end to end.
type botServer struct {
	updates []telegram.Update
	sent    []string
	mu      sync.Mutex
}

// newBotServer builds the fake Bot API and the client that answers from it.
func newBotServer(t *testing.T) (*botServer, *telegram.Client) {
	t.Helper()

	bs := &botServer{}
	tripper := nettest.Transport(http.HandlerFunc(bs.handle))

	return bs, telegram.NewClientOver(testToken, botHost, time.Second, time.Second, tripper)
}

// setUpdates scripts the batch the next getUpdates returns.
func (bs *botServer) setUpdates(updates []telegram.Update) {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	bs.updates = updates
}

// sentTexts returns the texts sendMessage received, in order.
func (bs *botServer) sentTexts() []string {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	return append([]string(nil), bs.sent...)
}

// handle routes one Bot API request by its trailing method segment.
func (bs *botServer) handle(writer http.ResponseWriter, request *http.Request) {
	switch path.Base(request.URL.Path) {
	case "getUpdates":
		bs.mu.Lock()
		updates := bs.updates
		bs.mu.Unlock()

		writeEnvelope(writer, updates)
	case "sendMessage":
		var req struct {
			Text string `json:"text"`
		}

		if err := json.NewDecoder(request.Body).Decode(&req); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)

			return
		}

		bs.mu.Lock()
		bs.sent = append(bs.sent, req.Text)
		bs.mu.Unlock()

		writeEnvelope(writer, telegram.Message{MessageID: wantMessageID})
	default:
		http.Error(writer, "unknown method", http.StatusNotFound)
	}
}

// writeEnvelope wraps a method result in the Bot API ok envelope.
func writeEnvelope(writer http.ResponseWriter, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		http.Error(writer, "marshal", http.StatusInternalServerError)

		return
	}

	if err := json.NewEncoder(writer).Encode(botEnvelope{OK: true, Result: raw}); err != nil {
		http.Error(writer, "encode", http.StatusInternalServerError)
	}
}

// TestClientRedactsTokenOnConnectionFailure proves a transport error, which
// carries the token in the *url.Error URL, is returned with the token replaced
// by the redaction marker (.agents/spec/parity/messaging-bridge.md, decision 3).
func TestClientRedactsTokenOnConnectionFailure(t *testing.T) {
	t.Parallel()

	bot := telegram.NewClientOver(testToken, botHost, time.Second, time.Second, nettest.Unreachable(errDialRefused))

	_, err := bot.GetUpdates(t.Context(), 0, 1)
	if err == nil {
		t.Fatal("expected a connection error")
	}

	rendered := err.Error()
	if strings.Contains(rendered, testToken) {
		t.Errorf("token leaked in error: %q", rendered)
	}

	if !strings.Contains(rendered, wantRedaction) {
		t.Errorf("expected redaction marker in %q", rendered)
	}
}

// TestClientRedactsTokenInAPIError proves an ok=false envelope whose description
// echoes the token is returned token-free.
func TestClientRedactsTokenInAPIError(t *testing.T) {
	t.Parallel()

	refuser := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		envelope := botEnvelope{OK: false, Description: "unauthorized for " + testToken}
		if err := json.NewEncoder(writer).Encode(envelope); err != nil {
			http.Error(writer, "encode", http.StatusInternalServerError)
		}
	})

	bot := telegram.NewClientOver(testToken, botHost, time.Second, time.Second,
		nettest.Transport(refuser))

	_, err := bot.GetUpdates(t.Context(), 0, 1)
	if err == nil {
		t.Fatal("expected an API error")
	}

	if rendered := err.Error(); strings.Contains(rendered, testToken) {
		t.Errorf("token leaked in error: %q", rendered)
	}
}

// TestClientRoundTrip proves GetUpdates decodes a batch and SendMessage returns
// the sent message id, against the fake Bot API server.
func TestClientRoundTrip(t *testing.T) {
	t.Parallel()

	server, bot := newBotServer(t)
	server.setUpdates([]telegram.Update{*messageUpdate(1, 100, "hi")})

	updates, err := bot.GetUpdates(t.Context(), 0, 1)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}

	if len(updates) != 1 || updates[0].Message.Text != "hi" {
		t.Fatalf("unexpected updates: %+v", updates)
	}

	messageID, err := bot.SendMessage(t.Context(), 100, "pong", nil)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if messageID != wantMessageID {
		t.Errorf("message id = %d, want %d", messageID, wantMessageID)
	}

	if sent := server.sentTexts(); len(sent) != 1 || sent[0] != "pong" {
		t.Fatalf("unexpected sends: %+v", sent)
	}
}
