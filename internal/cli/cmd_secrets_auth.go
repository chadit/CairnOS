package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/config"
)

// defaultVaultItem is the item name offered when neither chezmoi nor the
// operator names one. It matches what the workstation tooling already uses.
const defaultVaultItem = "chezmoi"

// cmdSecretsAuth creates the credentials file or checks the one that is there.
// It exists because the alternative is telling an operator to hand-write four
// variables into a file at the right mode, which is a step that goes wrong
// quietly and is only noticed at the next deploy.
func (a *App) cmdSecretsAuth(ctx context.Context, args []string) int {
	const label = "secrets auth"

	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)
	system := flags.Bool("system", false, "use the paths a system-mode unit reads")

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	dir, err := resolveSecretsDir(*dirFlag, *system)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	opts := pullOptions{dir: dir, cfg: &cfg, system: *system}
	path := opts.resolve(credentialsFileFrom(&cfg), config.DefaultBitwardenCredentialsFile)

	if _, statErr := os.Stat(path); statErr == nil {
		return a.checkVault(ctx, label, path, &cfg)
	}

	return a.writeCredentials(ctx, label, path, &cfg)
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

// writeCredentials prompts for what is missing and writes the file. Anything
// already in the process environment is taken from there without asking, so a
// host that injects one of these does not have it typed in twice.
func (a *App) writeCredentials(ctx context.Context, label, path string, cfg *config.Config) int {
	defaults := readChezmoiDefaults(a.logger)

	if defaults.itemName != "" || defaults.hasSecrets() {
		a.printOutf("%s: chezmoi config found; press Enter to accept its values\n", label)
	}

	reader := bufio.NewReader(a.stdin)

	creds, err := a.promptCredentials(ctx, reader, defaults, cfg)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	if writeErr := a.writeFileAtomic(path, renderCredentials(creds), envFilePerm); writeErr != nil {
		a.printErrf("%s: %v\n", label, writeErr)

		return ExitFailure
	}

	a.printOutf("%s: wrote %s (item %s); run `cairnos secrets pull` next\n", label, path, creds.Item)

	return ExitOK
}

// promptCredentials collects the four values, in the order an operator would
// find them in the vault UI.
func (a *App) promptCredentials(
	ctx context.Context, reader *bufio.Reader, defaults chezmoiDefaults, cfg *config.Config,
) (bitwarden.Credentials, error) {
	offered := itemDefault(defaults, cfg)

	item, err := a.ask(ctx, reader, askOptions{
		variable:    bitwarden.EnvItem,
		prompt:      "Bitwarden item name",
		fallback:    offered,
		shownAs:     offered,
		fromChezmoi: defaults.itemName != "",
	})
	if err != nil {
		return bitwarden.Credentials{}, err
	}

	creds := bitwarden.Credentials{Item: item}

	for _, field := range []struct {
		target   *string
		variable string
		prompt   string
		fallback string
	}{
		{&creds.ClientID, bitwarden.EnvClientID, "Bitwarden API client id", defaults.clientID},
		{&creds.ClientSecret, bitwarden.EnvClientSecret, "Bitwarden API client secret", defaults.clientSecret},
		{&creds.MasterKey, bitwarden.EnvMasterKey, "Bitwarden master password", defaults.masterKey},
	} {
		value, askErr := a.ask(ctx, reader, askOptions{
			variable:    field.variable,
			prompt:      field.prompt,
			fallback:    field.fallback,
			shownAs:     "from chezmoi",
			fromChezmoi: field.fallback != "",
			secret:      true,
		})
		if askErr != nil {
			return bitwarden.Credentials{}, askErr
		}

		*field.target = value
	}

	return creds, nil
}

// itemDefault picks the item name to offer: the config block first, since that
// is what the pull will use, then chezmoi's, then the conventional name.
func itemDefault(defaults chezmoiDefaults, cfg *config.Config) string {
	if cfg.Secrets.Bitwarden != nil && cfg.Secrets.Bitwarden.Item != "" {
		return cfg.Secrets.Bitwarden.Item
	}

	if defaults.itemName != "" {
		return defaults.itemName
	}

	return defaultVaultItem
}

