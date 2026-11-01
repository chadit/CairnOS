package config

import (
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/chadit/CairnOS/internal/environ"
)

// Source TOML keys. The parser matches against these and rejects anything else,
// so a typo'd key fails Load rather than leaving a checkout pointing at a ref or
// a path the operator never wrote, the same trap [credential.<name>] blocks use.
const (
	sourceKeyRepo       = "repo"
	sourceKeyDest       = "dest"
	sourceKeyRef        = "ref"
	sourceKeyDirtyReset = "dirty_reset"
	sourceKeyWorkflows  = "workflows"
	sourceKeyTrusted    = "trusted"
)

// DefaultSourceRef is what a block that names no ref follows.
const DefaultSourceRef = "main"

// defaultSourceDirtyReset is what a block naming no dirty_reset gets. True,
// because an unattended host that repairs its own checkout is worth more than
// one that keeps a stray edit and stops syncing. It is seeded before the key
// loop because a raw map cannot tell an absent bool from a false one afterwards.
const defaultSourceDirtyReset = true

// gitBinary is the executable every source operation shells out to. Config load
// refuses a host without it so the failure names the missing binary instead of
// surfacing later as a failed run.
const gitBinary = "git"

// sourceNamePattern is the block-name grammar. The name reaches a workflow
// param, a log field, and an error message, so it stays a plain identifier.
var sourceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// parseSourceBlocks turns the verbatim [source.<name>] blocks into typed
// Sources, rejecting unknown keys as it goes. Names are walked in sorted order
// so the first offending block is reported reproducibly.
func parseSourceBlocks(raw map[string]map[string]any) (map[string]Source, error) {
	sources := make(map[string]Source, len(raw))

	for _, name := range slices.Sorted(maps.Keys(raw)) {
		src, err := parseSourceBlock(name, raw[name])
		if err != nil {
			return nil, err
		}

		sources[name] = src
	}

	return sources, nil
}

// parseSourceBlock decodes one block strictly, seeding the defaults so an absent
// key carries what the sync will do. A key written blank keeps the blank and
// fails validation, since an operator who typed the key meant to name something.
// Keys are walked sorted so a block with several problems reports the same one
// each run.
func parseSourceBlock(name string, block map[string]any) (Source, error) {
	src := Source{Ref: DefaultSourceRef, DirtyReset: defaultSourceDirtyReset}

	for _, key := range slices.Sorted(maps.Keys(block)) {
		entry := sourceEntry{block: name, key: key, value: block[key]}
		if err := applySourceKey(&src, entry); err != nil {
			return Source{}, err
		}
	}

	return src, nil
}

// sourceEntry is one key of one block, carried together so every error names
// both without a wide parameter list.
type sourceEntry struct {
	value any
	block string
	key   string
}

// applySourceKey sets one parsed key on src or returns a naming error. It is
// split from the loop so the switch stays flat and each type assertion carries
// the key name into its error.
func applySourceKey(src *Source, entry sourceEntry) error {
	var err error

	switch entry.key {
	case sourceKeyRepo:
		src.Repo, err = sourceString(entry)
	case sourceKeyDest:
		src.Dest, err = sourceString(entry)
	case sourceKeyRef:
		src.Ref, err = sourceString(entry)
	case sourceKeyDirtyReset:
		src.DirtyReset, err = sourceBool(entry)
	case sourceKeyWorkflows:
		src.Workflows, err = sourceStrings(entry)
	case sourceKeyTrusted:
		src.Trusted, err = sourceBool(entry)
	default:
		err = fmt.Errorf("%w: source.%s has unknown key %q", ErrInvalidConfig, entry.block, entry.key)
	}

	return err
}

// sourceString asserts a string value or returns a key-named type error. The
// value is trimmed here so validation and the git argv see the same text: a ref
// of " main" would otherwise clear the blank check and then fail at boot naming a
// ref that reads as correct in the file.
func sourceString(entry sourceEntry) (string, error) {
	str, ok := entry.value.(string)
	if !ok {
		return "", fmt.Errorf("%w: source.%s.%s must be a string",
			ErrInvalidConfig, entry.block, entry.key)
	}

	return strings.TrimSpace(str), nil
}

// sourceStrings asserts an array of strings, trimming each element for the same
// reason sourceString trims: validation and the matcher must judge one text.
func sourceStrings(entry sourceEntry) ([]string, error) {
	items, ok := entry.value.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: source.%s.%s must be an array of strings",
			ErrInvalidConfig, entry.block, entry.key)
	}

	values := make([]string, 0, len(items))

	for _, item := range items {
		str, isString := item.(string)
		if !isString {
			return nil, fmt.Errorf("%w: source.%s.%s must be an array of strings",
				ErrInvalidConfig, entry.block, entry.key)
		}

		values = append(values, strings.TrimSpace(str))
	}

	return values, nil
}

