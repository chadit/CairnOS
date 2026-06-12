// Package vaulttest is the shared behavioral spec for vault.Store
// implementations. RunConformance is one suite every backend runs against a
// fresh store: the sqlite backend calls it, and the postgres backend calls it
// when a test DSN is configured. One spec, two backends, so a graph-traversal
// or backlink difference between them fails a test instead of going unnoticed
// (.agents/spec/05-memory-and-knowledge.md, the backend-swappable criterion).
package vaulttest

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/vault"
)

// Shared fixture values; goconst applies to tests in this repo's lint policy.
const (
	titleAlpha   = "alpha note"
	titleBravo   = "bravo note"
	bodyKafka    = "kafka consumer lag spiked during the friday deploy"
	tagInfra     = "infra"
	tagFinance   = "finance"
	projFinance  = "finance-app"
	projFinTag   = "project:finance-app"
	wordKafka    = "kafka"
	bodyLeaf     = "leaf"
	tagGoRules   = "go-rules"
	defaultLimit = 50
)

// twoDocs is the document count the list and roundtrip cases expect; a named
// constant keeps the magic-number check happy in test code.
const twoDocs = 2

// OpenFunc hands the suite a fresh, empty vault store. The implementation
// registers cleanup (t.Cleanup with Close) so each subtest gets an isolated
// database. Returning the interface keeps the suite backend-agnostic.
type OpenFunc func(t *testing.T) vault.Store

// RunConformance runs the full vault.Store behavioral suite against the store
// openFn hands out. Each named subtest opens its own store, so they are
// independent and the caller may run the suite under t.Parallel.
func RunConformance(t *testing.T, openFn OpenFunc) {
	t.Helper()

	cases := []struct {
		name string
		run  func(t *testing.T, openFn OpenFunc)
	}{
		{"SatisfiesInterface", testSatisfiesInterface},
		{"PutGetRoundTrip", testPutGetRoundTrip},
		{"PutOverwritesAndPreservesCreatedAt", testPutOverwrites},
		{"BacklinksDerived", testBacklinksDerived},
		{"DeleteRemovesDocumentAndLinks", testDeleteRemovesLinks},
		{"ListNewestFirst", testListNewestFirst},
		{"QueryByTag", testQueryByTag},
		{"QueryByProject", testQueryByProject},
		{"QueryOneHopTraversal", testQueryOneHop},
		{"QueryTwoHopTraversal", testQueryTwoHop},
		{"QueryHopBoundExcludesDistant", testQueryHopBound},
		{"QueryCombinesTagAndTraversal", testQueryCombined},
		{"QueryKeywordText", testQueryKeyword},
		{"SemanticQueryFallsBackToKeyword", testSemanticFallsBack},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, openFn)
		})
	}
}

// put stages one document and returns its id.
func put(t *testing.T, store vault.Store, doc *vault.Document) string {
	t.Helper()

	id, err := store.Put(t.Context(), doc)
	if err != nil {
		t.Fatalf("put %q: %v", doc.Title, err)
	}

	return id
}

// queryIDs runs a query and returns the hit ids in result order. The query is
// taken by pointer so the wide vault.Query struct does not copy per call
// (gocritic hugeParam); callers pass &vault.Query{...}.
func queryIDs(t *testing.T, store vault.Store, query *vault.Query) []string {
	t.Helper()

	result, err := store.Query(t.Context(), *query)
	if err != nil {
		t.Fatalf("query %+v: %v", query, err)
	}

	ids := make([]string, len(result.Documents))
	for idx := range result.Documents {
		ids[idx] = result.Documents[idx].ID
	}

	return ids
}

func testSatisfiesInterface(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)

	if _, err := store.List(t.Context(), defaultLimit); err != nil {
		t.Fatalf("list through interface: %v", err)
	}
}

func testPutGetRoundTrip(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	tags := []string{tagInfra, tagFinance}

	id := put(t, store, &vault.Document{
		Title: titleAlpha,
		Body:  bodyKafka,
		Tags:  tags,
	})
	if id == "" {
		t.Fatal("Put returned an empty id")
	}

	got, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}

	if got.ID != id || got.Title != titleAlpha || got.Body != bodyKafka {
		t.Errorf("document = %+v, want staged values", got)
	}

	if !reflect.DeepEqual(got.Tags, tags) {
		t.Errorf("tags = %v, want %v", got.Tags, tags)
	}

	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not stamped: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}

	if _, err := store.Get(t.Context(), "missing-id"); !errors.Is(err, vault.ErrDocumentNotFound) {
		t.Errorf("get missing = %v, want ErrDocumentNotFound", err)
	}
}

