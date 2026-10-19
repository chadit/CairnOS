package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/auth/authtest"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/mcp"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Workflow name and the seed values shared across the MCP server tests.
const (
	helloWorkflow  = "hello"
	memoryScope    = "global"
	memoryContent  = "remember the cairn"
	clientName     = "mcp-test-client"
	clientVersion  = "0.0.1"
	maxConcurrency = 4

	// Rule fixture the get_language_rules tool test queries.
	rulesLang  = "go"
	rulesKey   = "max_line"
	rulesValue = "120"
)

// helloTOML is a single mock action so a run completes without external
// services; the cost field gives the engine something to settle.
const helloTOML = `
[workflow]
name = "hello"

[action.greet]
runner = "mock"
params = { output = "hello world", cost_usd = 0.25 }
`

// fixture is one MCP service over a real sqlite store, memory, and engine, plus
// the connected client session the tests drive it through.
type fixture struct {
	service   *mcp.Service
	store     *sqlite.Store
	memory    *memory.SQLite
	authStore *auth.SQLite
	authn     *auth.Authenticator
}

// fakeRules is a minimal mcp.RulesLookup the fixture wires so the
// get_language_rules tool registers and answers; it returns a fixed merged map
// for one language and reports unknown otherwise.
type fakeRules struct {
	merged   map[string]string
	language string
}

// GetRules implements mcp.RulesLookup.
func (f *fakeRules) GetRules(_ context.Context, language, _ string) (map[string]string, error) {
	if language != f.language {
		return nil, errUnknownLanguage
	}

	return f.merged, nil
}

// errUnknownLanguage stands in for the rules store's unknown-language signal,
// kept a sentinel so the fake satisfies err113.
var errUnknownLanguage = errors.New("no rules for language")

// fixtureOptions tunes the service the fixture builds.
type fixtureOptions struct {
	// authEnabled wires a real authenticator so the scope-enforcement tests
	// can present tokens; off leaves auth disabled (every call passes).
	authEnabled bool
}

// authTestSecret meets the 32-byte HS256 floor.
const authTestSecret = "0123456789abcdef0123456789abcdef"

// hs256EnvName is the variable the authenticator reads its secret from.
const hs256EnvName = "CAIRNOS_AUTH_HS256_SECRET" //nolint:gosec // environment variable name, not a credential

// startFixture builds the service over fresh seams and seeds one workflow file
// plus one memory entry the tests query.
func startFixture(t *testing.T, opts fixtureOptions) *fixture {
	t.Helper()

	dir := t.TempDir()
	workflowsDir := filepath.Join(dir, "workflows")

	if err := os.MkdirAll(workflowsDir, 0o750); err != nil {
		t.Fatalf("create workflows dir: %v", err)
	}

	writeWorkflow(t, workflowsDir, "hello.toml", helloTOML)

	dbPath := filepath.Join(dir, "cairnos.db")

	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = store.Close() })

	mem, err := memory.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}

	t.Cleanup(func() { _ = mem.Close() })

	seedMemory(t, mem)

	logger := slog.New(slog.DiscardHandler)
	manager := api.NewManager(logger)
	manager.Bind(engine.New(testEngineConfig(manager), testRegistry(t), store))

	mcpCfg := &mcp.Config{
		Manager:      manager,
		Resolver:     api.NewWorkflowResolver(workflowsDir, logger),
		Store:        store,
		Memory:       mem,
		Rules:        &fakeRules{language: rulesLang, merged: map[string]string{rulesKey: rulesValue}},
		WorkflowsDir: workflowsDir,
		Logger:       logger,
	}

	authStore, authn := wireFixtureAuth(t, opts, dbPath, mcpCfg)

	service, err := mcp.NewService(mcpCfg)
	if err != nil {
		t.Fatalf("build mcp service: %v", err)
	}

	return &fixture{service: service, store: store, memory: mem, authStore: authStore, authn: authn}
}

// connect opens an in-memory client session to a server bound to token. The
// server transport must connect before the client so the client can initialize
// the session (the SDK's stated ordering).
func (f *fixture) connect(t *testing.T, token string) *sdk.ClientSession {
	t.Helper()

	serverTransport, clientTransport := sdk.NewInMemoryTransports()

	serverSession, err := f.service.Connect(t.Context(), token, serverTransport)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}

	t.Cleanup(func() { _ = serverSession.Close() })

	client := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: clientVersion}, nil)

	session, err := client.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}

	t.Cleanup(func() { _ = session.Close() })

	return session
}

