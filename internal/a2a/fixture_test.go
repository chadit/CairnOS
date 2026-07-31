package a2a_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/chadit/CairnOS/internal/a2a"
	"github.com/chadit/CairnOS/internal/api"
	a2av1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/a2a/v1"
	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Workflow names and statuses the service tests share.
const (
	helloSkill      = "hello"
	slowSkill       = "slow"
	streamSkill     = "stream"
	statusSucceeded = "succeeded"
	statusRunning   = "running"
)

// loopbackEphemeral asks for any free loopback port so parallel fixtures never
// collide.
const loopbackEphemeral = "127.0.0.1:0"

// shutdownTimeout bounds fixture teardown.
const shutdownTimeout = 5 * time.Second

// authTestSecret meets the 32-byte HS256 floor for the auth-enabled fixture.
const authTestSecret = "0123456789abcdef0123456789abcdef"

// hs256EnvName is the variable the authenticator reads its secret from.
const hs256EnvName = "CAIRNOS_AUTH_HS256_SECRET" //nolint:gosec // environment variable name, not a credential

// bearerPrefix is the Authorization scheme the fixtures attach.
const bearerPrefix = "Bearer "

// helloTOML is a two-action linear flow over the mock runner; the cost fields
// give usage assertions something to count.
const helloTOML = `
[workflow]
name = "hello"

[action.greet]
runner = "mock"
params = { output = "hello world", cost_usd = 0.25 }

[action.reply]
runner = "mock"
depends_on = ["greet"]
params = { output = "hello yourself", cost_usd = 0.25 }
`

// slowTOML naps long enough that a cancel always lands mid-delay; the mock
// honors cancellation, so nothing waits the full minute.
const slowTOML = `
[workflow]
name = "slow"

[action.nap]
runner = "mock"
params = { delay_ms = 60000 }
`

// streamTOML holds its first action long enough that a consumer calling
// TaskUpdates right after CreateTask still attaches to the live run. The
// manager drops a run's retained handle the moment the run finishes (release in
// api/manager.go), and a late consumer then falls back to the stored terminal
// record, which carries no per-action steps. hello's actions are instant, so a
// loaded machine can finish the run between the two calls; the delay here is
// what makes the per-action assertions about the live stream hold. It is not
// padding, so do not trim it away.
const streamTOML = `
[workflow]
name = "stream"

[action.greet]
runner = "mock"
params = { output = "hello world", delay_ms = 750 }

[action.reply]
runner = "mock"
depends_on = ["greet"]
params = { output = "hello yourself" }
`

// fixture is one running server exposing the A2A service plus the well-known
// card, with the handles tests poke it through.
type fixture struct {
	a2a       a2av1.A2AServiceClient
	login     cairnosv1.LoginServiceClient
	bundle    *a2a.Bundle
	authStore *auth.SQLite
	cardKey   ed25519.PublicKey
	httpBase  string
}

// fixtureOptions tunes the server the fixture boots.
type fixtureOptions struct {
	// authEnabled turns on the token interceptor and seeds the auth store.
	authEnabled bool
}

