package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
)

// sourceRepoURL is the clone URL every fixture block names. Nothing dials it:
// config validation only checks that the key carries a value.
const sourceRepoURL = "https://github.com/example/automation.git"

// wantWorkflowsArray is the refusal for a workflows value that is not an array
// of strings, whether the array itself or one element is the wrong type.
const wantWorkflowsArray = "source.automation.workflows must be an array of strings"

// wantGlobSlashes is the refusal for a glob that has a doubled or trailing
// slash, the two spellings that leave an empty element.
const wantGlobSlashes = "must not contain an empty element or end in a slash"

// sourceTOML builds one [source.automation] block from the lines a case
// wants, so each test states only the keys it is about.
func sourceTOML(lines ...string) string {
	return "[source.automation]\n" + strings.Join(lines, "\n") + "\n"
}

func TestLoadSourceParsesBlock(t *testing.T) {
	t.Parallel()

	toml := sourceTOML(
		`repo = "`+sourceRepoURL+`"`,
		`dest = "/opt/src/automation"`,
		`ref  = "v1.4.2"`,
	)

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	src, ok := cfg.Sources["automation"]
	if !ok {
		t.Fatal("source automation missing from parsed config")
	}

	if src.Repo != sourceRepoURL {
		t.Fatalf("repo = %q, want %q", src.Repo, sourceRepoURL)
	}

	if src.Dest != "/opt/src/automation" {
		t.Fatalf("dest = %q", src.Dest)
	}

	if src.Ref != "v1.4.2" {
		t.Fatalf("ref = %q, want v1.4.2", src.Ref)
	}
}

func TestLoadSourceDefaultsRef(t *testing.T) {
	t.Parallel()

	toml := sourceTOML(
		`repo = "`+sourceRepoURL+`"`,
		`dest = "/opt/src/automation"`,
	)

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Sources["automation"].Ref; got != config.DefaultSourceRef {
		t.Fatalf("ref = %q, want the %q default", got, config.DefaultSourceRef)
	}
}

// TestLoadSourceDefaultsDirtyReset covers the half of FR1 a raw map cannot
// recover later: an absent key has to arrive as true, not as the zero value.
func TestLoadSourceDefaultsDirtyReset(t *testing.T) {
	t.Parallel()

	toml := sourceTOML(
		`repo = "`+sourceRepoURL+`"`,
		`dest = "/opt/src/automation"`,
	)

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Sources["automation"].DirtyReset {
		t.Fatal("dirty_reset = false with the key absent, want the true default")
	}
}

func TestLoadSourceAcceptsDirtyResetFalse(t *testing.T) {
	t.Parallel()

	toml := sourceTOML(
		`repo        = "`+sourceRepoURL+`"`,
		`dest        = "/opt/src/automation"`,
		`dirty_reset = false`,
	)

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Sources["automation"].DirtyReset {
		t.Fatal("dirty_reset = true with the key written false")
	}
}

func TestLoadSourceParsesDiscoveryKeys(t *testing.T) {
	t.Parallel()

	toml := sourceTOML(
		`repo      = "`+sourceRepoURL+`"`,
		`dest      = "/opt/src/automation"`,
		`workflows = ["engines/*/cairnos/*.toml", " jobs/*.toml "]`,
		`trusted   = true`,
	)

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	src := cfg.Sources["automation"]

	want := []string{"engines/*/cairnos/*.toml", "jobs/*.toml"}
	if !slices.Equal(src.Workflows, want) {
		t.Fatalf("workflows = %q, want %q", src.Workflows, want)
	}

	if !src.Trusted {
		t.Fatal("trusted = false with the key written true")
	}
}

// TestLoadSourceLeavesDiscoveryOffByDefault proves a block written before the
// two keys existed keeps its old behavior: nothing discovered, nothing trusted.
func TestLoadSourceLeavesDiscoveryOffByDefault(t *testing.T) {
	t.Parallel()

	toml := sourceTOML(
		`repo = "`+sourceRepoURL+`"`,
		`dest = "/opt/src/automation"`,
	)

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	src := cfg.Sources["automation"]
	if len(src.Workflows) != 0 || src.Trusted {
		t.Fatalf("workflows = %q, trusted = %v, want none and false", src.Workflows, src.Trusted)
	}
}

