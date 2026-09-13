package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/chadit/CairnOS/internal/api"
	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// authTestSecret meets the 32-byte HS256 floor.
const authTestSecret = "0123456789abcdef0123456789abcdef"

// hs256EnvName mirrors the variable the authenticator reads its secret
// from; the fixture serves it from a fake environment, never the process.
const hs256EnvName = "CAIRNOS_AUTH_HS256_SECRET" //nolint:gosec // environment variable name, not a credential

// authTTL keeps the fixture's expiry math obvious.
const authTTL = time.Hour

// bearerPrefix is the Authorization scheme under test.
const bearerPrefix = "Bearer "

// Canonical auth codes asserted on the wire.
const (
	wireUnauthenticated = "auth.unauthenticated"
	wireTokenExpired    = "auth.token_expired" //nolint:gosec // canonical error code name, not a credential
	wireForbidden       = "auth.forbidden"
)

// Shared literals the package's assertions repeat.
const (
	statusOK          = "ok"
	statusError       = "error"
	memoryScopeGlobal = "global"
)

// authClock is an advanceable clock injected into the fixture's
// authenticator.
type authClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now implements the auth.Config.Now contract.
func (c *authClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// Advance moves the clock forward.
func (c *authClock) Advance(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(step)
}

// authFixture is one running API server with token enforcement on, plus
// the handles tests need to mint keys and tokens.
type authFixture struct {
	workflow   cairnosv1.WorkflowServiceClient
	memory     cairnosv1.MemoryServiceClient
	admin      cairnosv1.AdminServiceClient
	login      cairnosv1.LoginServiceClient
	chat       cairnosv1.ChatServiceClient
	governance cairnosv1.GovernanceServiceClient
	conn       *grpc.ClientConn
	authStore  *auth.SQLite
	clock      *authClock
	httpBase   string
}

// startAuthServer boots a complete server with auth enabled: HS256
// material from a fake environment, the auth store sharing the run
// store's database file, and the secure-default auth config.
func startAuthServer(t *testing.T) *authFixture {
	t.Helper()

	return startAuthServerWith(t)
}

// startAuthServerWith is startAuthServer with extra gRPC services attached, the
// hook the principal cases use to register a service that hands a handler's own
// context back to the caller.
func startAuthServerWith(t *testing.T, extensions ...*api.Extension) *authFixture {
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
		Issuer:     "cairnos-test",
		Audience:   "cairnos",
		TTL:        authTTL,
		RequireExp: true,
		RequireJTI: true,
		Now:        clock.Now,
	})

	logger := slog.New(slog.DiscardHandler)
	manager := api.NewManager(logger)

	manager.Bind(engine.New(testEngineConfig(manager), testRegistry(t), db))

	// The chat agent rides on the auth fixture so the chat scope tests share the
	// token-minting plumbing. Its driver always proposes the hello workflow, so
	// a SendMessage raises the confirmation gate and ConfirmAction exercises the
	// confirm-time write:workflow check.
	chatAgent := chat.New(&chat.Config{
		Store:         db,
		Driver:        &scriptedChatDriver{turns: []runner.Result{runWorkflowTurn()}},
		Personalities: chat.NewPersonalities(nil, ""),
		Trigger:       api.NewChatWorkflowTrigger(manager, api.NewWorkflowResolver(workflowsDir, logger)),
		NewID:         sqlite.NewID,
	})

	server := api.NewServer(&api.Config{
		GRPCAddr:     loopbackEphemeral,
		HTTPAddr:     loopbackEphemeral,
		WorkflowsDir: workflowsDir,
		Store:        db,
		Memory:       mem,
		Registry:     testRegistry(t),
		Manager:      manager,
		Chat:         chatAgent,
		Auth:         authn,
		Extensions:   extensions,
		Logger:       logger,
	})

	if err := server.Start(t.Context()); err != nil {
		t.Fatalf("start server: %v", err)
	}

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), shutdownTimeout)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown server: %v", err)
		}
	})

	conn, err := grpc.NewClient(server.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial grpc: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return &authFixture{
		workflow:   cairnosv1.NewWorkflowServiceClient(conn),
		memory:     cairnosv1.NewMemoryServiceClient(conn),
		admin:      cairnosv1.NewAdminServiceClient(conn),
		login:      cairnosv1.NewLoginServiceClient(conn),
		chat:       cairnosv1.NewChatServiceClient(conn),
		governance: cairnosv1.NewGovernanceServiceClient(conn),
		conn:       conn,
		authStore:  authStore,
		clock:      clock,
		httpBase:   "http://" + server.HTTPAddr(),
	}
}

