package hive_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/hive"
	"github.com/chadit/CairnOS/internal/store"
)

// fakeHive is an httptest server standing in for the central hive service so
// the client is exercised end to end without a real network or a real hive.
// It records the last publish body and the last subscribe categories so a test
// can prove what (and whether) the client sent.
type fakeHive struct {
	server         *httptest.Server
	tips           []hive.Tip
	lastPublish    *publishCapture
	lastCategories []string
	publishCount   int
}

// publishCapture is the decoded body of one publish request, so a test can
// assert the pseudonym, the observation count, and the tip the client sent.
type publishCapture struct {
	Tip           hive.Tip `json:"tip"`
	NodePseudonym string   `json:"node_pseudonym"`
	Observations  int      `json:"observations"`
}

// newFakeHive starts a mock hive that returns the given tips on GET and records
// POSTs. The caller closes it via t.Cleanup.
func newFakeHive(t *testing.T, tips []hive.Tip) *fakeHive {
	t.Helper()

	mock := &fakeHive{tips: tips}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tips", mock.handle)

	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)

	return mock
}

func (m *fakeHive) handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		m.serveTips(w, r)
	case http.MethodPost:
		m.recordPublish(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (m *fakeHive) serveTips(w http.ResponseWriter, r *http.Request) {
	m.lastCategories = r.URL.Query()["category"]

	w.Header().Set("Content-Type", "application/json")

	body := struct {
		Tips []hive.Tip `json:"tips"`
	}{Tips: m.tips}

	if err := json.NewEncoder(w).Encode(body); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (m *fakeHive) recordPublish(w http.ResponseWriter, r *http.Request) {
	var capture publishCapture

	if err := json.NewDecoder(r.Body).Decode(&capture); err != nil {
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	m.publishCount++
	m.lastPublish = &capture

	w.WriteHeader(http.StatusAccepted)
}

// enabledConfig builds a hive config pointed at the mock, opted into the given
// categories, with a known pseudonym.
func enabledConfig(endpoint string, categories []hive.Category) hive.Config {
	return hive.Config{
		Enabled:         true,
		Endpoint:        endpoint,
		NodePseudonym:   "node-test-pseudonym",
		Categories:      categories,
		KAnonymityFloor: 5,
		MinObservations: 5,
	}
}

func TestSubscribeReturnsTips(t *testing.T) {
	t.Parallel()

	served := []hive.Tip{
		{Kind: store.SuggestionCaching, Category: hive.CategoryJobSearch, RunnerType: hive.RunnerTypeOpenAI, CostBucket: bucket20},
		{Kind: store.SuggestionBatching, Category: hive.CategoryJobSearch, RunnerType: hive.RunnerTypeAnthropic, CostBucket: "10%"},
	}
	mock := newFakeHive(t, served)

	client, err := hive.NewHTTPClient(enabledConfig(mock.server.URL, []hive.Category{hive.CategoryJobSearch}), mock.server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	tips, err := client.Subscribe(t.Context(), []hive.Category{hive.CategoryJobSearch})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if len(tips) != len(served) {
		t.Fatalf("subscribe returned %d tips, want %d", len(tips), len(served))
	}

	if tips[0].Kind != store.SuggestionCaching || tips[1].Kind != store.SuggestionBatching {
		t.Errorf("subscribe tips = %+v, want the served order", tips)
	}
}

// TestSubscribeDropsOffAllowlistTips proves the client discards a received tip
// whose category is outside the fixed set, so a buggy or hostile server cannot
// smuggle an off-allowlist field into the node's queue.
func TestSubscribeDropsOffAllowlistTips(t *testing.T) {
	t.Parallel()

	served := []hive.Tip{
		{Kind: store.SuggestionCaching, Category: hive.CategoryJobSearch, RunnerType: hive.RunnerTypeOpenAI},
		{Kind: store.SuggestionCaching, Category: "not-a-real-category", RunnerType: hive.RunnerTypeOpenAI},
		{Kind: "prompt_refactor", Category: hive.CategoryJobSearch, RunnerType: hive.RunnerTypeOpenAI},
	}
	mock := newFakeHive(t, served)

	client, err := hive.NewHTTPClient(enabledConfig(mock.server.URL, []hive.Category{hive.CategoryJobSearch}), mock.server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	tips, err := client.Subscribe(t.Context(), []hive.Category{hive.CategoryJobSearch})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if len(tips) != 1 {
		t.Fatalf("subscribe kept %d tips, want 1 (the off-allowlist tips must be dropped)", len(tips))
	}
}

// TestSubscribeOnlySendsOptedInCategories proves a category the node did not
// opt into never reaches the request, even if the caller asks for it.
func TestSubscribeOnlySendsOptedInCategories(t *testing.T) {
	t.Parallel()

	mock := newFakeHive(t, nil)

	// Opted into job-search only.
	client, err := hive.NewHTTPClient(enabledConfig(mock.server.URL, []hive.Category{hive.CategoryJobSearch}), mock.server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// Ask for both job-search and code-gen; only job-search should go out.
	if _, err := client.Subscribe(t.Context(), []hive.Category{hive.CategoryJobSearch, hive.CategoryCodeGen}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	got := mock.lastCategories
	if len(got) != 1 || got[0] != string(hive.CategoryJobSearch) {
		t.Errorf("request categories = %v, want only job-search (the opted-in category)", got)
	}
}

// TestPublishRefusedWhenDisabled proves the gate: with publish_enabled false,
// Publish returns ErrPublishDisabled and never reaches the server.
func TestPublishRefusedWhenDisabled(t *testing.T) {
	t.Parallel()

	mock := newFakeHive(t, nil)

	cfg := enabledConfig(mock.server.URL, []hive.Category{hive.CategoryJobSearch})
	cfg.PublishEnabled = false // explicit: this is the default and the gate under test

	client, err := hive.NewHTTPClient(cfg, mock.server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	tip := hive.Tip{Kind: store.SuggestionCaching, Category: hive.CategoryJobSearch, RunnerType: hive.RunnerTypeOpenAI}

	err = client.Publish(t.Context(), tip, 100)
	if !errors.Is(err, hive.ErrPublishDisabled) {
		t.Errorf("publish error = %v, want ErrPublishDisabled", err)
	}

	if mock.publishCount != 0 {
		t.Errorf("disabled publish still reached the server %d times", mock.publishCount)
	}
}

// TestPublishRefusedBelowMinObservations proves the local floor: even with
// publishing enabled, a pattern seen fewer than min_observations times is
// refused before any bytes leave the node.
func TestPublishRefusedBelowMinObservations(t *testing.T) {
	t.Parallel()

	mock := newFakeHive(t, nil)

	cfg := enabledConfig(mock.server.URL, []hive.Category{hive.CategoryJobSearch})
	cfg.PublishEnabled = true
	cfg.MinObservations = 5

	client, err := hive.NewHTTPClient(cfg, mock.server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	tip := hive.Tip{Kind: store.SuggestionCaching, Category: hive.CategoryJobSearch, RunnerType: hive.RunnerTypeOpenAI}

	// Seen only 3 times, below the floor of 5.
	err = client.Publish(t.Context(), tip, 3)
	if !errors.Is(err, hive.ErrBelowMinObservations) {
		t.Errorf("publish error = %v, want ErrBelowMinObservations", err)
	}

	if mock.publishCount != 0 {
		t.Errorf("below-floor publish still reached the server %d times", mock.publishCount)
	}
}

// TestPublishSendsPseudonymNotHostname proves a published tip carries the
// rotating node pseudonym, never the hostname or a stable instance name. It
// also confirms an enabled, above-floor publish reaches the server.
func TestPublishSendsPseudonymNotHostname(t *testing.T) {
	t.Parallel()

	mock := newFakeHive(t, nil)

	pseudonym, err := hive.NewPseudonym()
	if err != nil {
		t.Fatalf("new pseudonym: %v", err)
	}

	cfg := enabledConfig(mock.server.URL, []hive.Category{hive.CategoryJobSearch})
	cfg.PublishEnabled = true
	cfg.NodePseudonym = pseudonym

	client, err := hive.NewHTTPClient(cfg, mock.server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	tip := hive.Tip{Kind: store.SuggestionCaching, Category: hive.CategoryJobSearch, RunnerType: hive.RunnerTypeOpenAI, CostBucket: bucket20}

	if err := client.Publish(t.Context(), tip, 10); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if mock.publishCount != 1 {
		t.Fatalf("publish reached the server %d times, want 1", mock.publishCount)
	}

	sent := mock.lastPublish
	if sent.NodePseudonym != pseudonym {
		t.Errorf("published pseudonym = %q, want the rotating pseudonym %q", sent.NodePseudonym, pseudonym)
	}

	if !strings.HasPrefix(sent.NodePseudonym, "node-") {
		t.Errorf("pseudonym %q is not a rotating node- id", sent.NodePseudonym)
	}

	host, _ := os.Hostname()
	if host != "" && strings.Contains(sent.NodePseudonym, host) {
		t.Errorf("pseudonym %q contains the hostname %q", sent.NodePseudonym, host)
	}
}

// TestNewHTTPClientRejectsDisabledAndMissingEndpoint proves a misconfigured
// node fails at construction rather than at first request.
func TestNewHTTPClientRejectsDisabledAndMissingEndpoint(t *testing.T) {
	t.Parallel()

	if _, err := hive.NewHTTPClient(hive.Config{Enabled: false}, nil); !errors.Is(err, hive.ErrHiveDisabled) {
		t.Errorf("disabled client error = %v, want ErrHiveDisabled", err)
	}

	if _, err := hive.NewHTTPClient(hive.Config{Enabled: true}, nil); !errors.Is(err, hive.ErrNoEndpoint) {
		t.Errorf("no-endpoint client error = %v, want ErrNoEndpoint", err)
	}
}