func testPutOverwrites(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	ctx := t.Context()

	id := put(t, store, &vault.Document{Title: titleAlpha, Body: "first body", Tags: []string{tagInfra}})

	before, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get before overwrite: %v", err)
	}

	if _, err := store.Put(ctx, &vault.Document{ID: id, Title: "renamed", Body: "second body"}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	after, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get after overwrite: %v", err)
	}

	if after.Title != "renamed" || after.Body != "second body" {
		t.Errorf("overwritten document = %+v, want renamed/second body", after)
	}

	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("overwrite moved created_at from %v to %v", before.CreatedAt, after.CreatedAt)
	}

	if len(after.Tags) != 0 {
		t.Errorf("overwrite kept stale tags %v, want cleared", after.Tags)
	}
}

func testBacklinksDerived(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	ctx := t.Context()

	// hub links to leaf; the backlink on leaf is derived, never stored on it.
	leafID := put(t, store, &vault.Document{Title: titleBravo, Body: bodyLeaf})
	hubID := put(t, store, &vault.Document{Title: titleAlpha, Body: "hub", Links: []string{leafID}})

	leaf, err := store.Get(ctx, leafID)
	if err != nil {
		t.Fatalf("get leaf: %v", err)
	}

	if !slices.Contains(leaf.Backlinks, hubID) {
		t.Errorf("leaf backlinks = %v, want to include hub %s", leaf.Backlinks, hubID)
	}

	hub, err := store.Get(ctx, hubID)
	if err != nil {
		t.Fatalf("get hub: %v", err)
	}

	if !slices.Contains(hub.Links, leafID) {
		t.Errorf("hub links = %v, want to include leaf %s", hub.Links, leafID)
	}

	if len(hub.Backlinks) != 0 {
		t.Errorf("hub has unexpected backlinks %v", hub.Backlinks)
	}
}

func testDeleteRemovesLinks(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	ctx := t.Context()

	leafID := put(t, store, &vault.Document{Title: titleBravo, Body: bodyLeaf})
	hubID := put(t, store, &vault.Document{Title: titleAlpha, Body: "hub", Links: []string{leafID}})

	if err := store.Delete(ctx, hubID); err != nil {
		t.Fatalf("delete hub: %v", err)
	}

	if _, err := store.Get(ctx, hubID); !errors.Is(err, vault.ErrDocumentNotFound) {
		t.Errorf("get deleted hub = %v, want ErrDocumentNotFound", err)
	}

	// Deleting the hub must drop the inbound link so the leaf has no dangling
	// backlink to a document that no longer exists.
	leaf, err := store.Get(ctx, leafID)
	if err != nil {
		t.Fatalf("get leaf after hub delete: %v", err)
	}

	if len(leaf.Backlinks) != 0 {
		t.Errorf("leaf kept dangling backlink %v after hub delete", leaf.Backlinks)
	}

	if err := store.Delete(ctx, hubID); !errors.Is(err, vault.ErrDocumentNotFound) {
		t.Errorf("second delete = %v, want ErrDocumentNotFound", err)
	}
}

func testListNewestFirst(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)

	firstID := put(t, store, &vault.Document{Title: titleAlpha, Body: "first"})
	secondID := put(t, store, &vault.Document{Title: titleBravo, Body: "second"})

	docs, err := store.List(t.Context(), defaultLimit)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(docs) != twoDocs {
		t.Fatalf("list returned %d docs, want 2", len(docs))
	}

	// ULIDs are monotonic within a process, so the second put sorts newest;
	// the list orders by created_at DESC then id DESC, so it leads.
	if docs[0].ID != secondID || docs[1].ID != firstID {
		t.Errorf("list order = [%s %s], want newest %s first", docs[0].ID, docs[1].ID, secondID)
	}
}

func testQueryByTag(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)

	infraID := put(t, store, &vault.Document{Title: titleAlpha, Body: bodyKafka, Tags: []string{tagInfra}})
	put(t, store, &vault.Document{Title: titleBravo, Body: "report", Tags: []string{tagFinance}})

	ids := queryIDs(t, store, &vault.Query{Tags: []string{tagInfra}})
	if !reflect.DeepEqual(ids, []string{infraID}) {
		t.Errorf("tag query ids = %v, want [%s]", ids, infraID)
	}
}

