package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/pkg/runner"
)

// GitHubName is the registry name of the GitHub REST runner.
const GitHubName = "github"

// githubTokenEnv names where the API token comes from. The token is optional:
// public reads (release feeds) work anonymously at a lower rate limit, while
// create_issue needs it.
const githubTokenEnv = "CAIRNOS_RUNNER_GITHUB_TOKEN" // #nosec G101 -- environment variable name, not a credential

// defaultGitHubBaseURL targets the public API; base_url in config points the
// runner at GitHub Enterprise hosts instead.
const defaultGitHubBaseURL = "https://api.github.com"

// githubAPIVersion pins the REST revision every request declares.
const githubAPIVersion = "2022-11-28"

// GitHub params and operations.
const (
	paramOp     = "op"
	paramRepo   = "repo"
	paramLimit  = "limit"
	paramTitle  = "title"
	paramLabels = "labels"
	paramSince  = "since"
	paramState  = "state"

	opReleases    = "releases"
	opCreateIssue = "create_issue"
	opListIssues  = "list_issues"
)

// list_issues output keys. title, body, and url reuse the shared param names so
// the surviving-issue map speaks the same vocabulary poll.GitHubIssues reads.
const (
	keyNumber    = "number"
	keyAuthor    = "author"
	keyCreatedAt = "created_at"
)

// defaultReleaseLimit caps the releases listing when no limit param is given.
const defaultReleaseLimit = 10

// defaultIssueState scopes list_issues when the state param is absent, and
// issuesPerPage caps one poll's page at GitHub's per_page maximum.
const (
	defaultIssueState = "open"
	issuesPerPage     = 100
)

// metaStatusCode is the Meta key carrying the HTTP status of the API call.
const metaStatusCode = "status_code"

// defaultGitHubTimeout backstops the factory-built client; REST calls are
// short, unlike model generations.
const defaultGitHubTimeout = 30 * time.Second

// GitHub reads release feeds and lists or creates issues over the REST v3 API,
// backing the release monitor and the github_issues poll source.
//
// Config: base_url (default https://api.github.com). Params: op (releases,
// list_issues, create_issue), repo as owner/name, plus limit for releases,
// since/state for list_issues, title/body/labels for create_issue. The token
// comes from CAIRNOS_RUNNER_GITHUB_TOKEN and is optional for public reads.
type GitHub struct {
	client *http.Client

	// env answers the token variable. Injecting it keeps a test from having to
	// set a token every other test in the process would then see.
	env environ.Lookup

	baseURL string
}

// NewGitHub builds the runner over an injected HTTP client so tests can
// point it at httptest servers.
func NewGitHub(cfg map[string]any, client *http.Client, lookup environ.Lookup) (*GitHub, error) {
	baseURL, err := configValue(cfg, cfgKeyBaseURL, defaultGitHubBaseURL)
	if err != nil {
		return nil, &configError{runner: GitHubName, err: err}
	}

	return &GitHub{baseURL: strings.TrimSuffix(baseURL, "/"), client: client, env: lookup}, nil
}

// GitHubFactory adapts NewGitHub to the registry contract with the shared
// egress guard wrapping the client. allow_private_targets opts out of the IP
// guard for operators pointing the runner at an internal GitHub Enterprise host.
func GitHubFactory(lookup environ.Lookup) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		allowPrivate, err := configValue(cfg, cfgKeyAllowPrivateTargets, false)
		if err != nil {
			return nil, &configError{runner: GitHubName, err: err}
		}

		return NewGitHub(cfg, safeHTTPClient(GitHubName, defaultGitHubTimeout, allowPrivate), lookup)
	}
}

// Name implements runner.Runner.
func (*GitHub) Name() string {
	return GitHubName
}

// Validate implements runner.Runner: op picks the operation and gates which
// other params are required. An unresolved op reference defers the per-op
// checks to launch, because the operation itself is unknown until then.
func (*GitHub) Validate(task *runner.Task) error {
	if err := requireStringParam(task, paramOp, GitHubName); err != nil {
		return err
	}

	opText, _ := task.Params[paramOp].(string)
	if !task.Resolved && containsRefDelimiter(opText) {
		return nil
	}

	switch opText {
	case opReleases:
		if err := validateGitHubRepo(task); err != nil {
			return err
		}

		return validateReleaseLimit(task)
	case opListIssues:
		if err := validateGitHubRepo(task); err != nil {
			return err
		}

		return validateListIssuesParams(task)
	case opCreateIssue:
		if err := validateGitHubRepo(task); err != nil {
			return err
		}

		return validateIssueParams(task)
	default:
		return paramError(GitHubName, "op",
			fmt.Sprintf("must be %s, %s, or %s, got %q", opReleases, opListIssues, opCreateIssue, opText))
	}
}

