package source

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
)

// symlinkMode is the ls-tree mode of a committed symbolic link.
const symlinkMode = "120000"

// trustedRef names the ref whose tree spec's files are trusted as: origin's tip
// of the configured branch as last fetched, or the pinned tag. Never HEAD:
// anything on the host that can write the checkout can commit there, and with
// dirty_reset off a sync keeps that commit. Each fetch rewrites the
// remote-tracking ref to what origin holds.
func trustedRef(ctx context.Context, git Git, spec Spec) (string, error) {
	want, err := resolveRef(ctx, git, spec)
	if err != nil {
		return "", err
	}

	if want.branch {
		return remoteRef(want.name), nil
	}

	return "refs/tags/" + want.name, nil
}

// CommittedFiles lists, sorted, the paths at spec's trusted ref that match any
// of globs under path.Match, where * stops at a slash. It reads that ref's tree
// rather than HEAD, the index, or the work tree, so a file something on the
// host wrote, staged, or committed into the checkout is never listed.
func CommittedFiles(ctx context.Context, git Git, spec Spec, globs []string) ([]string, error) {
	ref, err := trustedRef(ctx, git, spec)
	if err != nil {
		return nil, err
	}

	out, err := git(ctx, spec.Dest, "ls-tree", "-r", "-z", "--name-only", ref)
	if err != nil {
		return nil, fmt.Errorf("list files at %s in %s: %w", ref, spec.Dest, err)
	}

	var matched []string

	for name := range strings.SplitSeq(out, "\x00") {
		if name != "" && MatchesAny(globs, name) {
			matched = append(matched, name)
		}
	}

	slices.Sort(matched)

	return matched, nil
}

// MatchesAny reports whether rel, a slash-separated path relative to dest,
// matches one of globs. Config load has already refused a malformed glob, so a
// match error is unreachable from config and is treated as no match.
func MatchesAny(globs []string, rel string) bool {
	for _, glob := range globs {
		ok, err := path.Match(glob, rel)
		if err == nil && ok {
			return true
		}
	}

	return false
}

// CommittedAs reports whether every path in want, slash-separated relative to
// spec.Dest, is a regular file at spec's trusted ref whose object ID is the one
// want maps it to. The caller hashes the bytes it actually read, so nothing
// here looks at the disk, and one ls-tree answers every path.
func CommittedAs(ctx context.Context, git Git, spec Spec, want map[string]string) (bool, error) {
	if len(want) == 0 {
		return false, nil
	}

	ref, err := trustedRef(ctx, git, spec)
	if err != nil {
		return false, err
	}

	argv := append([]string{"ls-tree", "-z", ref, "--"}, slices.Sorted(maps.Keys(want))...)

	out, err := git(ctx, spec.Dest, argv...)
	if err != nil {
		return false, fmt.Errorf("read files at %s in %s: %w", ref, spec.Dest, err)
	}

	committed := regularFiles(out)

	for rel, id := range want {
		if committed[rel] != id {
			return false, nil
		}
	}

	return true, nil
}

// regularFiles maps each "<mode> <type> <id>\t<path>" entry of ls-tree -z
// output to its object ID, keeping regular files only. ls-tree treats a path
// argument as a prefix, and a symlink or submodule is not the bytes a reviewer
// read, so the caller's exact-path lookup finds neither.
func regularFiles(out string) map[string]string {
	files := make(map[string]string)

	for entry := range strings.SplitSeq(out, "\x00") {
		meta, name, found := strings.Cut(entry, "\t")
		if !found {
			continue
		}

		fields := strings.Fields(meta)
		if len(fields) == 3 && fields[1] == "blob" && fields[0] != symlinkMode {
			files[name] = fields[2]
		}
	}

	return files
}
