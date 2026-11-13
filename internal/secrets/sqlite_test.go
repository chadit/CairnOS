package secrets_test

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/secrets"
	"github.com/chadit/CairnOS/internal/secrets/secretstest"
	"github.com/chadit/CairnOS/internal/store"
)

const (
	sqliteDriver = "sqlite"
	sqliteDBFile = "secrets.db"
	// corruptTailSize is the run of zero bytes after a bare SQLite header, so
	// the file is neither empty nor a database.
	corruptTailSize = 64
	// rolledBackStamp is a timestamp far from any real write, so a version
	// derived from it can never coincide with the one a fresh seal bound.
	rolledBackStamp = "2030-01-01T00:00:00.000000000Z"
)

// sqliteDSN builds the pragma-tuned DSN both the store and the raw tamper
// connection use, so the recency test writes under the same WAL settings.
func sqliteDSN(path string) string {
	return "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
}

// sealerFromKeys builds a sealer over a KeySet the conformance suite handed out.
func sealerFromKeys(t *testing.T, keys secretstest.KeySet) *secrets.Sealer {
	t.Helper()

	sealer, err := secrets.NewSealer(keys.Primary, keys.Previous...)
	if err != nil {
		t.Fatalf("build sealer: %v", err)
	}

	return sealer
}

// openSQLiteAt opens (or reopens) a store at path under keys and closes it on
// cleanup. The conformance suite reopens the same path under different keys to
// exercise rotation, so path stays stable per subtest.
func openSQLiteAt(t *testing.T, path string, keys secretstest.KeySet) *secrets.SQLite {
	t.Helper()

	secretStore, err := secrets.OpenSQLite(t.Context(), path, sealerFromKeys(t, keys))
	if err != nil {
		t.Fatalf("open sqlite secret store: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := secretStore.Close(); closeErr != nil {
			t.Errorf("close sqlite secret store: %v", closeErr)
		}
	})

	return secretStore
}

// TestSQLiteConformance runs the behavioral spec shared with the postgres
// backend. Each subtest keys its database file off its own name, so reopening
// under a new key set (the rotation cases) hits the same file while distinct
// subtests stay isolated and parallel.
func TestSQLiteConformance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	secretstest.RunConformance(t, func(t *testing.T, keys secretstest.KeySet) secrets.Store {
		t.Helper()

		path := filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".db")

		return openSQLiteAt(t, path, keys)
	})
}

// TestSQLiteNoPlaintextAtRest is the per-backend at-rest proof: after sealing a
// distinctive value, the plaintext appears in none of the database's raw files
// (main, WAL, shared-memory). Only the AES-256-GCM envelope is on disk.
func TestSQLiteNoPlaintextAtRest(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), sqliteDBFile)
	secretStore := openSQLiteAt(t, path, secretstest.KeySet{Primary: randomKey(t)})

	if err := secretStore.Set(t.Context(), testName, testValue); err != nil {
		t.Fatalf("set: %v", err)
	}

	if got, err := secretStore.Get(t.Context(), testName); err != nil || got != testValue {
		t.Fatalf("get = %q, %v; want %q", got, err, testValue)
	}

	matches, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatalf("glob db files: %v", err)
	}

	var scanned bool

	for _, file := range matches {
		data, readErr := os.ReadFile(file) // #nosec G304 -- path is the test's own temp file
		if readErr != nil {
			t.Fatalf("read %s: %v", file, readErr)
		}

		if len(data) > 0 {
			scanned = true
		}

		if bytes.Contains(data, []byte(testValue)) {
			t.Errorf("raw db file %s carries the plaintext value", file)
		}
	}

	if !scanned {
		t.Fatal("no non-empty db files to scan")
	}
}

// TestSQLiteRefusesAFileThatIsNotADatabase covers a truncated or overwritten
// store file. Opening has to fail loudly: a store that came up empty would
// answer every read with ErrNotFound, which reads as a host with no secrets
// configured rather than a host whose secrets are gone.
func TestSQLiteRefusesAFileThatIsNotADatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), sqliteDBFile)

	// A valid header followed by nothing is what a half-written copy leaves,
	// and it is the shape an empty-file check would wave through.
	corrupt := append([]byte("SQLite format 3\x00"), make([]byte, corruptTailSize)...)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("write the corrupt database: %v", err)
	}

	secretStore, err := secrets.OpenSQLite(t.Context(), path, sealerFromKeys(t, secretstest.KeySet{Primary: randomKey(t)}))
	if err == nil {
		t.Cleanup(func() { _ = secretStore.Close() })
		t.Fatal("OpenSQLite accepted a file that is not a database")
	}
}

// TestSQLiteRolledBackCiphertextFailsOpen is the rollback case at the store
// level: a DB-write attacker who advances a row's recency
// stamp past the ciphertext it was sealed under cannot open it, because
// updated_at is bound into the AAD. The value opens cleanly before the tamper
// and collapses to ErrOpenSealed after, and the error leaks no plaintext.
func TestSQLiteRolledBackCiphertextFailsOpen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), sqliteDBFile)
	secretStore := openSQLiteAt(t, path, secretstest.KeySet{Primary: randomKey(t)})
	ctx := t.Context()

	if err := secretStore.Set(ctx, testName, testValue); err != nil {
		t.Fatalf("set: %v", err)
	}

	if got, err := secretStore.Get(ctx, testName); err != nil || got != testValue {
		t.Fatalf("control get = %q, %v; want %q", got, err, testValue)
	}

	raw, err := sql.Open(sqliteDriver, sqliteDSN(path))
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}

	t.Cleanup(func() { _ = raw.Close() })

	if _, execContextErr := raw.ExecContext(ctx,
		"UPDATE secrets SET updated_at = ? WHERE workspace = ? AND name = ?",
		rolledBackStamp, store.DefaultWorkspace, testName); execContextErr != nil {
		t.Fatalf("tamper updated_at: %v", execContextErr)
	}

	_, err = secretStore.Get(ctx, testName)
	if !errors.Is(err, secrets.ErrOpenSealed) {
		t.Fatalf("get after recency tamper = %v, want ErrOpenSealed", err)
	}

	if message := err.Error(); strings.Contains(message, testValue) {
		t.Errorf("error leaks the plaintext value: %v", err)
	}
}
