// Package webhooktest holds the registration-store conformance suite shared by
// the webhook package's backends. The sqlite unit test and the postgres
// integration test both drive it, so the two backends are held to one
// behavioral contract.
package webhooktest

import (
	"errors"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/webhook"
)

// regCount is the number of registrations the list-ordering case adds, so the
// assertions read against a named expectation rather than a bare literal.
const regCount = 3

// baseTime is the fixed clock the cases stamp CreatedAt from, so list ordering
// comes out the same on both backends without sleeping.
func baseTime() time.Time {
	return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
}

// RunConformance drives one backend through the store contract. Each case gets
// a fresh, empty store from newStore, so the cases stay independent and run in
// parallel.
func RunConformance(t *testing.T, newStore func(*testing.T) webhook.Store) {
	t.Helper()

	cases := []struct {
		name string
		run  func(*testing.T, webhook.Store)
	}{
		{"AddThenLookupByBothHashes", testAddThenLookupByBothHashes},
		{"RotateInvalidatesOldHashKeepsSibling", testRotateInvalidatesOldHash},
		{"RemoveDeletesRegistration", testRemoveDeletesRegistration},
		{"AddDuplicateWorkflowIsErrDuplicate", testAddDuplicateWorkflow},
		{"UnknownHashIsErrNotFound", testUnknownHash},
		{"UnknownWorkflowIsErrNotFound", testUnknownWorkflow},
		{"ListReturnsOldestFirst", testListOrdering},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			testCase.run(t, newStore(t))
		})
	}
}

// testAddThenLookupByBothHashes proves one Add is reachable by either token
// hash, and each lookup reports the endpoint whose hash matched.
func testAddThenLookupByBothHashes(t *testing.T, store webhook.Store) {
	t.Helper()

	reg := mustAdd(t, store, "alpha", baseTime())

	prod, endpoint := mustGetByHash(t, store, reg.ProdTokenHash)
	if endpoint != webhook.EndpointProd {
		t.Errorf("prod hash endpoint = %d, want EndpointProd", endpoint)
	}

	if prod.WorkflowName != reg.WorkflowName {
		t.Errorf("prod lookup workflow = %q, want %q", prod.WorkflowName, reg.WorkflowName)
	}

	_, endpoint = mustGetByHash(t, store, reg.TestTokenHash)
	if endpoint != webhook.EndpointTest {
		t.Errorf("test hash endpoint = %d, want EndpointTest", endpoint)
	}
}

// testRotateInvalidatesOldHash proves rotating one endpoint kills its old hash
// at that moment while the sibling endpoint's token keeps resolving.
func testRotateInvalidatesOldHash(t *testing.T, store webhook.Store) {
	t.Helper()

	reg := mustAdd(t, store, "beta", baseTime())
	oldProdHash := reg.ProdTokenHash

	_, newProdHash, err := webhook.MintToken()
	if err != nil {
		t.Fatalf("mint replacement token: %v", err)
	}

	if updateErr := store.UpdateTokenHash(t.Context(), "beta", webhook.EndpointProd, newProdHash); updateErr != nil {
		t.Fatalf("rotate prod: %v", updateErr)
	}

	if _, _, getErr := store.GetByTokenHash(t.Context(), oldProdHash); !errors.Is(getErr, webhook.ErrNotFound) {
		t.Errorf("old prod hash after rotate = %v, want ErrNotFound", getErr)
	}

	rotated, endpoint := mustGetByHash(t, store, newProdHash)
	if endpoint != webhook.EndpointProd || rotated.WorkflowName != "beta" {
		t.Errorf("new prod hash resolved to %q/%d, want beta/EndpointProd", rotated.WorkflowName, endpoint)
	}

	// The test endpoint's token is untouched by a prod rotation.
	_, endpoint = mustGetByHash(t, store, reg.TestTokenHash)
	if endpoint != webhook.EndpointTest {
		t.Errorf("sibling test hash endpoint = %d, want EndpointTest", endpoint)
	}
}

