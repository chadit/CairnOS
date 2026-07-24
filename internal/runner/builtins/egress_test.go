package builtins_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// guardTimeout is the token client's bound in these tests; the value is
// immaterial because the guard refuses or allows at dial, well before it.
const guardTimeout = 5 * time.Second

// allowPrivateKey is the per-runner opt-out config key under test.
const allowPrivateKey = "allow_private_targets"

// blockedTargetSuffix is the dotted suffix every runner's egress refusal
// carries, so a test can assert the classification without hardcoding the
// runner name.
const blockedTargetSuffix = ".blocked_target"

// loopbackServer starts an httptest server (bound to 127.0.0.1) that answers
// every request with a 200, so a fetch that reaches it is observable as a
// success and a fetch that the guard refuses never lands.
func loopbackServer(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)

	return server
}

// assertBlocked fails unless err is the permanent blocked_target a runner
// returns when the egress guard refuses a destination.
func assertBlocked(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("fetch to a blocked target succeeded; want it refused")
	}

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("blocked target error = %v, want a classified *runner.Error", err)
	}

	if classified.Class != runner.ClassPermanent {
		t.Fatalf("blocked target class = %v, want permanent", classified.Class)
	}

	if !strings.HasSuffix(classified.Code, blockedTargetSuffix) {
		t.Fatalf("blocked target code = %q, want a *%s code", classified.Code, blockedTargetSuffix)
	}
}

// TestHTTPRefusesLoopbackByDefault proves the SSRF guard: a fetch to the
// loopback test server is refused with a permanent blocked_target unless the
// operator opts in with allow_private_targets.
func TestHTTPRefusesLoopbackByDefault(t *testing.T) {
	t.Parallel()

	server := loopbackServer(t)

	guarded, err := builtins.HTTPFactory(nil)
	if err != nil {
		t.Fatalf("build guarded http runner: %v", err)
	}

	_, err = guarded.Execute(t.Context(), httpTask(map[string]any{urlKey: server.URL}))
	assertBlocked(t, err)
}

// TestHTTPAllowsLoopbackWhenOptedIn proves the opt-out: with
// allow_private_targets = true the IP guard is off and the same loopback fetch
// reaches the server.
func TestHTTPAllowsLoopbackWhenOptedIn(t *testing.T) {
	t.Parallel()

	server := loopbackServer(t)

	guarded, err := builtins.HTTPFactory(map[string]any{allowPrivateKey: true})
	if err != nil {
		t.Fatalf("build opted-in http runner: %v", err)
	}

	result, err := guarded.Execute(t.Context(), httpTask(map[string]any{urlKey: server.URL}))
	if err != nil {
		t.Fatalf("opted-in fetch to loopback: %v", err)
	}

	output, isMap := result.Output.(map[string]any)
	if !isMap || output[statusField] != http.StatusOK {
		t.Fatalf("opted-in fetch output = %#v, want a 200 result", result.Output)
	}
}

// TestHTTPRefusesRedirectToLoopback proves the dialer guard covers redirect
// hops: a public-looking first hop that 302s onto a loopback URL is refused,
// because every dial (including the redirect's) routes through the guarded
// dialer's Control. The default-guard client blocks the loopback hop with a
// blocked_target.
func TestHTTPRefusesRedirectToLoopback(t *testing.T) {
	t.Parallel()

	loopback := loopbackServer(t)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, loopback.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	guarded, err := builtins.HTTPFactory(nil)
	if err != nil {
		t.Fatalf("build guarded http runner: %v", err)
	}

	_, err = guarded.Execute(t.Context(), httpTask(map[string]any{urlKey: redirector.URL}))
	assertBlocked(t, err)
}

// TestHTTPRefusesRedirectToFileScheme proves the redirect scheme re-check: with
// the IP guard opted out (so the loopback redirector is reachable), a redirect
// to a file:// URL is still refused by CheckRedirect, the hop the dialer never
// sees because no dial happens for a file scheme.
func TestHTTPRefusesRedirectToFileScheme(t *testing.T) {
	t.Parallel()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "file:///etc/passwd")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	guarded, err := builtins.HTTPFactory(map[string]any{allowPrivateKey: true})
	if err != nil {
		t.Fatalf("build opted-in http runner: %v", err)
	}

	_, err = guarded.Execute(t.Context(), httpTask(map[string]any{urlKey: redirector.URL}))
	assertBlocked(t, err)
}

// TestHTTPRefusesFileScheme proves the scheme allowlist: a file:// target is
// refused at request build with a permanent blocked_target, before any dial.
func TestHTTPRefusesFileScheme(t *testing.T) {
	t.Parallel()

	guarded, err := builtins.HTTPFactory(nil)
	if err != nil {
		t.Fatalf("build guarded http runner: %v", err)
	}

	_, err = guarded.Execute(t.Context(), httpTask(map[string]any{urlKey: "file:///etc/passwd"}))
	assertBlocked(t, err)
}

