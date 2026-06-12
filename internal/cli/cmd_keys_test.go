package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/cli"
)

// keyLinePattern finds the printed plaintext key in keys add output.
var keyLinePattern = regexp.MustCompile(`ck_[0-9A-Z]+_[A-Za-z0-9_-]+`)

// runKeysCLI invokes the app and returns exit code, stdout, and stderr.
func runKeysCLI(ctx context.Context, t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := cli.New(&stdout, &stderr).Run(ctx, args)

	return code, stdout.String(), stderr.String()
}

// newKeysHome scaffolds a CairnOS home with default config.
func newKeysHome(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(""), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return dir
}

// addKey runs keys add and returns the key id and plaintext from the
// output.
func addKey(ctx context.Context, t *testing.T, dir, label, scopes string) (string, string) {
	t.Helper()

	code, stdout, stderr := runKeysCLI(ctx, t,
		"keys", "add", "--label", label, "--scopes", scopes, "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("keys add exit = %d, stderr %q", code, stderr)
	}

	plaintext := keyLinePattern.FindString(stdout)
	if plaintext == "" {
		t.Fatalf("keys add output %q carries no plaintext key", stdout)
	}

	keyID, _, err := auth.SplitKey(plaintext)
	if err != nil {
		t.Fatalf("printed key does not parse: %v", err)
	}

	return keyID, plaintext
}

func TestKeysAddPrintsPlaintextOnceAndStoresHash(t *testing.T) {
	t.Parallel()

	dir := newKeysHome(t)
	keyID, plaintext := addKey(t.Context(), t, dir, "ci", "read:run,write:workflow")

	// The plaintext must verify against the stored hash, and the store
	// must hold only the hash.
	authStore, err := auth.OpenSQLite(filepath.Join(dir, "data", "cairnos.db"))
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}

	t.Cleanup(func() { _ = authStore.Close() })

	key, err := authStore.GetKey(t.Context(), keyID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}

	_, secret, err := auth.SplitKey(plaintext)
	if err != nil {
		t.Fatalf("SplitKey: %v", err)
	}

	if err := auth.VerifyKey(secret, key.Hash); err != nil {
		t.Fatalf("stored hash does not verify the printed key: %v", err)
	}

	if strings.Contains(key.Hash, secret) {
		t.Fatal("store holds the secret in the clear")
	}

	if len(key.Scopes) != 2 || !auth.HasScope(key.Scopes, "read:run") {
		t.Fatalf("scopes = %v, want the requested pair", key.Scopes)
	}
}

func TestKeysAddRejectsUnknownScope(t *testing.T) {
	t.Parallel()

	dir := newKeysHome(t)

	code, _, stderr := runKeysCLI(t.Context(), t,
		"keys", "add", "--label", "x", "--scopes", "read:everything", "--dir", dir)
	if code != cli.ExitUsage {
		t.Fatalf("exit = %d, want usage error", code)
	}

	if !strings.Contains(stderr, "unknown scope") {
		t.Fatalf("stderr %q does not name the unknown scope", stderr)
	}
}

func TestKeysAddRequiresLabelAndScopes(t *testing.T) {
	t.Parallel()

	dir := newKeysHome(t)

	code, _, _ := runKeysCLI(t.Context(), t, "keys", "add", "--dir", dir)
	if code != cli.ExitUsage {
		t.Fatalf("exit = %d, want usage error for missing flags", code)
	}
}

func TestKeysListShowsKeysWithoutSecrets(t *testing.T) {
	t.Parallel()

	dir := newKeysHome(t)
	keyID, plaintext := addKey(t.Context(), t, dir, "list-me", "read:memory")

	code, stdout, stderr := runKeysCLI(t.Context(), t, "keys", "list", "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("keys list exit = %d, stderr %q", code, stderr)
	}

	if !strings.Contains(stdout, keyID) || !strings.Contains(stdout, "list-me") {
		t.Fatalf("keys list output %q missing the key row", stdout)
	}

	_, secret, err := auth.SplitKey(plaintext)
	if err != nil {
		t.Fatalf("SplitKey: %v", err)
	}

	if strings.Contains(stdout, secret) {
		t.Fatal("keys list leaked the secret")
	}
}

