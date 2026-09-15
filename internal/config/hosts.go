package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Host block keys. The parser matches against these and rejects anything else,
// naming the full path, so a typo'd key fails Load rather than leaving a host
// described in a way no job will see.
const (
	hostKeyType    = "type"
	hostKeySSH     = "ssh"
	hostKeyHarness = "harness"
)

// Harness block keys, checked the same way one level down.
const (
	harnessKeyPath     = "path"
	harnessKeyName     = "name"
	harnessKeyProvider = "provider"
)

// Host types. A worker runs the model CLIs; an agent drives them from
// somewhere else. Nothing in CairnOS branches on the value yet: it reaches the
// engine as a label so a job can tell the two apart.
const (
	HostTypeWorker = "worker"
	HostTypeAgent  = "agent"
)

// DefaultHarnessPath is where a block that names no path looks for its
// binary. It stays a "~/" path because only the host that runs the binary knows
// what home means; CairnOS never expands it.
const DefaultHarnessPath = "~/.local/bin"

// homePrefix is the one relative form such a path may take.
const homePrefix = "~/"

// hostNamePattern and harnessKeyPattern are the two block-name grammars. A host
// name allows dots and dashes because it usually mirrors an ssh alias; a cli key
// is the lower-case tool name a workflow and the engine both spell.
var (
	hostNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	harnessKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

// Harness is one AI command-line tool installed on a host, named from a
// [host.<name>.harness.<cli>] block. The engine launches Path + "/" + Name
// instead of trusting the worker host's PATH, which a non-login ssh shell
// usually leaves without ~/.local/bin on it.
type Harness struct {
	// Path is the directory holding the binary: absolute, or "~/"-prefixed for
	// the host to expand. Defaults to DefaultHarnessPath.
	Path string

	// Name is the binary's filename, which differs from the cli key whenever an
	// operator keeps a second build under its own name (claude-agent beside
	// claude). Defaults to the cli key.
	Name string

	// Provider is an optional label for whose model sits behind the tool, for a
	// job that reports or routes on it.
	Provider string
}

// Host is one machine CairnOS knows about, named from a [host.<name>] block.
// The table reaches every job as environment variables, so a job learns another
// host's tool layout from config rather than from its own constants.
type Host struct {
	// Harnesses holds one entry per installed CLI, keyed by the tool name.
	Harnesses map[string]Harness

	// Type is HostTypeWorker or HostTypeAgent.
	Type string

	// SSH is the alias a job uses to reach this host, when reaching it needs one.
	SSH string
}

// hostEntry is one key of one host block, carried together so every error names
// the full path without a wide parameter list.
type hostEntry struct {
	value any
	host  string
	key   string
}

// harnessEntry is one key of one harness block, for the same reason.
type harnessEntry struct {
	value any
	host  string
	cli   string
	key   string
}

// EnvToken derives the environment-safe half of a CAIRNOS_HOST_* name from a
// host or cli name: upper case, and every character outside [A-Z0-9] becomes an
// underscore. It is lossy on purpose, which is why validation rejects two names
// that land on the same token rather than letting one silently win.
func EnvToken(name string) string {
	return strings.Map(func(char rune) rune {
		if (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			return char
		}

		return '_'
	}, strings.ToUpper(name))
}

// parseHostBlocks turns the verbatim [host.<name>] blocks into typed Hosts.
// Names are walked in sorted order so the first offending block is reported
// reproducibly.
func parseHostBlocks(raw map[string]map[string]any) (map[string]Host, error) {
	hosts := make(map[string]Host, len(raw))

	for _, name := range slices.Sorted(maps.Keys(raw)) {
		host, err := parseHostBlock(name, raw[name])
		if err != nil {
			return nil, err
		}

		hosts[name] = host
	}

	return hosts, nil
}

// parseHostBlock decodes one host block strictly. go-toml hands the nested
// harness tables back inside this same map, so the whole two-level shape is
// decoded here rather than through a second struct tag.
func parseHostBlock(name string, block map[string]any) (Host, error) {
	host := Host{Harnesses: map[string]Harness{}}

	for _, key := range slices.Sorted(maps.Keys(block)) {
		entry := hostEntry{host: name, key: key, value: block[key]}
		if err := applyHostKey(&host, entry); err != nil {
			return Host{}, err
		}
	}

	return host, nil
}

// applyHostKey sets one parsed key on host or returns a naming error. It is
// split from the loop so the switch stays flat and each assertion carries the
// key path into its error.
func applyHostKey(host *Host, entry hostEntry) error {
	var err error

	switch entry.key {
	case hostKeyType:
		host.Type, err = hostString(entry)
	case hostKeySSH:
		host.SSH, err = hostString(entry)
	case hostKeyHarness:
		host.Harnesses, err = parseHarnessBlocks(entry)
	default:
		err = fmt.Errorf("%w: host.%s has unknown key %q", ErrInvalidConfig, entry.host, entry.key)
	}

	return err
}

// parseHarnessBlocks decodes the harness table of one host. Its value and each
// cli's value both have to be tables, and a scalar in either place is an
// operator mistake worth naming rather than ignoring.
func parseHarnessBlocks(entry hostEntry) (map[string]Harness, error) {
	table, isTable := entry.value.(map[string]any)
	if !isTable {
		return nil, fmt.Errorf("%w: host.%s.%s must be a table of harness blocks",
			ErrInvalidConfig, entry.host, entry.key)
	}

	harnesses := make(map[string]Harness, len(table))

	for _, cli := range slices.Sorted(maps.Keys(table)) {
		harness, err := parseHarnessBlock(entry.host, cli, table[cli])
		if err != nil {
			return nil, err
		}

		harnesses[cli] = harness
	}

	return harnesses, nil
}

// parseHarnessBlock decodes one harness block, seeding the two defaults so a
// block written empty still describes a launchable binary. A key written blank
// keeps the blank and fails validation, since an operator who typed the key
// meant to name something.
func parseHarnessBlock(host, cli string, raw any) (Harness, error) {
	block, isTable := raw.(map[string]any)
	if !isTable {
		return Harness{}, fmt.Errorf("%w: host.%s.harness.%s must be a table",
			ErrInvalidConfig, host, cli)
	}

	harness := Harness{Path: DefaultHarnessPath, Name: cli}

	for _, key := range slices.Sorted(maps.Keys(block)) {
		entry := harnessEntry{host: host, cli: cli, key: key, value: block[key]}
		if err := applyHarnessKey(&harness, entry); err != nil {
			return Harness{}, err
		}
	}

	return harness, nil
}

// applyHarnessKey sets one parsed key on the block or returns a naming error.
func applyHarnessKey(harness *Harness, entry harnessEntry) error {
	var err error

	switch entry.key {
	case harnessKeyPath:
		harness.Path, err = harnessString(entry)
	case harnessKeyName:
		harness.Name, err = harnessString(entry)
	case harnessKeyProvider:
		harness.Provider, err = harnessString(entry)
	default:
		err = fmt.Errorf("%w: host.%s.harness.%s has unknown key %q",
			ErrInvalidConfig, entry.host, entry.cli, entry.key)
	}

	return err
}

// hostString asserts a host-level string value. It trims for the same reason
// sourceString does: validation and the rendered environment have to judge the
// same text, or a value with a stray space passes here and breaks on the host.
func hostString(entry hostEntry) (string, error) {
	str, ok := entry.value.(string)
	if !ok {
		return "", fmt.Errorf("%w: host.%s.%s must be a string",
			ErrInvalidConfig, entry.host, entry.key)
	}

	return strings.TrimSpace(str), nil
}

// harnessString asserts a string value one level down, trimmed the same way.
func harnessString(entry harnessEntry) (string, error) {
	str, ok := entry.value.(string)
	if !ok {
		return "", fmt.Errorf("%w: host.%s.harness.%s.%s must be a string",
			ErrInvalidConfig, entry.host, entry.cli, entry.key)
	}

	return strings.TrimSpace(str), nil
}

// validateHosts checks every parsed host alongside validateSources in Load.
// Names are walked sorted so the first invalid block is reported reproducibly,
// and the token map catches two names that would render one variable.
func validateHosts(hosts map[string]Host) error {
	seen := make(map[string]string, len(hosts))

	for _, name := range slices.Sorted(maps.Keys(hosts)) {
		if err := claimHostToken(seen, name); err != nil {
			return err
		}

		host := hosts[name]
		if err := validateHost(name, &host); err != nil {
			return err
		}
	}

	return nil
}

// claimHostToken records one host's rendered token and refuses a second host
// that lands on it. EnvToken is lossy, so build_box and build-box are two config
// names and one variable; picking a winner would hand a job the wrong host's
// binary paths.
func claimHostToken(seen map[string]string, name string) error {
	token := EnvToken(name)

	if other, taken := seen[token]; taken {
		return fmt.Errorf("%w: host.%s and host.%s both render as CAIRNOS_HOST_%s; rename one",
			ErrInvalidConfig, other, name, token)
	}

	seen[token] = name

	return nil
}

// validateHost checks one host's name and type before its harnesses.
func validateHost(name string, host *Host) error {
	if !hostNamePattern.MatchString(name) {
		return fmt.Errorf("%w: host.%s name must match %s", ErrInvalidConfig, name, hostNamePattern)
	}

	if host.Type != HostTypeWorker && host.Type != HostTypeAgent {
		return fmt.Errorf("%w: host.%s.type %q is not %s or %s",
			ErrInvalidConfig, name, host.Type, HostTypeWorker, HostTypeAgent)
	}

	// The alias is held to the host-name grammar because that is what it is: an
	// ssh alias or a hostname. It reaches the environment of every child, and a
	// value carrying a shell metacharacter would be one interpolation away from
	// running a second command in whichever job reads it first.
	if host.SSH != "" && !hostNamePattern.MatchString(host.SSH) {
		return fmt.Errorf("%w: host.%s.ssh %q must match %s",
			ErrInvalidConfig, name, host.SSH, hostNamePattern)
	}

	return validateHarnesses(name, host.Harnesses)
}

// validateHarnesses checks one host's harnesses, refusing two cli keys that
// render the same variable for the same reason two hosts cannot.
func validateHarnesses(host string, harnesses map[string]Harness) error {
	seen := make(map[string]string, len(harnesses))

	for _, cli := range slices.Sorted(maps.Keys(harnesses)) {
		if err := claimHarnessToken(seen, host, cli); err != nil {
			return err
		}

		harness := harnesses[cli]
		if err := validateHarness(host, cli, &harness); err != nil {
			return err
		}
	}

	return nil
}

// claimHarnessToken is claimHostToken one level down.
func claimHarnessToken(seen map[string]string, host, cli string) error {
	token := EnvToken(cli)

	if other, taken := seen[token]; taken {
		return fmt.Errorf("%w: host.%s harnesses %s and %s both render as _HARNESS_%s; rename one",
			ErrInvalidConfig, host, other, cli, token)
	}

	seen[token] = cli

	return nil
}

// validateHarness checks one harness block's key and its two path parts.
func validateHarness(host, cli string, harness *Harness) error {
	if !harnessKeyPattern.MatchString(cli) {
		return fmt.Errorf("%w: host.%s.harness.%s name must match %s",
			ErrInvalidConfig, host, cli, harnessKeyPattern)
	}

	if err := validateHarnessPath(host, cli, harness.Path); err != nil {
		return err
	}

	return validateHarnessName(host, cli, harness.Name)
}

// validateHarnessPath keeps the directory resolvable on the host that runs the
// binary. A relative path other than "~/" would resolve against whatever working
// directory the job happened to have, which is not something config can promise.
func validateHarnessPath(host, cli, path string) error {
	if path == "" {
		return fmt.Errorf("%w: host.%s.harness.%s.path must be set", ErrInvalidConfig, host, cli)
	}

	if !filepath.IsAbs(path) && !strings.HasPrefix(path, homePrefix) {
		return fmt.Errorf("%w: host.%s.harness.%s.path %q must be absolute or start with %q",
			ErrInvalidConfig, host, cli, path, homePrefix)
	}

	// A trailing slash is refused rather than tidied because the consumers do not
	// agree on what a doubled one means. "~//" renders "~//claude", which a shell
	// reads as a path under home and Python's posixpath.join reads as the absolute
	// "/claude", so two lanes would launch two different binaries with no error
	// from either.
	if strings.HasSuffix(path, "/") {
		return fmt.Errorf("%w: host.%s.harness.%s.path %q must not end with a slash",
			ErrInvalidConfig, host, cli, path)
	}

	return nil
}

// validateHarnessName keeps the binary a filename rather than a second path
// segment or a value the shell would read as a flag.
func validateHarnessName(host, cli, name string) error {
	if name == "" {
		return fmt.Errorf("%w: host.%s.harness.%s.name must be set", ErrInvalidConfig, host, cli)
	}

	if strings.Contains(name, "/") {
		return fmt.Errorf("%w: host.%s.harness.%s.name %q must be one path element; put the directory in path",
			ErrInvalidConfig, host, cli, name)
	}

	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("%w: host.%s.harness.%s.name %q must not start with a dash",
			ErrInvalidConfig, host, cli, name)
	}

	return nil
}
