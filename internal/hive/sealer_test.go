package hive_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/hive"
	"github.com/chadit/CairnOS/internal/store"
)

const (
	aesKeySize     = 32
	paramMaxTokens = "max_tokens"
	latency30      = "30%"
	refHiveKey     = "hive_key"
	refHivePrevKey = "hive_prev_key"
)

// randomKey returns aesKeySize random bytes for an AES-256 sealer.
func randomKey(t *testing.T) []byte {
	t.Helper()

	key := make([]byte, aesKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return key
}

// newSealer builds a sealer over a fresh random key.
func newSealer(t *testing.T) hive.Sealer {
	t.Helper()

	sealer, err := hive.NewSealer(randomKey(t))
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}

	return sealer
}

// paramTuningTip is a tip whose fields are distinctive enough to spot in raw
// bytes, so the hiding test can prove none of them survive into ciphertext.
func paramTuningTip() hive.Tip {
	return hive.Tip{
		Kind:          store.SuggestionParamTuning,
		Category:      hive.CategoryCodeGen,
		RunnerType:    hive.RunnerTypeOpenAI,
		CostBucket:    bucket20,
		LatencyBucket: latency30,
		ParamShape:    &hive.ParamShape{Param: paramMaxTokens, Direction: hive.DirectionLowered},
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	t.Parallel()

	sealer := newSealer(t)
	tip := paramTuningTip()

	sealed, err := sealer.Seal(tip)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	got, err := sealer.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if got.Kind != tip.Kind || got.Category != tip.Category || got.RunnerType != tip.RunnerType ||
		got.CostBucket != tip.CostBucket || got.LatencyBucket != tip.LatencyBucket {
		t.Fatalf("round-trip tip = %+v, want %+v", got, tip)
	}

	if got.ParamShape == nil || got.ParamShape.Param != paramMaxTokens ||
		got.ParamShape.Direction != hive.DirectionLowered {
		t.Fatalf("round-trip param shape = %+v, want lowered max_tokens", got.ParamShape)
	}
}

// TestSealedEnvelopeHidesTipFields proves the sealing actually hides the coarse
// tip fields: even the bucketed savings and the param name, which are already
// content-free, are unreadable in the bytes that leave the node. Only the
// cleartext routing category survives.
func TestSealedEnvelopeHidesTipFields(t *testing.T) {
	t.Parallel()

	sealer := newSealer(t)
	tip := paramTuningTip()

	sealed, err := sealer.Seal(tip)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	wire, err := json.Marshal(sealed)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	onWire := string(wire)
	for _, secret := range []string{bucket20, latency30, paramMaxTokens, hive.DirectionLowered} {
		if strings.Contains(onWire, secret) {
			t.Errorf("sealed envelope leaks %q in %s", secret, onWire)
		}
	}

	if !strings.Contains(onWire, string(hive.CategoryCodeGen)) {
		t.Errorf("sealed envelope dropped the cleartext routing category: %s", onWire)
	}
}

// TestOpenRejectsWrongKey proves a node without the shared key cannot read a
// sealed tip: opening under a different key fails closed with ErrOpenSealed.
func TestOpenRejectsWrongKey(t *testing.T) {
	t.Parallel()

	sealed, err := newSealer(t).Seal(paramTuningTip())
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := newSealer(t).Open(sealed); !errors.Is(err, hive.ErrOpenSealed) {
		t.Errorf("open under wrong key = %v, want ErrOpenSealed", err)
	}
}

func TestNewSealerRejectsWrongSizeKey(t *testing.T) {
	t.Parallel()

	if _, err := hive.NewSealer(make([]byte, aesKeySize/2)); !errors.Is(err, hive.ErrKeyWrongSize) {
		t.Errorf("short key = %v, want ErrKeyWrongSize", err)
	}
}

// fakeSecrets is a one-value SecretResolver: it returns the same secret (or
// error) for any reference, so a test can hand the sealer a known key.
type fakeSecrets struct {
	err   error
	value string
}

func (f fakeSecrets) Get(_ context.Context, _ string) (string, error) {
	return f.value, f.err
}

func TestSealerFromSecretResolvesBase64Key(t *testing.T) {
	t.Parallel()

	key := randomKey(t)
	source := fakeSecrets{value: base64.StdEncoding.EncodeToString(key)}

	sealer, err := hive.SealerFromSecret(t.Context(), source, refHiveKey)
	if err != nil {
		t.Fatalf("sealer from secret: %v", err)
	}

	// A sealer built from the same key bytes opens what this one seals.
	twin, err := hive.NewSealer(key)
	if err != nil {
		t.Fatalf("twin sealer: %v", err)
	}

	sealed, err := sealer.Seal(paramTuningTip())
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := twin.Open(sealed); err != nil {
		t.Errorf("twin open: %v, want the same key to open it", err)
	}
}

func TestSealerFromSecretRejectsBadEncodingAndSize(t *testing.T) {
	t.Parallel()

	badEncoding := fakeSecrets{value: "not valid base64!!!"}
	if _, err := hive.SealerFromSecret(t.Context(), badEncoding, refHiveKey); !errors.Is(err, hive.ErrBadKeyEncoding) {
		t.Errorf("bad base64 = %v, want ErrBadKeyEncoding", err)
	}

	shortKey := fakeSecrets{value: base64.StdEncoding.EncodeToString(make([]byte, aesKeySize/2))}
	if _, err := hive.SealerFromSecret(t.Context(), shortKey, refHiveKey); !errors.Is(err, hive.ErrKeyWrongSize) {
		t.Errorf("short key = %v, want ErrKeyWrongSize", err)
	}
}

// TestRotatingSealerOpensPreviousAndPrimaryKeys proves the rotation window: a
// sealer holding the new primary plus the old key opens tips sealed under
// either, while a node that knows only the new key cannot open an old-key tip.
func TestRotatingSealerOpensPreviousAndPrimaryKeys(t *testing.T) {
	t.Parallel()

	oldKey := randomKey(t)
	newKey := randomKey(t)

	oldSealer, err := hive.NewSealer(oldKey)
	if err != nil {
		t.Fatalf("old sealer: %v", err)
	}

	ring, err := hive.NewRotatingSealer(newKey, oldKey)
	if err != nil {
		t.Fatalf("rotating sealer: %v", err)
	}

	// A tip another node sealed under the old key still opens during rotation.
	oldEnvelope, err := oldSealer.Seal(paramTuningTip())
	if err != nil {
		t.Fatalf("seal under old key: %v", err)
	}

	if _, openErr := ring.Open(oldEnvelope); openErr != nil {
		t.Errorf("ring could not open a previous-key tip: %v", openErr)
	}

	// A tip the rotating sealer seals itself (under the new primary) opens too.
	newEnvelope, err := ring.Seal(paramTuningTip())
	if err != nil {
		t.Fatalf("seal under new key: %v", err)
	}

	if _, openErr := ring.Open(newEnvelope); openErr != nil {
		t.Errorf("ring could not open its own primary-key tip: %v", openErr)
	}

	// A node that knows only the new key drops the old-key tip: the key id does
	// not match, so it is never force-decrypted.
	newOnly, err := hive.NewSealer(newKey)
	if err != nil {
		t.Fatalf("new-only sealer: %v", err)
	}

	if _, err := newOnly.Open(oldEnvelope); !errors.Is(err, hive.ErrOpenSealed) {
		t.Errorf("new-only open of an old-key tip = %v, want ErrOpenSealed", err)
	}
}

// TestRotatingSealerRejectsUnknownKey proves a ring drops a tip sealed under a
// key it does not hold, rather than trying to force it.
func TestRotatingSealerRejectsUnknownKey(t *testing.T) {
	t.Parallel()

	ring, err := hive.NewRotatingSealer(randomKey(t), randomKey(t))
	if err != nil {
		t.Fatalf("rotating sealer: %v", err)
	}

	strangerEnvelope, err := newSealer(t).Seal(paramTuningTip())
	if err != nil {
		t.Fatalf("seal under stranger key: %v", err)
	}

	if _, err := ring.Open(strangerEnvelope); !errors.Is(err, hive.ErrOpenSealed) {
		t.Errorf("open of a tip sealed under an unknown key = %v, want ErrOpenSealed", err)
	}
}

// twoKeySecrets resolves distinct keys per reference name, so a test can build a
// rotating sealer from secrets the way the CLI composition does.
type twoKeySecrets map[string]string

func (s twoKeySecrets) Get(_ context.Context, name string) (string, error) {
	value, ok := s[name]
	if !ok {
		return "", errUnknownSecret
	}

	return value, nil
}

var errUnknownSecret = errors.New("unknown secret reference")

// TestRotatingSealerFromSecretResolvesBothKeys proves the CLI entry point wires
// both refs into a ring that opens the previous key.
func TestRotatingSealerFromSecretResolvesBothKeys(t *testing.T) {
	t.Parallel()

	oldKey := randomKey(t)
	newKey := randomKey(t)

	source := twoKeySecrets{
		refHiveKey:     base64.StdEncoding.EncodeToString(newKey),
		refHivePrevKey: base64.StdEncoding.EncodeToString(oldKey),
	}

	ring, err := hive.RotatingSealerFromSecret(t.Context(), source, refHiveKey, refHivePrevKey)
	if err != nil {
		t.Fatalf("rotating sealer from secret: %v", err)
	}

	oldSealer, err := hive.NewSealer(oldKey)
	if err != nil {
		t.Fatalf("old sealer: %v", err)
	}

	oldEnvelope, err := oldSealer.Seal(paramTuningTip())
	if err != nil {
		t.Fatalf("seal under old key: %v", err)
	}

	if _, err := ring.Open(oldEnvelope); err != nil {
		t.Errorf("ring from secret could not open a previous-key tip: %v", err)
	}
}

// sealedMock is a hive server that captures the raw publish body and serves a
// caller-supplied raw subscribe body, so a sealed-path test can assert what
// bytes left the node and feed back arbitrary envelopes.
type sealedMock struct {
	server   *httptest.Server
	postBody []byte
	getBody  []byte
	posts    int
}

func newSealedMock(t *testing.T) *sealedMock {
	t.Helper()

	mock := &sealedMock{}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tips", mock.handle)

	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)

	return mock
}

