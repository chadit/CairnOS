package builtins_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

const (
	actionFetch  = "fetch"
	methodKey    = "method"
	pathKey      = "path"
	headersKey   = "headers"
	jsonParamKey = "json"
	statusField  = "status"
	bodyField    = "body"
	statusDone   = "done"
)

func httpTask(params map[string]any) *runner.Task {
	return &runner.Task{RunID: testRunID, ActionName: actionFetch, Params: params, Resolved: true}
}

func TestHTTPGetParsesJSONBodyReachableByField(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"count": 3, "ok": true}`)
	}))

	fetch, err := builtins.NewHTTP(nil, server.Client())
	if err != nil {
		t.Fatalf("new http: %v", err)
	}

	result, err := fetch.Execute(t.Context(), httpTask(map[string]any{urlKey: server.URL}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isMap := result.Output.(map[string]any)
	if !isMap {
		t.Fatalf("output is not a map: %#v", result.Output)
	}

	if output[statusField] != 200 {
		t.Fatalf("status = %#v, want 200", output[statusField])
	}

	body, isBodyMap := output[bodyField].(map[string]any)
	if !isBodyMap {
		t.Fatalf("body is not a parsed map: %#v", output[bodyField])
	}

	// The count field must be reachable for ${{ fetch.output.body.count }} to
	// resolve downstream. JSON numbers decode to float64.
	if body[jsonCountField] != float64(3) || body["ok"] != true {
		t.Fatalf("parsed body = %#v", body)
	}

	if result.Usage.Latency <= 0 {
		t.Fatalf("latency = %v, want a measured duration", result.Usage.Latency)
	}
}

func TestHTTPAppliesQueryAndHeaders(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "cairn" || r.Header.Get("X-Token") != "secret" {
			t.Errorf("query/header not applied: query=%q header=%q", r.URL.RawQuery, r.Header.Get("X-Token"))
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok": true}`)
	}))

	fetch, err := builtins.NewHTTP(nil, server.Client())
	if err != nil {
		t.Fatalf("new http: %v", err)
	}

	_, err = fetch.Execute(t.Context(), httpTask(map[string]any{
		urlKey:     server.URL,
		queryKey:   map[string]any{"q": "cairn"},
		headersKey: map[string]any{"X-Token": "secret"},
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}

func TestHTTPClassifiesStatusCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		retryAfter string
		status     int
		wantClass  runner.ErrorClass
		wantRetry  time.Duration
	}{
		{name: "500 is transient", status: http.StatusInternalServerError, wantClass: runner.ClassTransient},
		{name: "408 is transient", status: http.StatusRequestTimeout, wantClass: runner.ClassTransient},
		{name: "404 is permanent", status: http.StatusNotFound, wantClass: runner.ClassPermanent},
		{name: "429 honors retry-after", status: http.StatusTooManyRequests, retryAfter: "2", wantClass: runner.ClassRateLimited, wantRetry: 2 * time.Second},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}

				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, "nope")
			}))

			fetch, err := builtins.NewHTTP(nil, server.Client())
			if err != nil {
				t.Fatalf("new http: %v", err)
			}

			_, err = fetch.Execute(t.Context(), httpTask(map[string]any{urlKey: server.URL}))

			classified, isClassified := errors.AsType[*runner.Error](err)
			if !isClassified || classified.Class != tt.wantClass {
				t.Fatalf("error = %v, want class %v", err, tt.wantClass)
			}

			if classified.RetryAfter != tt.wantRetry {
				t.Fatalf("retry after = %v, want %v", classified.RetryAfter, tt.wantRetry)
			}
		})
	}
}

func TestHTTPNonJSONResponseReturnsStringBody(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "plain text reply")
	}))

	fetch, err := builtins.NewHTTP(nil, server.Client())
	if err != nil {
		t.Fatalf("new http: %v", err)
	}

	result, err := fetch.Execute(t.Context(), httpTask(map[string]any{urlKey: server.URL}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, _ := result.Output.(map[string]any)
	if output[bodyField] != "plain text reply" {
		t.Fatalf("body = %#v, want the raw string", output[bodyField])
	}
}

func TestHTTPJSONParamRejectsNonJSON(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "definitely not json")
	}))

	fetch, err := builtins.NewHTTP(nil, server.Client())
	if err != nil {
		t.Fatalf("new http: %v", err)
	}

	_, err = fetch.Execute(t.Context(), httpTask(map[string]any{urlKey: server.URL, jsonParamKey: true}))

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	if classified.Class != runner.ClassPermanent || classified.Code != "http.bad_json" {
		t.Fatalf("class/code = %v/%q, want permanent/http.bad_json", classified.Class, classified.Code)
	}
}

