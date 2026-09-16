package builtins_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// The block name a second vendor's endpoint would be configured under, and the
// two keys involved.
const (
	namedBlock    = "zai"
	namedKeyEnv   = "CAIRNOS_RUNNER_ZAI_API_KEY"
	namedKeyValue = "key-for-the-named-block"
	typeKeyValue  = "key-for-the-type"
)

// askTask is the one task every case validates: resolved, with the prompt the
// runners require, so the only thing left to fail on is the API key.
func askTask() *runner.Task {
	return &runner.Task{
		RunID:      "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		ActionName: "ask",
		Params:     map[string]any{"prompt": "hello"},
		Resolved:   true,
	}
}

// remoteConfig points a runner at an endpoint somebody else operates, which is
// what makes the key required at all.
func remoteConfig() map[string]any {
	return map[string]any{"model": "a-model", "base_url": "https://api.z.ai/v1"}
}

// validateNamed builds one named runner of each type and validates a task,
// returning the runner's own error so a case can read the message it carries.
func validateNamed(t *testing.T, kind string) *runner.Error {
	t.Helper()

	task := askTask()
	client := &http.Client{Timeout: time.Second}

	if kind == builtins.AnthropicName {
		llm, err := builtins.NewAnthropicFor(namedBlock, remoteConfig(), client)
		if err != nil {
			t.Fatalf("new anthropic: %v", err)
		}

		return classified(t, llm.Validate(task))
	}

	llm, err := builtins.NewOpenAIFor(namedBlock, remoteConfig(), client)
	if err != nil {
		t.Fatalf("new openai: %v", err)
	}

	return classified(t, llm.Validate(task))
}

// classified narrows a validate failure to the runner's own error, so a case
// reads the message field rather than the rendered string.
func classified(t *testing.T, err error) *runner.Error {
	t.Helper()

	if err == nil {
		return nil
	}

	structured, isStructured := errors.AsType[*runner.Error](err)
	if !isStructured {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	return structured
}

// TestNamedRunnerReadsItsOwnKey covers the first branch of the resolution
// order: a block with a name of its own carries its own credential, so two
// blocks of one type can point at two vendors.
//
// t.Setenv keeps these out of parallel on its own.
func TestNamedRunnerReadsItsOwnKey(t *testing.T) {
	for _, kind := range []string{builtins.OpenAIName, builtins.AnthropicName} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv(namedKeyEnv, namedKeyValue)

			if err := validateNamed(t, kind); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

// TestNamedRunnerFallsBackToTheTypeKey covers the second branch: a host that
// only ever set the per-type variable keeps working after a block gains a name.
//
// t.Setenv keeps these out of parallel on its own.
func TestNamedRunnerFallsBackToTheTypeKey(t *testing.T) {
	cases := []struct {
		kind   string
		keyEnv string
	}{
		{kind: builtins.OpenAIName, keyEnv: builtins.OpenAIKeyEnv},
		{kind: builtins.AnthropicName, keyEnv: builtins.AnthropicKeyEnv},
	}

	for _, testCase := range cases {
		t.Run(testCase.kind, func(t *testing.T) {
			t.Setenv(namedKeyEnv, "")
			t.Setenv(testCase.keyEnv, typeKeyValue)

			if err := validateNamed(t, testCase.kind); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

// TestNamedRunnerNamesBothKeysWhenNeitherIsSet is what an operator reads when
// the run stops. Naming only one of the two would send them to set a variable
// they may not have meant to use.
//
// t.Setenv keeps these out of parallel on its own.
func TestNamedRunnerNamesBothKeysWhenNeitherIsSet(t *testing.T) {
	cases := []struct {
		kind   string
		keyEnv string
	}{
		{kind: builtins.OpenAIName, keyEnv: builtins.OpenAIKeyEnv},
		{kind: builtins.AnthropicName, keyEnv: builtins.AnthropicKeyEnv},
	}

	for _, testCase := range cases {
		t.Run(testCase.kind, func(t *testing.T) {
			t.Setenv(namedKeyEnv, "")
			t.Setenv(testCase.keyEnv, "")

			err := validateNamed(t, testCase.kind)
			if err == nil {
				t.Fatal("validate accepted a remote endpoint with no key set")
			}

			for _, want := range []string{namedKeyEnv, testCase.keyEnv} {
				if !strings.Contains(err.Message, want) {
					t.Fatalf("the error does not name %s: %v", want, err)
				}
			}
		})
	}
}

// TestLocalNamedRunnerNeedsNoKey keeps the local-model path working: a server
// on this side of the network wants no credential, and the name a block carries
// does not change that.
//
// t.Setenv keeps these out of parallel on its own.
func TestLocalNamedRunnerNeedsNoKey(t *testing.T) {
	t.Setenv(namedKeyEnv, "")
	t.Setenv(builtins.OpenAIKeyEnv, "")

	llm, err := builtins.NewOpenAIFor("local", map[string]any{
		"model":    "a-model",
		"base_url": "http://127.0.0.1:8089/v1",
	}, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatalf("new openai: %v", err)
	}

	if validateErr := llm.Validate(askTask()); validateErr != nil {
		t.Fatalf("validate: %v", validateErr)
	}
}

// TestTypeNamedRunnerNamesOneKey covers the most common setup: a plain
// [runner.openai] block with no key. Both rules produce one variable, so the
// message says "the" and not "one of the", which would read as a list with one
// thing in it.
//
// t.Setenv keeps this out of parallel on its own.
func TestTypeNamedRunnerNamesOneKey(t *testing.T) {
	cases := []struct {
		kind   string
		keyEnv string
	}{
		{kind: builtins.OpenAIName, keyEnv: builtins.OpenAIKeyEnv},
		{kind: builtins.AnthropicName, keyEnv: builtins.AnthropicKeyEnv},
	}

	for _, testCase := range cases {
		t.Run(testCase.kind, func(t *testing.T) {
			t.Setenv(testCase.keyEnv, "")

			err := classified(t, validateTypeNamed(t, testCase.kind))
			if err == nil {
				t.Fatal("validate accepted a remote endpoint with no key set")
			}

			want := "set the " + testCase.keyEnv + " environment variable"
			if err.Message != want {
				t.Fatalf("message = %q, want %q", err.Message, want)
			}
		})
	}
}

// validateTypeNamed builds the runner the way a block named after its type is
// built, which is what collapses the key list to one name.
func validateTypeNamed(t *testing.T, kind string) error {
	t.Helper()

	task := askTask()
	client := &http.Client{Timeout: time.Second}

	if kind == builtins.AnthropicName {
		llm, err := builtins.NewAnthropic(remoteConfig(), client)
		if err != nil {
			t.Fatalf("new anthropic: %v", err)
		}

		//nolint:wrapcheck // the case reads the runner's own error
		return llm.Validate(task)
	}

	llm, err := builtins.NewOpenAI(remoteConfig(), client)
	if err != nil {
		t.Fatalf("new openai: %v", err)
	}

	//nolint:wrapcheck // the case reads the runner's own error
	return llm.Validate(task)
}
