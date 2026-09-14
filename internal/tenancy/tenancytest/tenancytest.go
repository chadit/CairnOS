// Package tenancytest is the shared behavioral spec for tenancy.Store
// implementations. RunConformance is one suite every backend runs against a
// fresh store: the sqlite backend calls it, and the postgres backend calls it
// when a test DSN is configured. One spec, two backends, so a workspace or
// membership difference between them fails a test instead of going unnoticed
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
package tenancytest

import (
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/tenancy"
)

// Shared fixture values; goconst applies to tests in this repo's lint policy.
const (
	workspaceAcme   = "acme"
	workspaceGlobex = "globex"
	subjectAlice    = "alice@example.com"
	subjectBob      = "bob@example.com"
	missingID       = "no-such-workspace"

	// wantTwoMembers names the member count so the suite reads as code rather
	// than tripping the magic-number check (this is library code, not _test.go).
	wantTwoMembers = 2
)

// OpenFunc hands the suite a fresh tenancy store with the default workspace
// already seeded. The implementation registers cleanup (t.Cleanup with Close)
// so each subtest gets an isolated database. Returning the interface keeps the
// suite backend-agnostic.
type OpenFunc func(t *testing.T) tenancy.Store

// RunConformance runs the full tenancy.Store behavioral suite against the
// store openFn hands out. Each named subtest opens its own store, so they are
// independent and the caller may run the suite under t.Parallel.
func RunConformance(t *testing.T, openFn OpenFunc) {
	t.Helper()

	cases := []struct {
		run  func(t *testing.T, openFn OpenFunc)
		name string
	}{
		{name: "DefaultWorkspaceSeeded", run: testDefaultWorkspaceSeeded},
		{name: "WorkspaceCRUD", run: testWorkspaceCRUD},
		{name: "DuplicateNameRejected", run: testDuplicateNameRejected},
		{name: "GetWorkspaceNotFound", run: testGetWorkspaceNotFound},
		{name: "AddMemberAndGetRole", run: testAddMemberAndGetRole},
		{name: "AddMemberOverwritesRole", run: testAddMemberOverwritesRole},
		{name: "AddMemberRejectsUnknownRole", run: testAddMemberRejectsUnknownRole},
		{name: "AddMemberRejectsMissingWorkspace", run: testAddMemberRejectsMissingWorkspace},
		{name: "GetMemberRoleNotFound", run: testGetMemberRoleNotFound},
		{name: "ListMembers", run: testListMembers},
		{name: "MembersIsolatedByWorkspace", run: testMembersIsolatedByWorkspace},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, openFn)
		})
	}
}

// createWorkspace stages one workspace and returns its assigned id.
func createWorkspace(t *testing.T, tenants tenancy.Store, name string) string {
	t.Helper()

	workspace := &tenancy.Workspace{Name: name}
	if err := tenants.CreateWorkspace(t.Context(), workspace); err != nil {
		t.Fatalf("CreateWorkspace(%q): %v", name, err)
	}

	return workspace.ID
}

func testDefaultWorkspaceSeeded(t *testing.T, openFn OpenFunc) {
	t.Helper()

	tenants := openFn(t)

	got, err := tenants.GetWorkspace(t.Context(), store.DefaultWorkspace)
	if err != nil {
		t.Fatalf("GetWorkspace(default): %v", err)
	}

	if got.Name != store.DefaultWorkspace {
		t.Fatalf("default workspace name = %q, want %q", got.Name, store.DefaultWorkspace)
	}
}

func testWorkspaceCRUD(t *testing.T, openFn OpenFunc) {
	t.Helper()

	tenants := openFn(t)
	id := createWorkspace(t, tenants, workspaceAcme)

	got, err := tenants.GetWorkspace(t.Context(), id)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}

	if got.Name != workspaceAcme {
		t.Fatalf("workspace name = %q, want %q", got.Name, workspaceAcme)
	}

	workspaces, err := tenants.ListWorkspaces(t.Context())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}

	// The default workspace plus the one created here.
	if len(workspaces) != wantTwoMembers {
		t.Fatalf("listed %d workspaces, want 2", len(workspaces))
	}
}

func testDuplicateNameRejected(t *testing.T, openFn OpenFunc) {
	t.Helper()

	tenants := openFn(t)
	createWorkspace(t, tenants, workspaceAcme)

	if err := tenants.CreateWorkspace(t.Context(), &tenancy.Workspace{Name: workspaceAcme}); err == nil {
		t.Fatal("duplicate workspace name succeeded, want an error")
	}
}

func testGetWorkspaceNotFound(t *testing.T, openFn OpenFunc) {
	t.Helper()

	tenants := openFn(t)

	if _, err := tenants.GetWorkspace(t.Context(), missingID); !errors.Is(err, tenancy.ErrWorkspaceNotFound) {
		t.Fatalf("GetWorkspace error = %v, want ErrWorkspaceNotFound", err)
	}
}