func TestHTTPPostEncodesObjectBody(t *testing.T) {
	t.Parallel()

	received := make(chan map[string]any, 1)

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if r.Method == http.MethodPost &&
			r.Header.Get("Content-Type") == "application/json" {
			if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil {
				received <- decoded
			}
		}

		w.WriteHeader(http.StatusOK)
	}))

	fetch, err := builtins.NewHTTP(nil, server.Client())
	if err != nil {
		t.Fatalf("new http: %v", err)
	}

	_, err = fetch.Execute(t.Context(), httpTask(map[string]any{
		urlKey:    server.URL,
		methodKey: http.MethodPost,
		bodyKey:   map[string]any{statusField: statusDone},
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	select {
	case decoded := <-received:
		if decoded[statusField] != statusDone {
			t.Fatalf("decoded body = %v", decoded)
		}
	default:
		t.Fatal("server saw no JSON POST with the right content type")
	}
}

func TestHTTPPathJoinsBaseURL(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/items" {
			t.Errorf("path = %q, want /v1/items", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok": true}`)
	}))

	fetch, err := builtins.NewHTTP(map[string]any{baseURLKey: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("new http: %v", err)
	}

	if _, err := fetch.Execute(t.Context(), httpTask(map[string]any{pathKey: "/v1/items"})); err != nil {
		t.Fatalf("execute: %v", err)
	}
}

func TestHTTPValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cfg      map[string]any
		params   map[string]any
		name     string
		resolved bool
		wantErr  bool
	}{
		{name: "missing url everywhere", params: map[string]any{}, resolved: true, wantErr: true},
		{name: "url param suffices", params: map[string]any{urlKey: exampleURL}, resolved: true, wantErr: false},
		{
			name: "path needs base_url", cfg: map[string]any{baseURLKey: exampleURL},
			params: map[string]any{pathKey: "/x"}, resolved: true, wantErr: false,
		},
		{name: "path without base_url fails", params: map[string]any{pathKey: "/x"}, resolved: true, wantErr: true},
		{
			name: "bad method", params: map[string]any{urlKey: exampleURL, methodKey: "FETCH"},
			resolved: true, wantErr: true,
		},
		{
			name:   "unresolved url reference defers",
			params: map[string]any{urlKey: unresolved}, resolved: false, wantErr: false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetch, err := builtins.NewHTTP(tt.cfg, &http.Client{Timeout: time.Second})
			if err != nil {
				t.Fatalf("new http: %v", err)
			}

			err = fetch.Validate(&runner.Task{
				RunID:      testRunID,
				ActionName: actionFetch,
				Params:     tt.params,
				Resolved:   tt.resolved,
			})

			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("validate error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestHTTPCredentialSendsBearer(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(authHeader); got != bearerPrefix+credToken {
			t.Errorf("Authorization = %q, want %q", got, bearerPrefix+credToken)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok": true}`)
	}))

	lookup := &fakeTokenLookup{tokens: map[string]string{credName: credToken}}

	fetch, err := builtins.NewHTTPWithCredentials(map[string]any{credentialKey: credName}, server.Client(), lookup)
	if err != nil {
		t.Fatalf("new http: %v", err)
	}

	if _, err := fetch.Execute(t.Context(), httpTask(map[string]any{urlKey: server.URL})); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if lookup.callCount() != 1 {
		t.Fatalf("credential minted %d tokens, want exactly 1 per request", lookup.callCount())
	}
}

