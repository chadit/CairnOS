// Package memorytest is the shared behavioral spec for memory.Store
// implementations. RunConformance is one suite every backend runs against a
// fresh store: the sqlite backend calls it, and the postgres backend calls
// it when a test DSN is configured. One spec, two backends, so a keyword-
// search or Load difference between them fails a test instead of going
// unnoticed (.agents/spec/05-memory-and-knowledge.md, the backend-swappable
// acceptance criterion).
package memorytest

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/store"
)

// Shared fixture values; goconst applies to tests in this repo's lint policy.
const (
	scopeProject = "project:finance-app"
	scopeGlobal  = "global"
	tagFinance   = "finance"
	wordKafka    = "kafka"
	contentKafka = "kafka consumer lag spiked during the friday deploy"
)

// Fixture numbers named so the suite reads as code rather than tripping the
// magic-number check (this is library code, not a _test.go file).
const (
	importanceHigh = 80
	importanceMid  = 70
	loadBudget     = 60
	bigCost        = 40
	tooBigCost     = 50
	smallCost      = 10
	replaceImp     = 90
	replaceCost    = 33
	tieImportance  = 90
	tieCost        = 5
	fullBudget     = 100
)

// OpenFunc hands the suite a fresh, empty memory store. The implementation
// registers cleanup (t.Cleanup with Close) so each subtest gets an isolated
// database. Returning the interface keeps the suite backend-agnostic.
type OpenFunc func(t *testing.T) memory.Store

// RunConformance runs the full memory.Store behavioral suite against the
// store openFn hands out. Each named subtest opens its own store, so they
// are independent and the caller may run the suite under t.Parallel.
func RunConformance(t *testing.T, openFn OpenFunc) {
	t.Helper()

	cases := []struct {
		run  func(t *testing.T, openFn OpenFunc)
		name string
	}{
		{name: "SatisfiesInterface", run: testSatisfiesInterface},
		{name: "AddGetRoundTrip", run: testAddGetRoundTrip},
		{name: "ReplaceAndDelete", run: testReplaceAndDelete},
		{name: "LoadRespectsBudgetAndImportance", run: testLoadRespectsBudgetAndImportance},
		{name: "LoadBreaksImportanceTiesByCreation", run: testLoadBreaksTiesByCreation},
		{name: "ListFiltersScopeAndIsolatesWorkspaceInStableOrder", run: testListScopeWorkspaceAndOrder},
		{name: "SearchFindsByContentAndTag", run: testSearchFindsByContentAndTag},
		{name: "SemanticSearchFallsBackToKeyword", run: testSemanticFallsBack},
		{name: "SearchIndexStaysInSyncAfterReplaceAndDelete", run: testSearchIndexStaysInSync},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, openFn)
		})
	}
}

// addEntry stages one entry and returns its id.
func addEntry(t *testing.T, mem memory.Store, entry *memory.Entry) string {
	t.Helper()

	id, err := mem.Add(t.Context(), entry)
	if err != nil {
		t.Fatalf("add entry %q: %v", entry.Content, err)
	}

	return id
}

// searchIDs runs a search and returns the hit ids in result order.
func searchIDs(t *testing.T, mem memory.Store, query memory.Query) []string {
	t.Helper()

	result, err := mem.Search(t.Context(), query)
	if err != nil {
		t.Fatalf("search %+v: %v", query, err)
	}

	ids := make([]string, len(result.Entries))
	for idx := range result.Entries {
		ids[idx] = result.Entries[idx].ID
	}

	return ids
}

// listIDs lists a scope in the default workspace and returns the entry ids in
// result order, so an ordering assertion reads as a slice comparison.
func listIDs(t *testing.T, mem memory.Store, scope string) []string {
	t.Helper()

	entries, err := mem.List(t.Context(), scope)
	if err != nil {
		t.Fatalf("list scope %q: %v", scope, err)
	}

	ids := make([]string, len(entries))
	for idx := range entries {
		ids[idx] = entries[idx].ID
	}

	return ids
}

