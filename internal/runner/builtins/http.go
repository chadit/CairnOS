package builtins

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// HTTPName is the registry name of the generic HTTP fetch runner.
const HTTPName = "http"

// HTTP params. method defaults to GET; url and path are alternatives (path
// appends to the configured base_url); headers and query are string maps; body
// is a string or object; json switches response parsing into JSON. paramQuery
// is shared with the memory tool.
const (
	paramMethod  = "method"
	paramPath    = "path"
	paramHeaders = "headers"
	paramJSON    = "json"
)

// cfgKeyDefaultHeaders names the config header map merged under per-call
// headers, so a token or accept header set once rides every request.
const cfgKeyDefaultHeaders = "default_headers"

// cfgKeyTimeout names the optional config timeout (seconds) that backstops the
// factory client when ctx and the task deadline leave it unbounded.
const cfgKeyTimeout = "timeout"

// defaultHTTPTimeout backstops the factory-built client. Per-task limits still
// come from ctx and the task deadline, which the engine sets tighter; this
// only catches an endpoint that hangs when no deadline is configured.
const defaultHTTPTimeout = 30 * time.Second

// Result.Output keys for an HTTP response: the status code, the body (a parsed
// map or array in JSON mode, a string otherwise), and the response headers.
const (
	outputStatus  = "status"
	outputBody    = "body"
	outputHeaders = "headers"
)

// Request body media types. A JSON response also triggers parsing when the json
// param is unset, matched on the leading token before any charset suffix.
const (
	contentTypeJSON = "application/json"
	contentTypeText = "text/plain; charset=utf-8"
)

// HTTP performs a single configurable HTTP request, the read-capable sibling to
// the webhook notification sink. It reaches the GET feeds and JSON APIs that
// POST-only webhook cannot.
//
// Config: base_url (prefix for path), timeout (seconds), default_headers
// (merged under per-call headers). Params: method (default GET), url or path,
// headers, query, body, json. No token is required by default.
type HTTP struct {
	baseURL        string
	defaultHeaders map[string]string
	client         *http.Client

	// lookup mints the credential's bearer token per request; nil when no
	// credential manager is wired. credential is the [credential.<name>] block
	// this runner binds to, or "" for the no-credential path.
	lookup     TokenLookup
	credential string
}

// NewHTTP builds the runner over an injected HTTP client so tests can point it
// at httptest servers. It is the no-credential path: a credential config key
// has no lookup to resolve against and fails the build.
func NewHTTP(cfg map[string]any, client *http.Client) (*HTTP, error) {
	return NewHTTPWithCredentials(cfg, client, nil)
}

// NewHTTPWithCredentials builds the runner over an injected client and an
// optional credential lookup. When the config sets a credential key, lookup
// must know it (Has) or construction fails, so an unknown or unwired credential
// surfaces at build time rather than on the first request.
func NewHTTPWithCredentials(cfg map[string]any, client *http.Client, lookup TokenLookup) (*HTTP, error) {
	baseURL, err := configValue(cfg, cfgKeyBaseURL, "")
	if err != nil {
		return nil, &configError{runner: HTTPName, err: err}
	}

	headers, err := configStringMap(cfg, cfgKeyDefaultHeaders)
	if err != nil {
		return nil, &configError{runner: HTTPName, err: err}
	}

	credential, err := requireCredential(cfg, lookup)
	if err != nil {
		return nil, &configError{runner: HTTPName, err: err}
	}

	return &HTTP{
		baseURL:        strings.TrimSuffix(baseURL, "/"),
		defaultHeaders: headers,
		client:         client,
		lookup:         lookup,
		credential:     credential,
	}, nil
}

// HTTPFactory adapts the runner to the registry contract with no credential
// lookup, so a credential key fails the build. It is HTTPFactoryWithCredentials
// with a nil lookup.
func HTTPFactory() runner.Factory {
	return HTTPFactoryWithCredentials(nil)
}

