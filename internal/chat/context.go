package chat

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/chadit/CairnOS/internal/store"
)

// Byte and count caps for the workspace-context and @-file-reference features.
// The shift on the doc cap is byte-size arithmetic (8 * 1024).
const (
	// maxContextDocBytes bounds one workspace context doc so a large file cannot
	// dominate every driver prompt of a turn.
	maxContextDocBytes = 8 << 10 // 8 KiB

	// maxFileRefsPerTurn caps how many @-file references one message may inline,
	// so a message packed with paths cannot fan out into an unbounded read count.
	maxFileRefsPerTurn = 8
)

// contextDocSuffix is the extension of a per-workspace context doc under the
// context directory: context/<workspace>.md.
const contextDocSuffix = ".md"

// fileRefSigil marks an @-file reference token in a user message.
const fileRefSigil = '@'

// Trust posture. Context docs and @-file roots are operator-authored local
// configuration under the CairnOS home, the same trust class as workflow TOML
// and chat personalities. Nothing here fetches over the network. Both features
// influence prompt text only: a context doc or an inlined file adds no tool,
// raises no budget, and cannot move the D9 confirmation gate. Two walls guard
// the reads all the same, because a request-supplied workspace id and a
// user-typed path are attacker-influenced even when the files are not: the
// workspace id is charset-checked before any context path is built, and an
// @-ref path must resolve inside an operator-configured allowlist root.

// A node without a context directory runs chat unchanged. The request
// workspace is charset-checked before a path is built: a traversal-shaped id
// fails the turn visibly rather than reading another workspace's file. A read
// error other than absence fails the turn because silently dropping context
// the operator wrote would be a surprise.

// loadContext reads the per-workspace context doc once for this turn and holds
// it on the turn, so every driver iteration renders the same section. Context
// is off when no context directory is wired. An absent doc is no section and
// no error; any other read error fails the turn.
func (t *chatTurn) loadContext(ctx context.Context) error {
	if t.agent.cfg.ContextDir == "" {
		return nil
	}

	workspace := store.WorkspaceFromContext(ctx)
	if !validWorkspaceID(workspace) {
		return errf(CodeBadWorkspace,
			"request workspace %q is not a valid id (must match ^[A-Za-z0-9_-]+$)", workspace)
	}

	path := filepath.Join(t.agent.cfg.ContextDir, workspace+contextDocSuffix)

	// #nosec G304 -- the context directory is the operator's own CairnOS home and the workspace component is charset-checked above
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("read workspace context: %w", err)
	}

	t.contextDoc = truncateContextDoc(string(data))

	return nil
}

// validWorkspaceID reports whether id matches ^[A-Za-z0-9_-]+$. WorkspaceFromContext
// returns the token's workspace claim verbatim and tenancy.CreateWorkspace does
// not charset-check it, so a request could otherwise carry a traversal-shaped id
// like ../../x whose contents would land in the prompt and the transcript. The
// default workspace ("default") passes, so unauthenticated local use is
// unaffected.
func validWorkspaceID(id string) bool {
	if id == "" {
		return false
	}

	for idx := range len(id) {
		char := id[idx]

		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '_' || char == '-':
		default:
			return false
		}
	}

	return true
}

// renderContextSection renders the workspace-context block for a driver prompt
// when the doc exists. It is empty when no doc loaded, so the prompt layout
// collapses to skill and history.
func renderContextSection(doc string) string {
	if doc == "" {
		return ""
	}

	return "Workspace context:\n" + doc + "\n\n"
}

// truncateContextDoc clips a context doc to the cap with the standard marker,
// so an oversize doc reads honestly rather than silently losing its tail.
func truncateContextDoc(doc string) string {
	if len(doc) <= maxContextDocBytes {
		return doc
	}

	return doc[:maxContextDocBytes] + truncationMarkerText
}

// truncationMarkerText trails any value clipped to a cap, matching agent.go's
// truncateResult so a truncated value reads the same across the chat surface.
const truncationMarkerText = "... (truncated)"

