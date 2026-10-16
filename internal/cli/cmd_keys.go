package cli

import (
	"context"
	"flag"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/store"
)

const keysUsage = `usage: cairnos keys <add|list|enable|disable> [flags]

  add      --label L --scopes s1,s2 [--workspace W]   Mint an access key; prints the plaintext once
  list                                                List keys (never the plaintext or hash)
  enable   <id>                                       Re-enable a key and lift its token denial
  disable  <id>                                       Disable a key and deny its live tokens
`

// cmdKeys dispatches the Phase 4a access-key management subcommands
// (.agents/spec/08-auth-and-access.md, "Access keys").
func (a *App) cmdKeys(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", keysUsage)

		return ExitUsage
	}

	switch args[0] {
	case subcmdAdd:
		return a.cmdKeysAdd(ctx, args[1:])
	case subcmdList:
		return a.cmdKeysList(ctx, args[1:])
	case "enable":
		return a.cmdKeysSetEnabled(ctx, "keys enable", args[1:], true)
	case "disable":
		return a.cmdKeysSetEnabled(ctx, "keys disable", args[1:], false)
	default:
		a.printErrf("unknown keys subcommand %q\n", args[0])
		a.printErrf("%s", keysUsage)

		return ExitUsage
	}
}

// cmdKeysAdd mints one access key. The plaintext appears exactly once on
// stdout; only the argon2id hash is stored, so a lost key means minting a
// new one.
func (a *App) cmdKeysAdd(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("keys add", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	labelFlag := flags.String("label", "", "human-readable key label (required)")
	scopesFlag := flags.String("scopes", "", "comma-separated scope list, e.g. read:run,write:workflow (required)")
	workspaceFlag := flags.String("workspace", "default", "workspace the key acts in; tokens it mints carry this workspace")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if *labelFlag == "" || *scopesFlag == "" {
		a.printErrf("keys add: --label and --scopes are required\n")

		return ExitUsage
	}

	scopes, err := auth.ParseScopes(*scopesFlag)
	if err != nil {
		a.printErrf("keys add: %v\n", err)

		return ExitUsage
	}

	if len(scopes) == 0 {
		a.printErrf("keys add: --scopes named no scopes\n")

		return ExitUsage
	}

	authStore, _, code, proceed := openAt(ctx, a, "keys add", *dirFlag, authBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(authStore)

	key, plaintext, err := auth.NewKey(*labelFlag, scopes, *workspaceFlag, time.Now())
	if err != nil {
		a.printErrf("keys add: %v\n", err)

		return ExitFailure
	}

	if err := authStore.CreateKey(ctx, key); err != nil {
		a.printErrf("keys add: %v\n", err)

		return ExitFailure
	}

	a.printOutf("created key %s (%s)\n\n", key.ID, key.Label)
	a.printOutf("  %s\n\n", plaintext)
	a.printOutf("This is the only time the key is shown; only its hash is stored.\n")

	return ExitOK
}

// cmdKeysList prints the key inventory: ids, labels, scopes, and state,
// never the plaintext or the hash.
func (a *App) cmdKeysList(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("keys list", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	authStore, _, code, proceed := openAt(ctx, a, "keys list", *dirFlag, authBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(authStore)

	keys, err := authStore.ListKeys(ctx)
	if err != nil {
		a.printErrf("keys list: %v\n", err)

		return ExitFailure
	}

	if len(keys) == 0 {
		a.printOutf("no access keys\n")

		return ExitOK
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "ID\tLABEL\tWORKSPACE\tSCOPES\tENABLED\tCREATED\n")

	for _, key := range keys {
		a.tabWritef(tab, "%s\t%s\t%s\t%s\t%t\t%s\n",
			key.ID, key.Label, keyWorkspaceOrDefault(key.Workspace),
			strings.Join(key.Scopes, ","), key.Enabled,
			formatTableTime(key.CreatedAt))
	}

	a.flushTab(tab)

	return ExitOK
}

// cmdKeysSetEnabled is the shared enable/disable spine. Disabling also
// denylists the key's live tokens (deny-by-sub, the spec's recommended
// default) so revocation is immediate, not just at the next login.
// Enabling lifts that denial, which restores any of the key's unexpired
// tokens too; revoke by key id afterwards to keep those dead.
func (a *App) cmdKeysSetEnabled(ctx context.Context, label string, args []string, enabled bool) int {
	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	keyID, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if keyID == "" {
		a.printErrf("usage: cairnos %s <id> [--dir D]\n", label)

		return ExitUsage
	}

	authStore, cfg, code, proceed := openAt(ctx, a, label, *dirFlag, authBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(authStore)

	if err := authStore.SetKeyEnabled(ctx, keyID, enabled); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	if enabled {
		if err := authStore.RemoveKeyDenials(ctx, keyID); err != nil {
			a.printErrf("%s: %v\n", label, err)

			return ExitFailure
		}

		a.printOutf("enabled %s\n", keyID)
		// Re-enabling lifts the deny-by-sub row, so any of the key's tokens
		// that were live before the disable are usable again. Warn so an
		// operator who wanted those dead runs the explicit revoke (spec L7).
		a.printOutf("warning: tokens minted before this key was disabled are valid again; run `cairnos auth revoke --key %s` to kill them\n", keyID)

		return ExitOK
	}

	// Disable denies every token minted up to now; the deny window outlasts
	// any live token because revokeWindow floors it at the 24-hour ceiling.
	// The key is disabled, so no fresh tokens can be minted past this point
	// anyway (unlike `auth revoke --key`, which leaves the key enabled).
	now := time.Now()
	if err := authStore.DenyKeySince(ctx, keyID, now, now.Add(revokeWindow(cfg.Auth.AccessTokenTTL))); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	a.printOutf("disabled %s and denied its live tokens\n", keyID)

	return ExitOK
}

// keyWorkspaceOrDefault renders a key's workspace for the list table, showing
// the default workspace name for a key created before tenancy stored one.
func keyWorkspaceOrDefault(workspace string) string {
	if workspace == "" {
		return store.DefaultWorkspace
	}

	return workspace
}
