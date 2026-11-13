package api_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/chadit/CairnOS/internal/api"
	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/auth/authtest"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/store/sqlite"
)

// wireUnauthenticated and wireForbidden already exist in auth_test.go; these
// name the hardening-specific wire shapes used here.
const wireTooManyLogins = "auth.too_many_logins" //nolint:gosec // canonical error code name, not a credential

// hardeningOptions tunes the hardening server fixture: the login limiter and the
// stream-recheck cadence, with an injectable stream clock so the tests drive the
// recheck deterministically.
type hardeningOptions struct {
	limiter       *auth.LoginLimiter
	streamClock   func() time.Time
	streamRecheck time.Duration
}

// hardeningFixture is one auth-enabled server built with the hardening knobs set,
// plus the handles the tests need.
type hardeningFixture struct {
	login     cairnosv1.LoginServiceClient
	workflow  cairnosv1.WorkflowServiceClient
	authStore *auth.SQLite
	clock     *authClock
}

// startHardeningServer boots an auth-enabled server with the hardening options
// applied. It mirrors startAuthServer but exposes the login limiter and the
// stream recheck so the hardening tests can exercise them.
func startHardeningServer(t *testing.T, opts hardeningOptions) *hardeningFixture {
	t.Helper()

	dir := t.TempDir()
	workflowsDir := filepath.Join(dir, "workflows")

	if err := os.MkdirAll(workflowsDir, 0o750); err != nil {
		t.Fatalf("create workflows dir: %v", err)
	}

	writeWorkflow(t, workflowsDir, "hello.toml", helloTOML)
	writeWorkflow(t, workflowsDir, "slow.toml", slowTOML)

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

	clock := &authClock{now: time.Now()}

	authn := auth.New(authStore, material, auth.Config{
		HashCost:   authtest.FloorHashCost(),
		Issuer:     testIssuer,
		Audience:   testAudience,
		TTL:        authTTL,
		RequireExp: true,
		RequireJTI: true,
		Now:        clock.Now,
	})

	logger := slog.New(slog.DiscardHandler)
	manager := api.NewManager(logger)
	transport := nettest.NewNetwork()

	manager.Bind(engine.New(testEngineConfig(manager), testRegistry(t), db))

	server := api.NewServer(&api.Config{
		Transport:     transport,
		GRPCAddr:      loopbackEphemeral,
		HTTPAddr:      loopbackEphemeral,
		WorkflowsDir:  workflowsDir,
		Store:         db,
		Memory:        mem,
		Registry:      testRegistry(t),
		Manager:       manager,
		Auth:          authn,
		LoginLimiter:  opts.limiter,
		StreamRecheck: opts.streamRecheck,
		StreamClock:   opts.streamClock,
		Logger:        logger,
	})

	if startErr := server.Start(t.Context()); startErr != nil {
		t.Fatalf("start server: %v", startErr)
	}

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), shutdownTimeout)
		defer cancel()

		if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
			t.Errorf("shutdown server: %v", shutdownErr)
		}
	})

	conn := transport.DialGRPC(t, server.GRPCAddr())

	return &hardeningFixture{
		login:     cairnosv1.NewLoginServiceClient(conn),
		workflow:  cairnosv1.NewWorkflowServiceClient(conn),
		authStore: authStore,
		clock:     clock,
	}
}

// seedHardeningKey mints and stores one enabled key with the given scopes,
// returning its plaintext.
func (f *hardeningFixture) seedKey(t *testing.T, scopes []string) string {
	t.Helper()

	key, plaintext, err := auth.NewKey("hardening", scopes, "", f.clock.Now(), authtest.FloorHashCost())
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}

	if err := f.authStore.CreateKey(t.Context(), key); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	return plaintext
}

// token runs the full key-to-token exchange for one scope set.
func (f *hardeningFixture) token(t *testing.T, scopes ...string) string {
	t.Helper()

	plaintext := f.seedKey(t, scopes)

	resp, err := f.login.Login(t.Context(), &cairnosv1.LoginRequest{Key: plaintext})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	return resp.GetToken()
}

// TestLoginConcurrencyCapShedsLoad proves the login concurrency cap: with a
// single-slot derivation pool, firing many simultaneous logins splits the
// callers into logins that got a working token and overflow refused with
// RESOURCE_EXHAUSTED, with nothing landing anywhere else. The split point is
// scheduling-dependent, so the test pins the two outcomes and their total
// rather than a fixed count.
func TestLoginConcurrencyCapShedsLoad(t *testing.T) {
	t.Parallel()

	// A one-slot pool and a high rate isolate the concurrency cap from the
	// rate limit. The per-peer rate is large so the bucket never trips here.
	fix := startHardeningServer(t, hardeningOptions{
		limiter: auth.NewLoginLimiter(auth.LoginLimitConfig{MaxConcurrent: 1, RatePerMinute: 100000}),
	})

	plaintext := fix.seedKey(t, []string{auth.ScopeReadRun})

	const callers = 8

	var (
		wg        sync.WaitGroup
		issued    atomic.Int64
		exhausted atomic.Int64
		other     atomic.Int64
	)

	for range callers {
		wg.Go(func() {
			resp, err := fix.login.Login(t.Context(), &cairnosv1.LoginRequest{Key: plaintext})

			switch {
			case err == nil && resp.GetToken() != "":
				issued.Add(1)
			case status.Code(err) == codes.ResourceExhausted && canonicalCode(t, err) == wireTooManyLogins:
				exhausted.Add(1)
			default:
				other.Add(1)

				t.Errorf("login answered neither a token nor %s: %v", wireTooManyLogins, err)
			}
		})
	}

	wg.Wait()

	if issued.Load() == 0 {
		t.Errorf("%d concurrent logins issued no token; the cap must shed load, not refuse everyone", callers)
	}

	if exhausted.Load() == 0 {
		t.Errorf("%d concurrent logins against a 1-slot pool, want at least one ResourceExhausted", callers)
	}

	if total := issued.Load() + exhausted.Load(); total != callers {
		t.Errorf("tokens %d + refusals %d = %d, want %d callers accounted for (%d landed elsewhere)",
			issued.Load(), exhausted.Load(), total, callers, other.Load())
	}
}

