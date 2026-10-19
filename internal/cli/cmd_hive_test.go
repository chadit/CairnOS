package cli_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/hive"
	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/tuning"
)

const (
	hiveCmd     = "hive"
	jobSearchID = "hive-sug-1"
)

// writeHiveConfig overwrites the home's config with a [hive] block pointed at
// endpoint, with publish either on or off.
func writeHiveConfig(t *testing.T, dir, endpoint string, publish bool) {
	t.Helper()

	cfg := "[hive]\nenabled = true\nendpoint = \"" + endpoint + "\"\ncategories = [\"job-search\"]\n"
	if publish {
		cfg += "publish_enabled = true\nmin_observations = 3\n"
	}

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write hive config: %v", err)
	}
}

// tipHandler stands in for a hive returning one job-search tip on GET.
func tipHandler() http.Handler {
	tip := hive.Tip{
		Kind:       store.SuggestionCaching,
		Category:   hive.CategoryJobSearch,
		RunnerType: hive.RunnerTypeOpenAI,
		CostBucket: "20%",
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusAccepted)

			return
		}

		w.Header().Set("Content-Type", "application/json")

		body := struct {
			Tips []hive.Tip `json:"tips"`
		}{Tips: []hive.Tip{tip}}

		if err := json.NewEncoder(w).Encode(body); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
}

// TestHiveStatusShowsGate proves status prints the publish gate state.
func TestHiveStatusShowsGate(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	writeHiveConfig(t, dir, "https://hive.example", false)

	code, stdout, stderr := runCLI(t, hiveCmd, "status", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("hive status exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "publish_enabled") || !strings.Contains(stdout, "false") {
		t.Errorf("status did not show the publish gate: %s", stdout)
	}
}

// TestHiveSubscribeLandsTipsInQueue proves a subscribe pulls tips and writes
// them to the suggestion queue as open suggestions for review.
func TestHiveSubscribeLandsTipsInQueue(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	writeHiveConfig(t, dir, nettest.BaseURL, false)

	// The hive answers in the process: a loopback hive can sit on a port with
	// stale connection tracking, and the dial then fails with no route to host.
	code, stdout, stderr := runConfiguredCLI(t, func(app *cli.App) *cli.App {
		return app.WithHiveTransport(nettest.Transport(tipHandler()))
	}, hiveCmd, "subscribe", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("hive subscribe exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "1 tip") {
		t.Errorf("subscribe did not report the pulled tip: %s", stdout)
	}

	db := openRunStore(t, dir)

	suggestions, err := db.ListSuggestions(t.Context(), store.SuggestionFilter{Status: store.SuggestionOpen})
	if err != nil {
		t.Fatalf("list suggestions: %v", err)
	}

	if len(suggestions) != 1 {
		t.Fatalf("queue has %d suggestions, want 1 from the hive", len(suggestions))
	}

	if suggestions[0].Evidence["source"] != "hive" {
		t.Errorf("ingested suggestion missing the hive source marker: %+v", suggestions[0].Evidence)
	}
}

// TestHivePublishRefusedWhenDisabled proves the gate at the CLI layer: with
// publish off, the command errors and nothing is sent.
func TestHivePublishRefusedWhenDisabled(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	writeHiveConfig(t, dir, "https://hive.example", false)
	seedHiveSuggestion(t, dir, jobSearchID)

	code, _, stderr := runCLI(t, hiveCmd, "publish", jobSearchID, "--observations", "10", flagDirArg, dir)
	if code == cli.ExitOK {
		t.Fatal("hive publish succeeded with publishing disabled")
	}

	if !strings.Contains(stderr, "disabled") {
		t.Errorf("publish error did not mention the disabled gate: %s", stderr)
	}
}

// TestHivePublishRefusedBelowObservations proves the local floor at the CLI
// layer: even with publishing on, below min_observations is refused.
func TestHivePublishRefusedBelowObservations(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	writeHiveConfig(t, dir, "https://hive.example", true)
	seedHiveSuggestion(t, dir, jobSearchID)

	// min_observations is 3 in the publish config; pass 1.
	code, _, stderr := runCLI(t, hiveCmd, "publish", jobSearchID, "--observations", "1", flagDirArg, dir)
	if code == cli.ExitOK {
		t.Fatal("hive publish succeeded below the observation floor")
	}

	if !strings.Contains(stderr, "min_observations") && !strings.Contains(stderr, "observations") {
		t.Errorf("publish error did not mention the observation floor: %s", stderr)
	}
}

// seedHiveSuggestion stores one caching suggestion the publish command can
// transform.
func seedHiveSuggestion(t *testing.T, dir, id string) {
	t.Helper()

	db := openRunStore(t, dir)

	err := db.SaveSuggestion(t.Context(), &store.Suggestion{
		ID:        id,
		Kind:      store.SuggestionCaching,
		Target:    "jobs.fetch",
		Rationale: cachingRationale,
		Evidence:  map[string]any{tuning.EvidenceRunner: cachingRunner},
		Status:    store.SuggestionApproved,
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed hive suggestion: %v", err)
	}
}