// Execute implements runner.Runner.
func (g *GitHub) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := g.Validate(task); err != nil {
		return runner.Result{}, err
	}

	op, _ := task.Params[paramOp].(string)

	switch op {
	case opCreateIssue:
		return g.createIssue(ctx, task)
	case opListIssues:
		return g.listIssues(ctx, task)
	default:
		return g.listReleases(ctx, task)
	}
}

// listReleases handles op = releases: GET the repo's release feed and map
// each entry onto the documented output keys.
func (g *GitHub) listReleases(ctx context.Context, task *runner.Task) (runner.Result, error) {
	repo, _ := task.Params[paramRepo].(string)

	limit := int64(defaultReleaseLimit)
	if value, isInt := task.Params[paramLimit].(int64); isInt {
		limit = value
	}

	url := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", g.baseURL, repo, limit)
	start := time.Now()

	body, status, err := g.send(ctx, http.MethodGet, url, nil)
	if err != nil {
		return runner.Result{}, err
	}

	var releases []githubRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		return runner.Result{}, &runner.Error{
			Class: runner.ClassPermanent, Code: GitHubName + codeSuffixBadResponse,
			Message: "decode releases: " + err.Error(), Cause: err,
		}
	}

	items := make([]any, 0, len(releases))
	for _, release := range releases {
		items = append(items, map[string]any{
			"tag":          release.TagName,
			keyName:        release.Name,
			"published_at": release.PublishedAt,
			paramBody:      release.Body,
		})
	}

	return runner.Result{
		Output: items,
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   map[string]string{metaStatusCode: strconv.Itoa(status)},
	}, nil
}

// createIssue handles op = create_issue: POST the issue and surface its
// number and URL for downstream actions.
func (g *GitHub) createIssue(ctx context.Context, task *runner.Task) (runner.Result, error) {
	repo, _ := task.Params[paramRepo].(string)
	title, _ := task.Params[paramTitle].(string)
	body, _ := task.Params[paramBody].(string)

	payload := githubIssueRequest{
		Title:  title,
		Body:   body,
		Labels: labelStrings(task.Params[paramLabels]),
	}

	url := g.baseURL + "/repos/" + repo + "/issues"
	start := time.Now()

	responseBody, status, err := g.send(ctx, http.MethodPost, url, &payload)
	if err != nil {
		return runner.Result{}, err
	}

	var issue githubIssueResponse
	if err := json.Unmarshal(responseBody, &issue); err != nil {
		return runner.Result{}, &runner.Error{
			Class: runner.ClassPermanent, Code: GitHubName + codeSuffixBadResponse,
			Message: "decode issue: " + err.Error(), Cause: err,
		}
	}

	return runner.Result{
		Output: map[string]any{keyNumber: issue.Number, paramURL: issue.HTMLURL},
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   map[string]string{metaStatusCode: strconv.Itoa(status)},
	}, nil
}

// listIssues handles op = list_issues, mapping each issue onto the fields the
// github_issues poll source offers a workflow. Two GitHub quirks need absorbing
// here: the since filter matches updated_at, so a fresh comment resurfaces an
// old issue, and pull requests come back as issue rows carrying a pull_request
// field.
func (g *GitHub) listIssues(ctx context.Context, task *runner.Task) (runner.Result, error) {
	repo, _ := task.Params[paramRepo].(string)

	state := defaultIssueState
	if value, ok := task.Params[paramState].(string); ok && value != "" {
		state = value
	}

	since, _ := task.Params[paramSince].(string)

	query := neturl.Values{}
	query.Set("state", state)
	query.Set("per_page", strconv.Itoa(issuesPerPage))

	if since != "" {
		query.Set(paramSince, since)
	}

	requestURL := g.baseURL + "/repos/" + repo + "/issues?" + query.Encode()
	start := time.Now()

	body, status, err := g.send(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return runner.Result{}, err
	}

	var issues []githubIssue
	if err := json.Unmarshal(body, &issues); err != nil {
		return runner.Result{}, &runner.Error{
			Class: runner.ClassPermanent, Code: GitHubName + codeSuffixBadResponse,
			Message: "decode issues: " + err.Error(), Cause: err,
		}
	}

	return runner.Result{
		Output: issueItems(issues, since),
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   map[string]string{metaStatusCode: strconv.Itoa(status)},
	}, nil
}

