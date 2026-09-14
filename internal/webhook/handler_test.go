package webhook_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/secrets"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/webhook"
)

// Shared test constants keep repeated literals in one place and out of goconst's
// way.
const (
	defaultRegID = "reg-1"
	defaultRunID = "run-777"
	workflowName = "wf"
	workflowPath = "/wf.toml"
	secretName   = "hook-secret"
	secretValue  = "s3cr3t-value"
	prodPrefix   = "/v1/hooks/"
	testPrefix   = "/v1/hooks/test/"
	statusOK     = "ok"
	statusError  = "error"
	sampleJSON   = `{"x":1}`

	hdrGitHubSig      = "X-Hub-Signature-256"
	hdrGitHubDelivery = "X-Github-Delivery"
	hdrNativeSig      = "X-Cairnos-Signature-256"
	hdrNativeTS       = "X-Cairnos-Timestamp"
	hdrNativeNonce    = "X-Cairnos-Nonce"
)

// syncBuffer is a concurrency-safe log sink, so the background drain goroutine
// and the test's log assertions never race on the underlying buffer.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (s *syncBuffer) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	written, _ := s.buf.Write(data)

	return written, nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.String()
}

// frozen is an injectable clock the limiters, replay cache, and native-signature
// timestamp check all read, so rate windows and freshness stay repeatable
// without sleeping.
type frozen struct {
	at time.Time
	mu sync.Mutex
}

func (f *frozen) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.at
}

func (f *frozen) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.at = f.at.Add(d)
}

// fakeRun is the terminal run record the test endpoint marshals back.
type fakeRun struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// storeEntry is one hash-to-registration mapping in the fake store.
type storeEntry struct {
	reg      *webhook.Registration
	endpoint webhook.Endpoint
}

// fakeStore answers GetByTokenHash from an in-memory hash map; the other Store
// methods are unused by the ingress handler and satisfy the interface as no-ops.
type fakeStore struct {
	byHash map[string]storeEntry
}

func newFakeStore() *fakeStore {
	return &fakeStore{byHash: map[string]storeEntry{}}
}

func (s *fakeStore) GetByTokenHash(_ context.Context, hash string) (*webhook.Registration, webhook.Endpoint, error) {
	entry, ok := s.byHash[hash]
	if !ok {
		return nil, webhook.EndpointProd, webhook.ErrNotFound
	}

	return entry.reg, entry.endpoint, nil
}

func (*fakeStore) Add(context.Context, *webhook.Registration) error { return nil }

func (*fakeStore) GetByWorkflow(context.Context, string) (*webhook.Registration, error) {
	return nil, webhook.ErrNotFound
}

func (*fakeStore) List(context.Context) ([]*webhook.Registration, error) { return nil, nil }

func (*fakeStore) UpdateTokenHash(context.Context, string, webhook.Endpoint, string) error {
	return nil
}

func (*fakeStore) Remove(context.Context, string) error { return nil }

func (*fakeStore) Close() error { return nil }

func (s *fakeStore) putHash(hash string, reg *webhook.Registration, endpoint webhook.Endpoint) {
	s.byHash[hash] = storeEntry{reg: reg, endpoint: endpoint}
}

// fakeRunStarter records the last StartRun call and hands back a controllable
// outcome. block leaves the outcome undelivered (a slow run); deliver pushes a
// terminal result to unblock the drain.
type fakeRunStarter struct {
	result        webhook.RunResult
	startErr      error
	lastInputs    map[string]any
	lastOutcome   chan webhook.RunResult
	lastWorkspace string
	lastTrigger   string
	runID         string
	calls         int
	mu            sync.Mutex
	block         bool
}

