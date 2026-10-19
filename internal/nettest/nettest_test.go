package nettest_test

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"

	"github.com/chadit/CairnOS/internal/nettest"
)

var errRefused = errors.New("connection refused")

// TestUnreachableFailsLikeADial proves a client over Unreachable sees what a
// failed dial gives it: one *url.Error, added by http.Client, around a
// *net.OpError that carries the cause.
func TestUnreachableFailsLikeADial(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: nettest.Unreachable(errRefused)}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, nettest.BaseURL, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}

	if !errors.Is(err, errRefused) {
		t.Fatalf("Do over Unreachable: %v, want an error carrying the cause", err)
	}

	urlErr, isURL := errors.AsType[*url.Error](err)
	if !isURL {
		t.Fatalf("error %v is not a *url.Error", err)
	}

	if _, isOp := errors.AsType[*net.OpError](urlErr.Err); !isOp {
		t.Fatalf("url.Error wraps %T, want the *net.OpError a dial returns", urlErr.Err)
	}
}
