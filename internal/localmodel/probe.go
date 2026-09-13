package localmodel

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// DefaultPort is the loopback port the managed llama-server listens on. It
// sits above the ranges the common local development servers claim, so a
// default install rarely collides with something already running.
const DefaultPort = 8089

// DefaultContextSize is the context window the sidecar starts with. 32K covers
// a long chat or a document-sized prompt while keeping the KV cache within
// what an ordinary laptop can spare.
const DefaultContextSize = 32768

// modelsPath is the OpenAI-compatible endpoint the probe calls. It answers
// once the model is loaded and needs no request body, which makes it the
// cheapest honest liveness check the server offers.
const modelsPath = "/models"

// probeInterval is how often WaitHealthy retries. A cold start spends its time
// mapping several gigabytes of weights, so polling faster buys nothing.
const probeInterval = time.Second

// probeTimeout bounds one probe request, separate from the overall wait.
const probeTimeout = 5 * time.Second

// BaseURL is the OpenAI-compatible endpoint of a managed server on port. It is
// bound to loopback: the model runs for this machine's agents, and a server
// with no authentication has no business on a routable address.
func BaseURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
}

// Probe reports whether a server at baseURL answers its models endpoint.
func Probe(ctx context.Context, client *http.Client, baseURL string) error {
	if client == nil {
		client = &http.Client{Timeout: probeTimeout}
	}

	// #nosec G107 -- baseURL is built from the configured loopback port
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+modelsPath, http.NoBody)
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", baseURL, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: %w %d", baseURL, ErrUnexpectedStatus, resp.StatusCode)
	}

	return nil
}

// WaitHealthy polls until the server answers or the budget runs out. An
// upgrade calls it before discarding the runtime it replaced, so a build that
// cannot start is caught while the old one is still on disk.
func WaitHealthy(ctx context.Context, client *http.Client, baseURL string, budget time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	var last error

	for {
		last = Probe(deadline, client, baseURL)
		if last == nil {
			return nil
		}

		select {
		case <-deadline.Done():
			return fmt.Errorf("%w (%s): %w", ErrServerUnhealthy, budget, last)
		case <-ticker.C:
		}
	}
}
