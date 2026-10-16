package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Markdown export layout. One file per entry, a fixed-key front matter block
// between two marker lines, then the entry content verbatim. The format is an
// offline view an operator can grep and edit; the store stays the record.
const (
	frontMatterMarker = "---"
	markdownExt       = ".md"

	keyID         = "id"
	keyScope      = "scope"
	keyImportance = "importance"
	keyTags       = "tags"
	keyCreatedAt  = "created_at"
	keyUpdatedAt  = "updated_at"
)

// importance bounds; a hand-edited value outside this range is rejected rather
// than stored, since the schema treats importance as a 0..100 ordering weight.
const (
	minImportance = 0
	maxImportance = 100
)

// Directory and file permissions match the applier and the run store: the
// export directory is owner-only, and each file is 0600 like every other file
// this repo writes.
const (
	exportDirPerm  = 0o700
	exportFilePerm = 0o600
)

// frontMatterKeys is the fixed front matter key set, every one mandatory.
// token_cost is deliberately absent: import leaves TokenCost zero so the store
// re-estimates it, giving offline content edits a fresh cost for free.
func frontMatterKeys() []string {
	return []string{keyID, keyScope, keyImportance, keyTags, keyCreatedAt, keyUpdatedAt}
}

// EncodeMarkdown renders one entry as its markdown file bytes: the fixed-key
// front matter block followed by the content, terminated with exactly one
// trailing newline so an editor that ensures a final newline does not register
// an edit on the next import.
func EncodeMarkdown(entry *Entry) []byte {
	var buf strings.Builder

	buf.WriteString(frontMatterMarker)
	buf.WriteByte('\n')

	writeField(&buf, keyID, entry.ID)
	writeField(&buf, keyScope, entry.Scope)
	writeField(&buf, keyImportance, strconv.Itoa(entry.Importance))
	writeField(&buf, keyTags, strings.Join(entry.Tags, ", "))
	writeField(&buf, keyCreatedAt, entry.CreatedAt.UTC().Format(time.RFC3339))
	writeField(&buf, keyUpdatedAt, entry.UpdatedAt.UTC().Format(time.RFC3339))

	buf.WriteString(frontMatterMarker)
	buf.WriteByte('\n')

	buf.WriteString(entry.Content)
	buf.WriteByte('\n')

	return []byte(buf.String())
}

// writeField renders one "key: value" front matter line.
func writeField(buf *strings.Builder, key, value string) {
	buf.WriteString(key)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteByte('\n')
}

// ParseMarkdown reads one markdown file back into an Entry. It rejects a
// missing, malformed, or wrong-keyed front matter block with ErrBadFrontMatter
// and a non-canonical id with ErrBadEntryID, both before any value reaches the
// store, so a hand edit can never corrupt an entry or smuggle a traversal id in.
func ParseMarkdown(data []byte) (Entry, error) {
	body, lines, err := splitFrontMatter(string(data))
	if err != nil {
		return Entry{}, err
	}

	fields, err := parseFields(lines)
	if err != nil {
		return Entry{}, err
	}

	return buildEntry(fields, body)
}

// splitFrontMatter peels the front matter block off the front of the file and
// returns the raw key lines plus the body, which is everything after the
// closing marker line with at most one trailing newline stripped.
func splitFrontMatter(text string) (string, []string, error) {
	rest, opened := strings.CutPrefix(text, frontMatterMarker+"\n")
	if !opened {
		return "", nil, fmt.Errorf("%w: first line must be %q", ErrBadFrontMatter, frontMatterMarker)
	}

	var lines []string

	for {
		line, after, found := strings.Cut(rest, "\n")
		if !found {
			return "", nil, fmt.Errorf("%w: missing closing %q marker", ErrBadFrontMatter, frontMatterMarker)
		}

		if line == frontMatterMarker {
			// The body owns exactly one trailing newline (see EncodeMarkdown),
			// so strip one if present and an editor-added final newline round
			// trips as no edit.
			return strings.TrimSuffix(after, "\n"), lines, nil
		}

		lines = append(lines, line)
		rest = after
	}
}

