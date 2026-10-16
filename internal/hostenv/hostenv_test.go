package hostenv_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/hostenv"
)

// Fixture values the cases share: the tool keys the hosts install, a plain
// absolute directory, the path it renders to, and the single-host block name
// with its SSH target. The rendered lines two cases both pin stay whole
// strings so the golden text is readable as the job sees it.
const (
	cliClaude  = "claude"
	cliCursor  = "cursor-agent"
	cliCodex   = "codex"
	optBin     = "/opt/bin"
	optBinPath = optBin + "/" + cliClaude
	boxHost    = "box"
	boxSSH     = "build-box"

	boxHostsIndex  = "CAIRNOS_HOSTS=box"
	claudeTildeVar = "CAIRNOS_HOST_BUILD_BOX_HARNESS_CLAUDE=~/.local/bin/claude-agent"
)

// twoHosts is the shape a two-host deployment describes: a worker carrying
// the model CLIs and an agent that drives them. The names differ in separator
// and case so the token derivation is visible in the golden slice.
func twoHosts() map[string]config.Host {
	return map[string]config.Host{
		"build_box": {
			Type:    config.HostTypeWorker,
			SSH:     boxSSH,
			Wrapper: "~/.local/bin/agent-env",
			Harnesses: map[string]config.HostCLI{
				cliClaude: {Path: config.DefaultHarnessPath, Name: "claude-agent"},
				cliCursor: {Path: config.DefaultHarnessPath, Name: cliCursor, Provider: "cursor"},
			},
		},
		"builder.example.com": {
			Type:      config.HostTypeAgent,
			Harnesses: map[string]config.HostCLI{cliCodex: {Path: optBin, Name: cliCodex}},
		},
	}
}

