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

// contentTypeJSON is the response media type that triggers JSON parsing even
// when the json param is unset, matched on the leading token before any
// charset suffix.
const contentTypeJSON = "application/json"

// HTTP performs a single configurable HTTP request, the read-capable sibling
// to the webhook notification sink (.agents/spec/03-runner-interface.md). It
// covers GET feeds and JSON APIs the agents wanted but webhook (POST-only)
// could not reach.
//
// Config: base_url (optional prefix for the path param), timeout (seconds,
// backstop only), and default_headers (merged under per-call headers). Params:
// method (default GET), url or path, headers, query, body (string or object),
// and json (parse the response body as JSON). No token is required by default;
// set one through default_headers or a per-call header when an endpoint needs
// it.
type HTTP struct {
	baseURL        string
	defaultHeaders map[string]string
	client         *http.Client
}

// NewHTTP builds the runner over an injected HTTP client so tests can point it
// at httptest servers.
func NewHTTP(cfg map[string]any, client *http.Client) (*HTTP, error) {
	baseURL, err := configValue(cfg, cfgKeyBaseURL, "")
	if err != nil {
		return nil, wrapConfigErr(HTTPName, err)
	}

	headers, err := configStringMap(cfg, cfgKeyDefaultHeaders)
	if err != nil {
		return nil, wrapConfigErr(HTTPName, err)
	}

	return &HTTP{
		baseURL:        strings.TrimSuffix(baseURL, "/"),
		defaultHeaders: headers,
		client:         client,
	}, nil
}

// HTTPFactory adapts NewHTTP to the registry contract with the shared egress
// guard wrapping the client: the timeout comes from the config or the default,
// and allow_private_targets opts out of the IP guard for operators that call
// internal endpoints on purpose.
func HTTPFactory(cfg map[string]any) (runner.Runner, error) {
	timeout, has, err := configInt(cfg, cfgKeyTimeout)
	if err != nil {
		return nil, wrapConfigErr(HTTPName, err)
	}

	allowPrivate, err := configValue(cfg, cfgKeyAllowPrivateTargets, false)
	if err != nil {
		return nil, wrapConfigErr(HTTPName, err)
	}

	clientTimeout := defaultHTTPTimeout
	if has && timeout > 0 {
		clientTimeout = time.Duration(timeout) * time.Second
	}

	return NewHTTP(cfg, safeHTTPClient(HTTPName, clientTimeout, allowPrivate))
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
		return runner.NewError(runner.ClassPermanent, HTTPName+".bad_method",
			"method must be a standard HTTP verb, got "+method)
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

	return runner.NewError(runner.ClassPermanent, HTTPName+".missing_url",
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

	if err := guardTarget(HTTPName, target); err != nil {
		return nil, err
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

	return request, nil
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
		return strings.NewReader(text), "text/plain; charset=utf-8", nil
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
		return runner.NewError(runner.ClassPermanent, HTTPName+".bad_headers",
			"headers must be a map of string to string")
	}

	for key, raw := range headers {
		text, isString := raw.(string)
		if !isString {
			return runner.NewError(runner.ClassPermanent, HTTPName+".bad_headers",
				"header "+key+" must be a string")
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
		return nil, runner.NewError(runner.ClassPermanent, HTTPName+".bad_query",
			"query must be a map of string to string")
	}

	values := make(url.Values, len(query))

	for key, raw := range query {
		text, isString := raw.(string)
		if !isString {
			return nil, runner.NewError(runner.ClassPermanent, HTTPName+".bad_query",
				"query "+key+" must be a string")
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
