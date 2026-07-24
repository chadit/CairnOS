package skills

import "errors"

// Sentinel errors for the skills library, per package convention: typed
// sentinels callers match with errors.Is and wrap with %w.
var (
	// ErrNotFound reports that Load found no skill with the requested name in
	// the directory. The chat adapter maps it onto chat.ErrSkillNotFound.
	ErrNotFound = errors.New("skill not found")

	// ErrBadFrontmatter reports a file whose frontmatter is malformed: an
	// unclosed block, an unknown key, a line that is not key: value, or an
	// effective name outside ^[a-z][a-z0-9_-]*$. The file is skipped with a warn
	// rather than guessed at, which is also the guard rail that keeps this parser
	// from creeping toward a YAML re-implementation.
	ErrBadFrontmatter = errors.New("bad skill frontmatter")
)
