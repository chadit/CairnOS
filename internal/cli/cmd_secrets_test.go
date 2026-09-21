package cli_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/secrets"
)

// Literals the secrets tests reuse, named so goconst stays quiet and a typo
// fails in one place. The two values are test-only sentinels, never real
// credentials.
const (
	secretsCmd      = "secrets"
	setSub          = "set"
	storeSecretName = "db_password"
	storeSecretVal  = "store-only-sentinel"
	envSecretVal    = "env-sentinel"
	secretEnvVar    = "CAIRNOS_SECRET_DB_PASSWORD"
	backendEnvVar   = "CAIRNOS_SECRETS_BACKEND"
	masterKeyEnvVar = "CAIRNOS_SECRETS_KEY"
	prevKeyEnvVar   = "CAIRNOS_SECRETS_PREV_KEY"
	shadowMarker    = "shadowed_by_env"
)

// secretEchoWorkflow echoes a ${{ secrets.db_password }} reference through the
// mock runner, so a run proves which source resolved the value.
const secretEchoWorkflow = `
[workflow]
name = "secretecho"

[action.emit]
runner = "mock"
params = { output = "value=${{ secrets.db_password }}" }
`

// runEnvCLIStdin drives one invocation with a captured stdin and a staged
// environment, so `secrets set` reads its value from the reader and the backend
// from the case rather than from the process. It returns the exit code and
// stderr; no stdin-driven case asserts stdout.
func runEnvCLIStdin(t *testing.T, env map[string]string, stdin string, args ...string) (int, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := cli.New(&stdout, &stderr).
		WithStdin(strings.NewReader(stdin)).
		WithEnv(environ.Map(env)).
		Run(t.Context(), args)

	return code, stderr.String()
}

// storeEnv is the environment a store-backed case runs under: the backend the
// mutating subcommands require, and a fresh CSPRNG master key of its own. Each
// case gets its own key, which is what lets them all run at once.
func storeEnv(t *testing.T) map[string]string {
	t.Helper()

	return map[string]string{backendEnvVar: "store", masterKeyEnvVar: freshMasterKey(t)}
}

// freshMasterKey generates a CSPRNG master key, returned so a caller can stage
// it as the primary and later as the previous key of a rotation window.
func freshMasterKey(t *testing.T) string {
	t.Helper()

	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}

	return key
}

// setStoreSecret seals storeSecretVal under storeSecretName through the CLI,
// failing on a non-zero exit.
func setStoreSecret(t *testing.T, env map[string]string, dir string) {
	t.Helper()

	code, stderr := runEnvCLIStdin(t, env, storeSecretVal, secretsCmd, setSub, storeSecretName, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("secrets set exited %d: %s", code, stderr)
	}
}

func TestSecretsSetGetRoundTrip(t *testing.T) {
	t.Parallel()

	env := storeEnv(t)
	dir := initDir(t)
	setStoreSecret(t, env, dir)

	code, stdout, stderr := runEnvCLI(t, env, secretsCmd, "get", storeSecretName, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("secrets get exited %d: %s", code, stderr)
	}

	// get writes the raw value plus exactly one trailing newline.
	if stdout != storeSecretVal+"\n" {
		t.Fatalf("get = %q, want %q", stdout, storeSecretVal+"\n")
	}
}

func TestSecretsSetTrimsOneTrailingNewline(t *testing.T) {
	t.Parallel()

	env := storeEnv(t)
	dir := initDir(t)

	// The piped value carries a trailing newline; exactly one is trimmed, so a
	// get returns the value with only the single newline get itself adds.
	code, stderr := runEnvCLIStdin(t, env, storeSecretVal+"\n", secretsCmd, setSub, storeSecretName, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("secrets set exited %d: %s", code, stderr)
	}

	_, stdout, _ := runEnvCLI(t, env, secretsCmd, "get", storeSecretName, flagDirArg, dir)
	if stdout != storeSecretVal+"\n" {
		t.Fatalf("get after set-with-newline = %q, want %q", stdout, storeSecretVal+"\n")
	}
}

