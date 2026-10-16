package mcp_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/mcp"
	"github.com/chadit/CairnOS/internal/nettest"
)

// Registry entry names the import and list tests share: a remote peer node and
// a tool name that appears twice so the dedupe path runs.
const (
	peerName        = "peer"
	toolCreateIssue = "create_issue"
)

func TestNewRegistryClientRejectsEmptyURL(t *testing.T) {
	t.Parallel()

	if _, err := mcp.NewRegistryClient("   ", "", nil); !errors.Is(err, mcp.ErrNoRegistryURL) {
		t.Fatalf("empty url err = %v, want ErrNoRegistryURL", err)
	}
}

func TestRegistryPublishPostsRecord(t *testing.T) {
	t.Parallel()

	var (
		gotPath string
		gotAuth string
		gotBody mcp.ServerEntry
	)

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode publish body: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
	}))

	client, err := mcp.NewRegistryClient(server.URL, "secret-token", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	record := mcp.ServerRecord("https://node.example/mcp", true)
	if err := client.Publish(t.Context(), &record); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if gotPath != "/publish" {
		t.Errorf("publish path = %q, want /publish", gotPath)
	}

	if gotAuth != "Bearer secret-token" {
		t.Errorf("authorization = %q, want bearer token", gotAuth)
	}

	if gotBody.Endpoint != "https://node.example/mcp" {
		t.Errorf("published endpoint = %q", gotBody.Endpoint)
	}

	if len(gotBody.Tools) != 3 {
		t.Errorf("published %d tools, want 3 (with rules)", len(gotBody.Tools))
	}
}

func TestRegistryPublishOmitsAuthHeaderWithoutToken(t *testing.T) {
	t.Parallel()

	var hadAuth bool

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]

		w.WriteHeader(http.StatusOK)
	}))

	client, err := mcp.NewRegistryClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	record := mcp.ServerRecord("https://node.example/mcp", false)
	if err := client.Publish(t.Context(), &record); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if hadAuth {
		t.Error("authorization header set with no token configured")
	}
}

func TestRegistryPublishSurfacesStatusError(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)

		if _, err := io.WriteString(w, "token rejected"); err != nil {
			t.Errorf("write registry body: %v", err)
		}
	}))

	client, err := mcp.NewRegistryClient(server.URL, "bad", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	record := mcp.ServerRecord("https://node.example/mcp", true)

	err = client.Publish(t.Context(), &record)
	if err == nil {
		t.Fatal("publish to a 403 registry returned no error")
	}

	regErr, ok := errors.AsType[*mcp.RegistryError](err)
	if !ok {
		t.Fatalf("error %v is not a *RegistryError", err)
	}

	if regErr.StatusCode != http.StatusForbidden {
		t.Errorf("status code = %d, want 403", regErr.StatusCode)
	}

	if !strings.Contains(regErr.Body, "token rejected") {
		t.Errorf("body = %q, want the registry message", regErr.Body)
	}
}

// TestRegistryStatusErrorSurvivesATruncatedBody proves a registry that drops
// the connection mid-body still yields a typed RegistryError with its status,
// and the read failure stays visible next to the partial body.
func TestRegistryStatusErrorSurvivesATruncatedBody(t *testing.T) {
	t.Parallel()

	const partialBody = "registry overloaded"

	// A real server, not the in-process fake, so the short body reaches the
	// client as a dropped connection.
	server := nettest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A declared length longer than what is written makes the server cut
		// the connection, so the client's body read ends in an unexpected EOF.
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusServiceUnavailable)

		if _, err := io.WriteString(w, partialBody); err != nil {
			t.Errorf("write partial registry body: %v", err)
		}
	}))

	client, err := mcp.NewRegistryClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	_, err = client.List(t.Context())

	regErr, ok := errors.AsType[*mcp.RegistryError](err)
	if !ok {
		t.Fatalf("error %v is not a *RegistryError", err)
	}

	if regErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status code = %d, want 503", regErr.StatusCode)
	}

	if !strings.Contains(regErr.Body, partialBody) || !strings.Contains(regErr.Body, "reading the body failed") {
		t.Errorf("body = %q, want the partial body and the read failure", regErr.Body)
	}
}

func TestRegistryListDecodesServers(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/servers" {
			t.Errorf("list path = %q, want /servers", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")

		if _, err := io.WriteString(w, `{"servers":[{"name":"peer","endpoint":"https://peer/mcp","tools":[{"name":"do_thing","description":"d"}]}]}`); err != nil {
			t.Errorf("write registry list: %v", err)
		}
	}))

	client, err := mcp.NewRegistryClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	servers, err := client.List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(servers) != 1 || servers[0].Name != peerName || len(servers[0].Tools) != 1 {
		t.Fatalf("decoded servers = %+v", servers)
	}
}