// seedKey mints and stores one enabled key with the given scopes in the
// default workspace.
func (f *authFixture) seedKey(t *testing.T, scopes []string) (*auth.Key, string) {
	t.Helper()

	return f.seedKeyInWorkspace(t, "", scopes)
}

// seedKeyInWorkspace mints and stores one enabled key with the given scopes in
// the named workspace, so a test can prove a token scoped to one workspace
// cannot see another's data.
func (f *authFixture) seedKeyInWorkspace(t *testing.T, workspace string, scopes []string) (*auth.Key, string) {
	t.Helper()

	key, plaintext, err := auth.NewKey("fixture", scopes, workspace, f.clock.Now())
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}

	if err := f.authStore.CreateKey(t.Context(), key); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	return key, plaintext
}

// token runs the full key-to-token exchange for one scope set.
func (f *authFixture) token(t *testing.T, scopes ...string) string {
	t.Helper()

	token, _ := f.tokenAndSubject(t, scopes...)

	return token
}

// tokenAndSubject is token plus the key id the token's subject claim carries,
// which is what a case asserting on the stamped principal has to compare with.
func (f *authFixture) tokenAndSubject(t *testing.T, scopes ...string) (string, string) {
	t.Helper()

	key, plaintext := f.seedKey(t, scopes)

	resp, err := f.login.Login(t.Context(), &cairnosv1.LoginRequest{Key: plaintext})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	return resp.GetToken(), key.ID
}

// authedCtx attaches a bearer token the way a native gRPC client would.
func authedCtx(t *testing.T, token string) context.Context {
	t.Helper()

	return metadata.AppendToOutgoingContext(t.Context(), "authorization", bearerPrefix+token)
}

// wrapRPC adds the operation name to a table-driven call's error; the
// status package unwraps wrapped errors when classifying, so assertions on
// status.Code still see the original.
func wrapRPC(op string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	return nil
}

// canonicalCode digs the canonical dotted code out of a gRPC error's
// status details.
func canonicalCode(t *testing.T, err error) string {
	t.Helper()

	for _, detail := range status.Convert(err).Details() {
		if canonical, isCanonical := detail.(*cairnosv1.Error); isCanonical {
			return canonical.GetCode()
		}
	}

	return ""
}

// jtiOf reads the jti claim straight out of a token's payload segment; the
// caller already holds the token, so naming it needs no verification.
func jtiOf(t *testing.T, token string) string {
	t.Helper()

	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		t.Fatalf("token has %d segments, want 3", len(segments))
	}

	payload, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatalf("decode token payload: %v", err)
	}

	var claims struct {
		JTI string `json:"jti"`
	}

	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal token payload: %v", err)
	}

	return claims.JTI
}

func TestLoginExchangesKeyForWorkingToken(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)
	token := fix.token(t, auth.ScopeReadRun)

	if _, err := fix.workflow.ListRuns(authedCtx(t, token), &cairnosv1.ListRunsRequest{}); err != nil {
		t.Fatalf("ListRuns with a fresh token: %v", err)
	}
}

