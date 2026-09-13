package cli_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/store"
)

// localModelWorkflow names the runner only the [runner.local] block defines,
// so a pass proves the wiring registered a second chat-completions runner
// under that name.
const localModelWorkflow = `
[workflow]
name = "local_model"

[action.ask]
runner = "local"
params = { prompt = "hello" }
`

// localModelReply is what the stand-in llama-server answers. llama-server
// speaks the chat-completions shape, which is the whole reason the openai
// runner can talk to it unchanged.
const localModelReply = `{
	"model": "gemma-4-e4b",
	"choices": [{"message": {"role": "assistant", "content": "hello back"}, "finish_reason": "stop"}],
	"usage": {"prompt_tokens": 3, "completion_tokens": 2}
}`

// openaiKeyEnvName is the variable a hosted call would need and a local one
// must not.
const openaiKeyEnvName = "CAIRNOS_RUNNER_OPENAI_API_KEY" // #nosec G101 -- variable name, not a credential

// TestRunExecutesLocalModelRunnerWithoutAnAPIKey proves the whole local-model
// path: the [runner.local] block registers under its own name, the openai
// runner reaches the loopback server through base_url, and no hosted API key
// is needed for a server that wants none.
func TestRunExecutesLocalModelRunnerWithoutAnAPIKey(t *testing.T) {
	t.Setenv(openaiKeyEnvName, "")

	dir := initDir(t)
	appendLocalModelBlock(t, dir, localModelServer(t))
	path := writeWorkflow(t, dir, localModelWorkflow)

	code, stdout, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "succeeded") {
		t.Fatalf("summary missing success:\n%s", stdout)
	}

	db := openRunStore(t, dir)

	runs, err := db.ListRuns(t.Context(), store.ListFilter{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	results, err := db.ListActionResults(t.Context(), runs[0].ID)
	if err != nil {
		t.Fatalf("list action results: %v", err)
	}

	if len(results) != 1 || results[0].Output != "hello back" {
		t.Fatalf("results = %+v, want the local model's answer", results)
	}
}

// TestLocalModelBlockSendsTheConfiguredModel proves the block's model string
// reaches the server, which is how one llama-server names what it loaded.
func TestLocalModelBlockSendsTheConfiguredModel(t *testing.T) {
	t.Setenv(openaiKeyEnvName, "")

	var seen string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		seen = string(body)

		_, _ = w.Write([]byte(localModelReply))
	}))
	defer server.Close()

	dir := initDir(t)
	appendLocalModelBlock(t, dir, server.URL)
	path := writeWorkflow(t, dir, localModelWorkflow)

	if code, _, stderr := runCLI(t, runCmd, path, flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(seen, `"model":"gemma-4-e4b"`) {
		t.Errorf("request did not carry the configured model:\n%s", seen)
	}
}

// localModelServer stands in for llama-server on loopback.
func localModelServer(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(localModelReply))
	}))
	t.Cleanup(server.Close)

	return server.URL
}

// appendLocalModelBlock adds the [runner.local] block `cairnos model setup`
// writes, with base_url pointed at a test server instead of the sidecar.
func appendLocalModelBlock(t *testing.T, dir, baseURL string) {
	t.Helper()

	cfgPath := filepath.Join(dir, "config.toml")

	existing, err := os.ReadFile(cfgPath) // #nosec G304 -- test-owned scaffold path
	if err != nil {
		t.Fatalf("read scaffolded config: %v", err)
	}

	block := "\n[runner.local]\n" +
		"type     = \"openai\"\n" +
		"model    = \"gemma-4-e4b\"\n" +
		"base_url = " + strconv.Quote(baseURL) + "\n"

	if err := os.WriteFile(cfgPath, append(existing, []byte(block)...), 0o600); err != nil {
		t.Fatalf("write config with the local model block: %v", err)
	}
}

// openaiBuiltinWorkflow references the built-in runner name, so a pass proves a
// block literally named openai keeps configuring the built-in instead of
// tripping duplicate registration.
const openaiBuiltinWorkflow = `
[workflow]
name = "openai_builtin"

[action.ask]
runner = "openai"
params = { prompt = "hello" }
`

// TestRunOpenAINamedBlockConfiguresBuiltin proves the self-collision guard
// holds: a [runner.openai] block that also sets type = "openai" configures the
// runner already registered under that name rather than failing at wiring.
func TestRunOpenAINamedBlockConfiguresBuiltin(t *testing.T) {
	t.Setenv(openaiKeyEnvName, "")

	dir := initDir(t)
	appendOpenAIBlock(t, dir, localModelServer(t))
	path := writeWorkflow(t, dir, openaiBuiltinWorkflow)

	code, stdout, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if strings.Contains(stderr, "already registered") {
		t.Fatalf("the block named openai tripped duplicate registration:\n%s", stderr)
	}

	if !strings.Contains(stdout, "succeeded") {
		t.Fatalf("summary missing success:\n%s", stdout)
	}
}

// appendOpenAIBlock adds a [runner.openai] block that names its own type, the
// shape someone writes when pointing the built-in at a local server.
func appendOpenAIBlock(t *testing.T, dir, baseURL string) {
	t.Helper()

	cfgPath := filepath.Join(dir, "config.toml")

	existing, err := os.ReadFile(cfgPath) // #nosec G304 -- test-owned scaffold path
	if err != nil {
		t.Fatalf("read scaffolded config: %v", err)
	}

	block := "\n[runner.openai]\n" +
		"type     = \"openai\"\n" +
		"model    = \"gemma-4-e4b\"\n" +
		"base_url = " + strconv.Quote(baseURL) + "\n"

	if err := os.WriteFile(cfgPath, append(existing, []byte(block)...), 0o600); err != nil {
		t.Fatalf("write config with the openai block: %v", err)
	}
}