// parseFields turns the front matter lines into a key/value map, rejecting an
// unknown key, a duplicate key, a malformed line, or a missing key. Guessing
// past any of these would silently corrupt an entry on import after a hand edit.
func parseFields(lines []string) (map[string]string, error) {
	keys := frontMatterKeys()
	fields := make(map[string]string, len(keys))

	for _, line := range lines {
		rawKey, rawValue, found := strings.Cut(line, ":")
		if !found {
			return nil, fmt.Errorf("%w: line %q is not key: value", ErrBadFrontMatter, line)
		}

		key := strings.TrimSpace(rawKey)

		if !slices.Contains(keys, key) {
			return nil, fmt.Errorf("%w: unknown key %q", ErrBadFrontMatter, key)
		}

		if _, dup := fields[key]; dup {
			return nil, fmt.Errorf("%w: duplicate key %q", ErrBadFrontMatter, key)
		}

		fields[key] = strings.TrimSpace(rawValue)
	}

	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("%w: missing key %q", ErrBadFrontMatter, key)
		}
	}

	return fields, nil
}

// buildEntry validates every field value and assembles the Entry. TokenCost is
// left zero on purpose so the store re-estimates it from the imported content.
func buildEntry(fields map[string]string, body string) (Entry, error) {
	id := fields[keyID]
	if err := validateEntryID(id); err != nil {
		return Entry{}, err
	}

	importance, err := strconv.Atoi(fields[keyImportance])
	if err != nil {
		return Entry{}, fmt.Errorf("%w: %s %q is not an integer", ErrBadFrontMatter, keyImportance, fields[keyImportance])
	}

	if importance < minImportance || importance > maxImportance {
		return Entry{}, fmt.Errorf("%w: %s %d is outside 0..100", ErrBadFrontMatter, keyImportance, importance)
	}

	createdAt, err := time.Parse(time.RFC3339, fields[keyCreatedAt])
	if err != nil {
		return Entry{}, fmt.Errorf("%w: %s %q is not RFC 3339", ErrBadFrontMatter, keyCreatedAt, fields[keyCreatedAt])
	}

	updatedAt, err := time.Parse(time.RFC3339, fields[keyUpdatedAt])
	if err != nil {
		return Entry{}, fmt.Errorf("%w: %s %q is not RFC 3339", ErrBadFrontMatter, keyUpdatedAt, fields[keyUpdatedAt])
	}

	return Entry{
		ID:         id,
		Scope:      fields[keyScope],
		Content:    body,
		Importance: importance,
		Tags:       splitMarkdownTags(fields[keyTags]),
		CreatedAt:  createdAt,
		UpdatedAt:  updatedAt,
	}, nil
}

// validateEntryID requires the canonical 26-character ULID form: ulid.Parse
// must accept it AND round-trip it unchanged. Crockford base32 decodes a
// lowercase id, so the round-trip check is what forces the canonical uppercase
// form and rejects a traversal-shaped id like "../../evil" before it can name
// an export file.
func validateEntryID(id string) error {
	parsed, err := ulid.Parse(id)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrBadEntryID, id)
	}

	if parsed.String() != id {
		return fmt.Errorf("%w: %q is not the canonical form", ErrBadEntryID, id)
	}

	return nil
}

// splitMarkdownTags parses the comma-separated tags value, trimming whitespace
// and dropping empties, so an empty tags line yields no tags. Mirrors the CLI's
// splitTags shape without importing it across the package boundary.
func splitMarkdownTags(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	tags := make([]string, 0, len(parts))

	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			tags = append(tags, trimmed)
		}
	}

	return tags
}

// ExportDir writes one <id>.md per entry (flat, so the directory greps) into
// dir, creating it if needed, and returns the entry count. Each id is checked
// against the canonical ULID form before it composes a filename, as defense in
// depth against a row some other tool wrote with a traversal-shaped id.
func ExportDir(ctx context.Context, store Retriever, scope, dir string) (int, error) {
	entries, err := store.List(ctx, scope)
	if err != nil {
		return 0, fmt.Errorf("list entries for export: %w", err)
	}

	if err := os.MkdirAll(dir, exportDirPerm); err != nil {
		return 0, fmt.Errorf("create export directory %q: %w", dir, err)
	}

	for idx := range entries {
		entry := &entries[idx]

		if err := validateEntryID(entry.ID); err != nil {
			return 0, fmt.Errorf("export entry: %w", err)
		}

		path := filepath.Join(dir, entry.ID+markdownExt)
		if err := os.WriteFile(path, EncodeMarkdown(entry), exportFilePerm); err != nil {
			return 0, fmt.Errorf("write export file %q: %w", path, err)
		}
	}

	return len(entries), nil
}