func (m *sealedMock) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		m.postBody = body
		m.posts++

		w.WriteHeader(http.StatusAccepted)

		return
	}

	w.Header().Set("Content-Type", "application/json")

	body := m.getBody
	if body == nil {
		body = []byte(`{"tips":[]}`)
	}

	if _, err := w.Write(body); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// sealedConfig is enabledConfig with publishing on, the shape the sealed path
// runs under.
func sealedConfig(endpoint string) hive.Config {
	cfg := enabledConfig(endpoint, []hive.Category{hive.CategoryCodeGen})
	cfg.PublishEnabled = true

	return cfg
}

// TestPublishSealsTipBeforeSend proves that with a sealer wired in, the bytes
// that leave the node carry an encrypted envelope, not the plaintext tip.
func TestPublishSealsTipBeforeSend(t *testing.T) {
	t.Parallel()

	mock := newSealedMock(t)
	sealer := newSealer(t)

	client, err := hive.NewHTTPClient(sealedConfig(mock.server.URL), mock.server.Client(), hive.WithSealer(sealer))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := client.Publish(t.Context(), paramTuningTip(), 10); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if mock.posts != 1 {
		t.Fatalf("publish reached the server %d times, want 1", mock.posts)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(mock.postBody, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if _, plaintext := body["tip"]; plaintext {
		t.Errorf("sealed publish still carries a plaintext tip field: %s", mock.postBody)
	}

	if _, sealed := body["sealed_tip"]; !sealed {
		t.Fatalf("sealed publish is missing the sealed_tip envelope: %s", mock.postBody)
	}

	onWire := string(mock.postBody)
	for _, secret := range []string{bucket20, latency30, paramMaxTokens} {
		if strings.Contains(onWire, secret) {
			t.Errorf("sealed publish body leaks %q: %s", secret, onWire)
		}
	}
}

// TestSubscribeOpensSealedTips proves the receive side: an envelope sealed under
// the shared key is opened back into a usable tip.
func TestSubscribeOpensSealedTips(t *testing.T) {
	t.Parallel()

	mock := newSealedMock(t)
	sealer := newSealer(t)

	sealed, err := sealer.Seal(hive.Tip{
		Kind:       store.SuggestionCaching,
		Category:   hive.CategoryCodeGen,
		RunnerType: hive.RunnerTypeOpenAI,
		CostBucket: bucket20,
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	mock.getBody = mustServeTips(t, sealed)

	client, err := hive.NewHTTPClient(sealedConfig(mock.server.URL), mock.server.Client(), hive.WithSealer(sealer))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	tips, err := client.Subscribe(t.Context(), []hive.Category{hive.CategoryCodeGen})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if len(tips) != 1 {
		t.Fatalf("subscribe opened %d tips, want 1", len(tips))
	}

	if tips[0].Kind != store.SuggestionCaching || tips[0].CostBucket != bucket20 {
		t.Errorf("opened tip = %+v, want the sealed caching tip", tips[0])
	}
}

// TestSubscribeDropsTipSealedWithOtherKey proves a node silently drops an
// envelope it cannot open, rather than surfacing ciphertext as a tip.
func TestSubscribeDropsTipSealedWithOtherKey(t *testing.T) {
	t.Parallel()

	mock := newSealedMock(t)

	otherKeySealed, err := newSealer(t).Seal(hive.Tip{
		Kind:       store.SuggestionCaching,
		Category:   hive.CategoryCodeGen,
		RunnerType: hive.RunnerTypeOpenAI,
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	mock.getBody = mustServeTips(t, otherKeySealed)

	client, err := hive.NewHTTPClient(sealedConfig(mock.server.URL), mock.server.Client(), hive.WithSealer(newSealer(t)))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	tips, err := client.Subscribe(t.Context(), []hive.Category{hive.CategoryCodeGen})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if len(tips) != 0 {
		t.Fatalf("subscribe kept %d tips, want 0 (an unopenable envelope must be dropped)", len(tips))
	}
}

// mustServeTips marshals a sealed envelope into the {"tips":[...]} subscribe
// body the mock serves. The envelope's concrete type is unexported, so the test
// shuttles it as an opaque value through json.Marshal.
func mustServeTips(t *testing.T, sealed any) []byte {
	t.Helper()

	envelope, err := json.Marshal(sealed)
	if err != nil {
		t.Fatalf("marshal sealed: %v", err)
	}

	body, err := json.Marshal(map[string][]json.RawMessage{"tips": {envelope}})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	return body
}