// HTTPFactoryWithCredentials returns a registry factory that wires lookup into
// each built HTTP runner, so a [runner.<name>] block with a credential key
// sends Authorization: Bearer. allow_private_targets opts out of the shared
// egress IP guard for operators that call internal endpoints on purpose. A nil
// lookup keeps the runner exactly as the bare factory.
func HTTPFactoryWithCredentials(lookup TokenLookup) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		timeout, has, err := configInt(cfg, cfgKeyTimeout)
		if err != nil {
			return nil, &configError{runner: HTTPName, err: err}
		}

		allowPrivate, err := configValue(cfg, cfgKeyAllowPrivateTargets, false)
		if err != nil {
			return nil, &configError{runner: HTTPName, err: err}
		}

		clientTimeout := defaultHTTPTimeout
		if has && timeout > 0 {
			clientTimeout = time.Duration(timeout) * time.Second
		}

		return NewHTTPWithCredentials(cfg, safeHTTPClient(HTTPName, clientTimeout, allowPrivate), lookup)
	}
}

// Name implements runner.Runner.
func (*HTTP) Name() string {
	return HTTPName
}

// Validate implements runner.Runner: a target URL must come from url or
// base_url+path, and method must be a known verb. Unresolved references in
// either defer the check to launch, matching the two-phase contract.
func (h *HTTP) Validate(task *runner.Task) error {
	if err := optionalStringParam(task, paramMethod, HTTPName); err != nil {
		return err
	}

	if err := optionalStringParam(task, paramURL, HTTPName); err != nil {
		return err
	}

	if err := optionalStringParam(task, paramPath, HTTPName); err != nil {
		return err
	}

	if err := h.validateTarget(task); err != nil {
		return err
	}

	return validateMethod(task)
}

// validateMethod checks that method, when present and resolved, is a verb the
// runner sends. An empty method defaults to GET at launch.
func validateMethod(task *runner.Task) error {
	method, _ := task.Params[paramMethod].(string)
	if method == "" {
		return nil
	}

	if !task.Resolved && containsRefDelimiter(method) {
		return nil
	}

	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
		return nil
	default:
		return paramError(HTTPName, "method", "must be a standard HTTP verb, got "+method)
	}
}

// Execute implements runner.Runner.
func (h *HTTP) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := h.Validate(task); err != nil {
		return runner.Result{}, err
	}

	request, err := h.buildRequest(ctx, task)
	if err != nil {
		return runner.Result{}, err
	}

	start := time.Now()

	response, err := h.client.Do(request)
	if err != nil {
		if blocked := classifyBlockedDial(HTTPName, err); blocked != nil {
			return runner.Result{}, blocked
		}

		return runner.Result{}, classifyAPITransport(ctx, HTTPName, err)
	}

	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxCapturedOutput))
	if err != nil {
		return runner.Result{}, &runner.Error{
			Class: runner.ClassTransient, Code: HTTPName + codeSuffixRead,
			Message: "read response: " + err.Error(), Cause: err,
		}
	}

	if err := classifyAPIStatusOnly(response); err != nil {
		return runner.Result{}, err
	}

	return buildResult(task, response, body, start)
}

// validateTarget enforces that a request has somewhere to go: a url param, or
// a path param under a configured base_url. An unresolved reference in either
// defers the check to launch.
func (h *HTTP) validateTarget(task *runner.Task) error {
	target, _ := task.Params[paramURL].(string)
	path, _ := task.Params[paramPath].(string)

	if !task.Resolved && (containsRefDelimiter(target) || containsRefDelimiter(path)) {
		return nil
	}

	if target != "" {
		return nil
	}

	if path != "" && h.baseURL != "" {
		return nil
	}

	return taskError(runner.ClassPermanent, HTTPName+".missing_url",
		"set url in params, or path with base_url in config")
}