func TestServerRecordGatesRulesTool(t *testing.T) {
	t.Parallel()

	withRules := mcp.ServerRecord("https://node/mcp", true)
	if len(withRules.Tools) != 3 || !hasTool(withRules.Tools, "get_language_rules") {
		t.Errorf("with rules: tools = %+v, want get_language_rules present", withRules.Tools)
	}

	without := mcp.ServerRecord("https://node/mcp", false)
	if len(without.Tools) != 2 || hasTool(without.Tools, "get_language_rules") {
		t.Errorf("without rules: tools = %+v, want get_language_rules absent", without.Tools)
	}

	if without.Transport != "http" {
		t.Errorf("transport = %q, want http", without.Transport)
	}
}

func TestImportConfigRendersRunnerBlocks(t *testing.T) {
	t.Parallel()

	servers := []mcp.ServerEntry{
		{
			Name:     "GitHub MCP",
			Endpoint: "https://mcp.example/github",
			Tools: []mcp.ToolEntry{
				{Name: toolCreateIssue},
				{Name: toolCreateIssue}, // duplicate tool name -> deduped runner key
			},
		},
		{Name: "empty", Endpoint: ""}, // skipped: no endpoint
	}

	rendered, names := mcp.ImportConfig(servers, "")

	if len(names) != 2 {
		t.Fatalf("defined %d runners, want 2: %v", len(names), names)
	}

	if names[0] != "github_mcp_create_issue" {
		t.Errorf("first runner name = %q, want slugified server_tool", names[0])
	}

	if names[1] != "github_mcp_create_issue_2" {
		t.Errorf("collision name = %q, want _2 suffix", names[1])
	}

	for _, want := range []string{
		"[runner.github_mcp_create_issue]",
		`type       = "mcp_client"`,
		`server_url = "https://mcp.example/github"`,
		`tool       = "create_issue"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered config missing %q\n%s", want, rendered)
		}
	}
}

func TestImportConfigReservesBuiltinClientName(t *testing.T) {
	t.Parallel()

	// Server "mcp" with tool "client" slugs to exactly mcp_client, the one name
	// the CLI wiring treats as configuration for the built-in instance.
	servers := []mcp.ServerEntry{
		{Name: "mcp", Endpoint: "https://mcp.example/tools", Tools: []mcp.ToolEntry{{Name: "client"}}},
	}

	rendered, names := mcp.ImportConfig(servers, "")

	if len(names) != 1 || names[0] != "mcp_client_2" {
		t.Fatalf("names = %v, want the reserved slug suffixed as mcp_client_2", names)
	}

	if !strings.Contains(rendered, "[runner.mcp_client_2]") {
		t.Errorf("rendered config missing [runner.mcp_client_2]\n%s", rendered)
	}

	if strings.Contains(rendered, "[runner.mcp_client]") {
		t.Errorf("rendered config emits the reserved [runner.mcp_client] block\n%s", rendered)
	}
}

func TestImportConfigHeaderMakesNoTokenClaim(t *testing.T) {
	t.Parallel()

	rendered, _ := mcp.ImportConfig(nil, "")

	if strings.Contains(rendered, "CAIRNOS_RUNNER") {
		t.Errorf("header still claims a per-runner token env var\n%s", rendered)
	}

	for _, want := range []string{
		"Authenticated MCP servers aren't supported yet",
		"sends no Authorization header",
		"allow_private_targets = true",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("header missing %q\n%s", want, rendered)
		}
	}
}

func TestImportConfigSkipsSelf(t *testing.T) {
	t.Parallel()

	servers := []mcp.ServerEntry{
		{Name: "self", Endpoint: "https://me/mcp/", Tools: []mcp.ToolEntry{{Name: "x"}}},
		{Name: peerName, Endpoint: "https://peer/mcp", Tools: []mcp.ToolEntry{{Name: "y"}}},
	}

	_, names := mcp.ImportConfig(servers, "https://me/mcp")

	if len(names) != 1 || names[0] != "peer_y" {
		t.Fatalf("names = %v, want only the peer (self skipped, trailing slash ignored)", names)
	}
}

// hasTool reports whether a tool name appears in a catalog.
func hasTool(tools []mcp.ToolEntry, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}

	return false
}
