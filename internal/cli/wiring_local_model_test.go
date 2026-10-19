package cli_test

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/nettest"
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
// runner sends to the loopback base_url, and no hosted API key is needed for a
// server that wants none.
func TestRunExecutesLocalModelRunnerWithoutAnAPIKey(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendLocalModelBlock(t, dir)
	path := writeWorkflow(t, dir, localModelWorkflow)

	code, stdout, stderr := runLocalModelCLI(t, localModelHandler(t), runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
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

	// The handler runs on the engine's goroutine, so the body it saw crosses
	// back to this one under a lock.
	var (
		seenMu sync.Mutex
		seen   string
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the model request: %v", err)
		}

		seenMu.Lock()
		seen = string(body)
		seenMu.Unlock()

		if _, err := w.Write([]byte(localModelReply)); err != nil {
			t.Errorf("write the model reply: %v", err)
		}
	})

	dir := initDir(t)
	appendLocalModelBlock(t, dir)
	path := writeWorkflow(t, dir, localModelWorkflow)

	if code, stdout, stderr := runLocalModelCLI(t, handler, runCmd, path, flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("run exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	seenMu.Lock()
	defer seenMu.Unlock()

	if !strings.Contains(seen, `"model":"gemma-4-e4b"`) {
		t.Errorf("request did not carry the configured model:\n%s", seen)
	}
}

// localModelURL is the base_url the cases configure. It has to be a loopback
// address because that is what waives the API key, but nothing dials it: the
// runner transport answers in the process, since a loopback socket can hit
// stale connection tracking and the run does not retry a failed model call.
const localModelURL = "http://127.0.0.1:8080"

// localModelHandler stands in for llama-server, answering every request with
// the canned reply.
func localModelHandler(t *testing.T) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(localModelReply)); err != nil {
			t.Errorf("write the model reply: %v", err)
		}
	})
}

// runLocalModelCLI drives one invocation with no API key staged and the model
// runners answered by handler, so a developer's own key or a local server can
// never answer for one of these cases.
func runLocalModelCLI(t *testing.T, handler http.Handler, args ...string) (int, string, string) {
	t.Helper()

	return runConfiguredCLI(t, func(app *cli.App) *cli.App {
		return app.WithEnv(environ.Map(noKeyEnv())).WithRunnerTransport(nettest.Transport(handler))
	}, args...)
}

// appendLocalModelBlock adds the [runner.local] block `cairnos model setup`
// writes, with base_url pointed at the in-process stand-in instead of the
// sidecar.
func appendLocalModelBlock(t *testing.T, dir string) {
	t.Helper()

	block := "\n[runner.local]\n" +
		"type     = \"openai\"\n" +
		"model    = \"gemma-4-e4b\"\n" +
		"base_url = " + strconv.Quote(localModelURL) + "\n"

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
	appendOpenAIBlock(t, dir)
	path := writeWorkflow(t, dir, openaiBuiltinWorkflow)

	code, stdout, stderr := runLocalModelCLI(t, localModelHandler(t), runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
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
func appendOpenAIBlock(t *testing.T, dir string) {
	t.Helper()

	block := "\n[runner.openai]\n" +
		"type     = \"openai\"\n" +
		"model    = \"gemma-4-e4b\"\n" +
		"base_url = " + strconv.Quote(localModelURL) + "\n"

	appendConfig(t, dir, block)
}
