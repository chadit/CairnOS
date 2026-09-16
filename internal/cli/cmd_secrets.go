package cli

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/secrets"
)

// envShadowMarker is the ls column value when an env var shadows a store row,
// greppable so an operator (or a test) can spot a silently masked rotation. Env
// wins the resolution chain, so a shadowed store value is not the one served.
const envShadowMarker = "shadowed_by_env"

// cmdSecrets dispatches the encrypted secret store subcommands
// (.agents/spec/parity/secrets-credentials.md, "CLI"). The help states the two
// rotation facts (a row under an unloaded key is lost outside the prev-key
// window, the ls key id is the only diagnostic) and that keygen output must come
// from a CSPRNG, so the hazards are visible before anything runs (security
// council amendment 3).
func (a *App) cmdSecrets(ctx context.Context, args []string) int {
	const usage = `usage: cairnos secrets <set|get|rm|ls|rotate|keygen|install-bw|auth|pull> [flags]

  set <name>   Read a value from stdin (never argv, which leaks into shell
               history and ps) and seal it. One trailing newline is trimmed.
  get <name>   Write the raw value plus one newline to stdout; diagnostics go
               to stderr.
  rm <name>    Delete a row; a missing name exits non-zero.
  ls           List name, key id, created, updated, and whether an env var
               shadows the row (env wins, so a shadowed store value is not
               served).
  rotate       Re-seal every row under the primary key and print the count.
               A row sealed under a key no longer loaded is lost by design; the
               previous-key window (CAIRNOS_SECRETS_PREV_KEY) is the only
               recovery path, and the ls key id column is the only diagnostic
               for which generation a stuck row belongs to.
  install-bw   Download the pinned Bitwarden CLI, check its sha256, and install
               it at ~/.local/bin/bw. --bin-dir installs somewhere else,
               --force replaces a binary that is already there.
  auth         Write the Bitwarden credentials file when it is missing,
               prompting for the item name and the API credentials and taking
               chezmoi's values as the defaults. When the file already exists,
               open the vault once and report the item and how many CAIRNOS_
               fields it carries, then lock it again.
  pull         Render the [secrets.bitwarden] fields into the service's second
               environment file. Runs at deployment, not at run time, because
               most credentials here are read by bare os.Getenv. --dry-run
               resolves everything and reports without writing.
  keygen       Print a fresh base64 master key from the OS CSPRNG. A hand-typed
               or passphrase-derived 32-byte key passes the length check yet is
               insecure; a master key must come from a CSPRNG. Key compromise
               loses the whole store.

Only set, get, rm, ls, and rotate need [secrets] backend = "store".
`

	if len(args) == 0 {
		a.printErrf("%s", usage)

		return ExitUsage
	}

	switch args[0] {
	case "set":
		return a.cmdSecretsSet(ctx, args[1:])
	case subcmdGet:
		return a.cmdSecretsGet(ctx, args[1:])
	case "rm":
		return a.cmdSecretsRm(ctx, args[1:])
	case "ls":
		return a.cmdSecretsLs(ctx, args[1:])
	case "rotate":
		return a.cmdSecretsRotate(ctx, args[1:])
	case "install-bw":
		return a.cmdSecretsInstallBW(ctx, args[1:])
	case "auth":
		return a.cmdSecretsAuth(ctx, args[1:])
	case "pull":
		return a.cmdSecretsPull(ctx, args[1:])
	case "keygen":
		return a.cmdSecretsKeygen(args[1:])
	default:
		a.printErrf("unknown secrets subcommand %q\n", args[0])
		a.printErrf("%s", usage)

		return ExitUsage
	}
}

// openSecretsAt loads the config, refuses to run unless the store backend is
// configured, and opens the encrypted store, the openStoreAt shape for the
// secrets subcommands. The backend guard means nobody writes to a store the
// engine is not reading. Key or store errors carry env var names only, never
// key material.
func (a *App) openSecretsAt(ctx context.Context, label, dirFlagValue string) (secrets.Store, int, bool) {
	if err := ctx.Err(); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	dir, err := resolveDir(dirFlagValue)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	if cfg.Secrets.Backend != config.SecretsBackendStore {
		a.printErrf("%s: secret store is not configured; set [secrets] backend = %q (currently %q)\n",
			label, config.SecretsBackendStore, cfg.Secrets.Backend)

		return nil, ExitFailure, false
	}

	secretStore, err := openSecretsStore(ctx, dir, &cfg)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	return secretStore, ExitOK, true
}

