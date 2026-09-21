package notify_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/notify"
)

const toolCodex = "codex"

func TestDiscordNotifierPostsAlert(t *testing.T) {
	t.Parallel()

	got := make(chan string, 1)

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- string(body)

		w.WriteHeader(http.StatusNoContent)
	}))

	notifier := notify.NewDiscordNotifierOver(server.URL, server.Transport())

	alert := notify.ReauthAlert{
		Tool:        toolCodex,
		Remediation: "codex exited (re-authenticate the codex CLI)",
		DedupKey:    "sched-7|2026-06-13T00:00:00Z",
	}

	if err := notifier.Notify(t.Context(), alert); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	select {
	case body := <-got:
		// The webhook body is JSON-escaped, so assert on the fields the operator
		// reads rather than reconstructing the exact escaped payload.
		for _, want := range []string{toolCodex, "re-authenticate the codex CLI", "sched-7"} {
			if !strings.Contains(body, want) {
				t.Errorf("posted body missing %q:\n%s", want, body)
			}
		}
	default:
		t.Fatal("webhook handler never received the post")
	}
}

func TestDiscordNotifierReportsErrorStatus(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	notifier := notify.NewDiscordNotifierOver(server.URL, server.Transport())

	err := notifier.Notify(t.Context(), notify.ReauthAlert{Tool: "qwen"})
	if !errors.Is(err, notify.ErrNotifyStatus) {
		t.Fatalf("Notify error = %v, want ErrNotifyStatus", err)
	}
}

func TestDiscordNotifierRejectsEmptyWebhook(t *testing.T) {
	t.Parallel()

	notifier := notify.NewDiscordNotifier("")

	err := notifier.Notify(t.Context(), notify.ReauthAlert{Tool: toolCodex})
	if !errors.Is(err, notify.ErrEmptyWebhook) {
		t.Fatalf("Notify error = %v, want ErrEmptyWebhook", err)
	}
}

func TestReauthAlertMessageOmitsEmptyFields(t *testing.T) {
	t.Parallel()

	got := make(chan string, 1)

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- string(body)

		w.WriteHeader(http.StatusNoContent)
	}))

	notifier := notify.NewDiscordNotifierOver(server.URL, server.Transport())

	// Only Tool is set: Account, Fix, and Job labels must not appear.
	if err := notifier.Notify(t.Context(), notify.ReauthAlert{Tool: toolCodex}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	body := <-got
	for _, absent := range []string{"Account:", "Fix:", "Job:"} {
		if strings.Contains(body, absent) {
			t.Errorf("message should omit empty field, found %q:\n%s", absent, body)
		}
	}
}
