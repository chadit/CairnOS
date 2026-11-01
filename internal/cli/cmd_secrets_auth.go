package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
)

// xdgConfigHomeEnv moves chezmoi's config directory off ~/.config when set.
const xdgConfigHomeEnv = "XDG_CONFIG_HOME"

// cmdSecretsAuth sets up the credentials `secrets pull` reads, or checks the
// ones already there, because a hand-written file at the wrong mode goes wrong
// quietly and surfaces only at the next deploy. It never copies a value from
// one place into another: a host keeps its vault credentials in one place.
func (a *App) cmdSecretsAuth(ctx context.Context, args []string) int {
	const label = "secrets auth"

	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)
	system := flags.Bool("system", false, "use the paths a system-mode unit reads")

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	opts, path, err := a.authTarget(*dirFlag, *system)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	present, err := credentialsPresent(path)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	if present {
		return a.checkVault(ctx, label, path, opts)
	}

	return a.setUpCredentials(ctx, label, path, opts)
}

// authTarget loads the config and resolves the credentials path the way
// `secrets pull` does, so the two commands always agree on the file.
func (a *App) authTarget(dirFlag string, system bool) (pullOptions, string, error) {
	dir, err := resolveSecretsDir(dirFlag, system, a.envLookup())
	if err != nil {
		return pullOptions{}, "", err
	}

	configPath := filepath.Join(dir, configFileName)

	cfg, err := config.LoadWith(configPath, a.envLookup())
	if err != nil {
		return pullOptions{}, "", fmt.Errorf("load %s: %w", configPath, err)
	}

	opts := pullOptions{dir: dir, dirFlag: dirFlag, cfg: &cfg, lookup: a.envLookup(), system: system}

	path, err := opts.resolve(credentialsFileFrom(&cfg), config.DefaultBitwardenCredentialsFile)

	return opts, path, err
}

// credentialsFileFrom reads the configured credentials path, falling back to the
// default when no block exists yet. `secrets auth` runs before the block does,
// so it cannot require one.
func credentialsFileFrom(cfg *config.Config) string {
	if cfg.Secrets.Bitwarden != nil {
		return cfg.Secrets.Bitwarden.CredentialsFile
	}

	return config.DefaultBitwardenCredentialsFile
}

// credentialsPresent reports whether path is a file to check rather than one
// to create. Any stat answer but "not there" counts as present, so the check
// reports it. A symlink whose target is gone is refused, since creating the
// file would replace the link.
func credentialsPresent(path string) (bool, error) {
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}

	if _, err := os.Lstat(path); err == nil {
		return false, fmt.Errorf("%s: %w", path, errDanglingCredentials)
	}

	return false, nil
}

// setUpCredentials gives a host with no credentials file one. A chezmoi config
// that already holds the vault keys wins over the prompts, because pointing at
// it keeps the one copy the operator already maintains.
func (a *App) setUpCredentials(ctx context.Context, label, path string, opts pullOptions) int {
	chezmoiPath, err := chezmoiToPointAt(opts)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	if chezmoiPath != "" {
		return a.pointAtChezmoi(label, chezmoiPath, opts)
	}

	if bitwarden.IsChezmoiConfig(path) {
		a.printErrf("%s: %s: %v\n", label, path, errNoChezmoiFile)

		return ExitFailure
	}

	return a.writeCredentials(ctx, label, path, opts)
}

// chezmoiToPointAt returns the chezmoi config holding the vault keys, or ""
// when the prompts should run instead. A system-mode run never points at one:
// the config it finds belongs to whoever ran sudo, under a home the service
// user cannot read and a later sudo may expand differently. No home means no
// chezmoi config to find.
func chezmoiToPointAt(opts pullOptions) (string, error) {
	if opts.system {
		return "", nil
	}

	path, found := chezmoiConfigPath(opts.lookup)
	if !found {
		return "", nil
	}

	held, err := bitwarden.HoldsChezmoiCredentials(path)
	if err != nil {
		return "", fmt.Errorf("read the chezmoi config: %w", err)
	}

	if !held {
		return "", nil
	}

	return path, nil
}

