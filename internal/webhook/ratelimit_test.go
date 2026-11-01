package webhook_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/webhook"
)

// hmacReg registers a workflow whose deliveries must be HMAC-signed under the
// configured test secret.
func hmacReg(t *testing.T, env *testRig) string {
	t.Helper()

	prodToken, _ := register(t, env, func(reg *webhook.Registration) { reg.HMACSecretRef = secretName })

	return prodToken
}

// nativeHeaders builds the native signature headers for a body.
func nativeHeaders(timestamp, nonce string, body []byte) map[string]string {
	return map[string]string{
		hdrNativeSig:   nativeSignature(timestamp, nonce, body),
		hdrNativeTS:    timestamp,
		hdrNativeNonce: nonce,
	}
}

// TestPerRegistrationRateLimit proves the per-registration bucket admits its
// burst, sheds the next delivery with 429, and refills after a minute. The
// registration override (1) is tighter than the default (60), so the override
// wins.
func TestPerRegistrationRateLimit(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken, _ := register(t, env, func(reg *webhook.Registration) { reg.RatePerMinute = 1 })
	body := []byte(sampleJSON)

	first := env.post(t, prodPrefix+prodToken, body, nil)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", first.Code)
	}

	second := env.post(t, prodPrefix+prodToken, body, nil)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second delivery status = %d, want 429", second.Code)
	}

	if got := decodeErr(t, second).Error.Code; got != webhook.CodeRateLimited {
		t.Errorf("code = %q, want %q", got, webhook.CodeRateLimited)
	}

	env.clock.advance(time.Minute)

	third := env.post(t, prodPrefix+prodToken, body, nil)
	if third.Code != http.StatusAccepted {
		t.Errorf("delivery after a minute of refill status = %d, want 202", third.Code)
	}
}

// TestRegistrationOverrideLoosensDefault proves a registration override looser
// than the config default wins: a default of 1 would shed the second delivery,
// but the override of 3 lets three through.
func TestRegistrationOverrideLoosensDefault(t *testing.T) {
	t.Parallel()

	env := newRig(t, func(cfg *webhook.HandlerConfig) { cfg.RatePerMinute = 1 })
	prodToken, _ := register(t, env, func(reg *webhook.Registration) { reg.RatePerMinute = 3 })
	body := []byte(sampleJSON)

	for attempt := range 3 {
		rec := env.post(t, prodPrefix+prodToken, body, nil)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("delivery %d status = %d, want 202 under override 3", attempt+1, rec.Code)
		}
	}
}

// TestUnknownTokenGlobalRateCap proves unknown-token traffic is capped
// process-wide (security amendment 3): with a global rate of 2, two distinct
// unknown tokens answer 404 and the third is shed with a generic 429.
func TestUnknownTokenGlobalRateCap(t *testing.T) {
	t.Parallel()

	env := newRig(t, func(cfg *webhook.HandlerConfig) { cfg.UnknownRatePerMinute = 2 })

	const numUnknownPosts = 3

	statuses := make([]int, 0, numUnknownPosts)

	var last *httptest.ResponseRecorder

	for range numUnknownPosts {
		token, _ := mint(t)
		rec := env.post(t, prodPrefix+token, []byte(sampleJSON), nil)
		statuses = append(statuses, rec.Code)
		last = rec
	}

	if statuses[0] != http.StatusNotFound || statuses[1] != http.StatusNotFound {
		t.Fatalf("first two unknown tokens statuses = %v, want two 404s", statuses)
	}

	if statuses[2] != http.StatusTooManyRequests {
		t.Fatalf("third unknown token status = %d, want 429", statuses[2])
	}

	if got := decodeErr(t, last).Error.Code; got != webhook.CodeRateLimited {
		t.Errorf("code = %q, want %q", got, webhook.CodeRateLimited)
	}
}

// TestNativeSignatureAccepted proves a correct native signature over
// timestamp + nonce + body is accepted.
func TestNativeSignatureAccepted(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken := hmacReg(t, env)
	body := []byte(sampleJSON)

	headers := nativeHeaders(timestampNow(env), "nonce-accept", body)

	rec := env.post(t, prodPrefix+prodToken, body, headers)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}

// TestNativeReplayedNonceRejected proves a replay of a validly signed native
// request inside the window is rejected on the reused nonce, and starts no
// second run (security amendment 1).
func TestNativeReplayedNonceRejected(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken := hmacReg(t, env)
	body := []byte(sampleJSON)
	headers := nativeHeaders(timestampNow(env), "nonce-replay", body)

	first := env.post(t, prodPrefix+prodToken, body, headers)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", first.Code)
	}

	second := env.post(t, prodPrefix+prodToken, body, headers)
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("replayed delivery status = %d, want 401", second.Code)
	}

	if got := decodeErr(t, second).Error.Code; got != webhook.CodeReplayed {
		t.Errorf("code = %q, want %q", got, webhook.CodeReplayed)
	}

	if calls := env.runs.callCount(); calls != 1 {
		t.Errorf("StartRun calls = %d, want 1 (the replay must start no run)", calls)
	}
}