// ImportSummary counts what an import did.
type ImportSummary struct {
	Added, Updated, Unchanged int
}

// importOutcome is the effect a single imported entry had on the store.
type importOutcome int

const (
	outcomeAdded importOutcome = iota
	outcomeUpdated
	outcomeUnchanged
)

// ImportDir parses every .md file directly in dir, then upserts them. It runs
// two passes: the first parses all files and aborts the whole import on any
// parse failure, so a half-edited directory can never half-import; the second
// applies. It never deletes; removal stays `cairnos memory rm`.
func ImportDir(ctx context.Context, store EntryStore, dir string) (ImportSummary, error) {
	parsed, err := parseImportDir(dir)
	if err != nil {
		return ImportSummary{}, err
	}

	var summary ImportSummary

	for idx := range parsed {
		outcome, applyErr := applyImported(ctx, store, &parsed[idx])
		if applyErr != nil {
			return ImportSummary{}, applyErr
		}

		switch outcome {
		case outcomeAdded:
			summary.Added++
		case outcomeUpdated:
			summary.Updated++
		case outcomeUnchanged:
			summary.Unchanged++
		}
	}

	return summary, nil
}

// parseImportDir is the first import pass: parse every .md file directly in dir
// in filename order, non-recursively, returning the entries or the first parse
// error. Nothing is written to the store here, so a bad file aborts before any
// change lands.
func parseImportDir(dir string) ([]Entry, error) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read import directory %q: %w", dir, err)
	}

	var parsed []Entry

	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() || !strings.HasSuffix(dirEntry.Name(), markdownExt) {
			continue
		}

		path := filepath.Join(dir, dirEntry.Name())

		data, readErr := os.ReadFile(path) // #nosec G304 -- path is the operator's import dir joined with a directory entry name; reading local files is the command's purpose
		if readErr != nil {
			return nil, fmt.Errorf("read import file %q: %w", path, readErr)
		}

		entry, parseErr := ParseMarkdown(data)
		if parseErr != nil {
			return nil, fmt.Errorf("parse %q: %w", path, parseErr)
		}

		parsed = append(parsed, entry)
	}

	return parsed, nil
}

// applyImported is the second import pass for one entry: a missing id is an Add
// (which preserves the brought id and CreatedAt), a present id whose scope,
// content, importance, or tags differ is a Replace (which refreshes UpdatedAt
// and re-estimates cost), and an identical entry is unchanged. The front-matter
// id is authoritative, not the filename.
func applyImported(ctx context.Context, store EntryStore, entry *Entry) (importOutcome, error) {
	existing, err := store.Get(ctx, entry.ID)
	if errors.Is(err, ErrEntryNotFound) {
		if _, addErr := store.Add(ctx, entry); addErr != nil {
			return outcomeAdded, fmt.Errorf("import add %s: %w", entry.ID, addErr)
		}

		return outcomeAdded, nil
	}

	if err != nil {
		return outcomeUnchanged, fmt.Errorf("import get %s: %w", entry.ID, err)
	}

	if entriesMatch(&existing, entry) {
		return outcomeUnchanged, nil
	}

	if err := store.Replace(ctx, entry.ID, entry); err != nil {
		return outcomeUpdated, fmt.Errorf("import replace %s: %w", entry.ID, err)
	}

	return outcomeUpdated, nil
}

// entriesMatch reports whether an imported entry carries no change against the
// stored one. It compares only the fields the file format carries: scope,
// content, importance, and tags. Timestamps and token cost are excluded because
// import never brings a new UpdatedAt and the store re-estimates cost.
func entriesMatch(stored, imported *Entry) bool {
	return stored.Scope == imported.Scope &&
		stored.Content == imported.Content &&
		stored.Importance == imported.Importance &&
		slices.Equal(stored.Tags, imported.Tags)
}