func testSatisfiesInterface(t *testing.T, openFn OpenFunc) {
	t.Helper()

	mem := openFn(t)

	if _, err := mem.Load(t.Context(), scopeGlobal, fullBudget); err != nil {
		t.Fatalf("load through interface: %v", err)
	}
}

func testAddGetRoundTrip(t *testing.T, openFn OpenFunc) {
	t.Helper()

	mem := openFn(t)
	content := "the finance API rate limit is 100 requests per second"
	tags := []string{tagFinance, "api"}

	id := addEntry(t, mem, &memory.Entry{
		Scope:      scopeProject,
		Content:    content,
		Importance: importanceHigh,
		Tags:       tags,
	})
	if id == "" {
		t.Fatal("Add returned an empty id")
	}

	got, err := mem.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get entry: %v", err)
	}

	if got.ID != id || got.Scope != scopeProject || got.Content != content || got.Importance != importanceHigh {
		t.Errorf("entry = %+v, want staged values", got)
	}

	if !reflect.DeepEqual(got.Tags, tags) {
		t.Errorf("tags = %v, want %v", got.Tags, tags)
	}

	// A zero TokenCost on Add is estimated as content bytes divided by four,
	// the documented Phase 0 heuristic both backends share.
	if want := len(content) / 4; got.TokenCost != want {
		t.Errorf("token cost = %d, want estimated %d", got.TokenCost, want)
	}

	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not stamped: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
}

func testReplaceAndDelete(t *testing.T, openFn OpenFunc) {
	t.Helper()

	mem := openFn(t)
	ctx := t.Context()

	id := addEntry(t, mem, &memory.Entry{
		Scope:      scopeProject,
		Content:    "old content",
		Importance: 50,
		CreatedAt:  time.Date(2026, 4, 1, 8, 0, 0, 0, time.UTC),
	})

	before, err := mem.Get(ctx, id)
	if err != nil {
		t.Fatalf("get before replace: %v", err)
	}

	replacement := &memory.Entry{
		Scope:      scopeProject,
		Content:    "new content",
		Importance: replaceImp,
		Tags:       []string{tagFinance},
		TokenCost:  replaceCost,
	}
	if innerErr := mem.Replace(ctx, id, replacement); innerErr != nil {
		t.Fatalf("replace entry: %v", innerErr)
	}

	after, err := mem.Get(ctx, id)
	if err != nil {
		t.Fatalf("get after replace: %v", err)
	}

	if after.Content != "new content" || after.Importance != replaceImp || after.TokenCost != replaceCost {
		t.Errorf("replaced entry = %+v, want new content, importance 90, cost 33", after)
	}

	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("replace moved created_at from %v to %v", before.CreatedAt, after.CreatedAt)
	}

	if err := mem.Delete(ctx, id); err != nil {
		t.Fatalf("delete entry: %v", err)
	}

	if _, err := mem.Get(ctx, id); !errors.Is(err, memory.ErrEntryNotFound) {
		t.Errorf("get deleted entry = %v, want ErrEntryNotFound", err)
	}

	if err := mem.Delete(ctx, id); !errors.Is(err, memory.ErrEntryNotFound) {
		t.Errorf("second delete = %v, want ErrEntryNotFound", err)
	}

	if err := mem.Replace(ctx, id, replacement); !errors.Is(err, memory.ErrEntryNotFound) {
		t.Errorf("replace of deleted entry = %v, want ErrEntryNotFound", err)
	}
}

