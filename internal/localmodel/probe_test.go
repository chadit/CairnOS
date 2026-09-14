package localmodel_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/localmodel"
)

// TestProbeAcceptsAnsweringServer proves the liveness check is satisfied by
// the models endpoint answering 200.
func TestProbeAcceptsAnsweringServer(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1"+localmodel.ModelsPath {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	if err := localmodel.Probe(t.Context(), server.Client(), server.URL+"/v1"); err != nil {
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

	err := localmodel.Probe(t.Context(), server.Client(), server.URL+"/v1")
	if !errors.Is(err, localmodel.ErrUnexpectedStatus) {
		t.Fatalf("err = %v, want localmodel.ErrUnexpectedStatus", err)
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

	if err := localmodel.WaitHealthy(t.Context(), server.Client(), server.URL+"/v1", 30*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

// TestWaitHealthyGivesUpOnADeadServer proves the budget is real, so an upgrade
// that broke the server reports it instead of hanging.
func TestWaitHealthyGivesUpOnADeadServer(t *testing.T) {
	t.Parallel()

	err := localmodel.WaitHealthy(t.Context(), nil, "http://127.0.0.1:1/v1", 50*time.Millisecond)
	if !errors.Is(err, localmodel.ErrServerUnhealthy) {
		t.Fatalf("err = %v, want localmodel.ErrServerUnhealthy", err)
	}
}

// TestBaseURLBindsLoopback proves the endpoint the config block points at is
// loopback: the server has no authentication, so it must not be reachable off
// the machine.
func TestBaseURLBindsLoopback(t *testing.T) {
	t.Parallel()

	if got := localmodel.BaseURL(localmodel.DefaultPort); got != "http://127.0.0.1:8089/v1" {
		t.Errorf("localmodel.BaseURL = %q", got)
	}
}