func (f *fakeRunStarter) StartRun(ctx context.Context, _ string, inputs map[string]any, trigger string) (webhook.StartedRun, error) {
	f.mu.Lock()
	f.calls++
	f.lastWorkspace = store.WorkspaceFromContext(ctx)
	f.lastInputs = inputs
	f.lastTrigger = trigger
	outcome := make(chan webhook.RunResult, 1)
	f.lastOutcome = outcome
	block := f.block
	result := f.result
	runID := f.runID
	startErr := f.startErr
	f.mu.Unlock()

	if startErr != nil {
		return webhook.StartedRun{}, startErr
	}

	if !block {
		outcome <- result
	}

	return webhook.StartedRun{RunID: runID, Outcome: outcome}, nil
}

func (f *fakeRunStarter) deliver(result webhook.RunResult) {
	f.mu.Lock()
	outcome := f.lastOutcome
	f.mu.Unlock()

	outcome <- result
}

func (f *fakeRunStarter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

func (f *fakeRunStarter) startedWorkspace() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.lastWorkspace
}

func (f *fakeRunStarter) startedInputs() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.lastInputs
}

func (f *fakeRunStarter) startedTrigger() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.lastTrigger
}

// fakePauseChecker answers the pause gate.
type fakePauseChecker struct {
	err    error
	paused bool
}

func (f *fakePauseChecker) IsPaused(context.Context, string) (bool, error) {
	return f.paused, f.err
}

// fakeSecrets resolves the HMAC secret or fails closed.
type fakeSecrets struct {
	err    error
	secret string
}

func (f *fakeSecrets) Get(context.Context, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}

	return f.secret, nil
}

// testRig groups the ingress handler under test with its fakes and log sink.
type testRig struct {
	store   *fakeStore
	runs    *fakeRunStarter
	pauses  *fakePauseChecker
	secrets *fakeSecrets
	mux     *http.ServeMux
	logs    *syncBuffer
	clock   *frozen
}

// newRig builds a handler over fakes, applying mutate to the config before
// construction so a test can tighten the body cap, the rates, or the timings.
func newRig(t *testing.T, mutate func(*webhook.HandlerConfig)) *testRig {
	t.Helper()

	logs := &syncBuffer{}
	clock := &frozen{at: time.Now()}
	backing := newFakeStore()
	runs := &fakeRunStarter{
		runID:  defaultRunID,
		result: webhook.RunResult{Run: fakeRun{ID: defaultRunID, Status: "succeeded"}},
	}
	pauses := &fakePauseChecker{}
	resolver := &fakeSecrets{secret: secretValue}

	cfg := &webhook.HandlerConfig{
		Store:                backing,
		Runs:                 runs,
		Pauses:               pauses,
		Secrets:              resolver,
		MaxBodyBytes:         1 << 20,
		RatePerMinute:        60,
		UnknownRatePerMinute: 120,
		TestWait:             30 * time.Second,
		SignatureSkew:        5 * time.Minute,
		Now:                  clock.now,
		Logger:               slog.New(slog.NewTextHandler(logs, nil)),
	}

	if mutate != nil {
		mutate(cfg)
	}

	handler := webhook.NewHandler(cfg)

	mux := http.NewServeMux()
	for pattern, fn := range handler.Routes() {
		mux.HandleFunc(pattern, fn)
	}

	return &testRig{
		store:   backing,
		runs:    runs,
		pauses:  pauses,
		secrets: resolver,
		mux:     mux,
		logs:    logs,
		clock:   clock,
	}
}

// post drives one POST through the routed mux and returns the recorder.
func (env *testRig) post(t *testing.T, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(body))
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	return rec
}

// register mints a fresh pair of tokens, stores their hashes, and returns the
// two raw tokens. mutate customizes the registration before it is stored.
func register(t *testing.T, env *testRig, mutate func(*webhook.Registration)) (string, string) {
	t.Helper()

	prodToken, prodHash := mint(t)
	testToken, testHash := mint(t)

	reg := &webhook.Registration{
		ID:            defaultRegID,
		WorkflowName:  workflowName,
		WorkflowPath:  workflowPath,
		ProdTokenHash: prodHash,
		TestTokenHash: testHash,
	}

	if mutate != nil {
		mutate(reg)
	}

	env.store.putHash(prodHash, reg, webhook.EndpointProd)
	env.store.putHash(testHash, reg, webhook.EndpointTest)

	return prodToken, testToken
}