func testLoadRespectsBudgetAndImportance(t *testing.T, openFn OpenFunc) {
	t.Helper()

	mem := openFn(t)

	bigID := addEntry(t, mem, &memory.Entry{Scope: scopeGlobal, Content: "most important", Importance: tieImportance, TokenCost: bigCost})
	tooBigID := addEntry(t, mem, &memory.Entry{Scope: scopeGlobal, Content: "does not fit", Importance: importanceHigh, TokenCost: tooBigCost})
	smallID := addEntry(t, mem, &memory.Entry{Scope: scopeGlobal, Content: "small filler", Importance: importanceMid, TokenCost: smallCost})
	addEntry(t, mem, &memory.Entry{Scope: "other-scope", Content: "wrong scope", Importance: 99, TokenCost: 1})

	loaded, err := mem.Load(t.Context(), scopeGlobal, loadBudget)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	ids := make([]string, len(loaded))
	for idx := range loaded {
		ids[idx] = loaded[idx].ID
	}

	// 40 fits the budget of 60, the 50-cost entry would cross the remaining
	// 20 and is excluded, then the 10-cost entry uses the leftover room.
	want := []string{bigID, smallID}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("loaded ids = %v, want %v", ids, want)
	}

	_ = tooBigID

	empty, err := mem.Load(t.Context(), scopeGlobal, 0)
	if err != nil {
		t.Fatalf("load with zero budget: %v", err)
	}

	if len(empty) != 0 {
		t.Errorf("zero budget loaded %d entries, want none", len(empty))
	}
}

func testLoadBreaksTiesByCreation(t *testing.T, openFn OpenFunc) {
	t.Helper()

	mem := openFn(t)
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	// The newer entry goes in first so ordering has to come from created_at,
	// not insertion order.
	newerID := addEntry(t, mem, &memory.Entry{Scope: scopeGlobal, Content: "newer note", Importance: tieImportance, TokenCost: tieCost, CreatedAt: base.Add(time.Hour)})
	olderID := addEntry(t, mem, &memory.Entry{Scope: scopeGlobal, Content: "older note", Importance: tieImportance, TokenCost: tieCost, CreatedAt: base})

	loaded, err := mem.Load(t.Context(), scopeGlobal, fullBudget)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(loaded) != 2 || loaded[0].ID != olderID || loaded[1].ID != newerID {
		t.Errorf("tie-break order wrong: got %+v, want older %s before newer %s", loaded, olderID, newerID)
	}
}

func testListScopeWorkspaceAndOrder(t *testing.T, openFn OpenFunc) {
	t.Helper()

	// Explicit ids so the created_at tie-break is checkable: idA and idB share
	// a timestamp, so only id ordering can put idA first. Referencing named
	// constants instead of repeating the literals keeps goconst quiet.
	const (
		idA = "list-entry-a"
		idB = "list-entry-b"
		idC = "list-entry-c"
		idD = "list-entry-d"
		idX = "list-entry-x"
	)

	mem := openFn(t)
	early := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	mid := early.Add(time.Hour)
	late := mid.Add(time.Hour)

	// Inserted out of created_at order so the result order can only come from
	// the query's ORDER BY, never insertion order.
	addEntry(t, mem, &memory.Entry{ID: idC, Scope: scopeGlobal, Content: "third by time", CreatedAt: mid})
	addEntry(t, mem, &memory.Entry{ID: idB, Scope: scopeGlobal, Content: "second by id", CreatedAt: early})
	addEntry(t, mem, &memory.Entry{ID: idA, Scope: scopeGlobal, Content: "first by id", CreatedAt: early})
	addEntry(t, mem, &memory.Entry{ID: idD, Scope: scopeProject, Content: "other scope", CreatedAt: late})

	// An entry in another workspace must never surface in the default one, so
	// exports stay tenant-scoped.
	otherCtx := store.ContextWithWorkspace(t.Context(), "tenant-b")
	if _, err := mem.Add(otherCtx, &memory.Entry{ID: idX, Scope: scopeGlobal, Content: "other tenant", CreatedAt: early}); err != nil {
		t.Fatalf("add in other workspace: %v", err)
	}

	// Empty scope lists every entry in the request workspace, created_at then
	// id, without crossing the workspace boundary.
	if got := listIDs(t, mem, ""); !reflect.DeepEqual(got, []string{idA, idB, idC, idD}) {
		t.Errorf("list all = %v, want created_at then id across scopes without the other tenant", got)
	}

	// A scope filter narrows to that scope only, keeping the same ordering.
	if got := listIDs(t, mem, scopeGlobal); !reflect.DeepEqual(got, []string{idA, idB, idC}) {
		t.Errorf("list scope = %v, want the three global entries in created_at then id order", got)
	}
}