func TestSecretsRmDeletesRow(t *testing.T) {
	t.Parallel()

	env := storeEnv(t)
	dir := initDir(t)
	setStoreSecret(t, env, dir)

	code, _, stderr := runEnvCLI(t, env, secretsCmd, "rm", storeSecretName, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("secrets rm exited %d: %s", code, stderr)
	}

	code, _, _ = runEnvCLI(t, env, secretsCmd, "get", storeSecretName, flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("get after rm exited %d, want %d", code, cli.ExitFailure)
	}
}

func TestSecretsLsShowsMetadataNeverValues(t *testing.T) {
	t.Parallel()

	env := storeEnv(t)
	dir := initDir(t)
	setStoreSecret(t, env, dir)

	code, stdout, stderr := runEnvCLI(t, env, secretsCmd, "ls", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("secrets ls exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, storeSecretName) {
		t.Fatalf("ls does not list the secret name:\n%s", stdout)
	}

	// ls is metadata only, so the sealed value must never appear.
	if strings.Contains(stdout, storeSecretVal) {
		t.Fatalf("ls leaked the secret value:\n%s", stdout)
	}
}

func TestSecretsLsMarksEnvShadowedRow(t *testing.T) {
	t.Parallel()

	env := storeEnv(t)
	dir := initDir(t)
	setStoreSecret(t, env, dir)

	// An env var shadows the store row, so ls flags it (env wins the chain).
	env[secretEnvVar] = envSecretVal

	code, stdout, stderr := runEnvCLI(t, env, secretsCmd, "ls", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("secrets ls exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, shadowMarker) {
		t.Fatalf("ls does not mark the env-shadowed row:\n%s", stdout)
	}

	// The marker reports the collision without leaking either value.
	if strings.Contains(stdout, storeSecretVal) || strings.Contains(stdout, envSecretVal) {
		t.Fatalf("ls leaked a value:\n%s", stdout)
	}
}

func TestSecretsRotateReSealsUnderNewPrimary(t *testing.T) {
	t.Parallel()

	env := storeEnv(t)
	dir := initDir(t)
	oldKey := env[masterKeyEnvVar]
	setStoreSecret(t, env, dir)

	// Promote a new primary and keep the old key as the previous, the rotation
	// window: reads still open by key id, and rotate re-seals the one stale row.
	newKey, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("generate new master key: %v", err)
	}

	env[masterKeyEnvVar] = newKey
	env[prevKeyEnvVar] = oldKey

	code, stdout, stderr := runEnvCLI(t, env, secretsCmd, "rotate", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("secrets rotate exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "rotated 1") {
		t.Fatalf("rotate did not re-seal the stale row:\n%s", stdout)
	}

	// The value still opens under the new primary after rotation.
	_, getOut, _ := runEnvCLI(t, env, secretsCmd, "get", storeSecretName, flagDirArg, dir)
	if getOut != storeSecretVal+"\n" {
		t.Fatalf("get after rotate = %q, want %q", getOut, storeSecretVal+"\n")
	}
}

func TestSecretsRefuseWithoutStoreBackend(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	// The default env backend has no store, so every mutating subcommand exits
	// non-zero pointing at the config key rather than writing a store the engine
	// is not reading.
	code, stderr := runEnvCLIStdin(t, nil, storeSecretVal, secretsCmd, setSub, storeSecretName, flagDirArg, dir)
	if code == cli.ExitOK {
		t.Fatal("secrets set succeeded without the store backend configured")
	}

	if !strings.Contains(stderr, "backend") || !strings.Contains(stderr, "store") {
		t.Fatalf("backend-guard error does not point at the config key:\n%s", stderr)
	}
}

func TestSecretsKeygenPrintsFreshCsprngKey(t *testing.T) {
	t.Parallel()

	code, first, stderr := runCLI(t, secretsCmd, "keygen")
	if code != cli.ExitOK {
		t.Fatalf("secrets keygen exited %d: %s", code, stderr)
	}

	_, second, _ := runCLI(t, secretsCmd, "keygen")

	// A CSPRNG never repeats, so two calls differ.
	if strings.TrimSpace(first) == strings.TrimSpace(second) {
		t.Fatal("two keygen calls produced the same key")
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(first))
	if err != nil {
		t.Fatalf("keygen output is not base64: %v", err)
	}

	if len(raw) != 32 {
		t.Fatalf("keygen key decoded to %d bytes, want 32", len(raw))
	}
}

func TestSecretsNeverLogsValue(t *testing.T) {
	t.Parallel()

	env := storeEnv(t)
	dir := initDir(t)
	setStoreSecret(t, env, dir)

	// Across ls and rotate, the sealed value must never reach stderr (the log
	// stream). get is the one command that writes it, and only to stdout.
	for _, sub := range []string{"ls", "rotate"} {
		_, stdout, stderr := runEnvCLI(t, env, secretsCmd, sub, flagDirArg, dir)
		if strings.Contains(stderr, storeSecretVal) {
			t.Fatalf("secrets %s leaked the value to stderr:\n%s", sub, stderr)
		}

		if sub == "ls" && strings.Contains(stdout, storeSecretVal) {
			t.Fatalf("secrets ls leaked the value to stdout:\n%s", stdout)
		}
	}

	_, _, getErr := runEnvCLI(t, env, secretsCmd, "get", storeSecretName, flagDirArg, dir)
	if strings.Contains(getErr, storeSecretVal) {
		t.Fatalf("secrets get leaked the value to stderr:\n%s", getErr)
	}
}

func TestSecretsStoreEnvWinsAndWarnsOnShadow(t *testing.T) {
	t.Parallel()

	env := storeEnv(t)
	dir := initDir(t)

	// The store holds the rotated value; a stale env var shadows it.
	setStoreSecret(t, env, dir)
	env[secretEnvVar] = envSecretVal

	path := writeWorkflow(t, dir, secretEchoWorkflow)

	code, stdout, stderr := runEnvCLI(t, env, runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	// Env wins the chain, so the stale env value is served, not the store value.
	// This is the documented hazard: a rotation looks done while the old secret
	// is still resolved.
	if !strings.Contains(stdout, "value="+envSecretVal) {
		t.Fatalf("run did not serve the env value (env-first):\n%s", stdout)
	}

	if strings.Contains(stdout, storeSecretVal) {
		t.Fatalf("run served the shadowed store value:\n%s", stdout)
	}

	// The startup warning names the shadowed store secret (never a value).
	if !strings.Contains(stderr, "shadowed") || !strings.Contains(stderr, storeSecretName) {
		t.Fatalf("startup warning did not name the shadowed secret:\n%s", stderr)
	}

	if strings.Contains(stderr, storeSecretVal) || strings.Contains(stderr, envSecretVal) {
		t.Fatalf("startup warning leaked a value:\n%s", stderr)
	}
}

func TestSecretsStoreOnlyValueResolves(t *testing.T) {
	t.Parallel()

	env := storeEnv(t)
	dir := initDir(t)

	// No env var for this secret, so the store is the only source.
	setStoreSecret(t, env, dir)

	path := writeWorkflow(t, dir, secretEchoWorkflow)

	code, stdout, stderr := runEnvCLI(t, env, runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "value="+storeSecretVal) {
		t.Fatalf("run did not resolve the store-only secret:\n%s", stdout)
	}
}

func TestSecretsEnvBackendUnchanged(t *testing.T) {
	t.Parallel()

	// The default env backend resolves only from the environment, byte-for-byte
	// today's behavior, and opens no store (so no shadow warning fires).
	env := map[string]string{secretEnvVar: envSecretVal}

	dir := initDir(t)
	path := writeWorkflow(t, dir, secretEchoWorkflow)

	code, stdout, stderr := runEnvCLI(t, env, runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "value="+envSecretVal) {
		t.Fatalf("env backend did not resolve the env secret:\n%s", stdout)
	}

	if strings.Contains(stderr, "shadowed") {
		t.Fatalf("env backend emitted a shadow warning:\n%s", stderr)
	}
}
