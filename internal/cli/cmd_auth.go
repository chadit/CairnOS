package cli

import (
	"context"
	"flag"
	"time"
)

const authUsage = `usage: cairnos auth revoke (--jti X | --key K) [--dir D]

  revoke --jti X   Denylist one token by its jti claim
  revoke --key K   Denylist every live token minted from access key K
`

// cmdAuth dispatches the Phase 4a revocation subcommand
// (.agents/spec/08-auth-and-access.md, Change 1). Revocation writes the
// shared denylist directly, so it works while the serve daemon holds the
// database open (WAL) and takes effect on the daemon's next verify.
func (a *App) cmdAuth(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", authUsage)

		return ExitUsage
	}

	switch args[0] {
	case "revoke":
		return a.cmdAuthRevoke(ctx, args[1:])
	default:
		a.printErrf("unknown auth subcommand %q\n", args[0])
		a.printErrf("%s", authUsage)

		return ExitUsage
	}
}

// cmdAuthRevoke denylists one token (--jti) or one key's every live token
// (--key). The deny window is now plus the configured token TTL, the
// upper bound of any live token's remaining life, since a bare jti does
// not carry its exp.
func (a *App) cmdAuthRevoke(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("auth revoke", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	jtiFlag := flags.String("jti", "", "token id (jti claim) to denylist")
	keyFlag := flags.String("key", "", "access key id whose live tokens to denylist")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if (*jtiFlag == "") == (*keyFlag == "") {
		a.printErrf("auth revoke: set exactly one of --jti or --key\n")

		return ExitUsage
	}

	authStore, cfg, code, proceed := openAt(ctx, a, "auth revoke", *dirFlag, authBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(authStore)

	now := time.Now()
	until := now.Add(revokeWindow(cfg.Auth.AccessTokenTTL))

	if *jtiFlag != "" {
		if err := authStore.DenyToken(ctx, *jtiFlag, until); err != nil {
			a.printErrf("auth revoke: %v\n", err)

			return ExitFailure
		}

		a.printOutf("revoked token %s until %s\n", *jtiFlag, formatTableTime(until))

		return ExitOK
	}

	// DenyKeySince denies only tokens minted at or before now, so the
	// still-enabled key keeps issuing usable tokens (spec M3).
	if err := authStore.DenyKeySince(ctx, *keyFlag, now, until); err != nil {
		a.printErrf("auth revoke: %v\n", err)

		return ExitFailure
	}

	a.printOutf("revoked every live token of key %s until %s\n", *keyFlag, formatTableTime(until))

	return ExitOK
}

// revokeCeiling caps the CLI deny window for tokens whose exp this command
// cannot look up: long enough that a shortened access_token_ttl cannot
// resurrect a still-live token (spec M2), and bounded so the row prunes.
const revokeCeiling = 24 * time.Hour

// revokeWindow is the CLI's deny-window bound: the larger of the configured
// TTL and the 24-hour ceiling, mirroring the authenticator's own fallback so
// the CLI and the served Revoke RPC agree.
func revokeWindow(ttl time.Duration) time.Duration {
	return max(ttl, revokeCeiling)
}
