package runner_test

import (
	"errors"
	"testing"

	"github.com/chadit/CairnOS/pkg/runner"
)

// registeredRunnerName is the one name the registry under test holds.
const registeredRunnerName = "wrapper"

// TestRegisteredAnswersBeforeBuild covers the startup check wiring runs before
// BuildAll: a registered name answers true while Get still finds no instance,
// and a name nothing registered answers false.
func TestRegisteredAnswersBeforeBuild(t *testing.T) {
	t.Parallel()

	registry := runner.NewRegistry()

	// The factory never runs, because nothing here builds the registry.
	unbuilt := func(map[string]any) (runner.Runner, error) { return nil, errors.New("never built") }

	if err := registry.Register(registeredRunnerName, unbuilt); err != nil {
		t.Fatalf("register %q: %v", registeredRunnerName, err)
	}

	cases := []struct {
		name       string
		runnerName string
		want       bool
	}{
		{name: "registered and not yet built", runnerName: registeredRunnerName, want: true},
		{name: "never registered", runnerName: "absent", want: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := registry.Registered(testCase.runnerName); got != testCase.want {
				t.Fatalf("Registered(%q) = %v, want %v", testCase.runnerName, got, testCase.want)
			}

			// Get finds nothing for an unregistered name either, so only a
			// registered row shows Registered answering ahead of the build.
			if !testCase.want {
				return
			}

			if _, built := registry.Get(testCase.runnerName); built {
				t.Fatalf("Get(%q) found an instance before BuildAll", testCase.runnerName)
			}
		})
	}
}

// TestZeroRegistryIsEmpty covers a Registry declared without NewRegistry: Get
// must report not found rather than panic, and Close has nothing to close.
func TestZeroRegistryIsEmpty(t *testing.T) {
	t.Parallel()

	var registry runner.Registry

	if instance, ok := registry.Get(registeredRunnerName); ok || instance != nil {
		t.Fatalf("Get on a zero Registry = %v, %v; want nil, false", instance, ok)
	}

	if registry.Registered(registeredRunnerName) {
		t.Fatal("Registered on a zero Registry answered true")
	}

	if err := registry.Close(); err != nil {
		t.Fatalf("Close on a zero Registry: %v", err)
	}
}
