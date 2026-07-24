package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for the raw pre-upgrade seeding

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/internal/store/storetest"
)

// openStore opens a fresh database under t.TempDir and closes it when the
// case finishes; every case exercises schema creation on a real file. It
// returns the store.Store interface so the shared conformance suite drives
// it the same way it drives the postgres backend.
func openStore(t *testing.T) store.Store {
	t.Helper()

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})

	return db
}

// TestStoreConformance runs the one behavioral spec shared with the postgres
// backend (internal/store/storetest). Proving sqlite passes the same suite
// the postgres backend must pass is the whole point of the shared suite:
// one spec, two backends.
func TestStoreConformance(t *testing.T) {
	t.Parallel()
	storetest.RunConformance(t, openStore)
}

// preUpgradeChatSchema is the chat slice of the schema as it existed before
// transcript search: no FTS table, no triggers. Seeding through it simulates
// a database written by an older binary, whose messages the FTS index has
// never seen.
const preUpgradeChatSchema = `
CREATE TABLE chat_sessions (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    personality  TEXT NOT NULL,
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL
);

CREATE TABLE chat_messages (
    session_id TEXT NOT NULL REFERENCES chat_sessions(id),
    seq        INTEGER NOT NULL,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (session_id, seq)
);
`

// Backfill fixtures shared by the seeding, canary, and assertions.
const (
	backfillSession = "sess-old"
	backfillContent = "the kangaroo hops at dawn"
	backfillQuery   = "kangaroo"
	backfillStamp   = "2026-06-01T00:00:00.000000000Z"
)

// TestOpenBackfillsChatSearchOnce proves the one-time FTS backfill guard from
// both sides: a database created before transcript search gains searchable
// history on its first Open (the 'rebuild' runs), and a reopen never
// reindexes again. The second half works by deleting a canary row from the
// FTS index directly, so only messages, and then reopening: if Open rebuilt
// every time, the canary would come back.
func TestOpenBackfillsChatSearchOnce(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")
	seedPreUpgradeChat(t, path)

	upgraded, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("first open after upgrade: %v", err)
	}

	hits, err := upgraded.SearchMessages(t.Context(), store.ChatSearchQuery{Text: backfillQuery})
	if err != nil {
		t.Fatalf("search after backfill: %v", err)
	}

	if len(hits) != 1 || hits[0].SessionID != backfillSession || hits[0].Content != backfillContent {
		t.Fatalf("backfill hits = %#v, want the one pre-upgrade message", hits)
	}

	if err := upgraded.Close(); err != nil {
		t.Fatalf("close upgraded store: %v", err)
	}

	dropSearchCanary(t, path)

	reopened, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := reopened.Close(); closeErr != nil {
			t.Errorf("close reopened store: %v", closeErr)
		}
	})

	ghosts, err := reopened.SearchMessages(t.Context(), store.ChatSearchQuery{Text: backfillQuery})
	if err != nil {
		t.Fatalf("search after reopen: %v", err)
	}

	if len(ghosts) != 0 {
		t.Fatalf("reopen found %d hits for the dropped canary, want 0: Open must not rebuild the index again", len(ghosts))
	}
}