// TestNativeStaleTimestampRejected proves a validly signed native request whose
// timestamp is outside the skew window is rejected as a replay.
func TestNativeStaleTimestampRejected(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken := hmacReg(t, env)
	body := []byte(sampleJSON)

	stale := strconv.FormatInt(env.clock.now().Add(-10*time.Minute).Unix(), 10)
	headers := nativeHeaders(stale, "nonce-stale", body)

	rec := env.post(t, prodPrefix+prodToken, body, headers)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	if got := decodeErr(t, rec).Error.Code; got != webhook.CodeReplayed {
		t.Errorf("code = %q, want %q", got, webhook.CodeReplayed)
	}

	if calls := env.runs.callCount(); calls != 0 {
		t.Errorf("StartRun calls = %d, want 0 for a stale timestamp", calls)
	}
}

// TestGitHubSignatureAcceptedThenReplayRejected proves the GitHub sha256=<hex>
// form is accepted once and a duplicate delivery id is rejected as a replay
// (security amendment 1).
func TestGitHubSignatureAcceptedThenReplayRejected(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken := hmacReg(t, env)
	body := []byte(sampleJSON)
	headers := map[string]string{
		hdrGitHubSig:      githubSignature(body),
		hdrGitHubDelivery: "gh-delivery-1",
	}

	first := env.post(t, prodPrefix+prodToken, body, headers)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", first.Code)
	}

	second := env.post(t, prodPrefix+prodToken, body, headers)
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate delivery status = %d, want 401", second.Code)
	}

	if got := decodeErr(t, second).Error.Code; got != webhook.CodeReplayed {
		t.Errorf("code = %q, want %q", got, webhook.CodeReplayed)
	}
}

// TestGitHubWrongSignatureRejected proves a wrong GitHub signature is a 401 and
// starts no run.
func TestGitHubWrongSignatureRejected(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken := hmacReg(t, env)
	body := []byte(sampleJSON)
	headers := map[string]string{
		hdrGitHubSig:      "sha256=" + strings.Repeat("0", 64),
		hdrGitHubDelivery: "gh-delivery-wrong",
	}

	rec := env.post(t, prodPrefix+prodToken, body, headers)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	if got := decodeErr(t, rec).Error.Code; got != webhook.CodeBadSignature {
		t.Errorf("code = %q, want %q", got, webhook.CodeBadSignature)
	}

	if calls := env.runs.callCount(); calls != 0 {
		t.Errorf("StartRun calls = %d, want 0 for a bad signature", calls)
	}
}

// TestGitHubMissingPrefixRejected proves a GitHub header without the sha256=
// prefix is rejected.
func TestGitHubMissingPrefixRejected(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken := hmacReg(t, env)
	body := []byte(sampleJSON)
	headers := map[string]string{
		hdrGitHubSig:      strings.Repeat("a", 64),
		hdrGitHubDelivery: "gh-delivery-noprefix",
	}

	rec := env.post(t, prodPrefix+prodToken, body, headers)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	if got := decodeErr(t, rec).Error.Code; got != webhook.CodeBadSignature {
		t.Errorf("code = %q, want %q", got, webhook.CodeBadSignature)
	}
}

// TestUndecodableSignatureRejected proves an undecodable hex digest is rejected.
func TestUndecodableSignatureRejected(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken := hmacReg(t, env)
	body := []byte(sampleJSON)
	headers := map[string]string{
		hdrGitHubSig:      "sha256=zzzz",
		hdrGitHubDelivery: "gh-delivery-undecodable",
	}

	rec := env.post(t, prodPrefix+prodToken, body, headers)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	if got := decodeErr(t, rec).Error.Code; got != webhook.CodeBadSignature {
		t.Errorf("code = %q, want %q", got, webhook.CodeBadSignature)
	}
}

// TestMissingSignatureHeaderRejected proves an HMAC-configured registration with
// no signature header at all is a 401.
func TestMissingSignatureHeaderRejected(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken := hmacReg(t, env)

	rec := env.post(t, prodPrefix+prodToken, []byte(sampleJSON), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	if got := decodeErr(t, rec).Error.Code; got != webhook.CodeBadSignature {
		t.Errorf("code = %q, want %q", got, webhook.CodeBadSignature)
	}
}

// TestHMACVerifiedBeforeRateBucket proves the request-path order fix (security
// amendment 4): bad-signature requests never charge the per-registration bucket,
// so a valid delivery still succeeds after a burst of forged ones. If HMAC were
// checked after the rate cap, the forged requests would drain the rate-1 bucket
// and block the real delivery.
func TestHMACVerifiedBeforeRateBucket(t *testing.T) {
	t.Parallel()

	env := newRig(t, nil)
	prodToken, _ := register(t, env, func(reg *webhook.Registration) {
		reg.HMACSecretRef = secretName
		reg.RatePerMinute = 1
	})
	body := []byte(sampleJSON)

	forged := map[string]string{
		hdrGitHubSig:      "sha256=" + strings.Repeat("0", 64),
		hdrGitHubDelivery: "forged",
	}

	for attempt := range 3 {
		rec := env.post(t, prodPrefix+prodToken, body, forged)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("forged delivery %d status = %d, want 401", attempt+1, rec.Code)
		}
	}

	valid := map[string]string{
		hdrGitHubSig:      githubSignature(body),
		hdrGitHubDelivery: "genuine",
	}

	rec := env.post(t, prodPrefix+prodToken, body, valid)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid delivery after forged burst status = %d, want 202 (bucket must not be drained by bad signatures)", rec.Code)
	}
}
