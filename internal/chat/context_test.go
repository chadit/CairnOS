package chat_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// contextSectionHeader is the leading marker of a rendered workspace-context
// block, shared by the positive and negative assertions.
const contextSectionHeader = "Workspace context:"

// bigCap is a per-file cap large enough that a test file never truncates, used
// where the test proves containment rather than truncation.
const bigCap = 1 << 20

// contextDeps configures a context-and-@-ref chat agent for one test.
type contextDeps struct {
	contextDir string
	allowlist  []string
	maxBytes   int
}

// newContextAgent wires a lean chat agent over a real sqlite store, the scripted
// driver, and the workspace-context and @-file-reference settings under test.
// No tools or trigger: these turns exercise context loading and @-ref inlining.
func newContextAgent(t *testing.T, driver *scriptedDriver, deps contextDeps) *chat.Agent {
	t.Helper()

	db := openStore(t)

	return chat.New(&chat.Config{
		Store:         db,
		Driver:        driver,
		Personalities: chat.NewPersonalities(nil, ""),
		ContextDir:    deps.contextDir,
		FileAllowlist: deps.allowlist,
		MaxFileBytes:  deps.maxBytes,
		NewID:         sqlite.NewID,
	})
}

// newSessionCtx opens a session under a specific request context (its workspace
// claim), so a test can drive a turn for a non-default workspace.
func newSessionCtx(ctx context.Context, t *testing.T, agent *chat.Agent) string {
	t.Helper()

	session, err := agent.CreateSession(ctx, "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	return session.ID
}

// writeContextDoc writes context/<workspace>.md under dir and returns the dir.
func writeContextDoc(t *testing.T, dir, workspace, content string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir context dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, workspace+".md"), []byte(content), 0o600); err != nil {
		t.Fatalf("write context doc: %v", err)
	}
}

// toolNotes returns the content of every tool-role message a session persisted,
// so an @-ref test can assert on the notes without the user/assistant rows.
func toolNotes(t *testing.T, agent *chat.Agent, sessionID string) []string {
	t.Helper()

	messages, err := agent.ListMessages(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}

	var notes []string

	for _, msg := range messages {
		if msg.Role == store.ChatRoleTool {
			notes = append(notes, msg.Content)
		}
	}

	return notes
}

// TestContextDocRendersEveryIteration proves a present workspace context doc
// renders into every driver prompt of a multi-iteration turn, not just the
// first.
func TestContextDocRendersEveryIteration(t *testing.T) {
	t.Parallel()

	const doc = "Deploys freeze on Fridays. The staging box is box-7."

	contextDir := filepath.Join(t.TempDir(), contextDirName())
	writeContextDoc(t, contextDir, store.DefaultWorkspace, doc)

	driver := &scriptedDriver{turns: []runner.Result{
		toolTurn("noop", map[string]any{}),
		finalTurn(plainAnswer),
	}}
	agent := newContextAgent(t, driver, contextDeps{contextDir: contextDir})
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "how do deploys work"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	prompts := driver.allPrompts()
	if len(prompts) != 2 {
		t.Fatalf("driver saw %d prompts, want 2 iterations", len(prompts))
	}

	for idx, prompt := range prompts {
		if !containsAll(prompt, contextSectionHeader, doc) {
			t.Errorf("iteration %d prompt missing the context section; got:\n%s", idx, prompt)
		}
	}
}

// TestContextDocAbsentNoSection proves an absent doc means no section and no
// error: the turn answers normally with no context block in the prompt.
func TestContextDocAbsentNoSection(t *testing.T) {
	t.Parallel()

	contextDir := filepath.Join(t.TempDir(), contextDirName())

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newContextAgent(t, driver, contextDeps{contextDir: contextDir})
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "hello"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	if strings.Contains(driver.lastPrompt(t), contextSectionHeader) {
		t.Errorf("absent context doc still rendered a section")
	}
}