func mint(t *testing.T) (string, string) {
	t.Helper()

	token, hash, err := webhook.MintToken()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	return token, hash
}

// errBody and okBody decode the two response envelope shapes.
type errBody struct {
	Error struct {
		Details map[string]any `json:"details"`
		Code    string         `json:"code"`
		Message string         `json:"message"`
	} `json:"error"`
	Status string `json:"status"`
}

type okBody struct {
	Status string `json:"status"`
	RunID  string `json:"run_id"`
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errBody {
	t.Helper()

	var body errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope %q: %v", rec.Body.String(), err)
	}

	return body
}

func decodeOK(t *testing.T, rec *httptest.ResponseRecorder) okBody {
	t.Helper()

	var body okBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode ok envelope %q: %v", rec.Body.String(), err)
	}

	return body
}

// githubSignature signs the body alone under the test secret in the GitHub
// sha256=<hex> form.
func githubSignature(body []byte) string {
	mac := hmac.New(sha256.New, []byte(secretValue))
	mac.Write(body)

	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// nativeSignature signs timestamp + nonce + body under the test secret in the
// native bare-hex form.
func nativeSignature(timestamp, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secretValue))
	mac.Write([]byte(timestamp + "\n" + nonce + "\n" + string(body)))

	return hex.EncodeToString(mac.Sum(nil))
}

// timestampNow formats the injected clock as unix seconds for a native
// signature.
func timestampNow(env *testRig) string {
	return strconv.FormatInt(env.clock.now().Unix(), 10)
}

// TestUnknownTokenReturns404 proves an unregistered but well-formed token is a
// 404 in the canonical envelope and starts no run.
func TestUnknownTokenReturns404(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	token, _ := mint(t)

	rec := env.post(t, prodPrefix+token, []byte(sampleJSON), nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	if got := decodeErr(t, rec).Error.Code; got != webhook.CodeUnknownToken {
		t.Errorf("code = %q, want %q", got, webhook.CodeUnknownToken)
	}

	if calls := env.runs.callCount(); calls != 0 {
		t.Errorf("StartRun calls = %d, want 0 for an unknown token", calls)
	}
}

// TestMalformedTokenReturns404 proves a token that is not 64 hex chars is
// rejected before any lookup or body read.
func TestMalformedTokenReturns404(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)

	rec := env.post(t, prodPrefix+"not-a-64-hex-token", []byte(sampleJSON), nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	if got := decodeErr(t, rec).Error.Code; got != webhook.CodeUnknownToken {
		t.Errorf("code = %q, want %q", got, webhook.CodeUnknownToken)
	}
}

// TestEndpointMismatchReturns404 proves a production token used on the test
// route (and the reverse) reads as unknown, so a leaked test token never opens
// the production path.
func TestEndpointMismatchReturns404(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken, testToken := register(t, env, nil)

	prodOnTest := env.post(t, testPrefix+prodToken, []byte(sampleJSON), nil)
	if prodOnTest.Code != http.StatusNotFound {
		t.Errorf("prod token on test route: status = %d, want 404", prodOnTest.Code)
	}

	testOnProd := env.post(t, prodPrefix+testToken, []byte(sampleJSON), nil)
	if testOnProd.Code != http.StatusNotFound {
		t.Errorf("test token on prod route: status = %d, want 404", testOnProd.Code)
	}

	if calls := env.runs.callCount(); calls != 0 {
		t.Errorf("StartRun calls = %d, want 0 on endpoint mismatch", calls)
	}
}

// TestBodyTooLargeReturns413 proves the handler caps its own body even though
// extension routes bypass the gateway's limitBody wrapper.
func TestBodyTooLargeReturns413(t *testing.T) {
	t.Parallel()

	env := newRig(t, func(cfg *webhook.HandlerConfig) { cfg.MaxBodyBytes = 16 })
	prodToken, _ := register(t, env, nil)

	rec := env.post(t, prodPrefix+prodToken, bytes.Repeat([]byte("a"), 128), nil)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}

	if got := decodeErr(t, rec).Error.Code; got != webhook.CodeBodyTooLarge {
		t.Errorf("code = %q, want %q", got, webhook.CodeBodyTooLarge)
	}
}

