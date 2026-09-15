package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/config"
)

// hostName is the block every fixture declares, and cliClaude the tool inside
// it. Both are spelled the way a two-host deployment would spell them.
const (
	hostName  = "build_box"
	cliClaude = "claude"

	// wantDashRefusal is the shared half of the dash guard's message, asserted
	// here for a binary name and in sources_test.go for a repo and a ref.
	wantDashRefusal = "must not start with a dash"

	// wantSlashRefusal is the trailing-slash guard's message, asserted for an
	// absolute path, a doubled slash, and the home-prefixed form.
	wantSlashRefusal = "must not end with a slash"
)

// hostTOML builds one [host.build_box] block from the lines a case wants,
// so each test states only the keys it is about.
func hostTOML(lines ...string) string {
	return "[host." + hostName + "]\n" + strings.Join(lines, "\n") + "\n"
}

func TestEnvTokenUpperCasesAndReplacesEverythingElse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want string
	}{
		{name: "build_box", want: "BUILD_BOX"},
		{name: "build-box", want: "BUILD_BOX"},
		{name: "box.example.com", want: "BOX_EXAMPLE_COM"},
		{name: "omp-glm", want: "OMP_GLM"},
		{name: "node2", want: "NODE2"},
	}

	for _, testCase := range cases {
		if got := config.EnvToken(testCase.name); got != testCase.want {
			t.Fatalf("EnvToken(%q) = %q, want %q", testCase.name, got, testCase.want)
		}
	}
}

func TestLoadHostParsesBlockAndDefaults(t *testing.T) {
	t.Parallel()

	toml := hostTOML(
		`type = "worker"`,
		`ssh  = "build-box"`,
	) + "\n[host." + hostName + ".harness." + cliClaude + "]\n" +
		"name = \"claude-agent\"\n" +
		"\n[host." + hostName + ".harness.omp-glm]\n" +
		"provider = \"zai\"\n"

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	host, ok := cfg.Hosts[hostName]
	if !ok {
		t.Fatalf("host %s missing from parsed config", hostName)
	}

	if host.Type != config.HostTypeWorker || host.SSH != "build-box" {
		t.Fatalf("host = %+v, want the worker type and the nas alias", host)
	}

	// claude names a binary, so only the path falls back to the default.
	claude := host.Harnesses[cliClaude]
	if claude.Path != config.DefaultHarnessPath || claude.Name != "claude-agent" {
		t.Fatalf("claude = %+v, want the default path and the configured name", claude)
	}

	// omp-glm names neither, so the key itself is the binary name.
	glm := host.Harnesses["omp-glm"]
	if glm.Path != config.DefaultHarnessPath || glm.Name != "omp-glm" || glm.Provider != "zai" {
		t.Fatalf("omp-glm = %+v, want both defaults and the provider label", glm)
	}
}

// TestLoadHostAcceptsAnEmptyToolBlock covers the shape the docs show for a tool
// installed under its own name in the default directory.
func TestLoadHostAcceptsAnEmptyToolBlock(t *testing.T) {
	t.Parallel()

	toml := hostTOML(`type = "agent"`) +
		"\n[host." + hostName + ".harness.copilot]\n"

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got := cfg.Hosts[hostName].Harnesses["copilot"]
	if got.Path != config.DefaultHarnessPath || got.Name != "copilot" {
		t.Fatalf("copilot = %+v, want both defaults filled in", got)
	}
}

