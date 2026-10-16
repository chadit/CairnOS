package cli_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/webhook"
)

// Webhook fixture literals, named per the repo's goconst-in-tests policy.
const (
	webhookCmd       = "webhook"
	webhookArgAdd    = "add"
	webhookArgList   = "list"
	webhookArgRotate = "rotate"
	webhookArgRemove = "remove"
	webhookName      = "hooked"

	flagProd = "--prod"
	flagTest = "--test"
	flagBoth = "--both"

	labelProduction = "production:"
	labelTest       = "test:"
)

const webhookWorkflow = `
[workflow]
name = "hooked"

[action.tick]
runner = "mock"
params = { output = "tick" }
`

// openWebhookStore opens the scaffolded home's registration store directly, so a
// caller can prove what a token hash resolves to after a CLI mutation.
func openWebhookStore(t *testing.T, dir string) *webhook.SQLiteWebhooks {
	t.Helper()

	store, err := webhook.OpenSQLiteWebhooks(filepath.Join(dir, dataDirName, dbFileName))
	if err != nil {
		t.Fatalf("open webhook store: %v", err)
	}

	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close webhook store: %v", err)
		}
	})

	return store
}

// webhookToken pulls the token off the add/rotate line whose label matches,
// reading the final path segment. The production line prints /v1/hooks/<token>
// and its sibling adds a /test/ segment, so the last path segment is the token
// either way.
func webhookToken(t *testing.T, out, label string) string {
	t.Helper()

	for line := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(line, label) {
			continue
		}

		fields := strings.Fields(line)
		path := fields[len(fields)-1]

		return path[strings.LastIndex(path, "/")+1:]
	}

	t.Fatalf("no %q line in output:\n%s", label, out)

	return ""
}

// addWebhook runs webhook add for path and returns the printed prod and test
// tokens, asserting each was printed exactly once and the save-now notice fired.
func addWebhook(t *testing.T, dir, path string, extra ...string) (string, string) {
	t.Helper()

	args := append([]string{webhookCmd, webhookArgAdd, path, flagDirArg, dir}, extra...)

	code, stdout, stderr := runCLI(t, args...)
	if code != cli.ExitOK {
		t.Fatalf("webhook add exited %d: %s", code, stderr)
	}

	prod := webhookToken(t, stdout, labelProduction)
	test := webhookToken(t, stdout, labelTest)

	if prod == test {
		t.Fatalf("prod and test tokens are identical: %s", prod)
	}

	if strings.Count(stdout, prod) != 1 || strings.Count(stdout, test) != 1 {
		t.Fatalf("each token must print exactly once:\n%s", stdout)
	}

	if !strings.Contains(stdout, "not retrievable") {
		t.Fatalf("add did not warn the tokens are unrecoverable:\n%s", stdout)
	}

	return prod, test
}

// resolves reports which endpoint a token maps to, or false when the hash no
// longer matches any registration (a rotated-away or removed token).
func resolves(t *testing.T, store webhook.Store, token string) (webhook.Endpoint, bool) {
	t.Helper()

	_, endpoint, err := store.GetByTokenHash(t.Context(), webhook.HashToken(token))
	if err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			return 0, false
		}

		t.Fatalf("lookup token hash: %v", err)
	}

	return endpoint, true
}

func TestWebhookAddStoresHashesAndPrintsTokens(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, webhookWorkflow)

	prod, test := addWebhook(t, dir, path)

	store := openWebhookStore(t, dir)

	if endpoint, ok := resolves(t, store, prod); !ok || endpoint != webhook.EndpointProd {
		t.Fatalf("prod token did not resolve to the production endpoint (ok=%v, endpoint=%d)", ok, endpoint)
	}

	if endpoint, ok := resolves(t, store, test); !ok || endpoint != webhook.EndpointTest {
		t.Fatalf("test token did not resolve to the test endpoint (ok=%v, endpoint=%d)", ok, endpoint)
	}
}

func TestWebhookListShowsHashesNeverTokens(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, webhookWorkflow)

	prod, test := addWebhook(t, dir, path)

	code, stdout, stderr := runCLI(t, webhookCmd, webhookArgList, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("webhook list exited %d: %s", code, stderr)
	}

	for _, want := range []string{webhookName, "HMAC", "RATE"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("webhook list missing %q:\n%s", want, stdout)
		}
	}

	// The store holds only hashes, so list cannot print either token.
	if strings.Contains(stdout, prod) || strings.Contains(stdout, test) {
		t.Fatalf("webhook list leaked a token:\n%s", stdout)
	}
}

