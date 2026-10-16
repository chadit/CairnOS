// Package hostenv renders the configured host table into environment
// assignments. It is the one place that decides what a CAIRNOS_HOST_* variable
// is called and what it holds, so a job on an agent machine learns another
// machine's tool layout from CairnOS instead of from its own constants.
package hostenv

import (
	"maps"
	"slices"
	"strings"

	"github.com/chadit/CairnOS/internal/config"
)

// Variable names. The prefix is reserved: [runner.exec] refuses an env key that
// starts with it, so the rendered table is the only thing that writes these.
const (
	hostsVar   = "CAIRNOS_HOSTS"
	hostPrefix = "CAIRNOS_HOST_"

	typeSuffix     = "_TYPE"
	sshSuffix      = "_SSH"
	wrapperSuffix  = "_WRAPPER"
	listSuffix     = "_HARNESSES"
	binInfix       = "_HARNESS_"
	providerSuffix = "_PROVIDER"
)

// listSeparator joins the two index variables. A space works because both
// grammars already exclude it.
const listSeparator = " "

// Render returns the CAIRNOS_HOST_* assignments for hosts in a stable order, so
// two processes holding the same config hand their children the same
// environment and a run is reproducible across them. An empty table renders
// nothing rather than an empty index variable, which keeps a host with no
// configuration behaving exactly as it did before this existed.
func Render(hosts map[string]config.Host) []string {
	if len(hosts) == 0 {
		return nil
	}

	names := slices.Sorted(maps.Keys(hosts))
	env := []string{hostsVar + "=" + strings.Join(names, listSeparator)}

	for _, name := range names {
		env = append(env, hostVars(name, hosts[name])...)
	}

	return env
}

// hostVars renders one host: its type, its alias when it has one, the index of
// tools installed on it, and each tool's own variables.
func hostVars(name string, host config.Host) []string {
	prefix := hostPrefix + config.EnvToken(name)

	vars := []string{prefix + typeSuffix + "=" + host.Type}

	// An absent alias is left out rather than set empty, so a consumer can tell
	// "no alias configured" from "alias configured as nothing".
	if host.SSH != "" {
		vars = append(vars, prefix+sshSuffix+"="+host.SSH)
	}

	// Left out rather than set empty for the same reason the alias is: a job
	// reading an empty value would run the launcher through nothing.
	if host.Wrapper != "" {
		vars = append(vars, prefix+wrapperSuffix+"="+host.Wrapper)
	}

	// A host with no tool blocks leaves the index out rather than setting it
	// empty, the same way an absent alias does. A job iterating the value would
	// otherwise get one empty field instead of none, and a job testing for the
	// variable would read its presence as "this host has tools".
	if len(host.Harnesses) == 0 {
		return vars
	}

	clis := slices.Sorted(maps.Keys(host.Harnesses))
	vars = append(vars, prefix+listSuffix+"="+strings.Join(clis, listSeparator))

	for _, cli := range clis {
		vars = append(vars, toolVars(prefix, cli, host.Harnesses[cli])...)
	}

	return vars
}

// toolVars renders one tool's launch path and, when the block named one, its
// provider label.
func toolVars(prefix, cli string, tool config.HostCLI) []string {
	key := prefix + binInfix + config.EnvToken(cli)

	vars := []string{key + "=" + launchPath(tool)}

	if tool.Provider != "" {
		vars = append(vars, key+providerSuffix+"="+tool.Provider)
	}

	return vars
}

// launchPath joins the directory and the binary with exactly one slash. Config
// load already refuses a path ending in one, so the trim is that guard's second
// half: a shell and Python's posixpath.join read a doubled slash differently.
// The rest passes through byte for byte, "~" included, since only the host that
// runs the binary knows what home means.
func launchPath(tool config.HostCLI) string {
	return strings.TrimRight(tool.Path, "/") + "/" + tool.Name
}
