//go:build !windows

package environ

import (
	"os"
	"strings"
)

// hasPathSeparator reports whether the name already says where the file lives.
func hasPathSeparator(file string) bool {
	return strings.ContainsRune(file, '/')
}

// candidates answers with the one name to try. Unix has no PATHEXT, so the
// Windows half of this pair is the only one that returns more than one.
func candidates(_ Lookup, path string) []string {
	return []string{path}
}

// isExecutable reports whether the path is a regular file someone may run. Any
// execute bit counts, the approximation os/exec makes rather than asking the
// kernel which of owner, group, or other this process is.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	mode := info.Mode()

	return !mode.IsDir() && mode&0o111 != 0
}