// chezmoiConfigPath is where chezmoi keeps its config, found the way chezmoi
// finds it: under $XDG_CONFIG_HOME when that is an absolute path, and under
// ~/.config otherwise. It reports false when neither can be named.
func chezmoiConfigPath(lookup environ.Lookup) (string, bool) {
	configHome := lookup.Get(xdgConfigHomeEnv)

	if !filepath.IsAbs(configHome) {
		home, err := homeDir(lookup)
		if err != nil {
			return "", false
		}

		configHome = filepath.Join(home, ".config")
	}

	return filepath.Join(configHome, "chezmoi", "chezmoi.toml"), true
}

// pointAtChezmoi sets credentials_file to the chezmoi config instead of copying
// its values, so the host keeps one copy of them.
func (a *App) pointAtChezmoi(label, chezmoiPath string, opts pullOptions) int {
	if err := requireItem(chezmoiPath, opts); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	pointer := homeRelative(chezmoiPath, opts.lookup)

	configPath, err := a.setCredentialsFile(opts, pointer)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	a.printOutf("%s: %s now points credentials_file at %s\n", label, configPath, pointer)

	return ExitOK
}

// requireItem refuses a chezmoi config that names no vault item when neither
// config.toml nor the environment names one, because the pull it would set up
// fails on the missing item. It runs before the edit, so a refusal leaves
// config.toml as it was.
func requireItem(chezmoiPath string, opts pullOptions) error {
	creds, err := bitwarden.LoadCredentials(chezmoiPath, opts.lookup.Get)
	if err != nil {
		return fmt.Errorf("read the chezmoi config: %w", err)
	}

	if _, err := authItem(opts.cfg, creds); err != nil {
		return fmt.Errorf("%s: %w", chezmoiPath, err)
	}

	return nil
}

// homeRelative writes path as ~/... when it sits under the operator's home, the
// form the README shows and resolveHomePath expands at use time.
func homeRelative(path string, lookup environ.Lookup) string {
	home, err := homeDir(lookup)
	if err != nil {
		return path
	}

	rest, underHome := strings.CutPrefix(path, filepath.Clean(home)+string(filepath.Separator))
	if !underHome {
		return path
	}

	return "~/" + filepath.ToSlash(rest)
}

// setCredentialsFile edits credentials_file into config.toml in place and
// returns the path it wrote. The edited text has to load through the real
// config loader before it replaces the file, so a failed edit leaves the
// operator's config as it was.
func (a *App) setCredentialsFile(opts pullOptions, pointer string) (string, error) {
	path, src, mode, err := readConfigForEdit(opts.dir)
	if err != nil {
		return "", err
	}

	edited, err := config.SetBitwardenCredentialsFile(src, pointer)
	if err != nil {
		return "", fmt.Errorf("%s left unchanged: %w", path, err)
	}

	if err := checkConfigLoads(opts.dir, edited, opts.lookup); err != nil {
		return "", fmt.Errorf("%s left unchanged: %w", path, err)
	}

	if err := a.writeFileAtomic(path, edited, mode); err != nil {
		return "", err
	}

	return path, nil
}

// readConfigForEdit reads config.toml for an in-place edit. A home with no
// config yet answers an empty one at the scaffold mode, and the home is
// created so the write has a directory to land in.
func readConfigForEdit(dir string) (string, []byte, os.FileMode, error) {
	path := filepath.Join(dir, configFileName)

	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return readConfigIn(dir)
	}

	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", nil, 0, fmt.Errorf("create %s: %w", dir, err)
	}

	return path, nil, filePerm, nil
}

// checkConfigLoads runs edited config text through the real loader from a temp
// file in the CairnOS home, not beside a symlinked config's target, so relative
// paths resolve exactly as they will when the command loads the real file.
func checkConfigLoads(dir string, content []byte, lookup environ.Lookup) error {
	tempPath, err := stageTemp(dir, configFileName+".check", content, filePerm)
	if err != nil {
		return err
	}

	var loadErr error

	if _, err := config.LoadWith(tempPath, lookup); err != nil {
		loadErr = fmt.Errorf("the edited config does not load: %w", err)
	}

	return errors.Join(loadErr, removeLeftover(tempPath))
}

