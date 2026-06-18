package mcp_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/mcp"
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode publish body: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "token rejected")
	}))
	t.Cleanup(server.Close)

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

func TestRegistryListDecodesServers(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/servers" {
			t.Errorf("list path = %q, want /servers", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"servers":[{"name":"peer","endpoint":"https://peer/mcp","tools":[{"name":"do_thing","description":"d"}]}]}`)
	}))
	t.Cleanup(server.Close)

	client, err := mcp.NewRegistryClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	servers, err := client.List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(servers) != 1 || servers[0].Name != "peer" || len(servers[0].Tools) != 1 {
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
				{Name: "create_issue"},
				{Name: "create_issue"}, // duplicate tool name -> deduped runner key
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

func TestImportConfigSkipsSelf(t *testing.T) {
	t.Parallel()

	servers := []mcp.ServerEntry{
		{Name: "self", Endpoint: "https://me/mcp/", Tools: []mcp.ToolEntry{{Name: "x"}}},
		{Name: "peer", Endpoint: "https://peer/mcp", Tools: []mcp.ToolEntry{{Name: "y"}}},
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