func TestLoginRejectsBadKeys(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)

	disabledKey, disabledPlaintext := fix.seedKey(t, []string{auth.ScopeReadRun})
	if err := fix.authStore.SetKeyEnabled(t.Context(), disabledKey.ID, false); err != nil {
		t.Fatalf("disable key: %v", err)
	}

	cases := []struct {
		name string
		key  string
	}{
		{name: "garbage", key: "not-a-key"},
		{name: "unknown id", key: "ck_01UNKNOWN_c2VjcmV0"},
		{name: "disabled key", key: disabledPlaintext},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := fix.login.Login(t.Context(), &cairnosv1.LoginRequest{Key: tt.key})
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("Login error = %v, want UNAUTHENTICATED", err)
			}

			if got := canonicalCode(t, err); got != wireUnauthenticated {
				t.Fatalf("canonical code = %q, want %q", got, wireUnauthenticated)
			}
		})
	}
}

// TestMalformedHashLoginIsIndistinguishable is the L3 fix: a key whose stored
// hash does not parse must answer the same opaque UNAUTHENTICATED /
// auth.unauthenticated as a wrong secret, never leaking that the stored hash
// is the problem. The real detail goes to the server log, not the wire.
func TestMalformedHashLoginIsIndistinguishable(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)

	// A key with a syntactically broken stored hash: VerifyKey returns
	// ErrHashMalformed, which L3 routes into the credential-failure path.
	broken := &auth.Key{
		ID:        "01HASHBROKEN0000000000000",
		Label:     "broken-hash",
		Scopes:    []string{auth.ScopeReadRun},
		Hash:      "$argon2id$not-a-valid-phc-string",
		Enabled:   true,
		CreatedAt: fix.clock.Now(),
	}

	if err := fix.authStore.CreateKey(t.Context(), broken); err != nil {
		t.Fatalf("CreateKey with malformed hash: %v", err)
	}

	// A normal key supplies the wrong-secret baseline.
	goodKey, _ := fix.seedKey(t, []string{auth.ScopeReadRun})

	cases := []struct {
		name string
		key  string
	}{
		{name: "malformed stored hash", key: "ck_" + broken.ID + "_anysecretvalue"},
		{name: "wrong secret", key: "ck_" + goodKey.ID + "_wrongsecretvalue"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := fix.login.Login(t.Context(), &cairnosv1.LoginRequest{Key: tt.key})
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("Login error = %v, want UNAUTHENTICATED", err)
			}

			if got := canonicalCode(t, err); got != wireUnauthenticated {
				t.Fatalf("canonical code = %q, want %q (the malformed-hash answer must match a wrong key)", got, wireUnauthenticated)
			}

			if msg := status.Convert(err).Message(); msg != "invalid or disabled access key" {
				t.Fatalf("wire message = %q, want the single opaque credential message", msg)
			}
		})
	}
}

func TestLoginAnswersDisabledWhenAuthOff(t *testing.T) {
	t.Parallel()

	// The plain fixture serves without auth; Login must say so instead of
	// minting tokens nothing would check.
	fix := startServer(t)
	login := cairnosv1.NewLoginServiceClient(fix.conn)

	_, err := login.Login(t.Context(), &cairnosv1.LoginRequest{Key: "ck_x_y"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Login error = %v, want FAILED_PRECONDITION", err)
	}

	if got := canonicalCode(t, err); got != "auth.disabled" {
		t.Fatalf("canonical code = %q, want auth.disabled", got)
	}
}

func TestHealthNeedsNoTokenWithAuthOn(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)

	resp, err := fix.admin.Health(t.Context(), &cairnosv1.HealthRequest{})
	if err != nil {
		t.Fatalf("tokenless Health with auth on: %v", err)
	}

	if resp.GetStatus() != statusOK {
		t.Fatalf("health status = %q, want ok", resp.GetStatus())
	}
}

