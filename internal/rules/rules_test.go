package rules_test

import (
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/rules/rulestest"
)

// openRules opens a fresh database under t.TempDir and closes it when the case
// finishes; every case exercises schema creation.
func openRules(t *testing.T) *rules.SQLite {
	t.Helper()

	svc, err := rules.OpenSQLite(filepath.Join(t.TempDir(), "rules.db"))
	if err != nil {
		t.Fatalf("open rules store: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := svc.Close(); closeErr != nil {
			t.Errorf("close rules store: %v", closeErr)
		}
	})

	return svc
}

// TestRulesConformance runs the one behavioral spec shared with the postgres
// backend (internal/rules/rulestest). Proving sqlite passes the same suite the
// postgres backend must pass is the whole point: one spec, two backends.
func TestRulesConformance(t *testing.T) {
	t.Parallel()
	rulestest.RunConformance(t, openRules)
}
