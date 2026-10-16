package environ_test

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/environ"
)

const (
	// setKey and setValue are a variable with an ordinary non-empty value.
	setKey   = "SET"
	setValue = "value"
	// emptyKey names a variable set to the empty string, which must still
	// read as found.
	emptyKey = "EMPTY"
	// snapshotKey is the variable whose source map changes after Map copies
	// it, and originalValue is what the copy must keep.
	snapshotKey   = "KEY"
	originalValue = "first"
	// pathVar is the search-path variable LookPath reads from the lookup.
	pathVar = "PATH"
)

func TestMapAnswersOnlyWhatItWasGiven(t *testing.T) {
	t.Parallel()

	lookup := environ.Map(map[string]string{setKey: setValue, emptyKey: ""})

	tests := map[string]struct {
		key       string
		wantValue string
		wantFound bool
	}{
		"set key":            {key: setKey, wantValue: setValue, wantFound: true},
		"key set to nothing": {key: emptyKey, wantValue: "", wantFound: true},
		"absent key":         {key: "MISSING", wantValue: "", wantFound: false},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			value, found := lookup(test.key)
			if value != test.wantValue || found != test.wantFound {
				t.Fatalf("lookup(%q) = %q, %v; want %q, %v", test.key, value, found, test.wantValue, test.wantFound)
			}

			if got := lookup.Get(test.key); got != test.wantValue {
				t.Fatalf("Get(%q) = %q; want %q", test.key, got, test.wantValue)
			}
		})
	}
}

func TestMapIgnoresLaterMutation(t *testing.T) {
	t.Parallel()

	vars := map[string]string{snapshotKey: originalValue}
	lookup := environ.Map(vars)
	vars[snapshotKey] = "second"

	if got := lookup.Get(snapshotKey); got != originalValue {
		t.Fatalf("Get after caller mutated its map = %q; want %q", got, originalValue)
	}
}

func TestNilLookupReadsAsEmpty(t *testing.T) {
	t.Parallel()

	var lookup environ.Lookup

	if got := lookup.Get("ANY"); got != "" {
		t.Fatalf("Get on a nil Lookup = %q; want empty", got)
	}
}

func TestEnvironSkipsUnsetKeys(t *testing.T) {
	t.Parallel()

	lookup := environ.Map(map[string]string{"A": "1", "B": ""})

	got := environ.Environ(lookup, "A", "MISSING", "B")
	want := []string{"A=1", "B="}

	if !slices.Equal(got, want) {
		t.Fatalf("Environ = %v; want %v", got, want)
	}
}

func TestLookPathSearchesTheSuppliedPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tool := writeExecutable(t, dir, "cairn-tool")

	lookup := environ.Map(map[string]string{pathVar: dir})

	got, err := environ.LookPath(lookup, filepath.Base(tool))
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}

	if got != tool {
		t.Fatalf("LookPath = %q; want %q", got, tool)
	}
}

func TestLookPathAcceptsAPathBearingName(t *testing.T) {
	t.Parallel()

	tool := writeExecutable(t, t.TempDir(), "cairn-tool")

	// No PATH at all, so a hit here can only have come from the name itself.
	got, err := environ.LookPath(environ.Map(nil), tool)
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}

	if got != tool {
		t.Fatalf("LookPath = %q; want %q", got, tool)
	}
}

func TestLookPathReportsMisses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lookup := environ.Map(map[string]string{pathVar: dir})

	tests := map[string]string{
		"empty name":           "",
		"name not on the path": "cairn-absent",
		"path-bearing name":    filepath.Join(dir, "cairn-absent"),
	}

	for name, file := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got, err := environ.LookPath(lookup, file); err == nil {
				t.Fatalf("LookPath(%q) = %q, nil; want an error", file, got)
			}
		})
	}
}

// writeExecutable drops a runnable file in dir and answers with its full path.
// Windows has no execute bit and resolves by extension, so the name grows one.
func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}