// writeCredentials prompts for the four values and writes the ones the
// environment does not already hold. A value set there stays there, so the
// host does not end up with a second copy of it.
func (a *App) writeCredentials(ctx context.Context, label, path string, opts pullOptions) int {
	creds, inEnv, err := a.promptCredentials(ctx, bufio.NewReader(a.stdin), opts.cfg)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	lines, err := fileLines(creds, inEnv)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	if len(lines) == 0 {
		a.printOutf("%s: the environment holds every value, so no credentials file was written\n", label)

		return ExitOK
	}

	if err := a.createCredentialsFile(path, renderCredentials(lines)); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	a.printOutf("%s: wrote %s (item %s); run `cairnos secrets pull%s` next\n",
		label, path, creds.Item, opts.commandFlags())

	return ExitOK
}

// createCredentialsFile makes the directory and publishes the file, refusing
// to replace one that appeared while the prompts were open.
func (a *App) createCredentialsFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	return a.createFileAtomic(path, content, envFilePerm)
}

// promptCredentials collects the four values, in the order an operator would
// find them in the vault UI, and reports which of them the environment
// supplied. The item name has no default of its own: a blank answer is
// accepted only when the config block already names the item.
func (a *App) promptCredentials(
	ctx context.Context, reader *bufio.Reader, cfg *config.Config,
) (bitwarden.Credentials, map[string]bool, error) {
	var (
		creds          bitwarden.Credentials
		configuredItem string
	)

	if cfg.Secrets.Bitwarden != nil {
		configuredItem = cfg.Secrets.Bitwarden.Item
	}

	inEnv := map[string]bool{}

	for _, field := range []struct {
		target *string
		ask    askOptions
	}{
		{&creds.Item, askOptions{variable: bitwarden.EnvItem, prompt: "Bitwarden item name", fallback: configuredItem}},
		{&creds.ClientID, askOptions{variable: bitwarden.EnvClientID, prompt: "Bitwarden API client id", secret: true}},
		{&creds.ClientSecret, askOptions{variable: bitwarden.EnvClientSecret, prompt: "Bitwarden API client secret", secret: true}},
		{&creds.MasterKey, askOptions{variable: bitwarden.EnvMasterKey, prompt: "Bitwarden master password", secret: true}},
	} {
		value, fromEnv, err := a.ask(ctx, reader, field.ask)
		if err != nil {
			return bitwarden.Credentials{}, nil, err
		}

		*field.target = value
		inEnv[field.ask.variable] = fromEnv
	}

	return creds, inEnv, nil
}

// askOptions is one prompt: which variable it fills, what to show, and what an
// empty answer means.
type askOptions struct {
	variable string
	prompt   string
	fallback string
	secret   bool
}

// ask returns the environment's value when it has one, reporting that it came
// from there, and otherwise reads a line. An empty line takes the fallback. A
// missing value with nothing to read from fails naming the variable rather than
// blocking on a pipe that will never produce a line.
func (a *App) ask(ctx context.Context, reader *bufio.Reader, opts askOptions) (string, bool, error) {
	if value := strings.TrimSpace(a.envLookup().Get(opts.variable)); value != "" {
		a.printErrf("  %s: kept in the environment, not written to the file\n", opts.variable)

		return value, true, nil
	}

	if ctx.Err() != nil {
		return "", false, fmt.Errorf("read %s: %w", opts.variable, errInterrupted)
	}

	line, err := a.readLine(ctx, reader, promptText(opts), opts.secret)
	if errors.Is(err, errInterrupted) {
		// Nothing ended the prompt's line: echo was off or the key printed
		// no newline, so the error would land after the prompt text.
		a.printErrf("\n")

		return "", false, fmt.Errorf("read %s: %w", opts.variable, errInterrupted)
	}

	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, fmt.Errorf("read %s: %w", opts.variable, err)
	}

	answer := strings.TrimSpace(line)
	if opts.secret {
		answer = trimLineEnd(line)
	}

	if answer != "" {
		return answer, false, nil
	}

	if opts.fallback != "" {
		return opts.fallback, false, nil
	}

	return "", false, fmt.Errorf("%w: %s", errNoValueForPrompt, opts.variable)
}