// buildRequest assembles one request: method, target URL, query, and headers,
// with a JSON-or-text body when a body param is present.
func (h *HTTP) buildRequest(ctx context.Context, task *runner.Task) (*http.Request, error) {
	target, err := h.resolveURL(task)
	if err != nil {
		return nil, err
	}

	method := strings.ToUpper(stringParam(task, paramMethod))
	if method == "" {
		method = http.MethodGet
	}

	bodyReader, contentType, err := requestBody(task.Params[paramBody])
	if err != nil {
		return nil, err
	}

	if guardTargetErr := guardTarget(HTTPName, target); guardTargetErr != nil {
		return nil, guardTargetErr
	}

	request, err := http.NewRequestWithContext(ctx, method, target, bodyReader)
	if err != nil {
		return nil, &runner.Error{
			Class: runner.ClassPermanent, Code: HTTPName + ".bad_url",
			Message: "build request: " + err.Error(), Cause: err,
		}
	}

	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	for key, value := range h.defaultHeaders {
		request.Header.Set(key, value)
	}

	if err := applyHeaders(request, task.Params[paramHeaders]); err != nil {
		return nil, err
	}

	if err := h.applyCredential(ctx, request); err != nil {
		return nil, err
	}

	return request, nil
}

// applyCredential sets the bound credential's bearer token as the Authorization
// header, after config defaults and per-call headers so an explicit
// Authorization from either wins. A mint failure is transient: token endpoints
// are usually briefly unavailable and the engine retries. The error names the
// credential and status only, never the token or the secret that minted it.
func (h *HTTP) applyCredential(ctx context.Context, request *http.Request) error {
	if h.credential == "" {
		return nil
	}

	if err := setBearerIfAbsent(ctx, request, h.lookup, h.credential); err != nil {
		return &runner.Error{
			Class: runner.ClassTransient, Code: HTTPName + ".credential",
			Message: "attach credential token: " + err.Error(), Cause: err,
		}
	}

	return nil
}

// resolveURL picks and assembles the target: the url param when set, otherwise
// base_url joined with the path param, then appends any query params.
func (h *HTTP) resolveURL(task *runner.Task) (string, error) {
	target := stringParam(task, paramURL)
	if target == "" {
		target = h.baseURL + ensureLeadingSlash(stringParam(task, paramPath))
	}

	query, err := queryValues(task.Params[paramQuery])
	if err != nil {
		return "", err
	}

	if len(query) == 0 {
		return target, nil
	}

	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}

	return target + separator + query.Encode(), nil
}

// buildResult shapes a 2xx response onto the documented Output map: status,
// body (parsed when the json param is set or the response is JSON, a string
// otherwise), and headers.
func buildResult(task *runner.Task, response *http.Response, body []byte, start time.Time) (runner.Result, error) {
	parsed, err := parseBody(task, response, body)
	if err != nil {
		return runner.Result{}, err
	}

	return runner.Result{
		Output: map[string]any{
			outputStatus:  response.StatusCode,
			outputBody:    parsed,
			outputHeaders: flattenHeaders(response.Header),
		},
		Usage: runner.Usage{Latency: time.Since(start)},
		Meta:  map[string]string{metaStatusCode: strconv.Itoa(response.StatusCode)},
	}, nil
}

// parseBody returns the response body as a parsed map or array when the json
// param is set or the response advertises JSON, and as a string otherwise. A
// parse failure under an explicit json request is permanent: the caller asked
// for JSON and the endpoint did not deliver.
func parseBody(task *runner.Task, response *http.Response, body []byte) (any, error) {
	wantJSON, _ := task.Params[paramJSON].(bool)
	isJSON := wantJSON || responseIsJSON(response)

	if !isJSON {
		return string(body), nil
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		if wantJSON {
			return nil, &runner.Error{
				Class: runner.ClassPermanent, Code: HTTPName + codeSuffixBadJSON,
				Message: "json requested but response body is not valid JSON: " + string(body),
				Cause:   err,
			}
		}

		// A JSON content type that fails to parse is the endpoint's mislabel,
		// not the caller's request, so fall back to the raw string.
		return string(body), nil
	}

	return decoded, nil
}

