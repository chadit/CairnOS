package localmodel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestProbeAcceptsAnsweringServer proves the liveness check is satisfied by
// the models endpoint answering 200.
func TestProbeAcceptsAnsweringServer(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1"+modelsPath {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	if err := Probe(t.Context(), server.Client(), server.URL+"/v1"); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

// TestProbeRejectsErrorStatus proves a server that answers with a failure is
// not counted as healthy, which is what keeps a bad upgrade from committing.
func TestProbeRejectsErrorStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "loading", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := Probe(t.Context(), server.Client(), server.URL+"/v1")
	if !errors.Is(err, ErrUnexpectedStatus) {
		t.Fatalf("err = %v, want ErrUnexpectedStatus", err)
	}
}

// TestWaitHealthyRidesOutASlowStart proves the upgrade probe keeps trying
// while the server maps its weights, rather than failing on the first refusal.
func TestWaitHealthyRidesOutASlowStart(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "loading", http.StatusServiceUnavailable)

			return
		}

		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	if err := WaitHealthy(t.Context(), server.Client(), server.URL+"/v1", 30*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

// TestWaitHealthyGivesUpOnADeadServer proves the budget is real, so an upgrade
// that broke the server reports it instead of hanging.
func TestWaitHealthyGivesUpOnADeadServer(t *testing.T) {
	t.Parallel()

	err := WaitHealthy(t.Context(), nil, "http://127.0.0.1:1/v1", 50*time.Millisecond)
	if !errors.Is(err, ErrServerUnhealthy) {
		t.Fatalf("err = %v, want ErrServerUnhealthy", err)
	}
}

// TestBaseURLBindsLoopback proves the endpoint the config block points at is
// loopback: the server has no authentication, so it must not be reachable off
// the machine.
func TestBaseURLBindsLoopback(t *testing.T) {
	t.Parallel()

	if got := BaseURL(DefaultPort); got != "http://127.0.0.1:8089/v1" {
		t.Errorf("BaseURL = %q", got)
	}
}
