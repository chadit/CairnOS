package cli

import (
	"context"
	"flag"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/hive"
	"github.com/chadit/CairnOS/internal/secrets"
	"github.com/chadit/CairnOS/internal/store"
)

const hiveUsage = `usage: cairnos hive <subscribe|publish|status> [flags]

  subscribe   Pull content-free tips for the opted-in categories into the
              suggestion review queue. Always safe: receive only.
  publish     Publish one approved suggestion as a content-free tip (gated;
              errors unless [hive] publish_enabled is set).
  status      Print the hive configuration: enabled, endpoint, categories, and
              the publish gate state.
`

// cmdHive dispatches the Phase 6 hive subcommands
// (.agents/spec/09-a2a-and-hive.md, "The Hive Network").
func (a *App) cmdHive(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", hiveUsage)

		return ExitUsage
	}

	switch args[0] {
	case "subscribe":
		return a.cmdHiveSubscribe(ctx, args[1:])
	case subcmdPublish:
		return a.cmdHivePublish(ctx, args[1:])
	case subcmdStatus:
		return a.cmdHiveStatus(args[1:])
	default:
		a.printErrf("unknown hive subcommand %q\n", args[0])
		a.printErrf("%s", hiveUsage)

		return ExitUsage
	}
}

// cmdHiveSubscribe pulls tips for the opted-in categories and lands them in the
// suggestion queue, so they go through the same human review and replay
// guardrail as local suggestions. Subscribing is receive-only, so it works
// regardless of the publish gate.
func (a *App) cmdHiveSubscribe(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("hive subscribe", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	db, cfg, code, proceed := openAt(ctx, a, "hive subscribe", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	if !cfg.Hive.Enabled {
		a.printErrf("hive subscribe: hive is disabled; set [hive] enabled = true\n")

		return ExitFailure
	}

	dir, err := resolveDir(*dirFlag, a.envLookup())
	if err != nil {
		a.printErrf("hive subscribe: %v\n", err)

		return ExitFailure
	}

	source, closeSource, err := a.buildSecretSource(ctx, dir, &cfg)
	if err != nil {
		a.printErrf("hive subscribe: %v\n", err)

		return ExitFailure
	}
	defer a.runCloser("secret source", closeSource)

	client, err := buildHiveClient(ctx, source, &cfg)
	if err != nil {
		a.printErrf("hive subscribe: %v\n", err)

		return ExitFailure
	}

	tips, err := client.Subscribe(ctx, hiveCategories(cfg.Hive.Categories))
	if err != nil {
		a.printErrf("hive subscribe: %v\n", err)

		return ExitFailure
	}

	ingester, err := hive.NewIngester(db, store.NewID)
	if err != nil {
		a.printErrf("hive subscribe: %v\n", err)

		return ExitFailure
	}

	saved, err := ingester.Ingest(ctx, tips)
	if err != nil {
		a.printErrf("hive subscribe: %v\n", err)

		return ExitFailure
	}

	a.printOutf("pulled %d tip(s) into the suggestion queue; review them with `cairnos suggestions list --status open`\n", saved)

	return ExitOK
}

// cmdHivePublish publishes one approved suggestion as a content-free tip, after
// the allowlist transform and the gate. It refuses unless publish_enabled is
// set, refuses anything the transform cannot reduce to a safe tip, and refuses
// below the local min_observations floor.
func (a *App) cmdHivePublish(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("hive publish", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	obsFlag := flags.Int("observations", 0, "how many independent times this node saw the pattern (checked against min_observations)")
	dirFlag := dirFlagOn(flags)

	id, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if id == "" {
		a.printErrf("usage: cairnos hive publish <suggestion-id> --observations N [--dir D]\n")

		return ExitUsage
	}

	db, cfg, code, proceed := openAt(ctx, a, "hive publish", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	dir, err := resolveDir(*dirFlag, a.envLookup())
	if err != nil {
		a.printErrf("hive publish: %v\n", err)

		return ExitFailure
	}

	return a.publishSuggestion(ctx, dir, db, &cfg, id, *obsFlag)
}

// publishSuggestion loads the suggestion, transforms it to a content-free tip,
// and publishes it through the gated client.
func (a *App) publishSuggestion(ctx context.Context, dir string, db store.Store, cfg *config.Config, id string, observations int) int {
	if !cfg.Hive.Enabled {
		a.printErrf("hive publish: hive is disabled; set [hive] enabled = true\n")

		return ExitFailure
	}

	suggestion, err := db.GetSuggestion(ctx, id)
	if err != nil {
		a.printErrf("hive publish: %v\n", err)

		return ExitFailure
	}

	category := publishCategory(cfg.Hive.Categories)

	transformer, err := hive.NewTransformer(category)
	if err != nil {
		a.printErrf("hive publish: %v\n", err)

		return ExitFailure
	}

	tip, ok := transformer.Transform(*suggestion)
	if !ok {
		a.printErrf("hive publish: %v: suggestion %s is %s\n", hive.ErrUnshareable, id, suggestion.Kind)

		return ExitFailure
	}

	source, closeSource, err := a.buildSecretSource(ctx, dir, cfg)
	if err != nil {
		a.printErrf("hive publish: %v\n", err)

		return ExitFailure
	}
	defer a.runCloser("secret source", closeSource)

	client, err := buildHiveClient(ctx, source, cfg)
	if err != nil {
		a.printErrf("hive publish: %v\n", err)

		return ExitFailure
	}

	if err := client.Publish(ctx, tip, observations); err != nil {
		a.printErrf("hive publish: %v\n", err)

		return ExitFailure
	}

	a.printOutf("published a content-free tip for suggestion %s\n", id)

	return ExitOK
}

// cmdHiveStatus prints the hive configuration so an operator can see the gate
// state without parsing the config file.
func (a *App) cmdHiveStatus(args []string) int {
	flags := flag.NewFlagSet("hive status", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	cfg, code, proceed := a.loadConfigAt(*dirFlag, "hive status")
	if !proceed {
		return code
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "FIELD\tVALUE\n")
	a.tabWritef(tab, "enabled\t%t\n", cfg.Hive.Enabled)
	a.tabWritef(tab, "endpoint\t%s\n", orNone(cfg.Hive.Endpoint))
	a.tabWritef(tab, "publish_enabled\t%t (gated on the privacy review and hosting decision)\n", cfg.Hive.PublishEnabled)
	a.tabWritef(tab, "categories\t%v\n", cfg.Hive.Categories)
	a.tabWritef(tab, "k_anonymity_floor\t%d\n", cfg.Hive.KAnonymityFloor)
	a.tabWritef(tab, "min_observations\t%d\n", cfg.Hive.MinObservations)
	a.flushTab(tab)

	return ExitOK
}

// buildHiveClient builds the HTTP hive client from config, minting a fresh
// rotating pseudonym when none is configured so the node never presents a
// stable id. When [hive] key_ref names a shared key, the client seals outbound
// tips and opens received envelopes under it; a configured-but-missing key is a
// hard error rather than a silent drop to plaintext.
func buildHiveClient(ctx context.Context, source secrets.Source, cfg *config.Config) (*hive.HTTPHiveClient, error) {
	pseudonym := cfg.Hive.NodePseudonym
	if pseudonym == "" {
		minted, err := hive.NewPseudonym()
		if err != nil {
			return nil, err //nolint:wrapcheck // the caller prefixes the command label
		}

		pseudonym = minted
	}

	opts, err := hiveSealerOption(ctx, source, cfg)
	if err != nil {
		return nil, err
	}

	client, err := hive.NewHTTPClient(hive.Config{
		Enabled:         cfg.Hive.Enabled,
		Endpoint:        cfg.Hive.Endpoint,
		NodePseudonym:   pseudonym,
		PublishEnabled:  cfg.Hive.PublishEnabled,
		Categories:      hiveCategories(cfg.Hive.Categories),
		KAnonymityFloor: cfg.Hive.KAnonymityFloor,
		MinObservations: cfg.Hive.MinObservations,
	}, nil, opts...)
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller prefixes the command label
	}

	return client, nil
}

// hiveSealerOption resolves the shared key through the SecretSource seam and
// returns the WithSealer option when [hive] key_ref is set, or no options when
// it is empty (the plaintext, no-key mode). The key is the privacy guarantee, so
// a lookup or decode failure stops construction.
func hiveSealerOption(ctx context.Context, source secrets.Source, cfg *config.Config) ([]hive.Option, error) {
	if cfg.Hive.KeyRef == "" {
		return nil, nil
	}

	sealer, err := hive.RotatingSealerFromSecret(ctx, source, cfg.Hive.KeyRef, cfg.Hive.PrevKeyRef)
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller prefixes the command label
	}

	return []hive.Option{hive.WithSealer(sealer)}, nil
}

// hiveCategories converts the config's category strings to hive.Category,
// dropping any outside the fixed set so a typo never reaches the wire.
func hiveCategories(raw []string) []hive.Category {
	out := make([]hive.Category, 0, len(raw))

	for _, name := range raw {
		category := hive.Category(name)
		if hive.ValidCategory(category) {
			out = append(out, category)
		}
	}

	return out
}

// publishCategory picks the category a published tip carries: the first valid
// opted-in category, falling back to the general tag so a tip always has a
// category from the fixed set.
func publishCategory(raw []string) hive.Category {
	for _, category := range hiveCategories(raw) {
		return category
	}

	return hive.CategoryGeneral
}

// orNone renders an empty string as a placeholder for the status table.
func orNone(value string) string {
	if value == "" {
		return "(none)"
	}

	return value
}