func TestLoadSourceRejectsBadBlocks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "unknown branch key",
			toml: sourceTOML(
				`repo   = "`+sourceRepoURL+`"`,
				`dest   = "/opt/src/automation"`,
				`branch = "main"`,
			),
			want: `unknown key "branch"`,
		},
		{
			name: "non-string ref",
			toml: sourceTOML(
				`repo = "`+sourceRepoURL+`"`,
				`dest = "/opt/src/automation"`,
				`ref  = 7`,
			),
			want: "source.automation.ref must be a string",
		},
		{
			name: "non-bool dirty_reset",
			toml: sourceTOML(
				`repo        = "`+sourceRepoURL+`"`,
				`dest        = "/opt/src/automation"`,
				`dirty_reset = "true"`,
			),
			want: "source.automation.dirty_reset must be a boolean",
		},
		{
			name: "non-string repo",
			toml: sourceTOML(
				`repo = 7`,
				`dest = "/opt/src/automation"`,
			),
			want: "source.automation.repo must be a string",
		},
		{
			name: "non-string dest",
			toml: sourceTOML(
				`repo = "`+sourceRepoURL+`"`,
				`dest = 7`,
			),
			want: "source.automation.dest must be a string",
		},
		{
			name: "missing repo",
			toml: sourceTOML(`dest = "/opt/src/automation"`),
			want: "source.automation.repo must be set",
		},
		{
			name: "blank ref",
			toml: sourceTOML(
				`repo = "`+sourceRepoURL+`"`,
				`dest = "/opt/src/automation"`,
				`ref  = "   "`,
			),
			want: "source.automation.ref must be set",
		},
		{
			name: "ref that git would read as an option",
			toml: sourceTOML(
				`repo = "`+sourceRepoURL+`"`,
				`dest = "/opt/src/automation"`,
				`ref  = "-x*"`,
			),
			want: wantDashRefusal,
		},
		{
			name: "repo that git would read as an option",
			toml: sourceTOML(
				`repo = "--upload-pack=touch /tmp/pwned"`,
				`dest = "/opt/src/automation"`,
			),
			want: wantDashRefusal,
		},
		{
			name: "relative dest",
			toml: sourceTOML(
				`repo = "`+sourceRepoURL+`"`,
				`dest = "src/automation"`,
			),
			want: "must be an absolute path",
		},
		{
			name: "workflows not an array",
			toml: sourceTOML(
				`repo      = "`+sourceRepoURL+`"`,
				`dest      = "/opt/src/automation"`,
				`workflows = "engines/*.toml"`,
			),
			want: wantWorkflowsArray,
		},
		{
			name: "workflows element not a string",
			toml: sourceTOML(
				`repo      = "`+sourceRepoURL+`"`,
				`dest      = "/opt/src/automation"`,
				`workflows = [7]`,
			),
			want: wantWorkflowsArray,
		},
		{
			name: "blank workflow glob",
			toml: sourceTOML(
				`repo      = "`+sourceRepoURL+`"`,
				`dest      = "/opt/src/automation"`,
				`workflows = ["  "]`,
			),
			want: "must not be empty",
		},
		{
			name: "absolute workflow glob",
			toml: sourceTOML(
				`repo      = "`+sourceRepoURL+`"`,
				`dest      = "/opt/src/automation"`,
				`workflows = ["/etc/*.toml"]`,
			),
			want: "must be relative to dest",
		},
		{
			name: "workflow glob climbing out of dest",
			toml: sourceTOML(
				`repo      = "`+sourceRepoURL+`"`,
				`dest      = "/opt/src/automation"`,
				`workflows = ["engines/../../*.toml"]`,
			),
			want: "must not contain a .. element",
		},
		{
			name: "workflow glob with a leading dot element",
			toml: sourceTOML(
				`repo      = "`+sourceRepoURL+`"`,
				`dest      = "/opt/src/automation"`,
				`workflows = ["./engines/*.toml"]`,
			),
			want: "must not contain a . element",
		},
		{
			name: "workflow glob with a doubled slash",
			toml: sourceTOML(
				`repo      = "`+sourceRepoURL+`"`,
				`dest      = "/opt/src/automation"`,
				`workflows = ["engines//*.toml"]`,
			),
			want: wantGlobSlashes,
		},
		{
			name: "workflow glob with a trailing slash",
			toml: sourceTOML(
				`repo      = "`+sourceRepoURL+`"`,
				`dest      = "/opt/src/automation"`,
				`workflows = ["engines/*/"]`,
			),
			want: wantGlobSlashes,
		},
		{
			name: "malformed workflow glob",
			toml: sourceTOML(
				`repo      = "`+sourceRepoURL+`"`,
				`dest      = "/opt/src/automation"`,
				`workflows = ["engines/[*.toml"]`,
			),
			want: "is not a valid pattern",
		},
		{
			name: "non-bool trusted",
			toml: sourceTOML(
				`repo    = "`+sourceRepoURL+`"`,
				`dest    = "/opt/src/automation"`,
				`trusted = "yes"`,
			),
			want: "source.automation.trusted must be a boolean",
		},
		{
			name: "name outside the identifier grammar",
			toml: `[source."build automation"]` + "\n" +
				`repo = "` + sourceRepoURL + `"` + "\n" +
				`dest = "/opt/src/automation"` + "\n",
			want: wantNameGrammarRefusal,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(writeConfig(t, testCase.toml))
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("load error = %v, want one wrapping ErrInvalidConfig", err)
			}

			message := err.Error()
			if !strings.Contains(message, testCase.want) {
				t.Fatalf("load error %q does not mention %q", message, testCase.want)
			}
		})
	}
}

