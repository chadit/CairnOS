package secrets_test

import (
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/secrets"
)

// apiTokenName is the second secret every shadow case lists, so a case can show
// the ones it did not set staying out of the answer.
const apiTokenName = "api_token"

func TestEnvVarNameCanonicalizes(t *testing.T) {
	t.Parallel()

	env := secrets.NewEnv()

	for _, name := range []string{"db_password", "db-password", "DB.PASSWORD"} {
		if got := env.EnvVarName(name); got != "CAIRNOS_SECRET_DB_PASSWORD" {
			t.Fatalf("EnvVarName(%q) = %q, want CAIRNOS_SECRET_DB_PASSWORD", name, got)
		}
	}
}

func TestShadowedByEnvReportsOnlySetVars(t *testing.T) {
	t.Parallel()

	lookup := environ.Map(map[string]string{secrets.EnvPrefix + "DB_PASSWORD": staleEnvValue})

	got := secrets.ShadowedByEnv([]string{testName, apiTokenName}, lookup)

	if !slices.Equal(got, []string{testName}) {
		t.Fatalf("ShadowedByEnv = %v, want [%s]", got, testName)
	}
}

// TestShadowedByEnvReportsAVariableSetToNothing is the case an operator reads
// as unset. Env still answers for the name, so Chain serves the empty string
// and never reaches the store: that collision has to be reported like any other.
func TestShadowedByEnvReportsAVariableSetToNothing(t *testing.T) {
	t.Parallel()

	lookup := environ.Map(map[string]string{secrets.EnvPrefix + "DB_PASSWORD": ""})

	got := secrets.ShadowedByEnv([]string{testName}, lookup)

	if !slices.Equal(got, []string{testName}) {
		t.Fatalf("ShadowedByEnv = %v, want [%s]", got, testName)
	}
}

// TestShadowedByEnvKeepsTheCallersOrderAndEntries pins what the caller prints:
// the answer is the argument list filtered, in the order it arrived, with one
// entry per entry. Both callers join it into a warning, so a reordered or
// deduplicated answer would not match the list the operator configured.
func TestShadowedByEnvKeepsTheCallersOrderAndEntries(t *testing.T) {
	t.Parallel()

	lookup := environ.Map(map[string]string{
		secrets.EnvPrefix + "DB_PASSWORD": staleEnvValue,
		secrets.EnvPrefix + "API_TOKEN":   staleEnvValue,
	})

	got := secrets.ShadowedByEnv([]string{apiTokenName, testName, apiTokenName}, lookup)

	want := []string{apiTokenName, testName, apiTokenName}
	if !slices.Equal(got, want) {
		t.Fatalf("ShadowedByEnv = %v, want %v", got, want)
	}
}

func TestShadowedByEnvEmptyWhenNothingSet(t *testing.T) {
	t.Parallel()

	if got := secrets.ShadowedByEnv([]string{testName, apiTokenName}, environ.Map(nil)); len(got) != 0 {
		t.Fatalf("ShadowedByEnv with nothing set = %v, want empty", got)
	}
}
