package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/chadit/CairnOS/internal/a2a"
	"github.com/chadit/CairnOS/internal/config"
)

const a2aUsage = `usage: cairnos a2a card [--dir D]

  card   Print this node's signed agent card (the JSON served at the
         well-known path), so a consumer can inspect or pin it.
`

// cmdA2A dispatches the Phase 4b A2A subcommands
// (.agents/spec/09-a2a-and-hive.md). The card command reads only the config
// and local key material, so it needs no context.
func (a *App) cmdA2A(args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", a2aUsage)

		return ExitUsage
	}

	switch args[0] {
	case "card":
		return a.cmdA2ACard(args[1:])
	default:
		a.printErrf("unknown a2a subcommand %q\n", args[0])
		a.printErrf("%s", a2aUsage)

		return ExitUsage
	}
}

// cmdA2ACard builds and prints this node's signed agent card. It resolves the
// signing key the same way serve does (file, then reused auth EdDSA key, then
// ephemeral), so the printed card matches what the running node would publish,
// give or take an ephemeral key that changes per invocation.
func (a *App) cmdA2ACard(args []string) int {
	flags := flag.NewFlagSet("a2a card", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	dir, err := resolveDir(*dirFlag)
	if err != nil {
		a.printErrf("a2a card: %v\n", err)

		return ExitFailure
	}

	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		a.printErrf("a2a card: load config: %v\n", err)

		return ExitFailure
	}

	card, err := a.buildCard(&cfg)
	if err != nil {
		a.printErrf("a2a card: %v\n", err)

		return ExitFailure
	}

	cardJSON, err := card.Marshal()
	if err != nil {
		a.printErrf("a2a card: %v\n", err)

		return ExitFailure
	}

	a.printOutf("%s\n", indentJSON(cardJSON))

	return ExitOK
}

// indentJSON pretty-prints the card for terminal display, falling back to the
// compact bytes when re-indenting fails (which it will not for a card this
// package just marshaled, but the fallback keeps the command from erroring on
// a print concern).
func indentJSON(data []byte) string {
	var out bytes.Buffer

	if err := json.Indent(&out, data, "", "  "); err != nil {
		return string(data)
	}

	return out.String()
}

// buildCard signs an agent card from the config, resolving the signing key
// through the same priority order serve uses, so the printed card matches what
// the running node would publish.
func (a *App) buildCard(cfg *config.Config) (*a2a.Card, error) {
	signingKey, _, err := a2a.ResolveSigningKey(cfg.A2A.CardSigningKeyFile, reusableAuthKey(cfg), a.logger)
	if err != nil {
		return nil, fmt.Errorf("resolve card signing key: %w", err)
	}

	endpoint := cfg.A2A.EndpointAdvertise
	if endpoint == "" {
		endpoint = a2aGRPCScheme + cfg.API.GRPC
	}

	bundle, err := a2a.NewBundle(&a2a.BundleConfig{
		NodeName:   cfg.Auth.Issuer,
		Version:    appVersion(),
		Endpoint:   endpoint,
		LoginURL:   "http://" + cfg.API.HTTP + "/v1/login",
		SigningKey: signingKey,
		Logger:     a.logger,
	})
	if err != nil {
		return nil, fmt.Errorf("build card: %w", err)
	}

	return bundle.Card, nil
}
