package cli_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
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

// noKeyEnv is an environment carrying no API key at all, which is the state a
// local model server runs under. It is staged rather than unset on the process,
// so a developer's own key can never answer for one of these cases.
func noKeyEnv() map[string]string {
	return nil
}

// TestRunExecutesLocalModelRunnerWithoutAnAPIKey proves the whole local-model
// path: the [runner.local] block registers under its own name, the openai
// runner reaches the loopback server through base_url, and no hosted API key
// is needed for a server that wants none.
func TestRunExecutesLocalModelRunnerWithoutAnAPIKey(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendLocalModelBlock(t, dir, localModelServer(t))
	path := writeWorkflow(t, dir, localModelWorkflow)

	code, stdout, stderr := runEnvCLI(t, noKeyEnv(), runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "succeeded") {
		t.Fatalf("summary missing success:\n%s", stdout)
	}

	if got := onlyActionOutput(t, dir); got != "hello back" {
		t.Fatalf("stored output = %#v, want the local model's answer", got)
	}
}

// TestLocalModelBlockSendsTheConfiguredModel proves the block's model string
// reaches the server, which is how one llama-server names what it loaded.
func TestLocalModelBlockSendsTheConfiguredModel(t *testing.T) {
	t.Parallel()

	var seen string

	// Real socket on purpose: the workflow run builds its runner clients from
	// config, so base_url has to be one something can dial.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the model request: %v", err)
		}

		seen = string(body)

		if _, err := w.Write([]byte(localModelReply)); err != nil {
			t.Errorf("write the model reply: %v", err)
		}
	}))
	defer server.Close()

	dir := initDir(t)
	appendLocalModelBlock(t, dir, server.URL)
	path := writeWorkflow(t, dir, localModelWorkflow)

	if code, _, stderr := runEnvCLI(t, noKeyEnv(), runCmd, path, flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(seen, `"model":"gemma-4-e4b"`) {
		t.Errorf("request did not carry the configured model:\n%s", seen)
	}
}

// localModelServer stands in for llama-server on loopback. Real socket on
// purpose, for the same reason the case above opens one.
func localModelServer(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(localModelReply)); err != nil {
			t.Errorf("write the model reply: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	return server.URL
}

// appendLocalModelBlock adds the [runner.local] block `cairnos model setup`
// writes, with base_url pointed at a test server instead of the sidecar.
func appendLocalModelBlock(t *testing.T, dir, baseURL string) {
	t.Helper()

	block := "\n[runner.local]\n" +
		"type     = \"openai\"\n" +
		"model    = \"gemma-4-e4b\"\n" +
		"base_url = " + strconv.Quote(baseURL) + "\n"

	appendConfig(t, dir, block)
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
	t.Parallel()

	dir := initDir(t)
	appendOpenAIBlock(t, dir, localModelServer(t))
	path := writeWorkflow(t, dir, openaiBuiltinWorkflow)

	code, stdout, stderr := runEnvCLI(t, noKeyEnv(), runCmd, path, flagDirArg, dir)
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

	block := "\n[runner.openai]\n" +
		"type     = \"openai\"\n" +
		"model    = \"gemma-4-e4b\"\n" +
		"base_url = " + strconv.Quote(baseURL) + "\n"

	appendConfig(t, dir, block)
}