// testRemoveDeletesRegistration proves Remove drops the row so neither the
// workflow nor its token hashes resolve, and a second Remove is ErrNotFound.
func testRemoveDeletesRegistration(t *testing.T, store webhook.Store) {
	t.Helper()

	reg := mustAdd(t, store, "gamma", baseTime())

	if err := store.Remove(t.Context(), "gamma"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if _, err := store.GetByWorkflow(t.Context(), "gamma"); !errors.Is(err, webhook.ErrNotFound) {
		t.Errorf("get after remove = %v, want ErrNotFound", err)
	}

	if _, _, err := store.GetByTokenHash(t.Context(), reg.ProdTokenHash); !errors.Is(err, webhook.ErrNotFound) {
		t.Errorf("hash lookup after remove = %v, want ErrNotFound", err)
	}

	if err := store.Remove(t.Context(), "gamma"); !errors.Is(err, webhook.ErrNotFound) {
		t.Errorf("second remove = %v, want ErrNotFound", err)
	}
}

// testAddDuplicateWorkflow proves the one-registration-per-workflow invariant:
// a second Add for the same workflow is ErrDuplicate.
func testAddDuplicateWorkflow(t *testing.T, store webhook.Store) {
	t.Helper()

	mustAdd(t, store, "delta", baseTime())

	_, prodHash, err := webhook.MintToken()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	_, testHash, err := webhook.MintToken()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	dup := &webhook.Registration{
		WorkflowName:  "delta",
		WorkflowPath:  "delta.toml",
		ProdTokenHash: prodHash,
		TestTokenHash: testHash,
		CreatedAt:     baseTime(),
	}
	if addErr := store.Add(t.Context(), dup); !errors.Is(addErr, webhook.ErrDuplicate) {
		t.Errorf("duplicate add = %v, want ErrDuplicate", addErr)
	}
}

// testUnknownHash proves an unmatched token hash is ErrNotFound with no row
// created.
func testUnknownHash(t *testing.T, store webhook.Store) {
	t.Helper()

	missing := webhook.HashToken("no-such-token")
	if _, _, err := store.GetByTokenHash(t.Context(), missing); !errors.Is(err, webhook.ErrNotFound) {
		t.Errorf("unknown hash = %v, want ErrNotFound", err)
	}
}

// testUnknownWorkflow proves lookups and mutations against an unknown workflow
// all surface ErrNotFound.
func testUnknownWorkflow(t *testing.T, store webhook.Store) {
	t.Helper()

	if _, err := store.GetByWorkflow(t.Context(), "ghost"); !errors.Is(err, webhook.ErrNotFound) {
		t.Errorf("get unknown workflow = %v, want ErrNotFound", err)
	}

	_, hash, err := webhook.MintToken()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	if updateErr := store.UpdateTokenHash(t.Context(), "ghost", webhook.EndpointProd, hash); !errors.Is(updateErr, webhook.ErrNotFound) {
		t.Errorf("rotate unknown workflow = %v, want ErrNotFound", updateErr)
	}
}

// testListOrdering proves List returns every registration oldest first.
func testListOrdering(t *testing.T, store webhook.Store) {
	t.Helper()

	want := []string{"one", "two", "three"}
	for idx, name := range want {
		mustAdd(t, store, name, baseTime().Add(time.Duration(idx)*time.Second))
	}

	regs, err := store.List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(regs) != regCount {
		t.Fatalf("list length = %d, want %d", len(regs), regCount)
	}

	for idx, reg := range regs {
		if reg.WorkflowName != want[idx] {
			t.Errorf("list[%d] = %q, want %q", idx, reg.WorkflowName, want[idx])
		}
	}
}

// mustAdd mints fresh tokens for workflowName, adds the registration stamped at
// createdAt, and returns it, failing the test on error.
func mustAdd(t *testing.T, store webhook.Store, workflowName string, createdAt time.Time) *webhook.Registration {
	t.Helper()

	_, prodHash, err := webhook.MintToken()
	if err != nil {
		t.Fatalf("mint prod token: %v", err)
	}

	_, testHash, err := webhook.MintToken()
	if err != nil {
		t.Fatalf("mint test token: %v", err)
	}

	reg := &webhook.Registration{
		WorkflowName:  workflowName,
		WorkflowPath:  workflowName + ".toml",
		ProdTokenHash: prodHash,
		TestTokenHash: testHash,
		CreatedAt:     createdAt,
	}
	if addErr := store.Add(t.Context(), reg); addErr != nil {
		t.Fatalf("add %s: %v", workflowName, addErr)
	}

	return reg
}

// mustGetByHash looks a registration up by token hash, failing the test when
// the hash does not resolve.
func mustGetByHash(t *testing.T, store webhook.Store, hash string) (*webhook.Registration, webhook.Endpoint) {
	t.Helper()

	reg, endpoint, err := store.GetByTokenHash(t.Context(), hash)
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}

	return reg, endpoint
}
