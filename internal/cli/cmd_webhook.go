package cli

import (
	"context"
	"flag"
	"path/filepath"

	"github.com/chadit/CairnOS/internal/webhook"
	"github.com/chadit/CairnOS/internal/workflow"
)

const webhookUsage = "usage: cairnos webhook <add|list|rotate|remove> [flags]\n"

// Ingress route prefixes the minted tokens complete; the handler serves POST on
// both. The CLI prints the path form because it does not know the serve host,
// so an operator prepends their own scheme and authority.
const (
	hookPathPrefix     = "/v1/hooks/"      // #nosec G101 -- route prefix, not a credential
	hookTestPathPrefix = "/v1/hooks/test/" // #nosec G101 -- route prefix, not a credential
)

// webhookTokenNotice fires once after tokens are printed: only hashes are
// stored, so a lost token means a rotate, never a lookup.
const webhookTokenNotice = "save these now; the tokens are not retrievable after this.\n"

// hmacLabelOn and hmacLabelOff render the HMAC column in webhook list. HMAC is
// on when a secret ref is set; the list never shows the ref value, only its
// presence.
const (
	hmacLabelOn  = "on"
	hmacLabelOff = "off"
)

func (a *App) cmdWebhook(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", webhookUsage)

		return ExitUsage
	}

	switch args[0] {
	case subcmdAdd:
		return a.cmdWebhookAdd(ctx, args[1:])
	case subcmdList:
		return a.cmdWebhookList(ctx, args[1:])
	case subcmdRotate:
		return a.cmdWebhookRotate(ctx, args[1:])
	case subcmdRemove:
		return a.cmdWebhookRemove(ctx, args[1:])
	default:
		a.printErrf("unknown webhook subcommand %q\n", args[0])
		a.printErrf("%s", webhookUsage)

		return ExitUsage
	}
}

