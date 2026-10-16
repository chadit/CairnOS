package memory_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/memory"
)

// Shared fixture literals, named so goconst stays quiet across the file.
const (
	scopeGlobal = "global"
	tagGo       = "go"
	tagSQLite   = "sqlite"
)

// fixedID is a canonical ULID reused across the front-matter error cases so the
// only thing under test in those cases is the front matter, not the id.
const fixedID = "01J8ZQ4V9GN0Y6M3T2C8KWQH5B"

// traversalID is an entry id shaped to escape the export directory if it ever
// reached a filename unchecked.
const traversalID = "../../evil"

// stamp is a second-precision UTC time so encode (RFC 3339, second precision)
// round-trips it exactly.
var stamp = time.Date(2026, 7, 3, 10, 15, 4, 0, time.UTC) //nolint:gochecknoglobals // shared read-only fixture

// assertEntryEqual compares the fields the markdown format carries. Timestamps
// use Equal so a UTC parse matches a UTC fixture without reflect's clock quirks;
// TokenCost is skipped because the format never carries it.
func assertEntryEqual(t *testing.T, got, want *memory.Entry) {
	t.Helper()

	if got.ID != want.ID || got.Scope != want.Scope || got.Content != want.Content ||
		got.Importance != want.Importance {
		t.Errorf("entry = %+v, want %+v", got, want)
	}

	if strings.Join(got.Tags, ",") != strings.Join(want.Tags, ",") {
		t.Errorf("tags = %v, want %v", got.Tags, want.Tags)
	}

	if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("timestamps = %v/%v, want %v/%v", got.CreatedAt, got.UpdatedAt, want.CreatedAt, want.UpdatedAt)
	}
}

func TestEncodeParseRoundTrip(t *testing.T) {
	t.Parallel()

	id := ulid.Make().String()

	cases := []struct {
		name    string
		content string
		tags    []string
	}{
		{name: "single line", content: "prefer table tests", tags: []string{tagGo, tagSQLite}},
		{name: "no tags", content: "one liner", tags: nil},
		{name: "multi line", content: "line one\nline two\nline three", tags: []string{tagGo}},
		{name: "trailing newline", content: "ends with a newline\n", tags: nil},
		{name: "empty body", content: "", tags: nil},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			want := memory.Entry{
				ID:         id,
				Scope:      scopeGlobal,
				Content:    tt.content,
				Importance: 50,
				Tags:       tt.tags,
				CreatedAt:  stamp,
				UpdatedAt:  stamp,
			}

			got, err := memory.ParseMarkdown(memory.EncodeMarkdown(&want))
			if err != nil {
				t.Fatalf("parse encoded entry: %v", err)
			}

			assertEntryEqual(t, &got, &want)
		})
	}
}

// validFrontMatter builds a well-formed file so each error case can mutate one
// line and prove that mutation is what trips the parser.
func validFrontMatter() string {
	return fmt.Sprintf(`---
id: %s
scope: %s
importance: 50
tags: %s, %s
created_at: 2026-07-03T10:15:04Z
updated_at: 2026-07-03T10:15:04Z
---
body content
`, fixedID, scopeGlobal, tagGo, tagSQLite)
}

func TestParseMarkdownRejectsBadFrontMatter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data string
	}{
		{name: "no opening marker", data: "id: " + fixedID + "\n"},
		{name: "no closing marker", data: "---\nid: " + fixedID + "\n"},
		{
			name: "missing key",
			data: strings.Replace(validFrontMatter(), "scope: "+scopeGlobal+"\n", "", 1),
		},
		{
			name: "unknown key",
			data: strings.Replace(validFrontMatter(), "tags: "+tagGo+", "+tagSQLite+"\n", "color: blue\ntags: "+tagGo+"\n", 1),
		},
		{
			name: "duplicate key",
			data: strings.Replace(validFrontMatter(), "importance: 50\n", "importance: 50\nimportance: 60\n", 1),
		},
		{
			name: "malformed line",
			data: strings.Replace(validFrontMatter(), "importance: 50\n", "importance 50\n", 1),
		},
		{
			name: "importance not an integer",
			data: strings.Replace(validFrontMatter(), "importance: 50\n", "importance: high\n", 1),
		},
		{
			name: "importance out of range",
			data: strings.Replace(validFrontMatter(), "importance: 50\n", "importance: 200\n", 1),
		},
		{
			name: "bad timestamp",
			data: strings.Replace(validFrontMatter(), "created_at: 2026-07-03T10:15:04Z\n", "created_at: yesterday\n", 1),
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := memory.ParseMarkdown([]byte(tt.data))
			if !errors.Is(err, memory.ErrBadFrontMatter) {
				t.Fatalf("parse %q error = %v, want ErrBadFrontMatter", tt.name, err)
			}
		})
	}
}

