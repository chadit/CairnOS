package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// Workflow-version fixture values; goconst applies to this package's tests.
const (
	versionHashOne   = "version-hash-1"
	versionHashTwo   = "version-hash-2"
	versionTOMLFirst = "name = \"alpha-workflow\"\nversion = \"1\"\n"
	versionTOMLAfter = "name = \"alpha-workflow\"\nversion = \"2\"\n"
)

func testWorkflowVersionRoundTrip[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	created := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	saved := &store.WorkflowVersion{
		Hash:           versionHashOne,
		WorkflowName:   workflowAlpha,
		DefinitionTOML: versionTOMLFirst,
		CreatedAt:      created,
	}
	if err := db.SaveWorkflowVersion(ctx, saved); err != nil {
		t.Fatalf("save workflow version: %v", err)
	}

	got, err := db.GetWorkflowVersion(ctx, versionHashOne)
	if err != nil {
		t.Fatalf("get workflow version: %v", err)
	}

	if got.WorkflowName != workflowAlpha || got.DefinitionTOML != versionTOMLFirst {
		t.Errorf("version = %+v, want the staged definition", got)
	}

	if !got.CreatedAt.Equal(created) {
		t.Errorf("version created_at = %v, want %v", got.CreatedAt, created)
	}
}

func testWorkflowVersionSaveStampsTime[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	before := time.Now().Add(-time.Minute)

	if err := db.SaveWorkflowVersion(ctx, &store.WorkflowVersion{
		Hash:           versionHashOne,
		WorkflowName:   workflowAlpha,
		DefinitionTOML: versionTOMLFirst,
	}); err != nil {
		t.Fatalf("save workflow version: %v", err)
	}

	got, err := db.GetWorkflowVersion(ctx, versionHashOne)
	if err != nil {
		t.Fatalf("get workflow version: %v", err)
	}

	if got.CreatedAt.Before(before) {
		t.Errorf("save did not stamp created_at: %v", got.CreatedAt)
	}
}

func testWorkflowVersionReSaveKeepsOriginal[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	first := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	if err := db.SaveWorkflowVersion(ctx, &store.WorkflowVersion{
		Hash:           versionHashOne,
		WorkflowName:   workflowAlpha,
		DefinitionTOML: versionTOMLFirst,
		CreatedAt:      first,
	}); err != nil {
		t.Fatalf("save first: %v", err)
	}

	// Re-saving the same hash must not overwrite the captured bytes or move
	// the timestamp: the snapshot is already the rollback point.
	if err := db.SaveWorkflowVersion(ctx, &store.WorkflowVersion{
		Hash:           versionHashOne,
		WorkflowName:   workflowAlpha,
		DefinitionTOML: versionTOMLAfter,
		CreatedAt:      first.Add(time.Hour),
	}); err != nil {
		t.Fatalf("re-save: %v", err)
	}

	got, err := db.GetWorkflowVersion(ctx, versionHashOne)
	if err != nil {
		t.Fatalf("get workflow version: %v", err)
	}

	if got.DefinitionTOML != versionTOMLFirst || !got.CreatedAt.Equal(first) {
		t.Errorf("re-save overwrote the original: %+v", got)
	}
}

func testWorkflowVersionNotFound[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)

	_, err := db.GetWorkflowVersion(t.Context(), "no-such-hash")
	if !errors.Is(err, store.ErrWorkflowVersionNotFound) {
		t.Errorf("GetWorkflowVersion error = %v, want ErrWorkflowVersionNotFound", err)
	}
}

func testWorkflowVersionListNewestFirst[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	base := time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC)

	if err := db.SaveWorkflowVersion(ctx, &store.WorkflowVersion{
		Hash: versionHashOne, WorkflowName: workflowAlpha, DefinitionTOML: versionTOMLFirst, CreatedAt: base,
	}); err != nil {
		t.Fatalf("save first: %v", err)
	}

	if err := db.SaveWorkflowVersion(ctx, &store.WorkflowVersion{
		Hash: versionHashTwo, WorkflowName: workflowAlpha, DefinitionTOML: versionTOMLAfter, CreatedAt: base.Add(time.Hour),
	}); err != nil {
		t.Fatalf("save second: %v", err)
	}

	// A version of a different workflow must not leak into alpha's list.
	if err := db.SaveWorkflowVersion(ctx, &store.WorkflowVersion{
		Hash: "version-hash-other", WorkflowName: workflowBeta, DefinitionTOML: versionTOMLFirst, CreatedAt: base.Add(twoUnits * time.Hour),
	}); err != nil {
		t.Fatalf("save other: %v", err)
	}

	versions, err := db.ListWorkflowVersions(ctx, workflowAlpha)
	if err != nil {
		t.Fatalf("list workflow versions: %v", err)
	}

	if len(versions) != twoUnits {
		t.Fatalf("listed %d versions, want 2 (beta must not leak in)", len(versions))
	}

	if versions[0].Hash != versionHashTwo || versions[1].Hash != versionHashOne {
		t.Errorf("version order = %q, %q, want hash-2 then hash-1 (newest first)", versions[0].Hash, versions[1].Hash)
	}
}

func testPruneLeavesWorkflowVersions[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)

	// A version captured well before the run-retention window must survive the
	// sweep: a rollback point outlives the runs that motivated the change.
	if err := db.SaveWorkflowVersion(ctx, &store.WorkflowVersion{
		Hash:           versionHashOne,
		WorkflowName:   workflowAlpha,
		DefinitionTOML: versionTOMLFirst,
		CreatedAt:      now.Add(-100 * day),
	}); err != nil {
		t.Fatalf("save old version: %v", err)
	}

	if _, err := db.Prune(ctx, store.PruneOptions{RunsOlderThan: retentionWindow, Now: now}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if _, err := db.GetWorkflowVersion(ctx, versionHashOne); err != nil {
		t.Errorf("version gone after prune: %v, want the rollback point kept", err)
	}
}
