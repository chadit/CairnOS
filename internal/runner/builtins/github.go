package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

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

	opReleases    = "releases"
	opCreateIssue = "create_issue"
)

// defaultReleaseLimit caps the releases listing when no limit param is given.
const defaultReleaseLimit = 10

// metaStatusCode is the Meta key carrying the HTTP status of the API call.
const metaStatusCode = "status_code"

// defaultGitHubTimeout backstops the factory-built client; REST calls are
// short, unlike model generations.
const defaultGitHubTimeout = 30 * time.Second

// GitHub reads release feeds and creates issues over the REST v3 API,
// backing the Phase 8 release monitor (.agents/spec/03-runner-interface.md).
//
// Config: base_url (default https://api.github.com). Params: op (releases or
// create_issue) and repo as owner/name, plus limit for releases and
// title/body/labels for create_issue. The token comes from
// CAIRNOS_RUNNER_GITHUB_TOKEN at call time and is optional for public reads.
type GitHub struct {
	baseURL string
	client  *http.Client
}

// NewGitHub builds the runner over an injected HTTP client so tests can
// point it at httptest servers.
func NewGitHub(cfg map[string]any, client *http.Client) (*GitHub, error) {
	baseURL, err := configValue(cfg, cfgKeyBaseURL, defaultGitHubBaseURL)
	if err != nil {
		return nil, wrapConfigErr(GitHubName, err)
	}

	return &GitHub{baseURL: strings.TrimSuffix(baseURL, "/"), client: client}, nil
}

// GitHubFactory adapts NewGitHub to the registry contract with the shared
// egress guard wrapping the client. allow_private_targets opts out of the IP
// guard for operators pointing the runner at an internal GitHub Enterprise host.
func GitHubFactory(cfg map[string]any) (runner.Runner, error) {
	allowPrivate, err := configValue(cfg, cfgKeyAllowPrivateTargets, false)
	if err != nil {
		return nil, wrapConfigErr(GitHubName, err)
	}

	return NewGitHub(cfg, safeHTTPClient(GitHubName, defaultGitHubTimeout, allowPrivate))
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
	case opCreateIssue:
		if err := validateGitHubRepo(task); err != nil {
			return err
		}

		return validateIssueParams(task)
	default:
		return runner.NewError(runner.ClassPermanent, GitHubName+".bad_op",
			fmt.Sprintf("op must be %s or %s, got %q", opReleases, opCreateIssue, opText))
	}
}

// Execute implements runner.Runner.
func (g *GitHub) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := g.Validate(task); err != nil {
		return runner.Result{}, err
	}

	op, _ := task.Params[paramOp].(string)
	if op == opCreateIssue {
		return g.createIssue(ctx, task)
	}

	return g.listReleases(ctx, task)
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
			"name":         release.Name,
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
		Output: map[string]any{"number": issue.Number, paramURL: issue.HTMLURL},
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   map[string]string{metaStatusCode: strconv.Itoa(status)},
	}, nil
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

	if token := os.Getenv(githubTokenEnv); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := g.client.Do(request)
	if err != nil {
		if blocked := classifyBlockedDial(GitHubName, err); blocked != nil {
			return nil, 0, blocked
		}

		return nil, 0, classifyAPITransport(ctx, GitHubName, err)
	}

	defer func() { _ = response.Body.Close() }()

	if err := classifyGitHubStatus(response); err != nil {
		return nil, response.StatusCode, err
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
		return runner.NewError(runner.ClassPermanent, GitHubName+".bad_repo",
			fmt.Sprintf("repo must be owner/name, got %q", repo))
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
		return runner.NewError(runner.ClassPermanent, GitHubName+".bad_limit", "limit must be an integer")
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
		return runner.NewError(runner.ClassPermanent, GitHubName+".bad_labels", "labels must be a list of strings")
	}

	for _, item := range items {
		if _, isString := item.(string); !isString {
			return runner.NewError(runner.ClassPermanent, GitHubName+".bad_labels", "labels must be a list of strings")
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

// githubIssueRequest is the create-issue request body.
type githubIssueRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

// githubIssueResponse is the subset of the created issue the output carries.
type githubIssueResponse struct {
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
}