func TestWebhookListReportsHMACState(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, webhookWorkflow)

	addWebhook(t, dir, path, "--hmac-secret", "gh_hook_key")

	code, stdout, _ := runCLI(t, webhookCmd, webhookArgList, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("webhook list exited %d", code)
	}

	// The row's HMAC column reads on because a secret ref backs it; the ref value
	// itself never appears.
	if !strings.Contains(stdout, "on") {
		t.Fatalf("webhook list did not report HMAC on:\n%s", stdout)
	}

	if strings.Contains(stdout, "gh_hook_key") {
		t.Fatalf("webhook list leaked the secret ref:\n%s", stdout)
	}
}

func TestWebhookRotateKillsOnlyTheRotatedEndpoint(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, webhookWorkflow)

	oldProd, test := addWebhook(t, dir, path)

	code, stdout, stderr := runCLI(t, webhookCmd, webhookArgRotate, webhookName, flagProd, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("webhook rotate --prod exited %d: %s", code, stderr)
	}

	newProd := webhookToken(t, stdout, labelProduction)

	if newProd == oldProd {
		t.Fatal("rotate reprinted the old prod token")
	}

	store := openWebhookStore(t, dir)

	if _, ok := resolves(t, store, oldProd); ok {
		t.Fatal("rotated-away prod token still resolves")
	}

	if endpoint, ok := resolves(t, store, newProd); !ok || endpoint != webhook.EndpointProd {
		t.Fatalf("new prod token does not resolve to production (ok=%v)", ok)
	}

	// Rotating prod must leave the sibling test token valid.
	if endpoint, ok := resolves(t, store, test); !ok || endpoint != webhook.EndpointTest {
		t.Fatalf("test token stopped resolving after a prod-only rotate (ok=%v)", ok)
	}
}

func TestWebhookRotateBothReplacesEachToken(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, webhookWorkflow)

	oldProd, oldTest := addWebhook(t, dir, path)

	code, stdout, stderr := runCLI(t, webhookCmd, webhookArgRotate, webhookName, flagBoth, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("webhook rotate --both exited %d: %s", code, stderr)
	}

	newProd := webhookToken(t, stdout, labelProduction)
	newTest := webhookToken(t, stdout, labelTest)

	store := openWebhookStore(t, dir)

	if _, ok := resolves(t, store, oldProd); ok {
		t.Fatal("old prod token survived --both rotate")
	}

	if _, ok := resolves(t, store, oldTest); ok {
		t.Fatal("old test token survived --both rotate")
	}

	if _, ok := resolves(t, store, newProd); !ok {
		t.Fatal("new prod token does not resolve after --both rotate")
	}

	if _, ok := resolves(t, store, newTest); !ok {
		t.Fatal("new test token does not resolve after --both rotate")
	}
}

func TestWebhookRotateWithoutEndpointIsUsageError(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, webhookWorkflow)
	addWebhook(t, dir, path)

	code, _, stderr := runCLI(t, webhookCmd, webhookArgRotate, webhookName, flagDirArg, dir)
	if code != cli.ExitUsage {
		t.Fatalf("rotate without an endpoint flag exited %d, want %d", code, cli.ExitUsage)
	}

	if !strings.Contains(stderr, "--prod") {
		t.Fatalf("rotate usage does not name the endpoint flags:\n%s", stderr)
	}
}

func TestWebhookAddRejectsDuplicateWorkflow(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, webhookWorkflow)
	addWebhook(t, dir, path)

	code, _, stderr := runCLI(t, webhookCmd, webhookArgAdd, path, flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("second add exited %d, want %d", code, cli.ExitFailure)
	}

	if !strings.Contains(stderr, "already exists") {
		t.Fatalf("duplicate add does not explain the conflict:\n%s", stderr)
	}
}

func TestWebhookRemoveDropsRegistration(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, webhookWorkflow)
	prod, _ := addWebhook(t, dir, path)

	code, stdout, stderr := runCLI(t, webhookCmd, webhookArgRemove, webhookName, flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, webhookName) {
		t.Fatalf("webhook remove (exit %d) output:\n%s\nstderr:%s", code, stdout, stderr)
	}

	store := openWebhookStore(t, dir)

	if _, ok := resolves(t, store, prod); ok {
		t.Fatal("token still resolves after remove")
	}

	code, stdout, _ = runCLI(t, webhookCmd, webhookArgList, flagDirArg, dir)
	if code != cli.ExitOK || strings.Contains(stdout, webhookName) {
		t.Fatalf("removed registration still listed (exit %d):\n%s", code, stdout)
	}
}

func TestWebhookRemoveUnknownFails(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	code, _, stderr := runCLI(t, webhookCmd, webhookArgRemove, "no-such-workflow", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("remove unknown exited %d, want %d", code, cli.ExitFailure)
	}

	if !strings.Contains(stderr, "not found") {
		t.Fatalf("remove miss does not explain the failure:\n%s", stderr)
	}
}