// promptText is the line the operator answers, naming what an empty answer
// takes. A secret's fallback is never shown.
func promptText(opts askOptions) string {
	if opts.fallback == "" || opts.secret {
		return "  " + opts.prompt + ": "
	}

	return "  " + opts.prompt + " [" + opts.fallback + "]: "
}

// credentialLine is one KEY=VALUE line of the credentials file.
type credentialLine struct {
	name  string
	value string
}

// fileLines picks the values the file carries: every answer the environment
// did not supply. A value holding a line break is refused, because the loader
// reads the file line by line and would split it.
func fileLines(creds bitwarden.Credentials, inEnv map[string]bool) ([]credentialLine, error) {
	var lines []credentialLine

	for _, line := range []credentialLine{
		{bitwarden.EnvItem, creds.Item},
		{bitwarden.EnvClientID, creds.ClientID},
		{bitwarden.EnvClientSecret, creds.ClientSecret},
		{bitwarden.EnvMasterKey, creds.MasterKey},
	} {
		if inEnv[line.name] {
			continue
		}

		if hasLineBreak(line.value) {
			return nil, fmt.Errorf("%w: %s", errCredentialLineBreak, line.name)
		}

		lines = append(lines, line)
	}

	return lines, nil
}

// renderCredentials builds the file. Every value is wrapped in single quotes
// because the loader strips one matching pair and unescapes nothing, so the wrap
// returns the inner text exactly. A master password that begins and ends with a
// quote comes back two characters shorter without it.
//
// Nothing is escaped here: no shell sources this file.
func renderCredentials(lines []credentialLine) []byte {
	var out strings.Builder

	out.WriteString("# Bitwarden API credentials for `cairnos secrets pull`. Keep this file 0600.\n")

	for _, line := range lines {
		fmt.Fprintf(&out, "%s='%s'\n", line.name, line.value)
	}

	return []byte(out.String())
}

// checkVault proves the stored credentials still open the vault and that the
// item gives the pull something to write, so an operator finds out here rather
// than from a failed deploy. LoadCredentials holds the file to the mode rule
// and names every missing value. It prints no value and never rewrites the file.
func (a *App) checkVault(ctx context.Context, label, path string, opts pullOptions) int {
	creds, err := bitwarden.LoadCredentials(path, a.envLookup().Get)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	count, err := a.countPulledValues(ctx, creds, opts)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	a.printOutf("%s: %s; the vault is locked again\n", label, count)

	return ExitOK
}

// countPulledValues reads the item and plans the pull the way `secrets pull`
// would, mappings included, and describes how many variables it would write.
// An item that gives the pull nothing fails here as it would there.
func (a *App) countPulledValues(ctx context.Context, creds bitwarden.Credentials, opts pullOptions) (string, error) {
	itemName, err := authItem(opts.cfg, creds)
	if err != nil {
		return "", err
	}

	item, err := a.readVaultItem(ctx, opts.dir, creds, itemName)
	if err != nil {
		return "", err
	}

	var mappings map[string]string

	if opts.cfg.Secrets.Bitwarden != nil {
		mappings = opts.cfg.Secrets.Bitwarden.Fields
	}

	values, err := planValues(opts.cfg, item, mappings)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("item %q gives the pull %d variables", item.Name, len(values)), nil
}

// authItem resolves the item for the status check, tolerating a host with no
// [secrets.bitwarden] block yet: the credentials file alone is enough to look.
func authItem(cfg *config.Config, creds bitwarden.Credentials) (string, error) {
	if cfg.Secrets.Bitwarden != nil {
		return resolveItem(cfg.Secrets.Bitwarden, creds)
	}

	if creds.Item != "" {
		return creds.Item, nil
	}

	return "", errNoVaultItem
}
