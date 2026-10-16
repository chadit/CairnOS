package builtins_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

const (
	bodyKey    = "body"
	urlKey     = "url"
	exampleURL = "http://example.test"
)

func webhookTask(url string, body any) *runner.Task {
	params := map[string]any{bodyKey: body}
	if url != "" {
		params[urlKey] = url
	}

	return &runner.Task{RunID: testRunID, ActionName: actionNotify, Params: params, Resolved: true}
}

func TestWebhookClassifiesStatusCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		retryAfter string
		status     int
		wantClass  runner.ErrorClass
		wantRetry  time.Duration
		wantOK     bool
	}{
		{name: "200 succeeds", status: http.StatusOK, wantOK: true},
		{name: case500, status: http.StatusInternalServerError, wantClass: runner.ClassTransient},
		{name: case408, status: http.StatusRequestTimeout, wantClass: runner.ClassTransient},
		{name: case429, status: http.StatusTooManyRequests, retryAfter: "2", wantClass: runner.ClassRateLimited, wantRetry: 2 * time.Second},
		{name: case404, status: http.StatusNotFound, wantClass: runner.ClassPermanent},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}

				w.WriteHeader(tt.status)
				writeBody(t, w, "ack")
			}))

			hook, err := builtins.NewWebhook(nil, server.Client())
			if err != nil {
				t.Fatalf("new webhook: %v", err)
			}

			result, err := hook.Execute(t.Context(), webhookTask(server.URL, "ping"))

			if tt.wantOK {
				if err != nil {
					t.Fatalf("execute: %v", err)
				}

				if result.Output != "ack" {
					t.Fatalf("output = %#v, want ack", result.Output)
				}

				if result.Meta["status_code"] != "200" {
					t.Fatalf("status_code = %q, want 200", result.Meta["status_code"])
				}

				return
			}

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

func TestWebhookMarshalsStructuredBodyAndUsesConfigURL(t *testing.T) {
	t.Parallel()

	received := make(chan map[string]any, 1)

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any

		if err := json.NewDecoder(r.Body).Decode(&decoded); err == nil &&
			r.Header.Get("Content-Type") == mediaJSON {
			received <- decoded
		}

		w.WriteHeader(http.StatusOK)
	}))

	hook, err := builtins.NewWebhook(map[string]any{urlKey: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("new webhook: %v", err)
	}

	// An empty params url exercises the config fallback.
	if _, err := hook.Execute(t.Context(), webhookTask("", map[string]any{statusField: statusDone})); err != nil {
		t.Fatalf("execute: %v", err)
	}

	select {
	case decoded := <-received:
		if decoded[statusField] != statusDone {
			t.Fatalf("decoded body = %v", decoded)
		}
	default:
		t.Fatal("server saw no JSON body with the right content type")
	}
}

func TestWebhookValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		params  map[string]any
		name    string
		cfgURL  string
		wantErr bool
	}{
		{name: caseMissingBody, cfgURL: exampleURL, params: map[string]any{}, wantErr: true},
		{name: caseMissingURL, params: map[string]any{bodyKey: "x"}, wantErr: true},
		{name: "config url suffices", cfgURL: exampleURL, params: map[string]any{bodyKey: "x"}, wantErr: false},
		{name: "param url suffices", params: map[string]any{bodyKey: "x", urlKey: exampleURL}, wantErr: false},
		{name: "non-string url", cfgURL: exampleURL, params: map[string]any{bodyKey: "x", urlKey: int64(1)}, wantErr: true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg map[string]any
			if tt.cfgURL != "" {
				cfg = map[string]any{urlKey: tt.cfgURL}
			}

			// Validate never sends, but the client still gets a timeout to
			// satisfy the no-unbounded-client rule.
			hook, err := builtins.NewWebhook(cfg, &http.Client{Timeout: time.Second})
			if err != nil {
				t.Fatalf("new webhook: %v", err)
			}

			err = hook.Validate(&runner.Task{
				RunID:      testRunID,
				ActionName: actionNotify,
				Params:     tt.params,
				Resolved:   true,
			})

			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("validate error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}
