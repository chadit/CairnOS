package hive_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chadit/CairnOS/internal/hive"
	"github.com/chadit/CairnOS/internal/store"
)

// captureStore is a store.SuggestionStore that records every saved suggestion,
// so the ingest tests prove what landed in the queue without a real database.
// Only SaveSuggestion is exercised; the other methods satisfy the interface and
// are never called by Ingest.
type captureStore struct {
	saved []*store.Suggestion
}

func (c *captureStore) SaveSuggestion(_ context.Context, suggestion *store.Suggestion) error {
	c.saved = append(c.saved, suggestion)

	return nil
}

func (*captureStore) GetSuggestion(context.Context, string) (*store.Suggestion, error) {
	return nil, store.ErrSuggestionNotFound
}

func (c *captureStore) ListSuggestions(context.Context, store.SuggestionFilter) ([]*store.Suggestion, error) {
	return c.saved, nil
}

func (*captureStore) UpdateSuggestionStatus(context.Context, string, store.SuggestionStatus) error {
	return nil
}

// idCounter mints stable, distinct ids for the ingest tests so the assertions
// do not depend on ULID timing.
type idCounter struct {
	n atomic.Int64
}

func (c *idCounter) next() string {
	return "ingest-" + string(rune('a'+c.n.Add(1)-1))
}

// TestIngestLandsTipsInQueueAsOpen proves a received tip becomes an open local
// suggestion marked as coming from the hive, so it flows through the same
// review queue as a local suggestion and is never auto-applied just because it
// arrived from the network.
func TestIngestLandsTipsInQueueAsOpen(t *testing.T) {
	t.Parallel()

	queue := &captureStore{}
	counter := &idCounter{}

	ingester, err := hive.NewIngester(queue, counter.next)
	if err != nil {
		t.Fatalf("new ingester: %v", err)
	}

	tips := []hive.Tip{
		{Kind: store.SuggestionCaching, Category: hive.CategoryJobSearch, RunnerType: hive.RunnerTypeOpenAI, CostBucket: bucket20},
		{Kind: store.SuggestionParamTuning, Category: hive.CategoryCodeGen, RunnerType: hive.RunnerTypeAnthropic, CostBucket: "30%", ParamShape: &hive.ParamShape{Param: "max_tokens", Direction: hive.DirectionLowered}},
	}

	count, err := ingester.Ingest(t.Context(), tips)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if count != len(tips) {
		t.Fatalf("ingest saved %d, want %d", count, len(tips))
	}

	for idx, saved := range queue.saved {
		if saved.Status != store.SuggestionOpen {
			t.Errorf("tip %d saved with status %q, want open (a hive tip waits for a human)", idx, saved.Status)
		}

		if saved.Evidence[hive.EvidenceSource] != hive.SourceHive {
			t.Errorf("tip %d missing the hive source marker: %+v", idx, saved.Evidence)
		}

		if saved.EstSavings.Confidence != store.ConfidenceLow {
			t.Errorf("tip %d confidence = %q, want low (its saving was measured elsewhere)", idx, saved.EstSavings.Confidence)
		}
	}
}

// TestIngestCarriesNoConcreteChange proves an ingested suggestion has no
// Proposed map: a tip carries a shape, not a value, so applying one is never
// automatic.
func TestIngestCarriesNoConcreteChange(t *testing.T) {
	t.Parallel()

	queue := &captureStore{}
	counter := &idCounter{}

	ingester, err := hive.NewIngester(queue, counter.next)
	if err != nil {
		t.Fatalf("new ingester: %v", err)
	}

	tips := []hive.Tip{
		{Kind: store.SuggestionParamTuning, Category: hive.CategoryCodeGen, RunnerType: hive.RunnerTypeOpenAI, CostBucket: bucket20, ParamShape: &hive.ParamShape{Param: "max_tokens", Direction: hive.DirectionLowered}},
	}

	if _, err := ingester.Ingest(t.Context(), tips); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	saved := queue.saved[0]
	if len(saved.Proposed) != 0 {
		t.Errorf("ingested suggestion carries a concrete change %+v, want none (a tip is a shape, not a value)", saved.Proposed)
	}

	shape, ok := saved.Evidence[hive.EvidenceParamShape].(string)
	if !ok || !strings.Contains(shape, "lowered max_tokens") {
		t.Errorf("param shape evidence = %v, want the lowered max_tokens shape", saved.Evidence[hive.EvidenceParamShape])
	}
}

// TestNewIngesterRejectsNilMinter proves a missing id minter is caught at
// construction rather than producing empty ids later.
func TestNewIngesterRejectsNilMinter(t *testing.T) {
	t.Parallel()

	if _, err := hive.NewIngester(&captureStore{}, nil); err == nil {
		t.Error("NewIngester accepted a nil id minter")
	}
}

// TestIngestRationaleCarriesNoRawContent proves the reviewer-facing rationale
// of an ingested suggestion names only content-free fields, so the rationale
// itself never carries content.
func TestIngestRationaleCarriesNoRawContent(t *testing.T) {
	t.Parallel()

	queue := &captureStore{}
	counter := &idCounter{}

	ingester, err := hive.NewIngester(queue, counter.next)
	if err != nil {
		t.Fatalf("new ingester: %v", err)
	}

	tips := []hive.Tip{
		{Kind: store.SuggestionCaching, Category: hive.CategoryJobSearch, RunnerType: hive.RunnerTypeOpenAI, CostBucket: bucket20},
	}

	if _, err := ingester.Ingest(t.Context(), tips); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	rationale := queue.saved[0].Rationale
	for _, fragment := range []string{string(hive.CategoryJobSearch), string(hive.RunnerTypeOpenAI), bucket20} {
		if !strings.Contains(rationale, fragment) {
			t.Errorf("rationale %q missing content-free fragment %q", rationale, fragment)
		}
	}
}

// TestIngestPropagatesSaveError proves a failing store surfaces as an error and
// the count reflects how many saved before the failure.
func TestIngestPropagatesSaveError(t *testing.T) {
	t.Parallel()

	failing := &failingStore{failOn: 1}
	counter := &idCounter{}

	ingester, err := hive.NewIngester(failing, counter.next)
	if err != nil {
		t.Fatalf("new ingester: %v", err)
	}

	tips := []hive.Tip{
		{Kind: store.SuggestionCaching, Category: hive.CategoryJobSearch, RunnerType: hive.RunnerTypeOpenAI},
		{Kind: store.SuggestionBatching, Category: hive.CategoryJobSearch, RunnerType: hive.RunnerTypeOpenAI},
	}

	count, err := ingester.Ingest(t.Context(), tips)
	if err == nil {
		t.Fatal("ingest returned no error when the store failed")
	}

	if count != 1 {
		t.Errorf("ingest count = %d, want 1 saved before the failure", count)
	}
}

// failingStore fails SaveSuggestion on the failOn-th call (1-based), so the
// ingest error path is exercised.
type failingStore struct {
	captureStore

	failOn int
	calls  int
}

func (f *failingStore) SaveSuggestion(ctx context.Context, suggestion *store.Suggestion) error {
	f.calls++
	if f.calls > f.failOn {
		return errors.New("store is down")
	}

	return f.captureStore.SaveSuggestion(ctx, suggestion)
}
