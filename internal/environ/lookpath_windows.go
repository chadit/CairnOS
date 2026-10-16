//go:build windows

package environ

import (
	"os"
	"strings"
)

// defaultPathExt stands in when PATHEXT is unset, matching what the Windows
// command processor falls back to.
const defaultPathExt = ".COM;.EXE;.BAT;.CMD"

// hasPathSeparator reports whether the name already says where the file lives.
// A drive letter counts: "c:tool" is relative to that drive, not to PATH.
func hasPathSeparator(file string) bool {
	return strings.ContainsAny(file, `:\/`)
}

// candidates answers with the bare path first, so a name that already carries
// its extension resolves to itself, then each PATHEXT spelling of it.
func candidates(lookup Lookup, path string) []string {
	pathExt := lookup.Get("PATHEXT")
	if pathExt == "" {
		pathExt = defaultPathExt
	}

	paths := []string{path}

	for ext := range strings.SplitSeq(pathExt, ";") {
		if ext != "" {
			paths = append(paths, path+ext)
		}
	}

	return paths
}

// isExecutable reports whether the path is a file at all. Windows carries no
// execute bit, so the extension the caller arrived with is the only filter.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return !info.IsDir()
}