// responseIsJSON reports whether the response advertises a JSON content type,
// matching the media type before any charset suffix.
func responseIsJSON(response *http.Response) bool {
	contentType := response.Header.Get("Content-Type")
	media, _, _ := strings.Cut(contentType, ";")

	return strings.TrimSpace(media) == contentTypeJSON
}

// requestBody encodes the body param: strings ride verbatim as text, anything
// else JSON-encodes. A nil body sends no payload and no content type.
func requestBody(value any) (io.Reader, string, error) {
	if value == nil {
		return nil, "", nil
	}

	if text, isString := value.(string); isString {
		return strings.NewReader(text), contentTypeText, nil
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, "", &runner.Error{
			Class: runner.ClassPermanent, Code: HTTPName + ".bad_body",
			Message: "encode body: " + err.Error(), Cause: err,
		}
	}

	return bytes.NewReader(encoded), contentTypeJSON, nil
}

// applyHeaders sets the per-call header map over the request, after the config
// defaults so a per-call header wins a collision. A non-map headers param is a
// permanent param error.
func applyHeaders(request *http.Request, value any) error {
	if value == nil {
		return nil
	}

	headers, isMap := value.(map[string]any)
	if !isMap {
		return paramError(HTTPName, "headers", "must be a map of string to string")
	}

	for key, raw := range headers {
		text, isString := raw.(string)
		if !isString {
			return taskError(runner.ClassPermanent, HTTPName+".bad_headers",
				"header %s must be a string", key)
		}

		request.Header.Set(key, text)
	}

	return nil
}

// queryValues turns the query param map into encodable values. An absent query
// yields empty values, so resolveURL appends nothing. A non-map query param is
// a permanent param error.
func queryValues(value any) (url.Values, error) {
	if value == nil {
		return url.Values{}, nil
	}

	query, isMap := value.(map[string]any)
	if !isMap {
		return nil, paramError(HTTPName, "query", "must be a map of string to string")
	}

	values := make(url.Values, len(query))

	for key, raw := range query {
		text, isString := raw.(string)
		if !isString {
			return nil, taskError(runner.ClassPermanent, HTTPName+".bad_query",
				"query %s must be a string", key)
		}

		values.Set(key, text)
	}

	return values, nil
}

// flattenHeaders collapses the multi-valued response header map onto the
// single-value string map Output exposes, joining repeats with the comma the
// HTTP spec uses for combined field values.
func flattenHeaders(header http.Header) map[string]any {
	flat := make(map[string]any, len(header))
	for key, values := range header {
		flat[key] = strings.Join(values, ", ")
	}

	return flat
}

// classifyAPIStatusOnly maps a non-2xx response to a classified error without
// the body snippet the LLM runners attach: the body already rides Output for
// the caller to inspect.
func classifyAPIStatusOnly(response *http.Response) error {
	status := response.StatusCode
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		return nil
	}

	message := HTTPName + " returned " + response.Status

	switch {
	case status == http.StatusTooManyRequests:
		return &runner.Error{
			Class:      runner.ClassRateLimited,
			Code:       HTTPName + codeSuffixRateLimited,
			Message:    message,
			RetryAfter: retryAfterHint(response.Header.Get(headerRetryAfter)),
		}
	case status == http.StatusRequestTimeout || status >= http.StatusInternalServerError:
		return &runner.Error{Class: runner.ClassTransient, Code: HTTPName + codeSuffixStatus, Message: message}
	default:
		return &runner.Error{Class: runner.ClassPermanent, Code: HTTPName + codeSuffixStatus, Message: message}
	}
}

// ensureLeadingSlash normalizes a path segment so base_url joins cleanly
// whether or not the operator wrote the slash.
func ensureLeadingSlash(path string) string {
	if path == "" || strings.HasPrefix(path, "/") {
		return path
	}

	return "/" + path
}