// TestBadPayloadReturns400 proves a non-object body in default mode is a 400.
func TestBadPayloadReturns400(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken, _ := register(t, env, nil)

	rec := env.post(t, prodPrefix+prodToken, []byte(`[1,2,3]`), nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	if got := decodeErr(t, rec).Error.Code; got != webhook.CodeBadPayload {
		t.Errorf("code = %q, want %q", got, webhook.CodeBadPayload)
	}
}

// TestWorkflowInvalidPassesThrough proves the engine's workflow.invalid error
// surfaces as a 400 whose message names the offending keys.
func TestWorkflowInvalidPassesThrough(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken, _ := register(t, env, nil)
	env.runs.startErr = cairnerr.Errors{
		cairnerr.Newf("workflow.invalid", "unknown input %q", "surprise"),
		cairnerr.Newf("workflow.invalid", "required input %q was not supplied", "needed"),
	}

	rec := env.post(t, prodPrefix+prodToken, []byte(sampleJSON), nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	body := decodeErr(t, rec)
	if body.Error.Code != "workflow.invalid" {
		t.Errorf("code = %q, want workflow.invalid", body.Error.Code)
	}

	for _, key := range []string{"surprise", "needed"} {
		if !strings.Contains(body.Error.Message, key) {
			t.Errorf("message %q does not name offending key %q", body.Error.Message, key)
		}
	}
}

// TestPayloadInputRawMode proves raw mode delivers the exact body bytes into the
// one declared input.
func TestPayloadInputRawMode(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken, _ := register(t, env, func(reg *webhook.Registration) { reg.PayloadInput = "raw" })

	raw := []byte(`this is not { valid json`)

	rec := env.post(t, prodPrefix+prodToken, raw, nil)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	inputs := env.runs.startedInputs()
	if got, ok := inputs["raw"].(string); !ok || got != string(raw) {
		t.Errorf("inputs[raw] = %v, want exact body %q", inputs["raw"], string(raw))
	}
}

// TestPausedReturns409 proves a paused workflow answers 409 and starts no run.
func TestPausedReturns409(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken, _ := register(t, env, nil)
	env.pauses.paused = true

	rec := env.post(t, prodPrefix+prodToken, []byte(sampleJSON), nil)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}

	if got := decodeErr(t, rec).Error.Code; got != webhook.CodePaused {
		t.Errorf("code = %q, want %q", got, webhook.CodePaused)
	}

	if calls := env.runs.callCount(); calls != 0 {
		t.Errorf("StartRun calls = %d, want 0 for a paused workflow", calls)
	}
}

// TestProductionReturns202 proves the production path answers 202 with the run
// id and stamps trigger "webhook".
func TestProductionReturns202(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken, _ := register(t, env, nil)

	rec := env.post(t, prodPrefix+prodToken, []byte(sampleJSON), nil)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	body := decodeOK(t, rec)
	if body.Status != statusOK || body.RunID != defaultRunID {
		t.Errorf("body = %+v, want status ok and run_id %q", body, defaultRunID)
	}

	if calls := env.runs.callCount(); calls != 1 {
		t.Errorf("StartRun calls = %d, want 1", calls)
	}

	if trigger := env.runs.startedTrigger(); trigger != "webhook" {
		t.Errorf("trigger = %q, want webhook", trigger)
	}
}

// TestWebhookRunStampsDefaultWorkspace proves the scope-bypass invariant: a
// webhook run executes under store.DefaultWorkspace regardless of the request
// context (security amendment 5).
func TestWebhookRunStampsDefaultWorkspace(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken, _ := register(t, env, nil)

	env.post(t, prodPrefix+prodToken, []byte(sampleJSON), nil)

	if env.runs.callCount() != 1 {
		t.Fatal("StartRun was not called")
	}

	if got := env.runs.startedWorkspace(); got != store.DefaultWorkspace {
		t.Errorf("run workspace = %q, want %q", got, store.DefaultWorkspace)
	}
}

// TestTestEndpointReturnsTerminalRun proves the test endpoint returns 200 with
// the terminal run when it finishes inside the wait, stamping trigger
// "webhook_test".
func TestTestEndpointReturnsTerminalRun(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	_, testToken := register(t, env, nil)

	rec := env.post(t, testPrefix+testToken, []byte(sampleJSON), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), defaultRunID) {
		t.Errorf("body %q does not carry the terminal run id", rec.Body.String())
	}

	if trigger := env.runs.startedTrigger(); trigger != "webhook_test" {
		t.Errorf("trigger = %q, want webhook_test", trigger)
	}
}