// TestContextDocUnreadableFailsTurn proves an unreadable doc fails the turn
// rather than silently dropping context the operator wrote.
func TestContextDocUnreadableFailsTurn(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("root bypasses file mode bits, so a mode-000 file stays readable")
	}

	contextDir := filepath.Join(t.TempDir(), contextDirName())
	writeContextDoc(t, contextDir, store.DefaultWorkspace, "secret ops doc")

	if err := os.Chmod(filepath.Join(contextDir, store.DefaultWorkspace+".md"), 0o000); err != nil {
		t.Fatalf("chmod context doc: %v", err)
	}

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newContextAgent(t, driver, contextDeps{contextDir: contextDir})
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "hello"); err == nil {
		t.Fatal("unreadable context doc did not fail the turn")
	}
}

// TestWorkspaceIDValidation proves a traversal-shaped workspace id fails the
// turn with chat.bad_workspace_id before any context path is built, while the
// default workspace passes unchanged.
func TestWorkspaceIDValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		workspace string
		wantCode  bool
	}{
		{name: "traversal", workspace: "../../x", wantCode: true},
		{name: "slash", workspace: "ops/sub", wantCode: true},
		{name: "default_passes", workspace: store.DefaultWorkspace, wantCode: false},
		{name: "hyphen_underscore_passes", workspace: "team-a_1", wantCode: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			contextDir := filepath.Join(t.TempDir(), contextDirName())

			driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
			agent := newContextAgent(t, driver, contextDeps{contextDir: contextDir})

			ctx := store.ContextWithWorkspace(t.Context(), tt.workspace)
			sessionID := newSessionCtx(ctx, t, agent)

			_, err := drive(t, func(out chan<- chat.Event) error {
				return agent.SendMessage(ctx, sessionID, "hello", out)
			})

			if tt.wantCode {
				if !hasCode(err, chat.CodeBadWorkspace) {
					t.Fatalf("workspace %q: error = %v, want chat.bad_workspace_id", tt.workspace, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("workspace %q: unexpected error %v", tt.workspace, err)
			}
		})
	}
}

// TestFileRefInlinesContained proves an @-reference inside an allowlisted root
// inlines its content as a persisted tool note that replays into the prompt.
func TestFileRefInlinesContained(t *testing.T) {
	t.Parallel()

	const content = "runbook: drain the node, then restart the agent."

	root := t.TempDir()
	target := filepath.Join(root, "runbook.md")

	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newContextAgent(t, driver, contextDeps{allowlist: []string{root}, maxBytes: bigCap})
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "read @"+target+" please"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	want := fmt.Sprintf("inlined @%s (%d bytes):\n%s", target, len(content), content)
	if notes := toolNotes(t, agent, sessionID); len(notes) != 1 || notes[0] != want {
		t.Fatalf("tool notes = %#v, want one inline note %q", notes, want)
	}

	if !strings.Contains(driver.lastPrompt(t), content) {
		t.Errorf("inlined content did not replay into the prompt")
	}
}

// TestFileRefContainmentRefusals covers the refusal cases that must all read the
// same note: a sibling directory sharing the root's name prefix, a symlink
// inside the root pointing outside, and a missing file.
func TestFileRefContainmentRefusals(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "data")

	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	sibling := filepath.Join(base, "databank")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}

	siblingFile := filepath.Join(sibling, "secret.txt")
	if err := os.WriteFile(siblingFile, []byte("sibling secret"), 0o600); err != nil {
		t.Fatalf("write sibling file: %v", err)
	}

	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("escaped"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatalf("symlink escape: %v", err)
	}

	tests := []struct {
		name   string
		target string
	}{
		{name: "sibling_prefix", target: siblingFile},
		{name: "symlink_escape", target: escape},
		{name: "missing_file", target: filepath.Join(root, "nope.txt")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
			agent := newContextAgent(t, driver, contextDeps{allowlist: []string{root}, maxBytes: bigCap})
			sessionID := newSession(t, agent)

			if _, err := sendMessage(t, agent, sessionID, "read @"+tt.target); err != nil {
				t.Fatalf("send message: %v", err)
			}

			want := fmt.Sprintf("refused @%s: outside the configured file allowlist", tt.target)
			if notes := toolNotes(t, agent, sessionID); len(notes) != 1 || notes[0] != want {
				t.Fatalf("tool notes = %#v, want one refusal note %q", notes, want)
			}
		})
	}
}