func TestKeysDisableEnableRoundTrip(t *testing.T) {
	t.Parallel()

	dir := newKeysHome(t)
	keyID, _ := addKey(t.Context(), t, dir, "toggle", "read:run")

	code, stdout, stderr := runKeysCLI(t.Context(), t, "keys", "disable", keyID, "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("keys disable exit = %d, stderr %q", code, stderr)
	}

	if !strings.Contains(stdout, "denied its live tokens") {
		t.Fatalf("disable output %q does not mention token denial", stdout)
	}

	authStore, err := auth.OpenSQLite(filepath.Join(dir, "data", "cairnos.db"))
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}

	t.Cleanup(func() { _ = authStore.Close() })

	key, err := authStore.GetKey(t.Context(), keyID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}

	if key.Enabled {
		t.Fatal("key still enabled after keys disable")
	}

	// Disable wrote the deny-by-sub row, the spec's immediate revocation. A
	// token issued at the key's creation time (before the revoke instant) is
	// denied.
	denied, err := authStore.IsDenied(t.Context(), "any", keyID, key.CreatedAt, time.Now())
	if err != nil {
		t.Fatalf("IsDenied: %v", err)
	}

	if !denied {
		t.Fatal("disable did not denylist the key's live tokens")
	}

	code, _, stderr = runKeysCLI(t.Context(), t, "keys", "enable", keyID, "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("keys enable exit = %d, stderr %q", code, stderr)
	}

	key, err = authStore.GetKey(t.Context(), keyID)
	if err != nil {
		t.Fatalf("GetKey after enable: %v", err)
	}

	if !key.Enabled {
		t.Fatal("key still disabled after keys enable")
	}

	denied, err = authStore.IsDenied(t.Context(), "any", keyID, key.CreatedAt, time.Now())
	if err != nil {
		t.Fatalf("IsDenied after enable: %v", err)
	}

	if denied {
		t.Fatal("enable did not lift the key's denial")
	}
}

// TestKeysEnableWarnsAboutResurrectedTokens is the L7 fix: re-enabling a key
// lifts its deny-by-sub row, so tokens minted before the disable are valid
// again; the command must say so and point at `auth revoke --key`.
func TestKeysEnableWarnsAboutResurrectedTokens(t *testing.T) {
	t.Parallel()

	dir := newKeysHome(t)
	keyID, _ := addKey(t.Context(), t, dir, "round-trip", "read:run")

	code, _, stderr := runKeysCLI(t.Context(), t, "keys", "disable", keyID, "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("keys disable exit = %d, stderr %q", code, stderr)
	}

	code, stdout, stderr := runKeysCLI(t.Context(), t, "keys", "enable", keyID, "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("keys enable exit = %d, stderr %q", code, stderr)
	}

	if !strings.Contains(stdout, "valid again") {
		t.Fatalf("enable output %q does not warn that pre-disable tokens are valid again", stdout)
	}

	if !strings.Contains(stdout, "auth revoke --key") {
		t.Fatalf("enable output %q does not point at `auth revoke --key`", stdout)
	}
}

func TestKeysDisableUnknownIDFails(t *testing.T) {
	t.Parallel()

	dir := newKeysHome(t)

	code, _, stderr := runKeysCLI(t.Context(), t, "keys", "disable", "no-such-key", "--dir", dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure for unknown key", code)
	}

	if !strings.Contains(stderr, "not found") {
		t.Fatalf("stderr %q does not explain the miss", stderr)
	}
}

func TestAuthRevokeByJTIAndKey(t *testing.T) {
	t.Parallel()

	dir := newKeysHome(t)
	keyID, _ := addKey(t.Context(), t, dir, "revoke-me", "read:run")

	code, stdout, stderr := runKeysCLI(t.Context(), t, "auth", "revoke", "--jti", "token-1", "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("auth revoke --jti exit = %d, stderr %q", code, stderr)
	}

	if !strings.Contains(stdout, "token-1") {
		t.Fatalf("revoke output %q does not echo the jti", stdout)
	}

	code, _, stderr = runKeysCLI(t.Context(), t, "auth", "revoke", "--key", keyID, "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("auth revoke --key exit = %d, stderr %q", code, stderr)
	}

	authStore, err := auth.OpenSQLite(filepath.Join(dir, "data", "cairnos.db"))
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}

	t.Cleanup(func() { _ = authStore.Close() })

	key, err := authStore.GetKey(t.Context(), keyID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}

	denied, err := authStore.IsDenied(t.Context(), "token-1", keyID, key.CreatedAt, time.Now())
	if err != nil {
		t.Fatalf("IsDenied: %v", err)
	}

	if !denied {
		t.Fatal("revocations did not land in the denylist")
	}
}

func TestAuthRevokeDemandsExactlyOneTarget(t *testing.T) {
	t.Parallel()

	dir := newKeysHome(t)

	cases := []struct {
		name string
		args []string
	}{
		{name: "neither", args: []string{"auth", "revoke", "--dir", dir}},
		{name: "both", args: []string{"auth", "revoke", "--jti", "a", "--key", "b", "--dir", dir}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, _, stderr := runKeysCLI(t.Context(), t, tt.args...)
			if code != cli.ExitUsage {
				t.Fatalf("exit = %d (stderr %q), want usage error", code, stderr)
			}
		})
	}
}
