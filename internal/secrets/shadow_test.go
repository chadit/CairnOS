package secrets_test

import (
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/secrets"
)

func TestEnvVarNameCanonicalizes(t *testing.T) {
	t.Parallel()

	env := secrets.NewEnv()

	// Dashes and dots fold to underscores and the name uppercases, so
	// secrets.db-password and secrets.db_password name one variable.
	for _, name := range []string{"db-password", "db.password", "DB_PASSWORD"} {
		if got := env.EnvVarName(name); got != "CAIRNOS_SECRET_DB_PASSWORD" {
			t.Fatalf("EnvVarName(%q) = %q, want CAIRNOS_SECRET_DB_PASSWORD", name, got)
		}
	}
}

func TestShadowedByEnvReportsOnlySetVars(t *testing.T) {
	t.Parallel()

	// A fake lookup, so the check needs no process environment. Only the
	// first name has a variable set, so only it is shadowed.
	set := map[string]struct{}{"CAIRNOS_SECRET_DB_PASSWORD": {}}
	lookup := func(name string) (string, bool) {
		_, ok := set[name]

		return "", ok
	}

	got := secrets.ShadowedByEnv([]string{testName, "api_token"}, lookup)

	if !slices.Equal(got, []string{testName}) {
		t.Fatalf("ShadowedByEnv = %v, want [%s]", got, testName)
	}
}

func TestShadowedByEnvEmptyWhenNothingSet(t *testing.T) {
	t.Parallel()

	lookup := func(string) (string, bool) { return "", false }

	if got := secrets.ShadowedByEnv([]string{testName, "api_token"}, lookup); len(got) != 0 {
		t.Fatalf("ShadowedByEnv with nothing set = %v, want empty", got)
	}
}
