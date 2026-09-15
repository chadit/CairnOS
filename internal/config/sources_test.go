package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/config"
)

// sourceRepoURL is the clone URL every fixture block names. Nothing dials it:
// config validation only checks that the key carries a value.
const sourceRepoURL = "https://github.com/example/automation.git"

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

func TestLoadSourceRejectsBadBlocks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "unknown key",
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
			name: "name outside the identifier grammar",
			toml: `[source."build automation"]` + "\n" +
				`repo = "` + sourceRepoURL + `"` + "\n" +
				`dest = "/opt/src/automation"` + "\n",
			want: "name must match",
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
// startup and failing on the first sync. It cannot run in parallel because it
// rewrites the process environment.
func TestLoadSourceRequiresGitOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	toml := sourceTOML(
		`repo = "`+sourceRepoURL+`"`,
		`dest = "/opt/src/automation"`,
	)

	_, err := config.Load(writeConfig(t, toml))
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("load error = %v, want one wrapping ErrInvalidConfig", err)
	}

	message := err.Error()
	if !strings.Contains(message, "git") {
		t.Fatalf("load error %q does not name the missing binary", message)
	}
}

// TestLoadWithoutSourcesSkipsGitProbe proves the probe is scoped to hosts that
// asked for a source: a config with no block loads on a host with no git at all.
// It cannot run in parallel because it rewrites the process environment.
func TestLoadWithoutSourcesSkipsGitProbe(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	if _, err := config.Load(writeConfig(t, "[telemetry]\nenabled = false\n")); err != nil {
		t.Fatalf("load with no source blocks = %v, want nil", err)
	}
}

// TestLoadSourceRejectsDestInsideCairnOSDir proves the home guard against the
// directory Load actually derives, the config file's own, rather than a literal
// path a fixture could drift from.
func TestLoadSourceRejectsDestInsideCairnOSDir(t *testing.T) {
	t.Parallel()

	home := t.TempDir()

	for _, dest := range []string{home, filepath.Join(home, "data", "checkout")} {
		toml := sourceTOML(
			`repo = "`+sourceRepoURL+`"`,
			`dest = "`+dest+`"`,
		)

		// The config has to live in home itself, since Load derives the directory
		// a source must stay out of from the config file's own path.
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