// TestReadMemoryTokenCannotRunWorkflow is the spec's acceptance bullet
// verbatim: a read:memory-only token must not start a workflow, while the
// scope it does hold keeps working.
func TestReadMemoryTokenCannotRunWorkflow(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)
	token := fix.token(t, auth.ScopeReadMemory)

	stream, err := fix.workflow.RunWorkflow(authedCtx(t, token), &cairnosv1.RunWorkflowRequest{WorkflowName: helloName})
	if err != nil {
		t.Fatalf("open RunWorkflow stream: %v", err)
	}

	_, err = stream.Recv()
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("RunWorkflow error = %v, want PERMISSION_DENIED", err)
	}

	if got := canonicalCode(t, err); got != wireForbidden {
		t.Fatalf("canonical code = %q, want %q", got, wireForbidden)
	}

	if _, err := fix.memory.SearchMemory(authedCtx(t, token), &cairnosv1.SearchMemoryRequest{Text: "x"}); err != nil {
		t.Fatalf("SearchMemory within scope: %v", err)
	}
}

func TestScopeEnforcementPerMethod(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)
	readMemory := fix.token(t, auth.ScopeReadMemory)
	admin := fix.token(t, auth.ScopeAdmin)

	cases := []struct {
		name     string
		token    string
		call     func(ctx context.Context) error
		wantCode codes.Code
	}{
		{
			name:  "read scope cannot write memory",
			token: readMemory,
			call: func(ctx context.Context) error {
				_, err := fix.memory.AddMemory(ctx, &cairnosv1.AddMemoryRequest{
					Entry: &cairnosv1.MemoryEntry{Scope: memoryScopeGlobal, Content: "x"},
				})

				return wrapRPC("AddMemory", err)
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:  "read memory cannot read runs",
			token: readMemory,
			call: func(ctx context.Context) error {
				_, err := fix.workflow.ListRuns(ctx, &cairnosv1.ListRunsRequest{})

				return wrapRPC("ListRuns", err)
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:  "non-admin cannot revoke",
			token: readMemory,
			call: func(ctx context.Context) error {
				_, err := fix.login.Revoke(ctx, &cairnosv1.RevokeRequest{Jti: "x"})

				return wrapRPC("Revoke", err)
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:  "read scope cannot replay a run",
			token: readMemory,
			call: func(ctx context.Context) error {
				stream, err := fix.workflow.ReplayRun(ctx, &cairnosv1.ReplayRunRequest{RunId: "x"})
				if err != nil {
					return wrapRPC("ReplayRun", err)
				}

				_, err = stream.Recv()

				return wrapRPC("ReplayRun", err)
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:  "read scope cannot save a workflow",
			token: readMemory,
			call: func(ctx context.Context) error {
				_, err := fix.workflow.SaveWorkflow(ctx, &cairnosv1.SaveWorkflowRequest{
					WorkflowName: helloName,
					Toml:         []byte(helloTOML),
				})

				return wrapRPC("SaveWorkflow", err)
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name:  "admin implies every scope",
			token: admin,
			call: func(ctx context.Context) error {
				_, err := fix.workflow.ListRuns(ctx, &cairnosv1.ListRunsRequest{})

				return wrapRPC("ListRuns", err)
			},
			wantCode: codes.OK,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.call(authedCtx(t, tt.token))
			if status.Code(err) != tt.wantCode {
				t.Fatalf("status = %v (err %v), want %v", status.Code(err), err, tt.wantCode)
			}
		})
	}
}

func TestMissingAndMalformedTokensRejected(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)

	cases := []struct {
		name   string
		header string
	}{
		{name: "no header", header: ""},
		{name: "wrong scheme", header: "Basic dXNlcjpwYXNz"},
		{name: "garbage bearer", header: bearerPrefix + "not-a-token"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			if tt.header != "" {
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", tt.header)
			}

			_, err := fix.workflow.ListRuns(ctx, &cairnosv1.ListRunsRequest{})
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("ListRuns error = %v, want UNAUTHENTICATED", err)
			}

			if got := canonicalCode(t, err); got != wireUnauthenticated {
				t.Fatalf("canonical code = %q, want %q", got, wireUnauthenticated)
			}
		})
	}
}