func testSearchFindsByContentAndTag(t *testing.T, openFn OpenFunc) {
	t.Helper()

	mem := openFn(t)

	kafkaID := addEntry(t, mem, &memory.Entry{Scope: scopeGlobal, Content: contentKafka, Tags: []string{"infra"}})
	reportID := addEntry(t, mem, &memory.Entry{Scope: scopeProject, Content: "quarterly report numbers look solid", Tags: []string{tagFinance}})

	cases := []struct {
		name  string
		want  []string
		query memory.Query
	}{
		{
			name:  "content word",
			query: memory.Query{Text: wordKafka},
			want:  []string{kafkaID},
		},
		{
			name:  "tag filter",
			query: memory.Query{Tags: []string{tagFinance}},
			want:  []string{reportID},
		},
		{
			name:  "content and tag combine",
			query: memory.Query{Text: "report", Tags: []string{tagFinance}},
			want:  []string{reportID},
		},
		{
			name:  "scope narrows keyword hits",
			query: memory.Query{Scope: scopeProject, Text: wordKafka},
			want:  []string{},
		},
		{
			name:  "no terms lists the scope",
			query: memory.Query{Scope: scopeGlobal},
			want:  []string{kafkaID},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ids := searchIDs(t, mem, tt.query)
			if len(ids) == 0 && len(tt.want) == 0 {
				return
			}

			if !reflect.DeepEqual(ids, tt.want) {
				t.Errorf("search ids = %v, want %v", ids, tt.want)
			}
		})
	}
}

func testSemanticFallsBack(t *testing.T, openFn OpenFunc) {
	t.Helper()

	mem := openFn(t)
	kafkaID := addEntry(t, mem, &memory.Entry{Scope: scopeGlobal, Content: contentKafka})

	result, err := mem.Search(t.Context(), memory.Query{Text: wordKafka, Semantic: true})
	if err != nil {
		t.Fatalf("semantic search: %v", err)
	}

	if !result.Fallback {
		t.Error("semantic query without a vector backend did not set Fallback")
	}

	if len(result.Entries) != 1 || result.Entries[0].ID != kafkaID {
		t.Errorf("fallback keyword search returned %+v, want the kafka entry", result.Entries)
	}

	keyword, err := mem.Search(t.Context(), memory.Query{Text: wordKafka})
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}

	if keyword.Fallback {
		t.Error("plain keyword search set Fallback")
	}
}

func testSearchIndexStaysInSync(t *testing.T, openFn OpenFunc) {
	t.Helper()

	mem := openFn(t)
	ctx := t.Context()

	id := addEntry(t, mem, &memory.Entry{Scope: scopeGlobal, Content: "alpha bravo memo"})

	if ids := searchIDs(t, mem, memory.Query{Text: "bravo"}); len(ids) != 1 || ids[0] != id {
		t.Fatalf("fresh entry not searchable: %v", ids)
	}

	err := mem.Replace(ctx, id, &memory.Entry{Scope: scopeGlobal, Content: "charlie delta memo"})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	if ids := searchIDs(t, mem, memory.Query{Text: "bravo"}); len(ids) != 0 {
		t.Errorf("old content still indexed after replace: %v", ids)
	}

	if ids := searchIDs(t, mem, memory.Query{Text: "charlie"}); len(ids) != 1 || ids[0] != id {
		t.Errorf("new content not indexed after replace: %v", ids)
	}

	if err := mem.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if ids := searchIDs(t, mem, memory.Query{Text: "charlie"}); len(ids) != 0 {
		t.Errorf("deleted entry still indexed: %v", ids)
	}
}
