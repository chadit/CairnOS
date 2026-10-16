package reauth_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/reauth"
)

const (
	dirPerm  = 0o700
	filePerm = 0o600

	keyTokens      = "tokens"
	keyAccessToken = "access_token"
	keyIDToken     = "id_token"
	claimExpiry    = "exp"
	keyEmail       = "email"

	codexAccount = "dev@example.com"
	qwenAccount  = "q@example.com"
)

func TestReadCredentialCodex(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	expiry := time.Now().Add(time.Hour)

	accessToken := makeJWT(t, map[string]any{claimExpiry: expiry.Unix()})
	idToken := makeJWT(t, map[string]any{keyEmail: codexAccount})

	writeCreds(t, filepath.Join(home, ".codex"), "auth.json", map[string]any{
		keyTokens: map[string]any{keyAccessToken: accessToken, keyIDToken: idToken},
	})

	cred, err := reauth.ReadCredential(reauth.ProviderCodex, home)
	if err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}

	if cred.Account != codexAccount {
		t.Errorf("Account = %q, want %q", cred.Account, codexAccount)
	}

	if cred.ExpiresAt.Unix() != expiry.Unix() {
		t.Errorf("ExpiresAt = %d, want %d", cred.ExpiresAt.Unix(), expiry.Unix())
	}

	if !cred.Valid(time.Now()) {
		t.Error("Valid = false, want true for a future expiry")
	}
}

func TestReadCredentialQwen(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	expiryMs := time.Now().Add(time.Hour).UnixMilli()

	writeCreds(t, filepath.Join(home, ".qwen"), "oauth_creds.json", map[string]any{
		"expiry_date": expiryMs,
		keyEmail:      qwenAccount,
	})

	cred, err := reauth.ReadCredential(reauth.ProviderQwen, home)
	if err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}

	if cred.Account != qwenAccount {
		t.Errorf("Account = %q, want %q", cred.Account, qwenAccount)
	}

	if cred.ExpiresAt.UnixMilli() != expiryMs {
		t.Errorf("ExpiresAt = %d ms, want %d ms", cred.ExpiresAt.UnixMilli(), expiryMs)
	}

	if !cred.Valid(time.Now()) {
		t.Error("Valid = false, want true for a future expiry")
	}
}

func TestReadCredentialClaude(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	expiryMs := time.Now().Add(time.Hour).UnixMilli()

	writeCreds(t, filepath.Join(home, ".claude"), ".credentials.json", map[string]any{
		"claudeAiOauth": map[string]any{"expiresAt": expiryMs},
	})

	cred, err := reauth.ReadCredential(reauth.ProviderClaude, home)
	if err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}

	if cred.ExpiresAt.UnixMilli() != expiryMs {
		t.Errorf("ExpiresAt = %d ms, want %d ms", cred.ExpiresAt.UnixMilli(), expiryMs)
	}

	if !cred.Valid(time.Now()) {
		t.Error("Valid = false, want true for a future expiry")
	}
}

func TestReadCredentialMissingFileIsNoCredential(t *testing.T) {
	t.Parallel()

	_, err := reauth.ReadCredential(reauth.ProviderCodex, t.TempDir())
	if !errors.Is(err, reauth.ErrNoCredential) {
		t.Fatalf("err = %v, want ErrNoCredential", err)
	}
}

func TestReadCredentialUnknownProvider(t *testing.T) {
	t.Parallel()

	_, err := reauth.ReadCredential(reauth.Provider("bogus"), t.TempDir())
	if !errors.Is(err, reauth.ErrUnknownProvider) {
		t.Fatalf("err = %v, want ErrUnknownProvider", err)
	}
}

func TestExpiredCredentialIsNotValid(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	past := time.Now().Add(-time.Hour)

	writeCreds(t, filepath.Join(home, ".codex"), "auth.json", map[string]any{
		keyTokens: map[string]any{keyAccessToken: makeJWT(t, map[string]any{claimExpiry: past.Unix()})},
	})

	cred, err := reauth.ReadCredential(reauth.ProviderCodex, home)
	if err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}

	if cred.Valid(time.Now()) {
		t.Error("Valid = true, want false for a past expiry")
	}
}

func TestMalformedTokenYieldsNotValid(t *testing.T) {
	t.Parallel()

	home := t.TempDir()

	// A non-JWT access token must not crash the reader; the expiry is simply
	// unknown, so the credential reports itself not valid and the job stays held.
	writeCreds(t, filepath.Join(home, ".codex"), "auth.json", map[string]any{
		keyTokens: map[string]any{keyAccessToken: "not-a-jwt", keyIDToken: ""},
	})

	cred, err := reauth.ReadCredential(reauth.ProviderCodex, home)
	if err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}

	if !cred.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want zero for an unreadable token", cred.ExpiresAt)
	}

	if cred.Valid(time.Now()) {
		t.Error("Valid = true, want false when the expiry could not be read")
	}
}

// zeroCredentialIsNotValid guards the safe default: a credential the reader could
// not populate must never look fresh.
func TestZeroCredentialIsNotValid(t *testing.T) {
	t.Parallel()

	if (reauth.Credential{}).Valid(time.Now()) {
		t.Error("zero Credential reported valid, want not valid")
	}
}

// makeJWT builds an unsigned three-part JWT carrying claims, enough for the probe
// which decodes the payload without verifying the signature.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()

	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	signature := base64.RawURLEncoding.EncodeToString([]byte("signature"))

	return header + "." + body + "." + signature
}

// writeCreds writes value as JSON to dir/name, creating dir.
func writeCreds(t *testing.T, dir, name string, value any) {
	t.Helper()

	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal creds: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, name), data, filePerm); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