func TestExpiredTokenRejectedWithItsOwnCode(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)
	token := fix.token(t, auth.ScopeReadRun)

	fix.clock.Advance(authTTL + time.Minute)

	_, err := fix.workflow.ListRuns(authedCtx(t, token), &cairnosv1.ListRunsRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ListRuns error = %v, want UNAUTHENTICATED", err)
	}

	if got := canonicalCode(t, err); got != wireTokenExpired {
		t.Fatalf("canonical code = %q, want %q", got, wireTokenExpired)
	}
}

// TestLoginRunRevokeFlow is the full Phase 4a integration: login, run a
// workflow with the token, revoke it, and watch the very next call fail
// before exp (.agents/spec/08-auth-and-access.md, Change 1).
func TestLoginRunRevokeFlow(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)
	runToken := fix.token(t, auth.ScopeWriteWorkflow)
	adminToken := fix.token(t, auth.ScopeAdmin)

	stream, err := fix.workflow.RunWorkflow(authedCtx(t, runToken), &cairnosv1.RunWorkflowRequest{WorkflowName: helloName})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	events := collectEvents(t, stream)
	if len(events) == 0 {
		t.Fatal("authed RunWorkflow produced no events")
	}

	resp, err := fix.login.Revoke(authedCtx(t, adminToken), &cairnosv1.RevokeRequest{Jti: jtiOf(t, runToken)})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if !strings.HasPrefix(resp.GetDenied(), "jti:") {
		t.Fatalf("denied = %q, want a jti: echo", resp.GetDenied())
	}

	followUp, err := fix.workflow.RunWorkflow(authedCtx(t, runToken), &cairnosv1.RunWorkflowRequest{WorkflowName: helloName})
	if err != nil {
		t.Fatalf("open post-revocation stream: %v", err)
	}

	if _, err := followUp.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("post-revocation call = %v, want UNAUTHENTICATED", err)
	}
}

func TestRevokeByKeyKillsLiveTokens(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)
	adminToken := fix.token(t, auth.ScopeAdmin)

	key, plaintext := fix.seedKey(t, []string{auth.ScopeReadRun})

	loginResp, err := fix.login.Login(t.Context(), &cairnosv1.LoginRequest{Key: plaintext})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	victim := loginResp.GetToken()

	if _, err := fix.workflow.ListRuns(authedCtx(t, victim), &cairnosv1.ListRunsRequest{}); err != nil {
		t.Fatalf("ListRuns before revocation: %v", err)
	}

	if _, err := fix.login.Revoke(authedCtx(t, adminToken), &cairnosv1.RevokeRequest{KeyId: key.ID}); err != nil {
		t.Fatalf("Revoke by key: %v", err)
	}

	_, err = fix.workflow.ListRuns(authedCtx(t, victim), &cairnosv1.ListRunsRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ListRuns after key revocation = %v, want UNAUTHENTICATED", err)
	}
}

func TestRevokeValidatesItsTarget(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)
	adminToken := fix.token(t, auth.ScopeAdmin)

	cases := []struct {
		name string
		req  *cairnosv1.RevokeRequest
	}{
		{name: "neither target", req: &cairnosv1.RevokeRequest{}},
		{name: "both targets", req: &cairnosv1.RevokeRequest{Jti: "a", KeyId: "b"}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := fix.login.Revoke(authedCtx(t, adminToken), tt.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("Revoke error = %v, want INVALID_ARGUMENT", err)
			}
		})
	}
}