// issueItems drops PR rows and, when since is set, issues whose created_at is
// not strictly newer than since, then projects each survivor onto the poll
// source's input keys.
func issueItems(issues []githubIssue, since string) []any {
	items := make([]any, 0, len(issues))

	for _, issue := range issues {
		if len(issue.PullRequest) > 0 {
			continue
		}

		if since != "" && !createdAfter(issue.CreatedAt, since) {
			continue
		}

		items = append(items, map[string]any{
			keyNumber:    issue.Number,
			paramTitle:   issue.Title,
			paramBody:    issue.Body,
			paramURL:     issue.HTMLURL,
			keyAuthor:    issue.User.Login,
			keyCreatedAt: issue.CreatedAt,
		})
	}

	return items
}

// createdAfter reports whether the created RFC3339 timestamp is strictly newer
// than since. An unparseable created stamp reads as not-after so a row we can't
// order never re-fires; an unparseable since reads as no lower bound.
func createdAfter(created, since string) bool {
	createdAt, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return false
	}

	lowerBound, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return true
	}

	return createdAt.After(lowerBound)
}

// validateListIssuesParams checks the op = list_issues params: since and state
// are optional strings (since is RFC3339, resolved at run time by the source).
func validateListIssuesParams(task *runner.Task) error {
	if err := optionalStringParam(task, paramSince, GitHubName); err != nil {
		return err
	}

	return optionalStringParam(task, paramState, GitHubName)
}

// send runs one REST call and returns the response body and status with
// failures already classified. A nil payload sends no body.
func (g *GitHub) send(ctx context.Context, method, url string, payload any) ([]byte, int, error) {
	var requestBody io.Reader

	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, &runner.Error{
				Class: runner.ClassPermanent, Code: GitHubName + ".bad_request",
				Message: "encode request: " + err.Error(), Cause: err,
			}
		}

		requestBody = strings.NewReader(string(encoded))
	}

	if err := guardTarget(GitHubName, url); err != nil {
		return nil, 0, err
	}

	request, err := http.NewRequestWithContext(ctx, method, url, requestBody)
	if err != nil {
		return nil, 0, &runner.Error{
			Class: runner.ClassPermanent, Code: GitHubName + ".bad_url",
			Message: "build request: " + err.Error(), Cause: err,
		}
	}

	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-Github-Api-Version", githubAPIVersion)

	if token := g.env.Get(githubTokenEnv); token != "" {
		request.Header.Set(headerAuthorization, "Bearer "+token)
	}

	response, err := g.client.Do(request)
	if err != nil {
		if blocked := classifyBlockedDial(GitHubName, err); blocked != nil {
			return nil, 0, blocked
		}

		return nil, 0, classifyAPITransport(ctx, GitHubName, err)
	}

	defer func() { _ = response.Body.Close() }()

	if classifyGitHubStatusErr := classifyGitHubStatus(response); classifyGitHubStatusErr != nil {
		return nil, response.StatusCode, classifyGitHubStatusErr
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxCapturedOutput))
	if err != nil {
		return nil, response.StatusCode, &runner.Error{
			Class: runner.ClassTransient, Code: GitHubName + codeSuffixRead,
			Message: "read response: " + err.Error(), Cause: err,
		}
	}

	return body, response.StatusCode, nil
}

