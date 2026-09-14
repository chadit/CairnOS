package builtins_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture names for the GitHub runner tests.
const (
	opKey        = "op"
	repoKey      = "repo"
	titleKey     = "title"
	limitKey     = "limit"
	sinceKey     = "since"
	stateKey     = "state"
	testRepo     = "chadit/CairnOS"
	opReleases   = "releases"
	opNewIssue   = "create_issue"
	opListIssues = "list_issues"
	actionWatch  = "watch"
)

// githubReleasesBody is the golden release-feed response.
const githubReleasesBody = `[
	{"tag_name": "v0.2.0", "name": "Second", "published_at": "2026-06-01T00:00:00Z", "body": "notes two"},
	{"tag_name": "v0.1.0", "name": "First", "published_at": "2026-05-01T00:00:00Z", "body": "notes one"}
]`

// githubIssueBody is the golden created-issue response.
const githubIssueBody = `{"number": 42, "html_url": "https://github.test/chadit/CairnOS/issues/42"}`

func githubTask(params map[string]any) *runner.Task {
	return &runner.Task{RunID: testRunID, ActionName: actionWatch, Params: params, Resolved: true}
}

func newGitHub(t *testing.T, server *httptest.Server) *builtins.GitHub {
	t.Helper()

	hub, err := builtins.NewGitHub(map[string]any{baseURLKey: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("new github: %v", err)
	}

	return hub
}

func TestGitHubListsReleases(t *testing.T) {
	t.Parallel()

	paths := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path + "?" + r.URL.RawQuery

		_, _ = io.WriteString(w, githubReleasesBody)
	}))
	defer server.Close()

	result, err := newGitHub(t, server).Execute(t.Context(), githubTask(map[string]any{
		opKey:    opReleases,
		repoKey:  testRepo,
		limitKey: int64(2),
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if got := <-paths; got != "/repos/chadit/CairnOS/releases?per_page=2" {
		t.Fatalf("request path = %q", got)
	}

	items, isList := result.Output.([]any)
	if !isList || len(items) != 2 {
		t.Fatalf("output = %#v", result.Output)
	}

	first, isMap := items[0].(map[string]any)
	if !isMap || first["tag"] != "v0.2.0" || first["name"] != "Second" ||
		first["published_at"] != "2026-06-01T00:00:00Z" || first[bodyKey] != "notes two" {
		t.Fatalf("first release = %#v", items[0])
	}
}

func TestGitHubCreatesIssue(t *testing.T) {
	t.Parallel()

	requests := make(chan map[string]any, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil &&
			r.Method == http.MethodPost && r.URL.Path == "/repos/chadit/CairnOS/issues" {
			requests <- decoded
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, githubIssueBody)
	}))
	defer server.Close()

	result, err := newGitHub(t, server).Execute(t.Context(), githubTask(map[string]any{
		opKey:    opNewIssue,
		repoKey:  testRepo,
		titleKey: "release v0.2.0",
		bodyKey:  "new release spotted",
		"labels": []any{"release", "automated"},
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isMap := result.Output.(map[string]any)
	if !isMap || output["number"] != int64(42) ||
		output[urlKey] != "https://github.test/chadit/CairnOS/issues/42" {
		t.Fatalf("output = %#v", result.Output)
	}

	select {
	case request := <-requests:
		labels, _ := request["labels"].([]any)
		if request[titleKey] != "release v0.2.0" || len(labels) != 2 {
			t.Fatalf("request = %v", request)
		}
	default:
		t.Fatal("server saw no issue POST")
	}
}

// githubIssuesBody mixes a fresh issue, a stale-but-recently-updated issue that
// GitHub's since (updated_at) filter would still return, and a pull request that
// arrives as an issue row. list_issues should keep only the fresh issue.
const githubIssuesBody = `[
	{"number": 10, "title": "new bug", "body": "found it", "html_url": "https://github.test/chadit/CairnOS/issues/10", "created_at": "2026-06-20T00:00:00Z", "user": {"login": "octocat"}},
	{"number": 9, "title": "old comment churn", "body": "still open", "html_url": "https://github.test/chadit/CairnOS/issues/9", "created_at": "2026-06-01T00:00:00Z", "user": {"login": "hubber"}},
	{"number": 8, "title": "a pull request", "body": "code", "html_url": "https://github.test/chadit/CairnOS/pull/8", "created_at": "2026-06-25T00:00:00Z", "user": {"login": "octocat"}, "pull_request": {"url": "https://github.test/pulls/8"}}
]`

func TestGitHubListsIssues(t *testing.T) {
	t.Parallel()

	queries := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries <- r.URL.RawQuery

		_, _ = io.WriteString(w, githubIssuesBody)
	}))
	defer server.Close()

	result, err := newGitHub(t, server).Execute(t.Context(), githubTask(map[string]any{
		opKey:    opListIssues,
		repoKey:  testRepo,
		sinceKey: "2026-06-15T00:00:00Z",
		stateKey: "all",
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	query := <-queries
	if !strings.Contains(query, "state=all") || !strings.Contains(query, "since=2026-06-15") {
		t.Fatalf("query = %q, want state and since passed through", query)
	}

	items, isList := result.Output.([]any)
	if !isList {
		t.Fatalf("output = %#v, want a list", result.Output)
	}

	if len(items) != 1 {
		t.Fatalf("kept %d issues, want 1 (stale created_at and the PR row dropped)", len(items))
	}

	issue, isMap := items[0].(map[string]any)
	if !isMap {
		t.Fatalf("issue = %#v, want a map", items[0])
	}

	if issue["number"] != int64(10) || issue[titleKey] != "new bug" || issue[bodyKey] != "found it" ||
		issue[urlKey] != "https://github.test/chadit/CairnOS/issues/10" ||
		issue["author"] != "octocat" || issue["created_at"] != "2026-06-20T00:00:00Z" {
		t.Fatalf("issue = %#v", issue)
	}
}

func TestGitHubListsIssuesEmptySinceKeepsAll(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, githubIssuesBody)
	}))
	defer server.Close()

	result, err := newGitHub(t, server).Execute(t.Context(), githubTask(map[string]any{
		opKey:   opListIssues,
		repoKey: testRepo,
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	items, _ := result.Output.([]any)
	if len(items) != 2 {
		t.Fatalf("kept %d issues, want 2 (no since filter, PR row still dropped)", len(items))
	}
}

func TestGitHubClassifiesStatusCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		headers   map[string]string
		name      string
		wantCode  string
		status    int
		wantClass runner.ErrorClass
	}{
		{
			name: "403 with exhausted rate limit", status: http.StatusForbidden,
			headers:   map[string]string{"X-Ratelimit-Remaining": "0"},
			wantClass: runner.ClassRateLimited, wantCode: "github.rate_limited",
		},
		{
			name: "403 with secondary retry-after", status: http.StatusForbidden,
			headers:   map[string]string{"Retry-After": "7"},
			wantClass: runner.ClassRateLimited, wantCode: "github.rate_limited",
		},
		{
			name: "plain 403 is permanent auth", status: http.StatusForbidden,
			wantClass: runner.ClassPermanent, wantCode: "github.auth",
		},
		{
			name: "401 is permanent", status: http.StatusUnauthorized,
			wantClass: runner.ClassPermanent, wantCode: "github.auth",
		},
		{
			name: "500 retries", status: http.StatusInternalServerError,
			wantClass: runner.ClassTransient, wantCode: "github.status",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for key, value := range tt.headers {
					w.Header().Set(key, value)
				}

				w.WriteHeader(tt.status)
			}))
			defer server.Close()

			_, err := newGitHub(t, server).Execute(t.Context(), githubTask(map[string]any{
				opKey: opReleases, repoKey: testRepo,
			}))

			classified, isClassified := errors.AsType[*runner.Error](err)
			if !isClassified {
				t.Fatalf("error %v is not a runner.Error", err)
			}

			if classified.Class != tt.wantClass || classified.Code != tt.wantCode {
				t.Fatalf("class/code = %v/%q, want %v/%q", classified.Class, classified.Code, tt.wantClass, tt.wantCode)
			}

			if tt.headers["Retry-After"] != "" && classified.RetryAfter != 7*time.Second {
				t.Fatalf("retry after = %v, want 7s", classified.RetryAfter)
			}
		})
	}
}

func TestGitHubValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		params   map[string]any
		name     string
		resolved bool
		wantErr  bool
	}{
		{name: "missing op", params: map[string]any{repoKey: testRepo}, resolved: true, wantErr: true},
		{name: "unknown op", params: map[string]any{opKey: "stargaze", repoKey: testRepo}, resolved: true, wantErr: true},
		{name: "unresolved op defers", params: map[string]any{opKey: unresolved}, wantErr: false},
		{name: "missing repo", params: map[string]any{opKey: opReleases}, resolved: true, wantErr: true},
		{name: "repo without owner", params: map[string]any{opKey: opReleases, repoKey: "CairnOS"}, resolved: true, wantErr: true},
		{name: "unresolved repo defers", params: map[string]any{opKey: opReleases, repoKey: unresolved}, wantErr: false},
		{name: "non-integer limit", params: map[string]any{opKey: opReleases, repoKey: testRepo, limitKey: "ten"}, resolved: true, wantErr: true},
		{name: "releases ok", params: map[string]any{opKey: opReleases, repoKey: testRepo, limitKey: int64(5)}, resolved: true, wantErr: false},
		{name: "issue missing title", params: map[string]any{opKey: opNewIssue, repoKey: testRepo, bodyKey: "x"}, resolved: true, wantErr: true},
		{name: "issue missing body", params: map[string]any{opKey: opNewIssue, repoKey: testRepo, titleKey: "x"}, resolved: true, wantErr: true},
		{name: "non-string label", params: map[string]any{opKey: opNewIssue, repoKey: testRepo, titleKey: "x", bodyKey: "y", "labels": []any{int64(1)}}, resolved: true, wantErr: true},
		{name: "issue ok", params: map[string]any{opKey: opNewIssue, repoKey: testRepo, titleKey: "x", bodyKey: "y"}, resolved: true, wantErr: false},
		{name: "list_issues missing repo", params: map[string]any{opKey: opListIssues}, resolved: true, wantErr: true},
		{name: "list_issues ok", params: map[string]any{opKey: opListIssues, repoKey: testRepo}, resolved: true, wantErr: false},
		{name: "list_issues with since and state", params: map[string]any{opKey: opListIssues, repoKey: testRepo, sinceKey: "2026-06-01T00:00:00Z", stateKey: "all"}, resolved: true, wantErr: false},
		{name: "list_issues non-string since", params: map[string]any{opKey: opListIssues, repoKey: testRepo, sinceKey: int64(1)}, resolved: true, wantErr: true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hub, err := builtins.NewGitHub(nil, &http.Client{Timeout: time.Second})
			if err != nil {
				t.Fatalf("new github: %v", err)
			}

			err = hub.Validate(&runner.Task{
				RunID:      testRunID,
				ActionName: actionWatch,
				Params:     tt.params,
				Resolved:   tt.resolved,
			})

			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("validate error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}