// TestTestEndpointTimesOutTo202 proves a slow run does not pin the connection:
// once the short wait elapses, the handler answers 202 with the run id and the
// run continues in the background.
func TestTestEndpointTimesOutTo202(t *testing.T) {
	t.Parallel()

	env := newRig(t, func(cfg *webhook.HandlerConfig) { cfg.TestWait = 20 * time.Millisecond })
	_, testToken := register(t, env, nil)
	env.runs.block = true

	start := time.Now()
	rec := env.post(t, testPrefix+testToken, []byte(sampleJSON), nil)
	elapsed := time.Since(start)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	if body := decodeOK(t, rec); body.RunID != defaultRunID {
		t.Errorf("run_id = %q, want %q", body.RunID, defaultRunID)
	}

	if elapsed > time.Second {
		t.Errorf("handler held the connection %s, well past the 20ms budget", elapsed)
	}

	// Unblock the background drain so it does not linger.
	env.runs.deliver(webhook.RunResult{Run: fakeRun{ID: defaultRunID, Status: "succeeded"}})
}

// TestSecretUnavailableReturns503 proves an unresolvable HMAC secret ref fails
// closed with a 503 and starts no run.
func TestSecretUnavailableReturns503(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken, _ := register(t, env, func(reg *webhook.Registration) { reg.HMACSecretRef = secretName })
	env.secrets.err = secrets.ErrNotFound

	body := []byte(sampleJSON)
	headers := map[string]string{
		hdrGitHubSig:      githubSignature(body),
		hdrGitHubDelivery: "d1",
	}

	rec := env.post(t, prodPrefix+prodToken, body, headers)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	if got := decodeErr(t, rec).Error.Code; got != webhook.CodeSecretUnavailable {
		t.Errorf("code = %q, want %q", got, webhook.CodeSecretUnavailable)
	}

	if calls := env.runs.callCount(); calls != 0 {
		t.Errorf("StartRun calls = %d, want 0 when the secret is unresolvable", calls)
	}
}

// TestHMACUsesConstantTimeCompare is the static check the acceptance criteria
// pin: the digest compare goes through hmac.Equal, never == or bytes.Equal on
// MAC values.
func TestHMACUsesConstantTimeCompare(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile("handler.go")
	if err != nil {
		t.Fatalf("read handler source: %v", err)
	}

	text := string(source)
	if !strings.Contains(text, "hmac.Equal(") {
		t.Error("handler must compare MAC digests with hmac.Equal")
	}

	if strings.Contains(text, "bytes.Equal(") {
		t.Error("handler must not compare MAC values with bytes.Equal")
	}
}

// assertNoTokenLeak fails when the captured logs carry the raw token or the
// request path (which embeds the token).
func assertNoTokenLeak(t *testing.T, logs, token, path string) {
	t.Helper()

	if strings.Contains(logs, token) {
		t.Errorf("logs leaked the raw token; logs = %q", logs)
	}

	if strings.Contains(logs, path) {
		t.Errorf("logs leaked the request path; logs = %q", logs)
	}
}

