package builtins_test

import (
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// JSON Schema words the Describe case reads back; goconst covers tests here.
const (
	jsonSchemaType   = "type"
	jsonSchemaString = "string"
)

// schemaProperty reads one property's schema out of a tool spec, failing when
// the spec does not declare it.
func schemaProperty(t *testing.T, spec runner.ToolSpec, name string) map[string]any {
	t.Helper()

	properties, isMap := spec.InputSchema["properties"].(map[string]any)
	if !isMap {
		t.Fatalf("input schema = %#v, want a properties map", spec.InputSchema)
	}

	property, isMap := properties[name].(map[string]any)
	if !isMap {
		t.Fatalf("properties = %#v, want %s declared", properties, name)
	}

	return property
}

func TestUsageDescribesTwoOptionalParams(t *testing.T) {
	t.Parallel()

	spec := newUsage(t, nil, builtins.UsageOptions{}).Describe()

	if spec.Name != builtins.UsageName {
		t.Fatalf("name = %q, want %q", spec.Name, builtins.UsageName)
	}

	if spec.InputSchema[jsonSchemaType] != "object" {
		t.Fatalf("schema type = %v, want object", spec.InputSchema[jsonSchemaType])
	}

	if host := schemaProperty(t, spec, usageHostKey); host[jsonSchemaType] != jsonSchemaString {
		t.Fatalf("host schema = %#v, want a string", host)
	}

	launchers := schemaProperty(t, spec, usageLaunchers)
	items, _ := launchers["items"].(map[string]any)

	if launchers[jsonSchemaType] != "array" || items[jsonSchemaType] != jsonSchemaString {
		t.Fatalf("launchers schema = %#v, want an array of strings", launchers)
	}

	// An empty tool call has to be valid, since "usage" with no arguments is the
	// ordinary chat request.
	if required, present := spec.InputSchema["required"]; present {
		t.Fatalf("required = %v, want both params optional", required)
	}
}

func TestUsageToolCallWithNoArgumentsProbesEveryLauncher(t *testing.T) {
	dir := pairedShims(t, claudeScript, codexScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{codexLauncher},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	task := usageTask(map[string]any{})

	if err := usage.Validate(task); err != nil {
		t.Fatalf("validate empty tool call: %v", err)
	}

	result, err := usage.Execute(t.Context(), task)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := report(t, result)
	for _, launcher := range []string{claudeLauncher, codexLauncher} {
		if len(findLogin(t, got, launcher).Windows) == 0 {
			t.Fatalf("report %v, want %s to report windows", got, launcher)
		}
	}

	text, ok := builtins.UsageToolText(result.Output)
	if !ok || text != got.Summary {
		t.Fatalf("tool text = %q (ok %v), want the summary alone:\n%s", text, ok, got.Summary)
	}
}

func TestUsageToolTextNamesTheFailedLoginsUnderTheTable(t *testing.T) {
	dir := pairedShims(t, claudeRefusalScript, codexScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{codexLauncher},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	result, err := usage.Execute(t.Context(), usageTask(map[string]any{}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	text, ok := builtins.UsageToolText(result.Output)
	if !ok {
		t.Fatalf("tool text rejected the runner's own output %#v", result.Output)
	}

	want := report(t, result).Summary + "\nfailed logins: " + claudeLauncher
	if text != want {
		t.Fatalf("tool text =\n%s\nwant\n%s", text, want)
	}
}

func TestUsageToolTextRejectsOtherOutputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		output any
		name   string
	}{
		{name: "plain string", output: "no table here"},
		{name: "map without a summary", output: map[string]any{"logins": []any{}}},
		{name: "nil", output: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if text, ok := builtins.UsageToolText(tc.output); ok || strings.TrimSpace(text) != "" {
				t.Fatalf("tool text = %q (ok %v), want a refusal", text, ok)
			}
		})
	}
}