// wireFixtureAuth opens the auth store and attaches an authenticator when the
// fixture asks for enforcement, returning both (nil, nil when auth is off) so
// tests can seed keys and mint tokens.
func wireFixtureAuth(t *testing.T, opts fixtureOptions, dbPath string, cfg *mcp.Config) (*auth.SQLite, *auth.Authenticator) {
	t.Helper()

	if !opts.authEnabled {
		return nil, nil
	}

	authStore, err := auth.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}

	t.Cleanup(func() { _ = authStore.Close() })

	material, err := auth.LoadMaterial(auth.AlgHS256, func(key string) string {
		if key == hs256EnvName {
			return authTestSecret
		}

		return ""
	})
	if err != nil {
		t.Fatalf("load material: %v", err)
	}

	authn := auth.New(authStore, material, auth.Config{
		HashCost:   authtest.FloorHashCost(),
		Issuer:     "cairnos-fixture",
		Audience:   "cairnos",
		TTL:        time.Hour,
		RequireExp: true,
		RequireJTI: true,
	})
	cfg.Auth = authn

	return authStore, authn
}

// token seeds an enabled key with the given scopes in the default workspace and
// exchanges it for a bearer token through the authenticator the fixture wired.
func (f *fixture) token(t *testing.T, scopes ...string) string {
	t.Helper()

	return f.workspaceToken(t, "", scopes...)
}

// workspaceToken seeds an enabled key bound to workspace with the given scopes
// and exchanges it for a bearer token, so a test can drive a run as a specific
// tenant and prove the run lands in that workspace. An empty workspace falls
// back to the default tenant, matching the access-key default.
func (f *fixture) workspaceToken(t *testing.T, workspace string, scopes ...string) string {
	t.Helper()

	key, plaintext, err := auth.NewKey("fixture", scopes, workspace, time.Now(), authtest.FloorHashCost())
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}

	if createKeyErr := f.authStore.CreateKey(t.Context(), key); createKeyErr != nil {
		t.Fatalf("CreateKey: %v", createKeyErr)
	}

	tokenStr, _, err := f.authn.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	return tokenStr
}

// seedMemory inserts the one entry the query_memory test searches for.
func seedMemory(t *testing.T, mem memory.Store) {
	t.Helper()

	if _, err := mem.Add(t.Context(), &memory.Entry{
		Scope:      memoryScope,
		Content:    memoryContent,
		Importance: 50,
		Tags:       []string{"note"},
	}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
}

// testEngineConfig mirrors the engine test defaults with the manager's hooks.
func testEngineConfig(manager *api.Manager) engine.Config {
	return engine.Config{
		MaxConcurrentActions: maxConcurrency,
		DefaultRetry:         reliability.Policy{MaxAttempts: 1},
		Breaker: reliability.BreakerConfig{
			FailureThreshold: 1,
			Window:           time.Minute,
			MinRequests:      1000,
			Cooldown:         time.Minute,
		},
		NewID:   manager.NewID,
		OnEvent: manager.Dispatch,
	}
}

// testRegistry builds a registry holding just the mock runner.
func testRegistry(t *testing.T) *runner.Registry {
	t.Helper()

	registry := runner.NewRegistry()

	if err := registry.Register(builtins.MockName, builtins.MockFactory()); err != nil {
		t.Fatalf("register mock: %v", err)
	}

	if err := registry.BuildAll(nil); err != nil {
		t.Fatalf("build runners: %v", err)
	}

	return registry
}

func writeWorkflow(t *testing.T, dir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write workflow %s: %v", name, err)
	}
}

// decodeStructured re-decodes a tool result's structured content into dst, the
// path the SDK marshals a typed Out value through to the client.
func decodeStructured(t *testing.T, content, dst any) {
	t.Helper()

	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}

	if err := json.Unmarshal(encoded, dst); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
}

// resultText joins a tool or resource result's text content, the surface a
// scope-denied call carries its canonical error code in.
func resultText(content []sdk.Content) string {
	texts := make([]string, 0, len(content))

	for _, block := range content {
		if textBlock, isText := block.(*sdk.TextContent); isText {
			texts = append(texts, textBlock.Text)
		}
	}

	return strings.Join(texts, "")
}
