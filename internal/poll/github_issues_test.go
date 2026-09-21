package poll_test

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/poll"
	"github.com/chadit/CairnOS/internal/runner/builtins"
)

// issuesBody mixes a fresh issue, a stale issue GitHub's since (updated_at)
// filter would still return, and a pull request arriving as an issue row. The
// list_issues op keeps only the fresh issue, so the adapter sees one event.
const issuesBody = `[
	{"number": 10, "title": "new bug", "body": "found it", "html_url": "https://github.test/o/r/issues/10", "created_at": "2026-06-20T00:00:00Z", "user": {"login": "octocat"}},
	{"number": 9, "title": "old churn", "body": "still open", "html_url": "https://github.test/o/r/issues/9", "created_at": "2026-06-01T00:00:00Z", "user": {"login": "hubber"}},
	{"number": 8, "title": "a pr", "body": "code", "html_url": "https://github.test/o/r/pull/8", "created_at": "2026-06-25T00:00:00Z", "user": {"login": "octocat"}, "pull_request": {"url": "https://github.test/pulls/8"}}
]`

func newIssuesSource(t *testing.T, server *nettest.Fake) *poll.GitHubIssues {
	t.Helper()

	hub, err := builtins.NewGitHub(map[string]any{"base_url": server.URL}, server.Client(), environ.Map(nil))
	if err != nil {
		t.Fatalf("new github: %v", err)
	}

	return poll.NewGitHubIssues(hub)
}

func TestGitHubIssuesPollAdvancesCursor(t *testing.T) {
	t.Parallel()

	queries := make(chan string, 1)

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries <- r.URL.RawQuery

		_, _ = io.WriteString(w, issuesBody)
	}))

	const cursor = "2026-06-15T00:00:00Z"

	events, next, err := newIssuesSource(t, server).Poll(t.Context(),
		map[string]any{"repo": "octocat/hello-world", "state": "all"}, cursor)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}

	if query := <-queries; !strings.Contains(query, "since=2026-06-15") {
		t.Fatalf("query = %q, want the cursor passed as since", query)
	}

	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (stale issue and PR row dropped)", len(events))
	}

	if next != "2026-06-20T00:00:00Z" {
		t.Fatalf("next cursor = %q, want the newest created_at seen", next)
	}

	event := events[0]
	if event.ID != "10" {
		t.Fatalf("event id = %q, want the issue number 10", event.ID)
	}

	if event.Inputs["number"] != int64(10) || event.Inputs["title"] != "new bug" ||
		event.Inputs["body"] != "found it" || event.Inputs["url"] != "https://github.test/o/r/issues/10" ||
		event.Inputs["author"] != "octocat" || event.Inputs["created_at"] != "2026-06-20T00:00:00Z" {
		t.Fatalf("event inputs = %#v", event.Inputs)
	}
}

func TestGitHubIssuesPollEmptyCursorNoBackfill(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)

		_, _ = io.WriteString(w, issuesBody)
	}))

	events, next, err := newIssuesSource(t, server).Poll(t.Context(),
		map[string]any{"repo": "octocat/hello-world"}, "")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}

	if len(events) != 0 {
		t.Fatalf("got %d events, want none on the first poll (no backfill)", len(events))
	}

	if hits.Load() != 0 {
		t.Fatalf("server saw %d requests, want 0: the first poll must not read the repo", hits.Load())
	}

	parsed, err := time.Parse(time.RFC3339, next)
	if err != nil {
		t.Fatalf("next cursor %q is not RFC3339: %v", next, err)
	}

	if drift := time.Since(parsed); drift < -5*time.Second || drift > 5*time.Second {
		t.Fatalf("next cursor %q is %v off from now, want roughly now", next, drift)
	}
}