// seedPreUpgradeChat writes one session and one message through the
// pre-search schema with a raw connection, so the resulting file looks
// exactly like a database an older binary left behind.
func seedPreUpgradeChat(t *testing.T, path string) {
	t.Helper()

	db := openRawDB(t, path)

	if _, err := db.ExecContext(t.Context(), preUpgradeChatSchema); err != nil {
		t.Fatalf("create pre-upgrade schema: %v", err)
	}

	_, err := db.ExecContext(t.Context(),
		`INSERT INTO chat_sessions (id, personality, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		backfillSession, "assistant", backfillStamp, backfillStamp)
	if err != nil {
		t.Fatalf("seed pre-upgrade session: %v", err)
	}

	_, err = db.ExecContext(t.Context(),
		`INSERT INTO chat_messages (session_id, seq, role, content, created_at) VALUES (?, 1, ?, ?, ?)`,
		backfillSession, store.ChatRoleUser, backfillContent, backfillStamp)
	if err != nil {
		t.Fatalf("seed pre-upgrade message: %v", err)
	}
}

// dropSearchCanary removes the seeded message from the FTS index without
// touching chat_messages, using FTS5's external-content 'delete' command. A
// later rebuild would re-index the surviving row, which is exactly what the
// reopen assertion watches for.
func dropSearchCanary(t *testing.T, path string) {
	t.Helper()

	db := openRawDB(t, path)

	_, err := db.ExecContext(t.Context(),
		`INSERT INTO chat_messages_fts(chat_messages_fts, rowid, content)
		 SELECT 'delete', rowid, content FROM chat_messages WHERE session_id = ? AND seq = 1`,
		backfillSession)
	if err != nil {
		t.Fatalf("drop search canary: %v", err)
	}
}

// openRawDB opens the database file directly, bypassing sqlite.Open, so the
// backfill test can stage and inspect states the store itself never writes.
func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close raw database: %v", closeErr)
		}
	})

	return db
}

// preParentRunsSchema is the runs table as it existed before replay added
// parent_run_id: no such column. Seeding through it simulates a database an
// older binary created, whose runs rows the guarded ALTER has never touched.
const preParentRunsSchema = `
CREATE TABLE runs (
    id            TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    workflow_hash TEXT NOT NULL,
    workspace_id  TEXT NOT NULL DEFAULT 'default',
    status        TEXT NOT NULL,
    inputs_json   TEXT NOT NULL DEFAULT '{}',
    trigger       TEXT NOT NULL,
    created_at    TIMESTAMP NOT NULL,
    started_at    TIMESTAMP,
    finished_at   TIMESTAMP,
    error_json    TEXT
);
`

// Pre-upgrade run fixtures shared by the seeding and assertions.
const (
	preParentRunID = "run-pre-upgrade"
	preParentStamp = "2026-06-01T00:00:00.000000000Z"
)

// TestOpenAddsParentRunIDColumn proves the guarded ALTER upgrades a database
// created before replay: a runs table lacking parent_run_id gains the column
// on the next Open, its old row reads back as a root run (empty ParentRunID),
// and a new child run with ParentRunID set round-trips through the upgraded
// schema.
func TestOpenAddsParentRunIDColumn(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")
	seedPreParentRun(t, path)

	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("open after upgrade: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})

	old, err := db.GetRun(t.Context(), preParentRunID)
	if err != nil {
		t.Fatalf("get pre-upgrade run: %v", err)
	}

	if old.ParentRunID != "" {
		t.Errorf("pre-upgrade run parent_run_id = %q, want empty", old.ParentRunID)
	}

	child := &store.Run{
		ID:           "run-child",
		WorkflowName: "alpha",
		WorkflowHash: "hash",
		Status:       store.RunPending,
		Trigger:      "replay",
		ParentRunID:  preParentRunID,
		CreatedAt:    time.Now(),
	}
	if err := db.CreateRun(t.Context(), child); err != nil {
		t.Fatalf("create child run after upgrade: %v", err)
	}

	got, err := db.GetRun(t.Context(), "run-child")
	if err != nil {
		t.Fatalf("get child run: %v", err)
	}

	if got.ParentRunID != preParentRunID {
		t.Errorf("child parent_run_id = %q, want %q", got.ParentRunID, preParentRunID)
	}
}

// seedPreParentRun writes one run through the pre-replay schema with a raw
// connection, so the resulting file looks like a database an older binary left
// behind, before parent_run_id existed.
func seedPreParentRun(t *testing.T, path string) {
	t.Helper()

	db := openRawDB(t, path)

	if _, err := db.ExecContext(t.Context(), preParentRunsSchema); err != nil {
		t.Fatalf("create pre-upgrade runs schema: %v", err)
	}

	_, err := db.ExecContext(t.Context(),
		`INSERT INTO runs (id, workflow_name, workflow_hash, workspace_id, status, inputs_json, trigger, created_at)
		 VALUES (?, ?, ?, 'default', ?, '{}', ?, ?)`,
		preParentRunID, "alpha", "hash", string(store.RunSucceeded), "cli", preParentStamp)
	if err != nil {
		t.Fatalf("seed pre-upgrade run: %v", err)
	}
}

// TestNewIDIsSortable checks the sqlite-specific NewID helper still hands out
// the time-sortable ULIDs the rest of the system assumes; the behavioral
// round-trips live in the shared suite.
func TestNewIDIsSortable(t *testing.T) {
	t.Parallel()

	first := sqlite.NewID()
	second := sqlite.NewID()

	if first == second {
		t.Fatalf("NewID returned the same id twice: %s", first)
	}

	if first >= second {
		t.Fatalf("NewID ids not time-sortable: %s !< %s", first, second)
	}
}