// TestWebhookRefusesLoopbackByDefault proves the same guard covers the webhook
// runner: a post to loopback is refused unless allow_private_targets opts in.
func TestWebhookRefusesLoopbackByDefault(t *testing.T) {
	t.Parallel()

	server := loopbackServer(t)

	guarded, err := builtins.WebhookFactory(nil)
	if err != nil {
		t.Fatalf("build guarded webhook runner: %v", err)
	}

	_, err = guarded.Execute(t.Context(), webhookTask(server.URL, "payload"))
	assertBlocked(t, err)
}

// TestWebhookAllowsLoopbackWhenOptedIn proves the webhook opt-out reaches an
// internal endpoint the operator declared safe.
func TestWebhookAllowsLoopbackWhenOptedIn(t *testing.T) {
	t.Parallel()

	server := loopbackServer(t)

	guarded, err := builtins.WebhookFactory(map[string]any{allowPrivateKey: true})
	if err != nil {
		t.Fatalf("build opted-in webhook runner: %v", err)
	}

	if _, err := guarded.Execute(t.Context(), webhookTask(server.URL, "payload")); err != nil {
		t.Fatalf("opted-in webhook post to loopback: %v", err)
	}
}

// TestMCPClientRefusesLoopbackByDefault proves the guard covers the MCP
// client's streamable HTTP transport: a server_url at the loopback echo
// server is refused with a permanent blocked_target unless the operator opts
// in with allow_private_targets.
func TestMCPClientRefusesLoopbackByDefault(t *testing.T) {
	t.Parallel()

	guarded, err := builtins.MCPClientFactory(map[string]any{
		serverURLKey: newMCPServerURL(t),
		toolKey:      echoTool,
	})
	if err != nil {
		t.Fatalf("build guarded mcp client: %v", err)
	}

	_, err = guarded.Execute(t.Context(), mcpTask(map[string]any{textKey: echoInput}))
	assertBlocked(t, err)
}

// TestMCPClientAllowsLoopbackWhenOptedIn proves the opt-out: with
// allow_private_targets = true the same loopback call reaches the echo tool
// and returns its output.
func TestMCPClientAllowsLoopbackWhenOptedIn(t *testing.T) {
	t.Parallel()

	guarded, err := builtins.MCPClientFactory(map[string]any{
		serverURLKey:    newMCPServerURL(t),
		toolKey:         echoTool,
		allowPrivateKey: true,
	})
	if err != nil {
		t.Fatalf("build opted-in mcp client: %v", err)
	}

	result, err := guarded.Execute(t.Context(), mcpTask(map[string]any{textKey: echoInput}))
	if err != nil {
		t.Fatalf("opted-in mcp call to loopback: %v", err)
	}

	if result.Output != "echo: "+echoInput {
		t.Fatalf("opted-in mcp call output = %#v, want the echo", result.Output)
	}
}

// TestMCPClientRefusesFileScheme proves the scheme allowlist runs at
// Validate: a file:// server_url fails as a permanent
// mcp_client.blocked_target before any request is built, so nothing dials.
// The opt-out does not weaken it: allow_private_targets drops only the IP
// guard, never the scheme guard.
func TestMCPClientRefusesFileScheme(t *testing.T) {
	t.Parallel()

	guarded, err := builtins.MCPClientFactory(map[string]any{
		serverURLKey:    "file:///etc/passwd",
		toolKey:         echoTool,
		allowPrivateKey: true,
	})
	if err != nil {
		t.Fatalf("build mcp client: %v", err)
	}

	_, err = guarded.Execute(t.Context(), mcpTask(map[string]any{textKey: echoInput}))

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("file scheme error = %v, want a classified *runner.Error", err)
	}

	if classified.Class != runner.ClassPermanent || classified.Code != "mcp_client.blocked_target" {
		t.Fatalf("class/code = %v/%q, want permanent mcp_client.blocked_target", classified.Class, classified.Code)
	}
}

// TestGuardedHTTPClientRefusesLoopbackByDefault proves the exported credentials
// token client carries the same dial-time IP guard as the runner clients: a
// request to a loopback address is refused when allowPrivate is false.
func TestGuardedHTTPClientRefusesLoopbackByDefault(t *testing.T) {
	t.Parallel()

	server := loopbackServer(t)
	client := builtins.GuardedHTTPClient(guardTimeout, false)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()

		t.Fatal("guarded client reached a loopback address; want it refused at dial")
	}
}

// TestGuardedHTTPClientAllowsLoopbackWhenOptedIn proves the allowPrivate opt-out:
// with the IP guard off, the same loopback request reaches the server, matching
// the runner semantics for internal endpoints.
func TestGuardedHTTPClientAllowsLoopbackWhenOptedIn(t *testing.T) {
	t.Parallel()

	server := loopbackServer(t)
	client := builtins.GuardedHTTPClient(guardTimeout, true)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("opted-in guarded client should reach loopback: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}
