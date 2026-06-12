package hive_test

import (
	"os"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/hive"
)

// TestNewPseudonymRotatesAndHidesHost proves two calls mint distinct ids (so a
// node rotates its pseudonym) and that the id carries nothing identifying: not
// the hostname, and a recognizable node- prefix rather than an instance name.
func TestNewPseudonymRotatesAndHidesHost(t *testing.T) {
	t.Parallel()

	first, err := hive.NewPseudonym()
	if err != nil {
		t.Fatalf("first pseudonym: %v", err)
	}

	second, err := hive.NewPseudonym()
	if err != nil {
		t.Fatalf("second pseudonym: %v", err)
	}

	if first == second {
		t.Errorf("two pseudonyms are identical (%q); a pseudonym must rotate", first)
	}

	if !strings.HasPrefix(first, "node-") {
		t.Errorf("pseudonym %q is not a node- id", first)
	}

	host, hostErr := os.Hostname()
	if hostErr == nil && host != "" && strings.Contains(first, host) {
		t.Errorf("pseudonym %q contains the hostname %q; it must not", first, host)
	}
}

// TestValidCategory pins the fixed shareable set: the defined categories are
// valid and an arbitrary string is not.
func TestValidCategory(t *testing.T) {
	t.Parallel()

	valid := []hive.Category{
		hive.CategoryJobSearch,
		hive.CategoryCodeGen,
		hive.CategoryResearch,
		hive.CategoryAutomation,
		hive.CategoryGeneral,
	}
	for _, category := range valid {
		if !hive.ValidCategory(category) {
			t.Errorf("category %q reported invalid, want valid", category)
		}
	}

	if hive.ValidCategory("smuggled-content") {
		t.Error("an arbitrary string passed ValidCategory; the set must be fixed")
	}
}