func TestRenderGolden(t *testing.T) {
	t.Parallel()

	// build_box sorts ahead of builder.example.com because "_" is below "e" in
	// byte order, which is the ordering Render promises and a reader of this
	// slice should not have to guess at.
	want := []string{
		"CAIRNOS_HOSTS=build_box builder.example.com",
		"CAIRNOS_HOST_BUILD_BOX_TYPE=worker",
		"CAIRNOS_HOST_BUILD_BOX_SSH=build-box",
		"CAIRNOS_HOST_BUILD_BOX_WRAPPER=~/.local/bin/agent-env",
		"CAIRNOS_HOST_BUILD_BOX_HARNESSES=claude cursor-agent",
		claudeTildeVar,
		"CAIRNOS_HOST_BUILD_BOX_HARNESS_CURSOR_AGENT=~/.local/bin/cursor-agent",
		"CAIRNOS_HOST_BUILD_BOX_HARNESS_CURSOR_AGENT_PROVIDER=cursor",
		"CAIRNOS_HOST_BUILDER_EXAMPLE_COM_TYPE=agent",
		"CAIRNOS_HOST_BUILDER_EXAMPLE_COM_HARNESSES=codex",
		"CAIRNOS_HOST_BUILDER_EXAMPLE_COM_HARNESS_CODEX=/opt/bin/codex",
	}

	got := hostenv.Render(twoHosts())
	if !slices.Equal(got, want) {
		t.Fatalf("Render mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestRenderKeepsTildeVerbatim is the half of the golden case worth stating on
// its own: CairnOS never expands "~", because the host that runs the binary is
// the only one that knows its own home.
func TestRenderKeepsTildeVerbatim(t *testing.T) {
	t.Parallel()

	rendered := hostenv.Render(twoHosts())

	if !slices.Contains(rendered, claudeTildeVar) {
		t.Fatalf("rendered %#v does not contain %q", rendered, claudeTildeVar)
	}
}

func TestRenderEmptyTableRendersNothing(t *testing.T) {
	t.Parallel()

	if got := hostenv.Render(nil); got != nil {
		t.Fatalf("Render(nil) = %#v, want nil", got)
	}

	if got := hostenv.Render(map[string]config.Host{}); got != nil {
		t.Fatalf("Render(empty) = %#v, want nil", got)
	}
}

// TestRenderIsStableAcrossCalls proves the order does not ride on Go's map
// iteration. Two serve processes with one config have to hand their children
// the same environment or a run stops being reproducible across them.
func TestRenderIsStableAcrossCalls(t *testing.T) {
	t.Parallel()

	first := hostenv.Render(twoHosts())

	for range 16 {
		if next := hostenv.Render(twoHosts()); !slices.Equal(next, first) {
			t.Fatalf("Render order changed between calls\nfirst: %#v\n next: %#v", first, next)
		}
	}
}

// TestRenderJoinsWithExactlyOneSlash is the second half of the guard config load
// applies. A doubled slash means different things to a shell and to Python's
// posixpath.join, which discards everything before an absolute second argument,
// so "~//claude" would have one lane launching under home and the other reading
// "/claude" from the filesystem root.
func TestRenderJoinsWithExactlyOneSlash(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path string
		want string
	}{
		{path: optBin, want: optBinPath},
		{path: optBin + "/", want: optBinPath},
		{path: optBin + "//", want: optBinPath},
		{path: config.DefaultHarnessPath, want: "~/.local/bin/claude"},
		{path: "~//", want: "~/claude"},
	}

	for _, testCase := range cases {
		hosts := map[string]config.Host{
			boxHost: {
				Type:      config.HostTypeWorker,
				Harnesses: map[string]config.HostCLI{cliClaude: {Path: testCase.path, Name: cliClaude}},
			},
		}

		want := "CAIRNOS_HOST_BOX_HARNESS_CLAUDE=" + testCase.want
		if got := hostenv.Render(hosts); !slices.Contains(got, want) {
			t.Fatalf("path %q rendered %#v, want %q", testCase.path, got, want)
		}
	}
}

// TestRenderOmitsTheToolIndexForAHostWithNoTools keeps the per-host index
// following the same rule as the alias and the top-level index: absent rather
// than empty, so a job can tell "no tools configured" from "configured none".
func TestRenderOmitsTheToolIndexForAHostWithNoTools(t *testing.T) {
	t.Parallel()

	hosts := map[string]config.Host{
		boxHost: {Type: config.HostTypeAgent, SSH: boxSSH},
	}

	want := []string{
		boxHostsIndex,
		"CAIRNOS_HOST_BOX_TYPE=agent",
		"CAIRNOS_HOST_BOX_SSH=build-box",
	}

	if got := hostenv.Render(hosts); !slices.Equal(got, want) {
		t.Fatalf("Render mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestRenderOmitsUnsetOptionalFields proves an absent alias or provider leaves
// the variable out entirely, so a consumer can tell "not configured" from
// "configured empty".
func TestRenderOmitsUnsetOptionalFields(t *testing.T) {
	t.Parallel()

	hosts := map[string]config.Host{
		boxHost: {
			Type:      config.HostTypeWorker,
			Harnesses: map[string]config.HostCLI{cliClaude: {Path: optBin, Name: cliClaude}},
		},
	}

	want := []string{
		boxHostsIndex,
		"CAIRNOS_HOST_BOX_TYPE=worker",
		"CAIRNOS_HOST_BOX_HARNESSES=claude",
		"CAIRNOS_HOST_BOX_HARNESS_CLAUDE=" + optBinPath,
	}

	if got := hostenv.Render(hosts); !slices.Equal(got, want) {
		t.Fatalf("Render mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestRenderLeavesOutAnAbsentWrapper is the other half of the golden case: a
// host that names no wrapper gets no variable, so a job can tell "run it
// directly" from "run it through nothing".
func TestRenderLeavesOutAnAbsentWrapper(t *testing.T) {
	t.Parallel()

	rendered := hostenv.Render(twoHosts())

	for _, entry := range rendered {
		if strings.HasPrefix(entry, "CAIRNOS_HOST_BUILDER_EXAMPLE_COM_WRAPPER") {
			t.Fatalf("rendered %#v, want no wrapper for the host that names none", rendered)
		}
	}
}