// startFixture boots a server with the A2A extension attached on ephemeral
// loopback ports. The mock runner backs every workflow, and the card is signed
// with a generated key the test holds the public half of.
func startFixture(t *testing.T, opts fixtureOptions) *fixture {
	t.Helper()

	dir := t.TempDir()
	workflowsDir := filepath.Join(dir, "workflows")

	if err := os.MkdirAll(workflowsDir, 0o750); err != nil {
		t.Fatalf("create workflows dir: %v", err)
	}

	writeWorkflow(t, workflowsDir, "hello.toml", helloTOML)
	writeWorkflow(t, workflowsDir, "slow.toml", slowTOML)
	writeWorkflow(t, workflowsDir, "stream.toml", streamTOML)

	dbPath := filepath.Join(dir, "cairnos.db")

	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	mem, err := memory.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}

	t.Cleanup(func() { _ = mem.Close() })

	logger := slog.New(slog.DiscardHandler)
	manager := api.NewManager(logger)

	manager.Bind(engine.New(testEngineConfig(manager), testRegistry(t), db))

	cardPub, cardPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate card key: %v", err)
	}

	bundle, err := a2a.NewBundle(&a2a.BundleConfig{
		NodeName:   "cairnos-fixture",
		Version:    "0.0.1",
		Endpoint:   "grpc://127.0.0.1:7777",
		LoginURL:   "http://127.0.0.1:8080/v1/login",
		SigningKey: cardPriv,
		Manager:    manager,
		Resolver:   api.NewWorkflowResolver(workflowsDir, logger),
		Store:      db,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("build a2a bundle: %v", err)
	}

	apiCfg := &api.Config{
		GRPCAddr:     loopbackEphemeral,
		HTTPAddr:     loopbackEphemeral,
		WorkflowsDir: workflowsDir,
		Store:        db,
		Memory:       mem,
		Registry:     testRegistry(t),
		Manager:      manager,
		Logger:       logger,
	}

	authStore := wireFixtureAuth(t, opts, dbPath, apiCfg)

	if err := bundle.Attach(apiCfg); err != nil {
		t.Fatalf("attach a2a bundle: %v", err)
	}

	server := api.NewServer(apiCfg)

	if err := server.Start(t.Context()); err != nil {
		t.Fatalf("start server: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), shutdownTimeout)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown server: %v", err)
		}
	})

	conn, err := grpc.NewClient(server.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial grpc: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return &fixture{
		a2a:       a2av1.NewA2AServiceClient(conn),
		login:     cairnosv1.NewLoginServiceClient(conn),
		bundle:    bundle,
		authStore: authStore,
		cardKey:   cardPub,
		httpBase:  "http://" + server.HTTPAddr(),
	}
}

// wireFixtureAuth opens the auth store and attaches an authenticator when the
// fixture asks for token enforcement, returning the store (nil when auth is
// off) so tests can seed keys.
func wireFixtureAuth(t *testing.T, opts fixtureOptions, dbPath string, apiCfg *api.Config) *auth.SQLite {
	t.Helper()

	if !opts.authEnabled {
		return nil
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

	apiCfg.Auth = auth.New(authStore, material, auth.Config{
		Issuer:     "cairnos-fixture",
		Audience:   "cairnos",
		TTL:        time.Hour,
		RequireExp: true,
		RequireJTI: true,
	})

	return authStore
}

// token seeds an enabled key with the given scopes and exchanges it for a
// bearer token in the default workspace.
func (f *fixture) token(t *testing.T, scopes ...string) string {
	t.Helper()

	return f.tokenInWorkspace(t, "", scopes...)
}

// tokenInWorkspace seeds an enabled key in the named workspace and exchanges it
// for a bearer token, so a test can prove a task created in one workspace is
// unreachable from another.
func (f *fixture) tokenInWorkspace(t *testing.T, workspace string, scopes ...string) string {
	t.Helper()

	key, plaintext, err := auth.NewKey("fixture", scopes, workspace, time.Now())
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}

	if err := f.authStore.CreateKey(t.Context(), key); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	resp, err := f.login.Login(t.Context(), &cairnosv1.LoginRequest{Key: plaintext})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	return resp.GetToken()
}

// testEngineConfig mirrors the engine test defaults: a breaker that cannot
// trip on tiny samples, with the manager's hooks wired in.
func testEngineConfig(manager *api.Manager) engine.Config {
	return engine.Config{
		MaxConcurrentActions: 4,
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

	if err := registry.Register(builtins.MockName, builtins.MockFactory); err != nil {
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

// collectUpdates drains a TaskUpdates stream to EOF.
func collectUpdates(t *testing.T, stream grpc.ServerStreamingClient[a2av1.TaskUpdate]) []*a2av1.TaskUpdate {
	t.Helper()

	var updates []*a2av1.TaskUpdate

	for {
		update, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return updates
		}

		if err != nil {
			t.Fatalf("recv task update: %v", err)
		}

		updates = append(updates, update)
	}
}
