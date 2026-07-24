package skills_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/skills"
)

// filePerm is the mode test skill files are written with.
const filePerm = 0o600

// newLibrary builds a Library over dir with a warn-capturing logger, so a test
// can prove a broken or shadowed file logged rather than failing silently.
func newLibrary(t *testing.T, dir string) (*skills.Library, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	return skills.New(dir, logger), buf
}

// writeSkill writes one skill file under dir, failing the test on error.
func writeSkill(t *testing.T, dir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), filePerm); err != nil {
		t.Fatalf("write skill %s: %v", name, err)
	}
}

// TestLoadParsesFrontmatter proves the happy path: a name/description
// frontmatter block parses into the fields and the body is everything after the
// closing fence.
func TestLoadParsesFrontmatter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSkill(t, dir, "deploy.md", "---\nname: deploy-check\ndescription: Verify a host before deploy.\n---\nRun the preflight checks.\n")

	lib, _ := newLibrary(t, dir)

	skill, err := lib.Load("deploy-check")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if skill.Name != "deploy-check" {
		t.Errorf("name = %q, want deploy-check", skill.Name)
	}

	if skill.Description != "Verify a host before deploy." {
		t.Errorf("description = %q", skill.Description)
	}

	if want := "Run the preflight checks.\n"; skill.Body != want {
		t.Errorf("body = %q, want %q", skill.Body, want)
	}
}

// TestNameFallsBackToStem proves a file with no frontmatter lists and loads
// under its file stem with an empty description and the whole content as body.
func TestNameFallsBackToStem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSkill(t, dir, "summarize.md", "Summarize the thread.\n")

	lib, _ := newLibrary(t, dir)

	skill, err := lib.Load("summarize")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if skill.Description != "" {
		t.Errorf("description = %q, want empty", skill.Description)
	}

	if want := "Summarize the thread.\n"; skill.Body != want {
		t.Errorf("body = %q, want %q", skill.Body, want)
	}
}

// TestBadFrontmatterFailsFile covers the three ErrBadFrontmatter shapes: an
// unknown key, an unclosed block, and an effective name outside the charset.
// Each fails the file, so a Load for that name returns ErrNotFound (the file
// was skipped) rather than a partial parse.
func TestBadFrontmatterFailsFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    string
		content string
	}{
		{
			name:    "unknown_key",
			file:    "a.md",
			content: "---\nname: a\ncolor: blue\n---\nbody\n",
		},
		{
			name:    "unclosed_block",
			file:    "b.md",
			content: "---\nname: b\ndescription: no closing fence\nstill going\n",
		},
		{
			name:    "bad_charset_name",
			file:    "c.md",
			content: "---\nname: Bad_Name\n---\nbody\n",
		},
		{
			name:    "bad_charset_stem",
			file:    "Bad Stem.md",
			content: "no frontmatter here\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			writeSkill(t, dir, tt.file, tt.content)

			lib, warnings := newLibrary(t, dir)

			if got := lib.List(); len(got) != 0 {
				t.Fatalf("List() = %d skills, want 0: a broken file must not list", len(got))
			}

			if !strings.Contains(warnings.String(), "skipping unloadable skill file") {
				t.Errorf("no warn logged for the broken file; log: %s", warnings.String())
			}
		})
	}
}

// TestBrokenFileNeverMasksSibling is the resolver property: a broken file is
// skipped with a warn while a healthy sibling still lists and loads.
func TestBrokenFileNeverMasksSibling(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSkill(t, dir, "aaa.md", "---\nname: broken\nunknown: key\n---\nbody\n")
	writeSkill(t, dir, "bbb.md", "---\nname: healthy\ndescription: works.\n---\nall good\n")

	lib, warnings := newLibrary(t, dir)

	listed := lib.List()
	if len(listed) != 1 || listed[0].Name != "healthy" {
		t.Fatalf("List() = %+v, want one healthy skill", listed)
	}

	if _, err := lib.Load("healthy"); err != nil {
		t.Errorf("load healthy: %v", err)
	}

	if !strings.Contains(warnings.String(), "aaa.md") {
		t.Errorf("broken file not named in warn; log: %s", warnings.String())
	}
}

// TestOversizeTruncates proves the body and description caps: a body over 16 KiB
// and a description over 256 bytes each clip with the truncation marker.
func TestOversizeTruncates(t *testing.T) {
	t.Parallel()

	const marker = "... (truncated)"

	dir := t.TempDir()

	longDescription := strings.Repeat("d", 300)
	longBody := strings.Repeat("x", (16<<10)+500)
	writeSkill(t, dir, "big.md", "---\nname: big\ndescription: "+longDescription+"\n---\n"+longBody)

	lib, _ := newLibrary(t, dir)

	skill, err := lib.Load("big")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(skill.Description) != 256+len(marker) || !strings.HasSuffix(skill.Description, marker) {
		t.Errorf("description len = %d, want 256+marker with marker suffix", len(skill.Description))
	}

	if len(skill.Body) != (16<<10)+len(marker) || !strings.HasSuffix(skill.Body, marker) {
		t.Errorf("body len = %d, want 16KiB+marker with marker suffix", len(skill.Body))
	}
}

// TestRescanFreshness proves the no-cache contract: a file written between two
// calls resolves on the second with no re-registration.
func TestRescanFreshness(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lib, _ := newLibrary(t, dir)

	if _, err := lib.Load("late"); err == nil {
		t.Fatal("load before write succeeded, want ErrNotFound")
	}

	writeSkill(t, dir, "late.md", "---\nname: late\n---\narrived later\n")

	skill, err := lib.Load("late")
	if err != nil {
		t.Fatalf("load after write: %v", err)
	}

	if want := "arrived later\n"; skill.Body != want {
		t.Errorf("body = %q, want %q", skill.Body, want)
	}
}

// TestDuplicateNameLexicalFirstWins proves that when two files carry the same
// effective name, the lexically-first filename wins and the shadowed file logs
// a warn.
func TestDuplicateNameLexicalFirstWins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSkill(t, dir, "aaa.md", "---\nname: dup\n---\nfirst file\n")
	writeSkill(t, dir, "bbb.md", "---\nname: dup\n---\nsecond file\n")

	lib, warnings := newLibrary(t, dir)

	skill, err := lib.Load("dup")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if want := "first file\n"; skill.Body != want {
		t.Errorf("body = %q, want the lexically-first file %q", skill.Body, want)
	}

	if listed := lib.List(); len(listed) != 1 {
		t.Errorf("List() = %d skills, want 1: the duplicate collapses", len(listed))
	}

	if !strings.Contains(warnings.String(), "shadowed") {
		t.Errorf("no shadow warn logged; log: %s", warnings.String())
	}
}

// TestLoadUnknownReturnsNotFound proves Load reports ErrNotFound for a name no
// file carries.
func TestLoadUnknownReturnsNotFound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSkill(t, dir, "present.md", "---\nname: present\n---\nhere\n")

	lib, _ := newLibrary(t, dir)

	_, err := lib.Load("absent")
	if !errors.Is(err, skills.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}