func TestParseMarkdownRejectsBadEntryID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		id   string
	}{
		{name: "lowercase", id: strings.ToLower(fixedID)},
		{name: "wrong length", id: "0123ABC"},
		{name: "traversal shaped", id: traversalID},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := strings.Replace(validFrontMatter(), "id: "+fixedID+"\n", "id: "+tt.id+"\n", 1)

			_, err := memory.ParseMarkdown([]byte(data))
			if !errors.Is(err, memory.ErrBadEntryID) {
				t.Fatalf("parse id %q error = %v, want ErrBadEntryID", tt.id, err)
			}
		})
	}
}

func TestExportDirRefusesTraversalID(t *testing.T) {
	t.Parallel()

	store := openMemory(t)
	ctx := t.Context()

	// A row written directly with a traversal-shaped id must fail export before
	// it composes any filename, so the check is defense in depth against rows
	// other tools wrote.
	if _, err := store.Add(ctx, &memory.Entry{ID: traversalID, Scope: scopeGlobal, Content: "hi"}); err != nil {
		t.Fatalf("seed bad-id row: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "out")

	_, err := memory.ExportDir(ctx, store, "", dir)
	if !errors.Is(err, memory.ErrBadEntryID) {
		t.Fatalf("export error = %v, want ErrBadEntryID", err)
	}
}

func TestImportDirAbortsBeforeAnyWrite(t *testing.T) {
	t.Parallel()

	store := openMemory(t)
	ctx := t.Context()
	dir := t.TempDir()

	goodID := ulid.Make().String()
	good := memory.Entry{ID: goodID, Scope: scopeGlobal, Content: "keep me", Importance: 50, CreatedAt: stamp, UpdatedAt: stamp}

	writeFile(t, dir, goodID+".md", string(memory.EncodeMarkdown(&good)))
	// A second file with malformed front matter must abort the whole import in
	// the first pass, before the good file's row is written.
	writeFile(t, dir, "broken.md", "not front matter\n")

	if _, err := memory.ImportDir(ctx, store, dir); !errors.Is(err, memory.ErrBadFrontMatter) {
		t.Fatalf("import error = %v, want ErrBadFrontMatter", err)
	}

	entries, err := store.List(ctx, "")
	if err != nil {
		t.Fatalf("list after failed import: %v", err)
	}

	if len(entries) != 0 {
		t.Fatalf("failed import wrote %d entries, want none", len(entries))
	}
}

func TestImportDirCountsAddedUpdatedUnchanged(t *testing.T) {
	t.Parallel()

	store := openMemory(t)
	ctx := t.Context()
	dir := t.TempDir()

	seedID := ulid.Make().String()
	if _, err := store.Add(ctx, &memory.Entry{ID: seedID, Scope: scopeGlobal, Content: "original", Importance: 50, Tags: []string{tagGo}}); err != nil {
		t.Fatalf("seed entry: %v", err)
	}

	count, err := memory.ExportDir(ctx, store, "", dir)
	if err != nil || count != 1 {
		t.Fatalf("export count = %d err = %v, want 1 and no error", count, err)
	}

	// Export-then-import with no edits is a pure no-op: every entry unchanged.
	if summary, importErr := memory.ImportDir(ctx, store, dir); importErr != nil ||
		summary != (memory.ImportSummary{Unchanged: 1}) {
		t.Fatalf("no-op import = %+v err = %v, want {Unchanged:1}", summary, importErr)
	}

	// Editing the file's content on disk makes the next import an update.
	editFileContent(t, filepath.Join(dir, seedID+".md"), "original", "edited offline")

	if summary, importErr := memory.ImportDir(ctx, store, dir); importErr != nil ||
		summary != (memory.ImportSummary{Updated: 1}) {
		t.Fatalf("edited import = %+v err = %v, want {Updated:1}", summary, importErr)
	}

	got, err := store.Get(ctx, seedID)
	if err != nil || got.Content != "edited offline" {
		t.Fatalf("stored content = %q err = %v, want edited offline", got.Content, err)
	}

	// A brand-new file adds one; the edited seed now imports unchanged.
	newID := ulid.Make().String()
	fresh := memory.Entry{ID: newID, Scope: scopeGlobal, Content: "brand new", Importance: 50, CreatedAt: stamp, UpdatedAt: stamp}
	writeFile(t, dir, newID+".md", string(memory.EncodeMarkdown(&fresh)))

	if summary, importErr := memory.ImportDir(ctx, store, dir); importErr != nil ||
		summary != (memory.ImportSummary{Added: 1, Unchanged: 1}) {
		t.Fatalf("mixed import = %+v err = %v, want {Added:1,Unchanged:1}", summary, importErr)
	}
}

// writeFile drops one file into dir with 0600 perms, matching the export shape.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// editFileContent rewrites a file, swapping old for new in its bytes, the way an
// operator editing a markdown export by hand would.
func editFileContent(t *testing.T, path, old, updated string) {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	swapped := strings.Replace(string(data), old, updated, 1)

	if err := os.WriteFile(path, []byte(swapped), 0o600); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}
}