func testQueryByProject(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)

	// A project is the "project:<name>" tag convention; the rules doc's
	// "all documents tagged python-rules for project finance-app".
	finID := put(t, store, &vault.Document{
		Title: titleAlpha, Body: "go rules", Tags: []string{tagGoRules, projFinTag},
	})
	put(t, store, &vault.Document{Title: titleBravo, Body: "other", Tags: []string{tagGoRules}})

	ids := queryIDs(t, store, &vault.Query{Tags: []string{tagGoRules}, Project: projFinance})
	if !reflect.DeepEqual(ids, []string{finID}) {
		t.Errorf("project query ids = %v, want [%s]", ids, finID)
	}
}

// linkChain builds a -> b -> c -> d and returns the four ids in order, so the
// traversal tests share one fixture.
func linkChain(t *testing.T, store vault.Store) (string, string, string, string) {
	t.Helper()

	dID := put(t, store, &vault.Document{Title: "d", Body: "leaf"})
	cID := put(t, store, &vault.Document{Title: "c", Body: "third", Links: []string{dID}})
	bID := put(t, store, &vault.Document{Title: "b", Body: "second", Links: []string{cID}})
	aID := put(t, store, &vault.Document{Title: "a", Body: "root", Links: []string{bID}})

	return aID, bID, cID, dID
}

func testQueryOneHop(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	aID, bID, _, _ := linkChain(t, store)

	ids := queryIDs(t, store, &vault.Query{LinkedFrom: aID, Hops: 1})
	if !reflect.DeepEqual(ids, []string{bID}) {
		t.Errorf("one-hop from %s = %v, want [%s]", aID, ids, bID)
	}
}

func testQueryTwoHop(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	aID, bID, cID, _ := linkChain(t, store)

	ids := queryIDs(t, store, &vault.Query{LinkedFrom: aID, Hops: 2})
	want := slices.Clone([]string{bID, cID})
	slices.Sort(want)
	slices.Sort(ids)

	if !reflect.DeepEqual(ids, want) {
		t.Errorf("two-hop from %s = %v, want %v", aID, ids, want)
	}
}

func testQueryHopBound(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	aID, _, _, dID := linkChain(t, store)

	// Two hops from a reaches b and c, never d (three hops out). The bound is
	// what keeps a deep chain from returning the whole graph.
	ids := queryIDs(t, store, &vault.Query{LinkedFrom: aID, Hops: 2})
	if slices.Contains(ids, dID) {
		t.Errorf("two-hop from %s reached distant %s: %v", aID, dID, ids)
	}
}

func testQueryCombined(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)

	// root links to two docs; only one carries the finance tag, so a combined
	// traversal-plus-tag query returns exactly that one.
	taggedID := put(t, store, &vault.Document{Title: titleBravo, Body: "tagged child", Tags: []string{tagFinance}})
	plainID := put(t, store, &vault.Document{Title: "plain", Body: "plain child"})
	rootID := put(t, store, &vault.Document{Title: titleAlpha, Body: "root", Links: []string{taggedID, plainID}})

	ids := queryIDs(t, store, &vault.Query{LinkedFrom: rootID, Hops: 1, Tags: []string{tagFinance}})
	if !reflect.DeepEqual(ids, []string{taggedID}) {
		t.Errorf("combined query = %v, want [%s] (plain %s should be filtered)", ids, taggedID, plainID)
	}
}

func testQueryKeyword(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)

	kafkaID := put(t, store, &vault.Document{Title: titleAlpha, Body: bodyKafka})
	put(t, store, &vault.Document{Title: titleBravo, Body: "quarterly report numbers"})

	ids := queryIDs(t, store, &vault.Query{Text: wordKafka})
	if !reflect.DeepEqual(ids, []string{kafkaID}) {
		t.Errorf("keyword query ids = %v, want [%s]", ids, kafkaID)
	}
}

func testSemanticFallsBack(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	kafkaID := put(t, store, &vault.Document{Title: titleAlpha, Body: bodyKafka})

	result, err := store.Query(t.Context(), vault.Query{Text: wordKafka, Semantic: true})
	if err != nil {
		t.Fatalf("semantic query: %v", err)
	}

	if !result.Fallback {
		t.Error("semantic query without a vector backend did not set Fallback")
	}

	if len(result.Documents) != 1 || result.Documents[0].ID != kafkaID {
		t.Errorf("fallback keyword query returned %+v, want the kafka document", result.Documents)
	}

	plain, err := store.Query(t.Context(), vault.Query{Text: wordKafka})
	if err != nil {
		t.Fatalf("keyword query: %v", err)
	}

	if plain.Fallback {
		t.Error("plain keyword query set Fallback")
	}
}