// inlineFileRefs processes the @-file references in a user message, persisting
// one tool-role note per reference so the content (or the refusal) replays
// into later turns through the existing history mechanism. An empty allowlist
// is a no-op, so the secure-by-default posture leaves every @-token inert.
// Past maxFileRefsPerTurn candidates it processes the first cap and records
// one ignored-count note.
func (t *chatTurn) inlineFileRefs(ctx context.Context, userMessage string) error {
	if len(t.agent.cfg.FileAllowlist) == 0 {
		return nil
	}

	candidates := fileRefCandidates(userMessage)
	if len(candidates) == 0 {
		return nil
	}

	var overflow int
	if len(candidates) > maxFileRefsPerTurn {
		overflow = len(candidates) - maxFileRefsPerTurn
		candidates = candidates[:maxFileRefsPerTurn]
	}

	for _, target := range candidates {
		if err := t.persistToolNote(ctx, t.fileRefNote(target)); err != nil {
			return err
		}
	}

	if overflow > 0 {
		note := fmt.Sprintf("ignored %d additional @-references (limit %d per message)", overflow, maxFileRefsPerTurn)
		if err := t.persistToolNote(ctx, note); err != nil {
			return err
		}
	}

	return nil
}

// fileRefCandidates returns the absolute-path @-references in a message: a
// whitespace-delimited token shaped @<absolute path>. Anything else (@here,
// @alice, a relative path) is inert text, so an ordinary mention never turns
// into refusal noise.
func fileRefCandidates(message string) []string {
	var refs []string

	for token := range strings.FieldsSeq(message) {
		if len(token) < 2 || token[0] != fileRefSigil {
			continue
		}

		path := token[1:]
		if !filepath.IsAbs(path) {
			continue
		}

		refs = append(refs, path)
	}

	return refs
}

// fileRefNote builds the tool-role note for one @-reference: the inlined
// content when the path resolves inside an allowlist root and reads, a
// byte-counted truncation when it is over the cap, or the refusal note
// otherwise. A missing file and an out-of-root path read the same refusal, so
// probing for a file outside the roots cannot tell existence from denial.
func (t *chatTurn) fileRefNote(target string) string {
	resolved, ok := containedInAny(t.agent.cfg.FileAllowlist, target)
	if !ok {
		return refusedFileNote(target)
	}

	// #nosec G304 -- the path passed the operator-configured allowlist containment check above
	data, err := os.ReadFile(resolved)
	if err != nil {
		return refusedFileNote(target)
	}

	limit := t.agent.cfg.MaxFileBytes
	if limit > 0 && len(data) > limit {
		return fmt.Sprintf("inlined @%s (first %d of %d bytes):\n%s", target, limit, len(data), data[:limit])
	}

	return fmt.Sprintf("inlined @%s (%d bytes):\n%s", target, len(data), data)
}

// refusedFileNote is the single refusal note both out-of-root and missing-file
// paths take, so the transcript never distinguishes the two.
func refusedFileNote(target string) string {
	return fmt.Sprintf("refused @%s: outside the configured file allowlist", target)
}

// containedInAny reports the resolved path of target when at least one
// allowlist root contains it.
func containedInAny(roots []string, target string) (string, bool) {
	for _, root := range roots {
		if resolved, ok := contained(root, target); ok {
			return resolved, true
		}
	}

	return "", false
}

// contained resolves target against one allowlisted root. Both sides go through
// Abs then EvalSymlinks, so a symlink inside a root cannot point out of it
// (macOS /tmp -> /private/tmp normalizes identically). The comparison uses
// filepath.Rel, so a sibling sharing the root's name prefix (root /data,
// target /databank/secret) is rejected. Any resolve error, including a missing
// file, reports not-contained.
func contained(root, target string) (string, bool) {
	realRoot, err := filepath.EvalSymlinks(absOf(root))
	if err != nil {
		return "", false
	}

	realTarget, err := filepath.EvalSymlinks(absOf(target))
	if err != nil {
		return "", false
	}

	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}

	return realTarget, true
}

// absOf returns the absolute form of path, falling back to path itself when the
// working directory cannot be resolved, so the caller's EvalSymlinks still runs
// on a best-effort absolute path.
func absOf(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}

	return abs
}