// TestHandlerNeverLogsTokenOrPath drives every listed status (404, 429, 401,
// 503, 400, 409, 202) and proves no log line carries the raw path or token
// (security amendment 6). Known-registration statuses also carry the
// registration ID.
func TestHandlerNeverLogsTokenOrPath(t *testing.T) {
	t.Parallel()

	t.Run("unknown_token_404", func(t *testing.T) {
		t.Parallel()

		env := newRig(t, nil)
		token, _ := mint(t)
		path := prodPrefix + token

		rec := env.post(t, path, []byte(sampleJSON), nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}

		assertNoTokenLeak(t, env.logs.String(), token, path)
	})

	t.Run("rate_limited_429", func(t *testing.T) {
		t.Parallel()

		env := newRig(t, nil)
		prodToken, _ := register(t, env, func(reg *webhook.Registration) { reg.RatePerMinute = 1 })
		path := prodPrefix + prodToken

		env.post(t, path, []byte(sampleJSON), nil)

		rec := env.post(t, path, []byte(sampleJSON), nil)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", rec.Code)
		}

		logs := env.logs.String()
		assertNoTokenLeak(t, logs, prodToken, path)

		if !strings.Contains(logs, defaultRegID) {
			t.Errorf("known-registration logs missing registration id; logs = %q", logs)
		}
	})

	t.Run("bad_signature_401", func(t *testing.T) {
		t.Parallel()

		env := newRig(t, nil)
		prodToken, _ := register(t, env, func(reg *webhook.Registration) { reg.HMACSecretRef = secretName })
		path := prodPrefix + prodToken
		headers := map[string]string{hdrGitHubSig: "sha256=" + strings.Repeat("0", 64)}

		rec := env.post(t, path, []byte(sampleJSON), headers)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}

		assertNoTokenLeak(t, env.logs.String(), prodToken, path)
	})

	t.Run("secret_unavailable_503", func(t *testing.T) {
		t.Parallel()

		env := newRig(t, nil)
		prodToken, _ := register(t, env, func(reg *webhook.Registration) { reg.HMACSecretRef = secretName })
		env.secrets.err = secrets.ErrNotFound
		path := prodPrefix + prodToken
		body := []byte(sampleJSON)
		headers := map[string]string{hdrGitHubSig: githubSignature(body), hdrGitHubDelivery: "d1"}

		rec := env.post(t, path, body, headers)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}

		assertNoTokenLeak(t, env.logs.String(), prodToken, path)
	})

	t.Run("bad_payload_400", func(t *testing.T) {
		t.Parallel()

		env := newRig(t, nil)
		prodToken, _ := register(t, env, nil)
		path := prodPrefix + prodToken

		rec := env.post(t, path, []byte(`[1,2,3]`), nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}

		assertNoTokenLeak(t, env.logs.String(), prodToken, path)
	})

	t.Run("paused_409", func(t *testing.T) {
		t.Parallel()

		env := newRig(t, nil)
		prodToken, _ := register(t, env, nil)
		env.pauses.paused = true
		path := prodPrefix + prodToken

		rec := env.post(t, path, []byte(sampleJSON), nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}

		assertNoTokenLeak(t, env.logs.String(), prodToken, path)
	})

	t.Run("accepted_202", func(t *testing.T) {
		t.Parallel()

		env := newRig(t, nil)
		prodToken, _ := register(t, env, nil)
		path := prodPrefix + prodToken

		rec := env.post(t, path, []byte(sampleJSON), nil)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}

		logs := env.logs.String()
		assertNoTokenLeak(t, logs, prodToken, path)

		if !strings.Contains(logs, defaultRegID) {
			t.Errorf("known-registration logs missing registration id; logs = %q", logs)
		}
	})
}