// cmdSecretsSet reads the value from stdin (never argv, which leaks into shell
// history and ps) and seals it, trimming exactly one trailing newline so a
// heredoc or echo pipe stores the intended value.
func (a *App) cmdSecretsSet(ctx context.Context, args []string) int {
	const label = "secrets set"

	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	name, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if name == "" {
		a.printErrf("usage: cairnos secrets set <name> [--dir D]  (value read from stdin)\n")

		return ExitUsage
	}

	secretStore, code, proceed := a.openSecretsAt(ctx, label, *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(secretStore)

	raw, err := io.ReadAll(a.stdin)
	if err != nil {
		a.printErrf("%s: read value from stdin: %v\n", label, err)

		return ExitFailure
	}

	if err := secretStore.Set(ctx, name, trimOneTrailingNewline(string(raw))); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	a.printOutf("set %s\n", secrets.NormalizeName(name))

	return ExitOK
}

// cmdSecretsGet writes the raw value plus one trailing newline to stdout;
// explicit retrieval is the point, so everything else stays on stderr.
func (a *App) cmdSecretsGet(ctx context.Context, args []string) int {
	const label = "secrets get"

	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	name, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if name == "" {
		a.printErrf("usage: cairnos secrets get <name> [--dir D]\n")

		return ExitUsage
	}

	secretStore, code, proceed := a.openSecretsAt(ctx, label, *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(secretStore)

	value, err := secretStore.Get(ctx, name)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	a.printOutf("%s\n", value)

	return ExitOK
}

// cmdSecretsRm deletes one row; a missing name exits non-zero naming the secret,
// so a caller can tell a delete from a no-op.
func (a *App) cmdSecretsRm(ctx context.Context, args []string) int {
	const label = "secrets rm"

	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	name, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if name == "" {
		a.printErrf("usage: cairnos secrets rm <name> [--dir D]\n")

		return ExitUsage
	}

	secretStore, code, proceed := a.openSecretsAt(ctx, label, *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(secretStore)

	if err := secretStore.Delete(ctx, name); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	a.printOutf("removed %s\n", secrets.NormalizeName(name))

	return ExitOK
}

// cmdSecretsLs prints one row per secret with its key id and timestamps and a
// marker for any row an env var shadows. It never prints a value: List returns
// metadata only, and the shadow check reads variable names, not values.
func (a *App) cmdSecretsLs(ctx context.Context, args []string) int {
	const label = "secrets ls"

	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	secretStore, code, proceed := a.openSecretsAt(ctx, label, *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(secretStore)

	metas, err := secretStore.List(ctx)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	names := make([]string, len(metas))
	for idx := range metas {
		names[idx] = metas[idx].Name
	}

	shadowed := make(map[string]bool, len(names))
	for _, name := range secrets.ShadowedByEnv(names, os.LookupEnv) {
		shadowed[name] = true
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "NAME\tKEY ID\tCREATED\tUPDATED\tENV_SHADOW\n")

	for idx := range metas {
		meta := &metas[idx]
		marker := "-"

		if shadowed[meta.Name] {
			marker = envShadowMarker
		}

		a.tabWritef(tab, "%s\t%s\t%s\t%s\t%s\n",
			meta.Name, meta.KeyID, formatTableTime(meta.CreatedAt), formatTableTime(meta.UpdatedAt), marker)
	}

	a.flushTab(tab)

	return ExitOK
}

// cmdSecretsRotate re-seals every row not under the primary key and prints the
// count, so an operator can retire the previous key once the count settles.
func (a *App) cmdSecretsRotate(ctx context.Context, args []string) int {
	const label = "secrets rotate"

	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	secretStore, code, proceed := a.openSecretsAt(ctx, label, *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(secretStore)

	count, err := secretStore.Rotate(ctx)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	a.printOutf("rotated %d secret(s) to the primary key\n", count)

	return ExitOK
}

// cmdSecretsKeygen prints a fresh base64 master key from the OS CSPRNG. It needs
// no store, so it works before [secrets] backend is configured: an operator
// generates a key here, sets CAIRNOS_SECRETS_KEY, then enables the store. The
// value goes to stdout as the command's output, never a log line.
func (a *App) cmdSecretsKeygen(args []string) int {
	const label = "secrets keygen"

	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	key, err := secrets.GenerateKey()
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	a.printOutf("%s\n", key)

	return ExitOK
}

// trimOneTrailingNewline drops a single trailing newline (LF or CRLF) so a
// value piped with echo or a heredoc stores what the operator meant, while a
// value that intentionally ends in two newlines keeps the second.
func trimOneTrailingNewline(value string) string {
	trimmed := strings.TrimSuffix(value, "\n")

	return strings.TrimSuffix(trimmed, "\r")
}