// TestFileRefSymlinkedRootMatches proves a root reached through a symlinked
// parent (the macOS /tmp -> /private/tmp shape) still contains a target after
// both sides normalize through EvalSymlinks.
func TestFileRefSymlinkedRootMatches(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	realDir := filepath.Join(base, "real")

	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real dir: %v", err)
	}

	const content = "reached through a symlinked root"

	if err := os.WriteFile(filepath.Join(realDir, "doc.md"), []byte(content), 0o600); err != nil {
		t.Fatalf("write doc: %v", err)
	}

	linkDir := filepath.Join(base, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink root: %v", err)
	}

	target := filepath.Join(linkDir, "doc.md")

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newContextAgent(t, driver, contextDeps{allowlist: []string{linkDir}, maxBytes: bigCap})
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "read @"+target); err != nil {
		t.Fatalf("send message: %v", err)
	}

	notes := toolNotes(t, agent, sessionID)
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "inlined @"+target) {
		t.Fatalf("tool notes = %#v, want one inline note for the symlinked-root target", notes)
	}
}

// TestFileRefOverCapTruncates proves a contained file larger than the cap
// truncates with the byte counts in the record.
func TestFileRefOverCapTruncates(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "big.txt")

	content := strings.Repeat("x", 200)
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}

	const limit = 64

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newContextAgent(t, driver, contextDeps{allowlist: []string{root}, maxBytes: limit})
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "read @"+target); err != nil {
		t.Fatalf("send message: %v", err)
	}

	want := fmt.Sprintf("inlined @%s (first %d of %d bytes):\n%s", target, limit, len(content), content[:limit])
	if notes := toolNotes(t, agent, sessionID); len(notes) != 1 || notes[0] != want {
		t.Fatalf("tool notes = %#v, want one truncated note %q", notes, want)
	}
}

// TestFileRefLimitNote proves the first maxFileRefsPerTurn candidates process
// and one note records the ignored overflow.
func TestFileRefLimitNote(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	refs := make([]string, 0, 9)
	for idx := range 9 {
		refs = append(refs, "@"+filepath.Join(root, fmt.Sprintf("f%d.txt", idx)))
	}

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newContextAgent(t, driver, contextDeps{allowlist: []string{root}, maxBytes: bigCap})
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, strings.Join(refs, " ")); err != nil {
		t.Fatalf("send message: %v", err)
	}

	notes := toolNotes(t, agent, sessionID)
	// Eight processed refs (all missing, so refusals) plus one limit note.
	if len(notes) != 9 {
		t.Fatalf("persisted %d tool notes, want 8 processed + 1 limit note", len(notes))
	}

	if want := "ignored 1 additional @-references (limit 8 per message)"; notes[8] != want {
		t.Fatalf("last note = %q, want %q", notes[8], want)
	}
}

// TestFileRefEmptyAllowlistInert proves the secure default: an empty allowlist
// leaves every @-token inert, with no reads and no notes.
func TestFileRefEmptyAllowlistInert(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "doc.md")

	if err := os.WriteFile(target, []byte("readable"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newContextAgent(t, driver, contextDeps{maxBytes: bigCap})
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "read @"+target); err != nil {
		t.Fatalf("send message: %v", err)
	}

	if notes := toolNotes(t, agent, sessionID); len(notes) != 0 {
		t.Fatalf("empty allowlist produced tool notes: %#v", notes)
	}
}

// TestFileRefNonAbsoluteInert proves @here, @alice, and relative @-tokens are
// inert text even with an allowlist configured, so ordinary mentions never turn
// into refusal noise.
func TestFileRefNonAbsoluteInert(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newContextAgent(t, driver, contextDeps{allowlist: []string{root}, maxBytes: bigCap})
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "ping @here and @alice about ./notes and @../rel"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	if notes := toolNotes(t, agent, sessionID); len(notes) != 0 {
		t.Fatalf("non-absolute @-tokens produced tool notes: %#v", notes)
	}
}

// contextDirName is the scaffold subdirectory name for per-workspace context
// docs, mirrored here so the tests build the same path serve wiring does.
func contextDirName() string { return "context" }
