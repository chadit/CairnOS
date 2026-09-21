package environ

import (
	"io/fs"
	"os/exec"
	"path/filepath"
)

// LookPath follows exec.LookPath's rules over the PATH the lookup answers, so a
// caller can aim a resolver at a temporary directory without changing what the
// rest of the process sees. A name carrying a separator is checked where it
// stands. The current directory is never searched, which exec.LookPath does on
// Windows, so the answer depends on the lookup alone.
func LookPath(lookup Lookup, file string) (string, error) {
	if file == "" {
		return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
	}

	if hasPathSeparator(file) {
		if path, ok := firstExecutable(candidates(lookup, file)); ok {
			return path, nil
		}

		return "", &exec.Error{Name: file, Err: fs.ErrNotExist}
	}

	for _, dir := range filepath.SplitList(lookup.Get("PATH")) {
		// An empty PATH element means the current directory, the same reading
		// the shell and os/exec give it.
		if dir == "" {
			dir = "."
		}

		if path, ok := firstExecutable(candidates(lookup, filepath.Join(dir, file))); ok {
			return path, nil
		}
	}

	return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
}

// firstExecutable answers with the first candidate that is a runnable file.
func firstExecutable(paths []string) (string, bool) {
	for _, path := range paths {
		if isExecutable(path) {
			return path, true
		}
	}

	return "", false
}