// TestLoginRateLimitTripsAfterBurst proves the per-peer login rate limit: with a small
// per-minute allowance and a wide pool, a single peer's logins pass up to the
// burst and then trip RESOURCE_EXHAUSTED, all from one frozen instant so the
// bucket never refills mid-test.
func TestLoginRateLimitTripsAfterBurst(t *testing.T) {
	t.Parallel()

	const burst = 3

	// A frozen clock means no refill: exactly burst logins pass, the next
	// trips. A wide pool keeps the concurrency cap out of the way.
	frozen := time.Now()
	limiter := auth.NewLoginLimiter(auth.LoginLimitConfig{
		MaxConcurrent: 100,
		RatePerMinute: burst,
		Now:           func() time.Time { return frozen },
	})

	fix := startHardeningServer(t, hardeningOptions{limiter: limiter})
	plaintext := fix.seedKey(t, []string{auth.ScopeReadRun})

	// The first burst logins succeed (sharing the gateway-loopback peer key).
	for i := range burst {
		if _, err := fix.login.Login(t.Context(), &cairnosv1.LoginRequest{Key: plaintext}); err != nil {
			t.Fatalf("login %d within burst = %v, want success", i, err)
		}
	}

	// The next login over the same peer trips the rate limit.
	_, err := fix.login.Login(t.Context(), &cairnosv1.LoginRequest{Key: plaintext})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("login past burst = %v, want ResourceExhausted", err)
	}

	if got := canonicalCode(t, err); got != wireTooManyLogins {
		t.Fatalf("canonical code = %q, want %q", got, wireTooManyLogins)
	}
}

// streamRecheckInterval is the stream recheck cadence under test; the injected
// stream clock jumps a full interval per read, so a recheck fires on the next
// send after a revoke without any real time passing.
const streamRecheckInterval = 15 * time.Second

// TestStreamRevokeMidStreamTerminates proves the stream recheck end to end: a long-lived
// RunWorkflow stream keeps delivering until the token is revoked mid-stream,
// then the next send past the recheck interval re-verifies, fails, and ends
// the stream with UNAUTHENTICATED. The stream clock is injected and advances a
// full interval per read, so the recheck is driven deterministically with no
// sleeping.
func TestStreamRevokeMidStreamTerminates(t *testing.T) {
	t.Parallel()

	// The stream clock jumps one interval on every read, so each server send
	// after the first triggers a recheck. A mutex guards the shared instant
	// because gRPC reads it from the serving goroutine.
	var (
		clockMu sync.Mutex
		instant = time.Now()
	)

	streamClock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()

		instant = instant.Add(streamRecheckInterval)

		return instant
	}

	fix := startHardeningServer(t, hardeningOptions{
		streamRecheck: streamRecheckInterval,
		streamClock:   streamClock,
	})

	runToken := fix.token(t, auth.ScopeWriteWorkflow)
	adminToken := fix.token(t, auth.ScopeAdmin)

	// The slow workflow naps after its action starts, holding the stream open.
	stream, err := fix.workflow.RunWorkflow(authedCtx(t, runToken), &cairnosv1.RunWorkflowRequest{WorkflowName: slowName})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	runID := waitForRunning(t, stream)

	// Revoke the run token, then cancel the run so the engine emits more
	// events; the next send past the recheck interval finds the revocation.
	if _, err := fix.login.Revoke(authedCtx(t, adminToken), &cairnosv1.RevokeRequest{Jti: jtiOf(t, runToken)}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if _, err := fix.workflow.CancelRun(authedCtx(t, adminToken), &cairnosv1.CancelRunRequest{RunId: runID}); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	// Draining the stream now hits a send whose recheck fails, ending it with
	// UNAUTHENTICATED rather than a clean EOF.
	if code := drainUntilError(t, stream); code != codes.Unauthenticated {
		t.Fatalf("stream after mid-stream revoke ended with %v, want UNAUTHENTICATED", code)
	}
}

// waitForRunning reads a RunWorkflow stream until an action reports running,
// returning the run id.
func waitForRunning(t *testing.T, stream grpc.ServerStreamingClient[cairnosv1.RunEvent]) string {
	t.Helper()

	for {
		evt, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv while waiting for a running action: %v", err)
		}

		if evt.GetAction().GetStatus() == "running" {
			return evt.GetRunId()
		}
	}
}

// drainUntilError reads a stream to its first non-nil receive and returns the
// status code; a clean EOF would surface as codes.OK, which the recheck assertion
// rejects.
func drainUntilError(t *testing.T, stream grpc.ServerStreamingClient[cairnosv1.RunEvent]) codes.Code {
	t.Helper()

	for {
		if _, err := stream.Recv(); err != nil {
			return status.Code(err)
		}
	}
}