// cmdWebhookAdd registers one workflow for webhook ingress: it validates the
// file, mints independent production and test tokens, stores only their hashes,
// and prints the two URL paths with their tokens exactly once. A workflow that
// already has a registration is ErrDuplicate: one registration per workflow.
func (a *App) cmdWebhookAdd(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("webhook add", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)
	hmacSecret := flags.String("hmac-secret", "", "secrets.Source name for HMAC verification (empty disables)")
	payloadInput := flags.String("payload-input", "", "declared input that receives the raw request body")
	rate := flags.Int("rate", 0, "per-registration rate cap per minute (0 uses the [webhook] default)")

	path, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if path == "" {
		a.printErrf("usage: cairnos webhook add <workflow.toml> [--hmac-secret N] [--payload-input I] [--rate N] [--dir D]\n")

		return ExitUsage
	}

	flow, err := workflow.Load(path)
	if err != nil {
		a.printWorkflowErrors(err)

		return ExitUsage
	}

	// Store the absolute path so serve loads the file fresh per hit no matter
	// which directory it started from (FireRunner precedent).
	absPath, err := filepath.Abs(path)
	if err != nil {
		a.printErrf("webhook add: resolve path: %v\n", err)

		return ExitFailure
	}

	prodToken, prodHash, err := webhook.MintToken()
	if err != nil {
		a.printErrf("webhook add: %v\n", err)

		return ExitFailure
	}

	testToken, testHash, err := webhook.MintToken()
	if err != nil {
		a.printErrf("webhook add: %v\n", err)

		return ExitFailure
	}

	scheds, _, code, proceed := openAt(ctx, a, "webhook add", *dirFlag, scheduleBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(scheds)

	hooks, _, code, proceed := openAt(ctx, a, "webhook add", *dirFlag, webhookBackends(scheds))
	if !proceed {
		return code
	}
	defer a.runCloser("webhook store", ownedCloser(hooks))

	reg := &webhook.Registration{
		WorkflowName:  flow.Name,
		WorkflowPath:  absPath,
		ProdTokenHash: prodHash,
		TestTokenHash: testHash,
		HMACSecretRef: *hmacSecret,
		PayloadInput:  *payloadInput,
		RatePerMinute: *rate,
	}

	if err := hooks.Add(ctx, reg); err != nil {
		a.printErrf("webhook add: %v\n", err)

		return ExitFailure
	}

	a.printOutf("production: POST %s%s\n", hookPathPrefix, prodToken)
	a.printOutf("test:       POST %s%s\n", hookTestPathPrefix, testToken)
	a.printOutf("%s", webhookTokenNotice)

	return ExitOK
}

// cmdWebhookList shows every registration's ID, workflow, HMAC on/off, rate,
// and creation time. It cannot print a token: the store holds only hashes.
func (a *App) cmdWebhookList(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("webhook list", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	scheds, _, code, proceed := openAt(ctx, a, "webhook list", *dirFlag, scheduleBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(scheds)

	hooks, _, code, proceed := openAt(ctx, a, "webhook list", *dirFlag, webhookBackends(scheds))
	if !proceed {
		return code
	}
	defer a.runCloser("webhook store", ownedCloser(hooks))

	list, err := hooks.List(ctx)
	if err != nil {
		a.printErrf("webhook list: %v\n", err)

		return ExitFailure
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "ID\tWORKFLOW\tHMAC\tRATE\tCREATED\n")

	for _, reg := range list {
		a.tabWritef(tab, "%s\t%s\t%s\t%d\t%s\n",
			reg.ID, reg.WorkflowName, hmacLabel(reg.HMACSecretRef), reg.RatePerMinute, formatTableTime(reg.CreatedAt))
	}

	a.flushTab(tab)

	return ExitOK
}

// cmdWebhookRotate mints a replacement token for the chosen endpoint(s) and
// prints it once; the old hash is overwritten, so the old token dies at that
// moment while the untouched endpoint's token survives.
func (a *App) cmdWebhookRotate(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("webhook rotate", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)
	prod := flags.Bool("prod", false, "rotate the production token")
	test := flags.Bool("test", false, "rotate the test token")
	both := flags.Bool("both", false, "rotate both tokens")

	name, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if name == "" {
		a.printErrf("usage: cairnos webhook rotate <workflow> (--prod | --test | --both) [--dir D]\n")

		return ExitUsage
	}

	rotateProd := *prod || *both
	rotateTest := *test || *both

	if !rotateProd && !rotateTest {
		a.printErrf("webhook rotate: choose --prod, --test, or --both\n")

		return ExitUsage
	}

	scheds, _, code, proceed := openAt(ctx, a, "webhook rotate", *dirFlag, scheduleBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(scheds)

	hooks, _, code, proceed := openAt(ctx, a, "webhook rotate", *dirFlag, webhookBackends(scheds))
	if !proceed {
		return code
	}
	defer a.runCloser("webhook store", ownedCloser(hooks))

	if rotateProd && !a.rotateEndpoint(ctx, hooks, name, webhook.EndpointProd, "production", hookPathPrefix) {
		return ExitFailure
	}

	if rotateTest && !a.rotateEndpoint(ctx, hooks, name, webhook.EndpointTest, "test", hookTestPathPrefix) {
		return ExitFailure
	}

	a.printOutf("%s", webhookTokenNotice)

	return ExitOK
}

// rotateEndpoint mints and stores a fresh token for one endpoint and prints the
// new path, reporting whether the rotation succeeded so the caller stops on the
// first failure (an unknown workflow is ErrNotFound).
func (a *App) rotateEndpoint(ctx context.Context, hooks webhook.Store, name string, endpoint webhook.Endpoint, label, prefix string) bool {
	token, hash, err := webhook.MintToken()
	if err != nil {
		a.printErrf("webhook rotate: %v\n", err)

		return false
	}

	if err := hooks.UpdateTokenHash(ctx, name, endpoint, hash); err != nil {
		a.printErrf("webhook rotate: %v\n", err)

		return false
	}

	a.printOutf("%s: POST %s%s\n", label, prefix, token)

	return true
}

// cmdWebhookRemove deletes a workflow's registration, so its tokens stop
// resolving. An unknown workflow is ErrNotFound.
func (a *App) cmdWebhookRemove(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("webhook remove", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	name, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if name == "" {
		a.printErrf("usage: cairnos webhook remove <workflow> [--dir D]\n")

		return ExitUsage
	}

	scheds, _, code, proceed := openAt(ctx, a, "webhook remove", *dirFlag, scheduleBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(scheds)

	hooks, _, code, proceed := openAt(ctx, a, "webhook remove", *dirFlag, webhookBackends(scheds))
	if !proceed {
		return code
	}
	defer a.runCloser("webhook store", ownedCloser(hooks))

	if err := hooks.Remove(ctx, name); err != nil {
		a.printErrf("webhook remove: %v\n", err)

		return ExitFailure
	}

	a.printOutf("removed %s\n", name)

	return ExitOK
}

// hmacLabel renders the HMAC column: on when a secret ref backs the
// registration, off otherwise. It never exposes the ref value.
func hmacLabel(secretRef string) string {
	if secretRef == "" {
		return hmacLabelOff
	}

	return hmacLabelOn
}