// askOptions is one prompt: which variable it fills, what to show, and what an
// empty answer means.
type askOptions struct {
	variable    string
	prompt      string
	fallback    string
	shownAs     string
	fromChezmoi bool
	secret      bool
}

// ask returns the environment's value when it has one, otherwise reads a line.
// An empty line takes the fallback. A missing value with nothing to read from
// fails naming the variable rather than blocking on a pipe that will never
// produce a line.
func (a *App) ask(ctx context.Context, reader *bufio.Reader, opts askOptions) (string, error) {
	if value := strings.TrimSpace(os.Getenv(opts.variable)); value != "" {
		a.printOutf("  %s: taken from the environment\n", opts.variable)

		return value, nil
	}

	line, err := a.readLine(ctx, reader, promptText(opts), opts.secret)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read %s: %w", opts.variable, err)
	}

	answer := strings.TrimSpace(line)
	if answer != "" {
		return answer, nil
	}

	if opts.fallback != "" {
		return opts.fallback, nil
	}

	return "", fmt.Errorf("%w: %s", errNoValueForPrompt, opts.variable)
}

// promptText is the line the operator answers, naming what an empty answer takes.
func promptText(opts askOptions) string {
	return "  " + opts.prompt + defaultHint(opts) + ": "
}

// defaultHint renders what an empty answer will take, never the value itself
// when it is a secret.
func defaultHint(opts askOptions) string {
	switch {
	case opts.fromChezmoi && opts.secret:
		return " [from chezmoi]"
	case opts.fallback != "":
		return " [" + opts.shownAs + "]"
	default:
		return ""
	}
}

// renderCredentials builds the file. Every value is wrapped in single quotes
// because the loader strips one matching pair and unescapes nothing, so the wrap
// returns the inner text exactly. A master password that begins and ends with a
// quote comes back two characters shorter without it.
//
// Nothing is escaped here: no shell sources this file.
func renderCredentials(creds bitwarden.Credentials) []byte {
	var out strings.Builder

	out.WriteString("# Bitwarden API credentials for `cairnos secrets pull`. Keep this file 0600.\n")

	for _, pair := range [][2]string{
		{bitwarden.EnvItem, creds.Item},
		{bitwarden.EnvClientID, creds.ClientID},
		{bitwarden.EnvClientSecret, creds.ClientSecret},
		{bitwarden.EnvMasterKey, creds.MasterKey},
	} {
		fmt.Fprintf(&out, "%s='%s'\n", pair[0], pair[1])
	}

	return []byte(out.String())
}

// checkVault proves the stored credentials still open the vault, so an operator
// finds out here rather than from a failed deploy. It prints what it found and
// never a value.
func (a *App) checkVault(ctx context.Context, label, path string, cfg *config.Config) int {
	binary, err := a.ensureBitwardenBinary(ctx, label)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	creds, err := bitwarden.LoadCredentials(path, os.Getenv)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	itemName, err := authItem(cfg, creds)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	client := &bitwarden.Client{Binary: binary, Logger: a.logger}

	item, err := client.Read(ctx, creds, itemName)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	a.printOutf("%s: item %q carries %d CAIRNOS_ fields; the vault is locked again\n",
		label, item.Name, countCairnosFields(item))

	return ExitOK
}

// ensureBitwardenBinary finds the CLI, installing the pinned release when the
// host has none. Failing here with "not on PATH" would send the operator off to
// the README in the middle of a check they asked for.
func (a *App) ensureBitwardenBinary(ctx context.Context, label string) (string, error) {
	binary, err := lookBitwardenBinary()
	if err == nil {
		return binary, nil
	}

	dir, err := bwInstallDir("")
	if err != nil {
		return "", err
	}

	a.printOutf("%s: %s is not on PATH; installing the pinned release into %s\n", label, bwBinary, dir)

	result, err := a.installBitwarden(ctx, dir, false)
	if err != nil {
		return "", err
	}

	a.printOutf("%s: installed %s (version %s)\n", label, result.Path, result.Version)

	return result.Path, nil
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

	return "", fmt.Errorf("%w: %s", errNoVaultItem, bitwarden.EnvItem)
}