func testAddMemberAndGetRole(t *testing.T, openFn OpenFunc) {
	t.Helper()

	tenants := openFn(t)
	id := createWorkspace(t, tenants, workspaceAcme)

	if err := tenants.AddMember(t.Context(), id, subjectAlice, tenancy.RoleEditor); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	role, err := tenants.GetMemberRole(t.Context(), id, subjectAlice)
	if err != nil {
		t.Fatalf("GetMemberRole: %v", err)
	}

	if role != tenancy.RoleEditor {
		t.Fatalf("role = %q, want %q", role, tenancy.RoleEditor)
	}
}

func testAddMemberOverwritesRole(t *testing.T, openFn OpenFunc) {
	t.Helper()

	tenants := openFn(t)
	id := createWorkspace(t, tenants, workspaceAcme)

	if err := tenants.AddMember(t.Context(), id, subjectAlice, tenancy.RoleViewer); err != nil {
		t.Fatalf("first AddMember: %v", err)
	}

	if err := tenants.AddMember(t.Context(), id, subjectAlice, tenancy.RoleAdmin); err != nil {
		t.Fatalf("second AddMember: %v", err)
	}

	role, err := tenants.GetMemberRole(t.Context(), id, subjectAlice)
	if err != nil {
		t.Fatalf("GetMemberRole: %v", err)
	}

	if role != tenancy.RoleAdmin {
		t.Fatalf("role after overwrite = %q, want %q", role, tenancy.RoleAdmin)
	}
}

func testAddMemberRejectsUnknownRole(t *testing.T, openFn OpenFunc) {
	t.Helper()

	tenants := openFn(t)
	id := createWorkspace(t, tenants, workspaceAcme)

	if err := tenants.AddMember(t.Context(), id, subjectAlice, "superuser"); !errors.Is(err, tenancy.ErrRoleUnknown) {
		t.Fatalf("AddMember unknown role error = %v, want ErrRoleUnknown", err)
	}
}

func testAddMemberRejectsMissingWorkspace(t *testing.T, openFn OpenFunc) {
	t.Helper()

	tenants := openFn(t)

	if err := tenants.AddMember(t.Context(), missingID, subjectAlice, tenancy.RoleViewer); !errors.Is(err, tenancy.ErrWorkspaceNotFound) {
		t.Fatalf("AddMember missing workspace error = %v, want ErrWorkspaceNotFound", err)
	}
}

func testGetMemberRoleNotFound(t *testing.T, openFn OpenFunc) {
	t.Helper()

	tenants := openFn(t)
	id := createWorkspace(t, tenants, workspaceAcme)

	if _, err := tenants.GetMemberRole(t.Context(), id, subjectBob); !errors.Is(err, tenancy.ErrMemberNotFound) {
		t.Fatalf("GetMemberRole error = %v, want ErrMemberNotFound", err)
	}
}

func testListMembers(t *testing.T, openFn OpenFunc) {
	t.Helper()

	tenants := openFn(t)
	id := createWorkspace(t, tenants, workspaceAcme)

	if err := tenants.AddMember(t.Context(), id, subjectAlice, tenancy.RoleAdmin); err != nil {
		t.Fatalf("AddMember alice: %v", err)
	}

	if err := tenants.AddMember(t.Context(), id, subjectBob, tenancy.RoleViewer); err != nil {
		t.Fatalf("AddMember bob: %v", err)
	}

	members, err := tenants.ListMembers(t.Context(), id)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}

	if len(members) != wantTwoMembers {
		t.Fatalf("listed %d members, want 2", len(members))
	}
}

// testMembersIsolatedByWorkspace is the tenancy invariant the spec rests on: a
// member of one workspace is not a member of another, so ListMembers never
// leaks across the boundary.
func testMembersIsolatedByWorkspace(t *testing.T, openFn OpenFunc) {
	t.Helper()

	tenants := openFn(t)
	acme := createWorkspace(t, tenants, workspaceAcme)
	globex := createWorkspace(t, tenants, workspaceGlobex)

	if err := tenants.AddMember(t.Context(), acme, subjectAlice, tenancy.RoleEditor); err != nil {
		t.Fatalf("AddMember acme: %v", err)
	}

	globexMembers, err := tenants.ListMembers(t.Context(), globex)
	if err != nil {
		t.Fatalf("ListMembers globex: %v", err)
	}

	if len(globexMembers) != 0 {
		t.Fatalf("globex has %d members, want 0; membership leaked across workspaces", len(globexMembers))
	}

	if _, err := tenants.GetMemberRole(t.Context(), globex, subjectAlice); !errors.Is(err, tenancy.ErrMemberNotFound) {
		t.Fatalf("alice resolved in globex; membership must isolate by workspace (err=%v)", err)
	}
}