// TestHTTPExplicitAuthorizationWinsOverCredential pins the precedence rule: an
// Authorization from default_headers or a per-call header wins, and the
// credential is never even minted in that case.
func TestHTTPExplicitAuthorizationWinsOverCredential(t *testing.T) {
	t.Parallel()

	const explicit = "Bearer explicit-operator-token"

	cases := []struct {
		cfg    map[string]any
		params map[string]any
		name   string
	}{
		{
			name:   "default_headers authorization wins",
			cfg:    map[string]any{credentialKey: credName, defaultHeadersKey: map[string]any{authHeader: explicit}},
			params: map[string]any{},
		},
		{
			name:   "per-call header authorization wins",
			cfg:    map[string]any{credentialKey: credName},
			params: map[string]any{headersKey: map[string]any{authHeader: explicit}},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get(authHeader); got != explicit {
					t.Errorf("Authorization = %q, want the explicit header %q", got, explicit)
				}

				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok": true}`)
			}))

			lookup := &fakeTokenLookup{tokens: map[string]string{credName: credToken}}

			params := tt.params
			params[urlKey] = server.URL

			fetch, err := builtins.NewHTTPWithCredentials(tt.cfg, server.Client(), lookup)
			if err != nil {
				t.Fatalf("new http: %v", err)
			}

			if _, err := fetch.Execute(t.Context(), httpTask(params)); err != nil {
				t.Fatalf("execute: %v", err)
			}

			if lookup.callCount() != 0 {
				t.Fatalf("credential minted %d tokens, want 0 when an explicit Authorization is set", lookup.callCount())
			}
		})
	}
}

// TestHTTPCredentialTokenNeverInError proves the bearer token never rides an
// error string, even when a hostile endpoint echoes it back in an error body:
// the runner attaches the token to the request, gets a 500, and the classified
// error carries the status only.
func TestHTTPCredentialTokenNeverInError(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "denied for "+r.Header.Get(authHeader))
	}))

	lookup := &fakeTokenLookup{tokens: map[string]string{credName: credToken}}

	fetch, err := builtins.NewHTTPWithCredentials(map[string]any{credentialKey: credName}, server.Client(), lookup)
	if err != nil {
		t.Fatalf("new http: %v", err)
	}

	_, err = fetch.Execute(t.Context(), httpTask(map[string]any{urlKey: server.URL}))
	if err == nil {
		t.Fatal("500 response returned no error")
	}

	// Scan the whole rendered error chain for the secret. This is a secret-leak
	// check, not error-identity matching, so a substring search is the point.
	rendered := err.Error()
	if strings.Contains(rendered, credToken) {
		t.Fatalf("error string leaked the bearer token: %v", err)
	}

	if lookup.callCount() != 1 {
		t.Fatalf("credential minted %d tokens, want exactly 1 (the token was sent, just not leaked)", lookup.callCount())
	}
}

// TestHTTPCredentialMintFailureIsTransient checks a token endpoint that is
// briefly unavailable surfaces as a transient runner error, so the engine
// retries rather than treating it as a permanent config fault.
func TestHTTPCredentialMintFailureIsTransient(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	lookup := &fakeTokenLookup{
		tokens: map[string]string{credName: credToken},
		err:    errors.New("token endpoint unreachable"),
	}

	fetch, err := builtins.NewHTTPWithCredentials(map[string]any{credentialKey: credName}, server.Client(), lookup)
	if err != nil {
		t.Fatalf("new http: %v", err)
	}

	_, err = fetch.Execute(t.Context(), httpTask(map[string]any{urlKey: server.URL}))

	classified, ok := errors.AsType[*runner.Error](err)
	if !ok {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	if classified.Class != runner.ClassTransient || classified.Code != "http.credential" {
		t.Fatalf("class/code = %v/%q, want transient http.credential", classified.Class, classified.Code)
	}
}

// TestHTTPNilLookupSendsNoBearer keeps the no-credential path byte-for-byte:
// with no credential key and a nil lookup, no Authorization header is added.
func TestHTTPNilLookupSendsNoBearer(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(authHeader); got != "" {
			t.Errorf("Authorization = %q, want none without a credential", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok": true}`)
	}))

	fetch, err := builtins.NewHTTPWithCredentials(nil, server.Client(), nil)
	if err != nil {
		t.Fatalf("new http: %v", err)
	}

	if _, err := fetch.Execute(t.Context(), httpTask(map[string]any{urlKey: server.URL})); err != nil {
		t.Fatalf("execute: %v", err)
	}
}