// TestLoadSourceRequiresGitOnPath proves FR3 against an empty PATH: a host with
// no git binary fails config load with an error naming it, instead of clearing
// startup and failing on the first sync.
// emptyPath answers a PATH with nothing on it, which is how these cases stage a
// host that has no binary to find without rewriting the process environment.
func emptyPath(t *testing.T) environ.Lookup {
	t.Helper()

	return environ.Map(map[string]string{"PATH": t.TempDir()})
}

func TestLoadSourceRequiresGitOnPath(t *testing.T) {
	t.Parallel()

	toml := sourceTOML(
		`repo = "`+sourceRepoURL+`"`,
		`dest = "/opt/src/automation"`,
	)

	_, err := config.LoadWith(writeConfig(t, toml), emptyPath(t))
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("load error = %v, want one wrapping ErrInvalidConfig", err)
	}

	message := err.Error()
	if !strings.Contains(message, "git") {
		t.Fatalf("load error %q does not name the missing binary", message)
	}
}

// TestLoadWithoutSourcesSkipsGitProbe proves the probe is scoped to hosts that
// asked for a source: a config with no block loads on a host with no binary at
// all.
func TestLoadWithoutSourcesSkipsGitProbe(t *testing.T) {
	t.Parallel()

	if _, err := config.LoadWith(writeConfig(t, "[telemetry]\nenabled = false\n"), emptyPath(t)); err != nil {
		t.Fatalf("load with no source blocks = %v, want nil", err)
	}
}

// TestLoadSourceRejectsDestInsideCairnOSDir proves the home guard against the
// directory Load actually derives, the config file's own, rather than a literal
// path a fixture could drift from.
// TestLoadSourceRejectsDestInsideCairnOSDir writes the config into home itself,
// since Load derives the off-limits directory from the config file's own path.
func TestLoadSourceRejectsDestInsideCairnOSDir(t *testing.T) {
	t.Parallel()

	home := t.TempDir()

	for _, dest := range []string{home, filepath.Join(home, "data", "checkout")} {
		toml := sourceTOML(
			`repo = "`+sourceRepoURL+`"`,
			`dest = "`+dest+`"`,
		)

		path := filepath.Join(home, "config.toml")
		if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}

		_, err := config.Load(path)
		if !errors.Is(err, config.ErrInvalidConfig) {
			t.Fatalf("dest %q loaded with error %v, want one wrapping ErrInvalidConfig", dest, err)
		}
	}
}