func TestLoadHostRejectsBadBlocks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "unknown host key",
			toml: hostTOML(`type = "worker"`, `region = "us-east"`),
			want: `host.build_box has unknown key "region"`,
		},
		{
			name: "unknown tool key",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness." + cliClaude + "]\nbin = \"claude\"\n",
			want: `host.build_box.harness.claude has unknown key "bin"`,
		},
		{
			name: "non-string host value",
			toml: hostTOML(`type = 7`),
			want: "host.build_box.type must be a string",
		},
		{
			name: "non-string tool value",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness." + cliClaude + "]\nname = 7\n",
			want: "host.build_box.harness.claude.name must be a string",
		},
		{
			name: "scalar where the tool table belongs",
			toml: hostTOML(`type = "worker"`, `harness = "claude"`),
			want: "host.build_box.harness must be a table",
		},
		{
			name: "scalar where one tool block belongs",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness]\n" + cliClaude + " = \"yes\"\n",
			want: "host.build_box.harness.claude must be a table",
		},
		{
			name: "unknown type",
			toml: hostTOML(`type = "builder"`),
			want: `host.build_box.type "builder" is not worker or agent`,
		},
		{
			name: "missing type",
			toml: hostTOML(`ssh = "build-box"`),
			want: "is not worker or agent",
		},
		{
			name: "host name outside the grammar",
			toml: "[host.\"_leading\"]\ntype = \"worker\"\n",
			want: "name must match",
		},
		{
			name: "tool key outside the grammar",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness.Claude]\n",
			want: "host.build_box.harness.Claude name must match",
		},
		{
			name: "relative path",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness." + cliClaude + "]\npath = \"bin\"\n",
			want: "must be absolute or start with",
		},
		{
			name: "blank path",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness." + cliClaude + "]\npath = \"  \"\n",
			want: "host.build_box.harness.claude.path must be set",
		},
		{
			name: "blank name",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness." + cliClaude + "]\nname = \"  \"\n",
			want: "host.build_box.harness.claude.name must be set",
		},
		{
			name: "path ending in a slash",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness." + cliClaude + "]\npath = \"/opt/bin/\"\n",
			want: wantSlashRefusal,
		},
		{
			name: "path ending in a doubled slash",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness." + cliClaude + "]\npath = \"/opt/bin//\"\n",
			want: wantSlashRefusal,
		},
		{
			name: "home path ending in a doubled slash",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness." + cliClaude + "]\npath = \"~//\"\n",
			want: wantSlashRefusal,
		},
		{
			name: "ssh alias carrying a shell metacharacter",
			toml: hostTOML(`type = "worker"`, `ssh  = "build-box; rm -rf /"`),
			want: "host.build_box.ssh",
		},
		{
			name: "ssh alias with a space",
			toml: hostTOML(`type = "worker"`, `ssh  = "build box"`),
			want: "must match",
		},
		{
			name: "name carrying a directory",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness." + cliClaude + "]\nname = \"bin/claude\"\n",
			want: "must be one path element",
		},
		{
			name: "name that reads as a flag",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness." + cliClaude + "]\nname = \"-rf\"\n",
			want: wantDashRefusal,
		},
		{
			name: "two hosts rendering one variable",
			toml: hostTOML(`type = "worker"`) +
				"\n[host.\"build-box\"]\ntype = \"agent\"\n",
			want: "both render as CAIRNOS_HOST_BUILD_BOX",
		},
		{
			name: "two tools rendering one variable",
			toml: hostTOML(`type = "worker"`) +
				"\n[host." + hostName + ".harness.omp-glm]\n" +
				"\n[host." + hostName + ".harness.omp_glm]\n",
			want: "both render as _HARNESS_OMP_GLM",
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

// TestLoadHostTrimsToolValues proves validation and the rendered environment
// judge the same text. A name with a stray space would otherwise clear the
// blank check here and produce a path no host can launch.
func TestLoadHostTrimsToolValues(t *testing.T) {
	t.Parallel()

	toml := hostTOML(`type = "worker"`) +
		"\n[host." + hostName + ".harness." + cliClaude + "]\n" +
		"path = \"  /opt/bin  \"\n" +
		"name = \"  claude-agent  \"\n"

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got := cfg.Hosts[hostName].Harnesses[cliClaude]
	if got.Path != "/opt/bin" || got.Name != "claude-agent" {
		t.Fatalf("tool = %+v, want both values trimmed", got)
	}
}

// TestLoadHostAcceptsAHostWithNoTools covers the shape an operator writes for a
// machine that is only an ssh target: the block loads, and the renderer leaves
// the tool index out entirely.
func TestLoadHostAcceptsAHostWithNoTools(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, hostTOML(`type = "agent"`, `ssh  = "build-box.example.com"`)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	host := cfg.Hosts[hostName]
	if host.SSH != "build-box.example.com" {
		t.Fatalf("ssh = %q, want the dotted alias accepted", host.SSH)
	}

	if len(host.Harnesses) != 0 {
		t.Fatalf("harnesses = %+v, want none", host.Harnesses)
	}
}

// TestLoadWithoutHostsLeavesAnEmptyTable proves the absent case stays usable:
// wiring renders nothing rather than having to guard a nil map.
func TestLoadWithoutHostsLeavesAnEmptyTable(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, "[telemetry]\nenabled = false\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Hosts) != 0 {
		t.Fatalf("hosts = %+v, want an empty table", cfg.Hosts)
	}
}