// classifyGitHubStatus extends the shared API status mapping with GitHub's
// rate-limit signaling: both primary (403 with an exhausted
// X-RateLimit-Remaining) and secondary (Retry-After) limits surface as 403,
// not 429, so they need a check before the generic 403-is-auth rule.
func classifyGitHubStatus(response *http.Response) error {
	status := response.StatusCode
	limited := status == http.StatusForbidden &&
		(response.Header.Get("X-Ratelimit-Remaining") == "0" || response.Header.Get(headerRetryAfter) != "")

	if !limited {
		return classifyAPIStatus(GitHubName, response)
	}

	return &runner.Error{
		Class:      runner.ClassRateLimited,
		Code:       GitHubName + codeSuffixRateLimited,
		Message:    GitHubName + " returned " + response.Status + bodySnippet(response),
		RetryAfter: githubRetryHint(response),
	}
}

// githubRetryHint prefers the Retry-After header and falls back to the
// X-RateLimit-Reset epoch, GitHub's primary-limit signal. Zero means no
// usable hint.
func githubRetryHint(response *http.Response) time.Duration {
	if hint := retryAfterHint(response.Header.Get(headerRetryAfter)); hint > 0 {
		return hint
	}

	reset, err := strconv.ParseInt(response.Header.Get("X-Ratelimit-Reset"), 10, 64)
	if err != nil {
		return 0
	}

	return time.Until(time.Unix(reset, 0))
}

// validateGitHubRepo checks the repo param: required, and shaped owner/name
// once resolved.
func validateGitHubRepo(task *runner.Task) error {
	if err := requireStringParam(task, paramRepo, GitHubName); err != nil {
		return err
	}

	repo, _ := task.Params[paramRepo].(string)
	if !task.Resolved && containsRefDelimiter(repo) {
		return nil
	}

	if !strings.Contains(repo, "/") {
		return paramError(GitHubName, "repo", fmt.Sprintf("must be owner/name, got %q", repo))
	}

	return nil
}

// validateReleaseLimit checks the optional limit param for op = releases. An
// unresolved reference reads as a string and defers to launch.
func validateReleaseLimit(task *runner.Task) error {
	raw, exists := task.Params[paramLimit]
	if !exists {
		return nil
	}

	if text, isString := raw.(string); isString && !task.Resolved && containsRefDelimiter(text) {
		return nil
	}

	if _, isInt := raw.(int64); !isInt {
		return paramError(GitHubName, "limit", "must be an integer")
	}

	return nil
}

// validateIssueParams checks the op = create_issue params: title and body
// required, labels an optional list of strings.
func validateIssueParams(task *runner.Task) error {
	if err := requireStringParam(task, paramTitle, GitHubName); err != nil {
		return err
	}

	if err := requireStringParam(task, paramBody, GitHubName); err != nil {
		return err
	}

	raw, exists := task.Params[paramLabels]
	if !exists {
		return nil
	}

	if text, isString := raw.(string); isString && !task.Resolved && containsRefDelimiter(text) {
		return nil
	}

	items, isList := raw.([]any)
	if !isList {
		return paramError(GitHubName, "labels", "must be a list of strings")
	}

	for _, item := range items {
		if _, isString := item.(string); !isString {
			return paramError(GitHubName, "labels", "must be a list of strings")
		}
	}

	return nil
}

// labelStrings converts the labels param into the wire shape. Validate
// already vetted the element types; anything else returns nil and the field
// is omitted.
func labelStrings(raw any) []string {
	items, isList := raw.([]any)
	if !isList {
		return nil
	}

	labels := make([]string, 0, len(items))

	for _, item := range items {
		if text, isString := item.(string); isString {
			labels = append(labels, text)
		}
	}

	return labels
}

// githubRelease is the subset of a release entry the output carries.
type githubRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	PublishedAt string `json:"published_at"`
	Body        string `json:"body"`
}

// githubIssue is the subset of an issues-list row list_issues reads. A
// non-empty PullRequest marks a pull request, which GitHub returns on the
// issues endpoint and the poll source drops.
type githubIssue struct {
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	HTMLURL     string          `json:"html_url"`
	CreatedAt   string          `json:"created_at"`
	User        githubUser      `json:"user"`
	PullRequest json.RawMessage `json:"pull_request"`
	Number      int64           `json:"number"`
}

// githubUser is the subset of an issue's author the output carries.
type githubUser struct {
	Login string `json:"login"`
}

// githubIssueRequest is the create-issue request body.
type githubIssueRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

// githubIssueResponse is the subset of the created issue the output carries.
type githubIssueResponse struct {
	HTMLURL string `json:"html_url"`
	Number  int64  `json:"number"`
}
