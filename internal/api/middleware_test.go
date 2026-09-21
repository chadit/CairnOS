package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// Security middleware fixture values the M1/M4 gateway tests turn on.
const (
	corsTestOrigin      = "https://app.example"
	testMaxBodyBytes    = 1024
	testMaxRecvMsgBytes = 1 << 20
)

// TestGatewaySetsSecurityHeaders proves the M1 hardening headers ride every
// gateway response: nosniff, frame-deny, and the strict CSP.
func TestGatewaySetsSecurityHeaders(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fix.httpBase+"/routes", http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := fix.transport.httpClient().Do(req)
	if err != nil {
		t.Fatalf("GET /routes: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}

	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}

	if got := resp.Header.Get("Content-Security-Policy"); got != "default-src 'none'" {
		t.Errorf("Content-Security-Policy = %q, want default-src 'none'", got)
	}
}

// TestGatewayCORSEchoesAllowlistedOrigin proves the M1 CORS allowlist: a
// request from an allowlisted origin echoes that exact origin (never a
// wildcard), and an unlisted origin gets no allow header.
func TestGatewayCORSEchoesAllowlistedOrigin(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	allowed := corsHeaderForOrigin(t, fix, corsTestOrigin)
	if allowed != corsTestOrigin {
		t.Errorf("allow-origin for allowlisted origin = %q, want %q", allowed, corsTestOrigin)
	}

	denied := corsHeaderForOrigin(t, fix, "https://evil.example")
	if denied != "" {
		t.Errorf("allow-origin for unlisted origin = %q, want empty", denied)
	}
}

// corsHeaderForOrigin sends one request carrying the given Origin and returns
// the Access-Control-Allow-Origin the gateway answered with.
func corsHeaderForOrigin(t *testing.T, fix *fixture, origin string) string {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fix.httpBase+"/routes", http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set("Origin", origin)

	resp, err := fix.transport.httpClient().Do(req)
	if err != nil {
		t.Fatalf("GET /routes: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	return resp.Header.Get("Access-Control-Allow-Origin")
}

// TestGatewayRejectsOverLimitBody proves the M4 body cap: a POST whose body
// exceeds max_body_bytes is rejected with 413 instead of being buffered.
func TestGatewayRejectsOverLimitBody(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	oversized := `{"inputs":{"pad":"` +
		strings.Repeat("x", testMaxBodyBytes*2) + `"}}`

	resp := httpPost(t, fix.transport, fix.httpBase+"/v1/workflows/hello/runs", oversized)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit POST status = %d, want 413", resp.StatusCode)
	}
}