// sourceBool asserts a bool value or returns a key-named type error. A quoted
// "true" is a mistake worth failing the load over, since the tree it governs is
// the one a sync may overwrite.
func sourceBool(entry sourceEntry) (bool, error) {
	flag, ok := entry.value.(bool)
	if !ok {
		return false, fmt.Errorf("%w: source.%s.%s must be a boolean",
			ErrInvalidConfig, entry.block, entry.key)
	}

	return flag, nil
}

// validateSources checks every parsed source and, when any exist, that the
// binary they all shell out to sits on the PATH lookup answers. homeDir is the
// config file's own directory, the home no synced tree may land inside, so a
// source declared in a scaffold cannot sync over it. Names are walked sorted so
// the first invalid block reports reproducibly.
func validateSources(sources map[string]Source, homeDir string, lookup environ.Lookup) error {
	if len(sources) == 0 {
		return nil
	}

	for _, name := range slices.Sorted(maps.Keys(sources)) {
		src := sources[name]
		if err := validateSource(name, &src, homeDir); err != nil {
			return err
		}
	}

	if _, err := environ.LookPath(lookup, gitBinary); err != nil {
		return fmt.Errorf("%w: a [source.<name>] block needs the %s binary on PATH: %w",
			ErrInvalidConfig, gitBinary, err)
	}

	return nil
}

// validateSource fails fast on a block that could not be cloned or could be
// cloned somewhere CairnOS already owns.
func validateSource(name string, src *Source, homeDir string) error {
	if !sourceNamePattern.MatchString(name) {
		return fmt.Errorf("%w: source.%s name must match %s", ErrInvalidConfig, name, sourceNamePattern)
	}

	if err := validateSourceArg(name, sourceKeyRepo, src.Repo); err != nil {
		return err
	}

	if err := validateSourceArg(name, sourceKeyRef, src.Ref); err != nil {
		return err
	}

	if err := validateSourceDest(name, src.Dest, homeDir); err != nil {
		return err
	}

	return validateSourceWorkflows(name, src.Workflows)
}

// validateSourceWorkflows refuses a glob that could name a file outside dest or
// that the matcher would reject at every sync. Discovery joins each match onto
// dest, so an absolute glob or a ".." element is the one way a pattern could
// reach a file the source does not carry.
func validateSourceWorkflows(name string, globs []string) error {
	for _, glob := range globs {
		if err := validateWorkflowGlob(glob); err != nil {
			return fmt.Errorf("%w: source.%s.%s %q %w", ErrInvalidConfig, name, sourceKeyWorkflows, glob, err)
		}
	}

	return nil
}

// validateWorkflowGlob checks one pattern. Matching runs on git's slash
// separated paths, so the checks split on "/" rather than the host separator.
// git never writes a "." or empty element or a trailing slash, so a glob with
// one would match nothing and discovery would stay silent about it.
func validateWorkflowGlob(glob string) error {
	if glob == "" {
		return errGlobEmpty
	}

	if path.IsAbs(glob) || filepath.IsAbs(glob) {
		return errGlobAbsolute
	}

	elements := strings.Split(glob, "/")

	switch {
	case slices.Contains(elements, ".."):
		return errGlobParent
	case slices.Contains(elements, "."):
		return errGlobDot
	case slices.Contains(elements, ""):
		return errGlobSlashes
	}

	if _, err := path.Match(glob, ""); err != nil {
		return fmt.Errorf("is not a valid pattern: %w", err)
	}

	return nil
}

// validateSourceArg rejects the two values that reach a git argv. Parsing has
// already trimmed them, so both clauses judge the same text. A blank one names
// nothing, and one starting with a dash would be read as an option by the tag
// command, which offers no end-of-options marker.
func validateSourceArg(name, key, value string) error {
	if value == "" {
		return fmt.Errorf("%w: source.%s.%s must be set", ErrInvalidConfig, name, key)
	}

	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%w: source.%s.%s %q must not start with a dash; git would read it as an option",
			ErrInvalidConfig, name, key, value)
	}

	return nil
}

// validateSourceDest keeps the checkout out of the CairnOS home. That directory
// holds state CairnOS prunes and backs up on its own schedule, so a checkout
// living there would be swept with it.
func validateSourceDest(name, dest, homeDir string) error {
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("%w: source.%s.dest must be an absolute path", ErrInvalidConfig, name)
	}

	clean := filepath.Clean(dest)
	home := filepath.Clean(homeDir)

	if clean == home || strings.HasPrefix(clean, home+string(filepath.Separator)) {
		return fmt.Errorf("%w: source.%s.dest %q must be outside the CairnOS directory %q",
			ErrInvalidConfig, name, clean, home)
	}

	return nil
}
