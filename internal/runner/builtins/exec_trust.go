package builtins

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/source"
	"github.com/chadit/CairnOS/pkg/runner"
)

// trustCheckTimeout bounds the git reads one trust check makes. The load pass
// validates without a deadline, so without a bound a hung disk would hang a
// run's start.
const trustCheckTimeout = 10 * time.Second

// ExecTrust names source checkouts whose committed workflow files may run exec
// actions without an allow_actions entry. allow_actions stops an agent loop
// enabling exec itself; a trusted source's files change only through an
// owner-reviewed push, which carries the same consent.
//
// Nothing is cached: checks are cheap git calls, and a kept answer could
// outlive a fetch that dropped the file.
type ExecTrust struct {
	// Git reads the trusted ref in each checkout.
	Git source.Git

	// Sources are the sources marked trusted; each one's Dest and Ref decide
	// which tree is trusted.
	Sources []source.Spec
}

// enabled reports whether any source is trusted, so a host without one pays a
// length check and no git call.
func (t ExecTrust) enabled() bool {
	return len(t.Sources) > 0 && t.Git != nil
}

// allows reports whether every file the action was read from is, byte for
// byte, the file at a trusted source's trusted ref. The workflow file picks the
// source, and each stdin_file it included must sit in the same one. An error
// means the check itself failed, which denies the action.
func (t ExecTrust) allows(ctx context.Context, definition []runner.DefinitionFile) (bool, error) {
	if !t.enabled() || len(definition) == 0 {
		return false, nil
	}

	for _, spec := range t.Sources {
		want, inside := relativeIDs(spec.Dest, definition)
		if !inside {
			continue
		}

		checkCtx, cancel := context.WithTimeout(ctx, trustCheckTimeout)
		allowed, err := source.CommittedAs(checkCtx, t.Git, spec, want)

		cancel()

		if err != nil {
			return false, fmt.Errorf("check %s against source %s: %w", definition[0].Path, spec.Name, err)
		}

		return allowed, nil
	}

	return false, nil
}

// relativeIDs maps each definition file's path relative to dest onto its
// object ID, or reports false when any file falls outside dest. Paths are
// resolved through symlinks first, so a link inside a checkout pointing
// elsewhere is judged by where it lands, and a checkout reached through a
// linked parent still matches.
func relativeIDs(dest string, definition []runner.DefinitionFile) (map[string]string, bool) {
	root, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return nil, false
	}

	want := make(map[string]string, len(definition))

	for _, file := range definition {
		rel, inside := within(root, file.Path)
		if !inside {
			return nil, false
		}

		want[rel] = file.BlobID
	}

	return want, true
}

// within reports whether path resolves to somewhere strictly inside root,
// returning the slash-separated path git expects relative to it.
func within(root, path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}

	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}

	return filepath.ToSlash(rel), true
}