// httpDo sends one gateway request with an optional bearer token and
// returns status code plus body.
func httpDo(t *testing.T, method, url, body, token string) (int, []byte) {
	t.Helper()

	var reader io.Reader = http.NoBody

	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(t.Context(), method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set("Content-Type", jsonContentType)

	if token != "" {
		req.Header.Set("Authorization", bearerPrefix+token)
	}

	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return resp.StatusCode, payload
}

// envelopeCode decodes the canonical HTTP error envelope's code.
func envelopeCode(t *testing.T, body []byte) string {
	t.Helper()

	var envelope struct {
		Status string `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope %q: %v", body, err)
	}

	return envelope.Error.Code
}

// TestGatewayBearerFlow proves the HTTP face inherits enforcement through
// the gateway: login over HTTP, the forwarded Authorization header reaches
// the interceptor, and failures keep the canonical envelope shape.
func TestGatewayBearerFlow(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)
	_, plaintext := fix.seedKey(t, []string{auth.ScopeReadRun})

	// Health stays open over HTTP too.
	healthStatus, _ := httpDo(t, http.MethodGet, fix.httpBase+"/v1/health", "", "")
	if healthStatus != http.StatusOK {
		t.Fatalf("tokenless /v1/health = %d, want 200", healthStatus)
	}

	// A bad key answers 401 with the canonical envelope.
	badStatus, badBody := httpDo(t, http.MethodPost, fix.httpBase+"/v1/login", `{"key":"nope"}`, "")
	if badStatus != http.StatusUnauthorized {
		t.Fatalf("bad login = %d, want 401", badStatus)
	}

	if got := envelopeCode(t, badBody); got != wireUnauthenticated {
		t.Fatalf("bad login envelope code = %q, want %q", got, wireUnauthenticated)
	}

	// The real key logs in and the token works on a scoped route.
	loginBody, err := json.Marshal(map[string]string{"key": plaintext})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}

	okStatus, okBody := httpDo(t, http.MethodPost, fix.httpBase+"/v1/login", string(loginBody), "")
	if okStatus != http.StatusOK {
		t.Fatalf("login = %d (%s), want 200", okStatus, okBody)
	}

	var loginResp struct {
		Token string `json:"token"`
	}

	if err := json.Unmarshal(okBody, &loginResp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}

	runsStatus, _ := httpDo(t, http.MethodGet, fix.httpBase+"/v1/runs", "", loginResp.Token)
	if runsStatus != http.StatusOK {
		t.Fatalf("authed /v1/runs = %d, want 200", runsStatus)
	}

	// Without the header the same route answers 401.
	noneStatus, noneBody := httpDo(t, http.MethodGet, fix.httpBase+"/v1/runs", "", "")
	if noneStatus != http.StatusUnauthorized {
		t.Fatalf("tokenless /v1/runs = %d, want 401", noneStatus)
	}

	if got := envelopeCode(t, noneBody); got != wireUnauthenticated {
		t.Fatalf("tokenless envelope code = %q, want %q", got, wireUnauthenticated)
	}

	// A scope the token does not hold answers 403 with auth.forbidden.
	cancelStatus, cancelBody := httpDo(t, http.MethodPost, fix.httpBase+"/v1/runs/none/cancel", "{}", loginResp.Token)
	if cancelStatus != http.StatusForbidden {
		t.Fatalf("out-of-scope cancel = %d, want 403", cancelStatus)
	}

	if got := envelopeCode(t, cancelBody); got != wireForbidden {
		t.Fatalf("out-of-scope envelope code = %q, want %q", got, wireForbidden)
	}
}

// TestNetworkBindPolicy walks the guard matrix on real listeners: 0.0.0.0
// is bindable everywhere and is not loopback, so it stands in for any
// network interface.
func TestNetworkBindPolicy(t *testing.T) {
	t.Parallel()

	const allInterfaces = "0.0.0.0:0"

	cases := []struct {
		name     string
		policy   api.BindPolicy
		wantErr  error
		wantWarn bool
	}{
		{
			name:    "auth off refused",
			policy:  api.BindPolicy{},
			wantErr: api.ErrNetworkBind,
		},
		{
			name:    "auth on without TLS refused",
			policy:  api.BindPolicy{AuthEnabled: true},
			wantErr: api.ErrPlaintextNetworkBind,
		},
		{
			name:   "auth on behind TLS proxy allowed",
			policy: api.BindPolicy{AuthEnabled: true, TLSTerminated: true},
		},
		{
			name:     "open serve flag allowed with loud warning",
			policy:   api.BindPolicy{ServeWithoutAuthOnNetwork: true},
			wantWarn: true,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logBuf bytes.Buffer

			logger := slog.New(slog.NewTextHandler(&logBuf, nil))
			server := newGuardServer(t, allInterfaces, allInterfaces, tt.policy, logger)

			err := server.Start(t.Context())

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Start error = %v, want %v", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("Start: %v", err)
			}

			t.Cleanup(func() {
				shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), shutdownTimeout)
				defer cancel()

				_ = server.Shutdown(shutdownCtx)
			})

			gotWarn := strings.Contains(logBuf.String(), "WITHOUT AUTHENTICATION")
			if gotWarn != tt.wantWarn {
				t.Fatalf("warning logged = %t, want %t (log: %s)", gotWarn, tt.wantWarn, logBuf.String())
			}
		})
	}
}

// tokenInWorkspace mints a key in the named workspace and exchanges it for a
// token, so a call made with the result acts in that workspace.
func (f *authFixture) tokenInWorkspace(t *testing.T, workspace string, scopes ...string) string {
	t.Helper()

	_, plaintext := f.seedKeyInWorkspace(t, workspace, scopes)

	resp, err := f.login.Login(t.Context(), &cairnosv1.LoginRequest{Key: plaintext})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	return resp.GetToken()
}

// TestWorkspaceIsolatesRunsAcrossTokens is the API-level multi-tenancy
// acceptance: a token scoped to workspace A runs a workflow, and a token
// scoped to workspace B can neither list nor fetch that run
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"). The workspace
// rides the token claim, the interceptor lifts it onto the context, and the
// store filters by it.
func TestWorkspaceIsolatesRunsAcrossTokens(t *testing.T) {
	t.Parallel()

	const (
		workspaceAcme   = "acme"
		workspaceGlobex = "globex"
	)

	fix := startAuthServer(t)

	// Admin scope so each token can both run a workflow and read run history.
	acmeToken := fix.tokenInWorkspace(t, workspaceAcme, auth.ScopeAdmin)
	globexToken := fix.tokenInWorkspace(t, workspaceGlobex, auth.ScopeAdmin)

	// Acme runs a workflow; the run lands in the acme workspace.
	stream, err := fix.workflow.RunWorkflow(authedCtx(t, acmeToken),
		&cairnosv1.RunWorkflowRequest{WorkflowName: helloName})
	if err != nil {
		t.Fatalf("acme RunWorkflow: %v", err)
	}

	events := collectEvents(t, stream)
	if len(events) == 0 {
		t.Fatal("acme RunWorkflow produced no events")
	}

	acmeRunID := events[0].GetRunId()

	// Acme sees its run.
	acmeList, err := fix.workflow.ListRuns(authedCtx(t, acmeToken), &cairnosv1.ListRunsRequest{})
	if err != nil {
		t.Fatalf("acme ListRuns: %v", err)
	}

	if len(acmeList.GetRuns()) != 1 || acmeList.GetRuns()[0].GetRunId() != acmeRunID {
		t.Fatalf("acme ListRuns = %v, want only %s", acmeList.GetRuns(), acmeRunID)
	}

	// Globex's list is empty; acme's run is invisible across the boundary.
	globexList, err := fix.workflow.ListRuns(authedCtx(t, globexToken), &cairnosv1.ListRunsRequest{})
	if err != nil {
		t.Fatalf("globex ListRuns: %v", err)
	}

	if len(globexList.GetRuns()) != 0 {
		t.Fatalf("globex saw %d runs, want 0; acme's run leaked across the workspace boundary", len(globexList.GetRuns()))
	}

	// Globex cannot fetch acme's run by id; it reads as not found.
	_, err = fix.workflow.GetRun(authedCtx(t, globexToken), &cairnosv1.GetRunRequest{RunId: acmeRunID})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("globex GetRun(acme run) code = %v, want NotFound", status.Code(err))
	}
}
