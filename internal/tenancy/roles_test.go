package tenancy_test

import (
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/tenancy"
)

// TestScopesForRole pins the role-to-scope mapping the OAuth login and the CLI
// rely on: admin holds every scope, editor reads and writes the working set
// but never admin, and viewer is read-only. An unknown role yields no scopes.
func TestScopesForRole(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		role     string
		mustHave []string
		mustLack []string
		wantOK   bool
	}{
		{
			name:     "admin holds admin scope",
			role:     tenancy.RoleAdmin,
			wantOK:   true,
			mustHave: []string{auth.ScopeAdmin, auth.ScopeWriteWorkflow, auth.ScopeReadRun},
		},
		{
			name:     "editor writes workflow and memory but not admin",
			role:     tenancy.RoleEditor,
			wantOK:   true,
			mustHave: []string{auth.ScopeWriteWorkflow, auth.ScopeWriteMemory, auth.ScopeReadRun},
			mustLack: []string{auth.ScopeAdmin},
		},
		{
			name:     "viewer is read-only",
			role:     tenancy.RoleViewer,
			wantOK:   true,
			mustHave: []string{auth.ScopeReadRun, auth.ScopeReadWorkflow, auth.ScopeReadMemory},
			mustLack: []string{auth.ScopeAdmin, auth.ScopeWriteWorkflow, auth.ScopeWriteMemory},
		},
		{
			name:   "unknown role yields nothing",
			role:   "superuser",
			wantOK: false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scopes, ok := tenancy.ScopesForRole(tt.role)
			if ok != tt.wantOK {
				t.Fatalf("ScopesForRole(%q) ok = %t, want %t", tt.role, ok, tt.wantOK)
			}

			for _, want := range tt.mustHave {
				if !slices.Contains(scopes, want) {
					t.Errorf("role %q missing scope %q", tt.role, want)
				}
			}

			for _, absent := range tt.mustLack {
				if slices.Contains(scopes, absent) {
					t.Errorf("role %q unexpectedly grants scope %q", tt.role, absent)
				}
			}
		})
	}
}

// TestValidRole reports the defined roles as valid and anything else as
// invalid, so a typo'd role fails at membership creation.
func TestValidRole(t *testing.T) {
	t.Parallel()

	for _, role := range []string{tenancy.RoleAdmin, tenancy.RoleEditor, tenancy.RoleViewer} {
		if !tenancy.ValidRole(role) {
			t.Errorf("ValidRole(%q) = false, want true", role)
		}
	}

	if tenancy.ValidRole("superuser") {
		t.Error("ValidRole(superuser) = true, want false")
	}
}

// TestScopesForRoleReturnsCopy proves the returned slice is independent of the
// shared table, so a caller mutating it cannot corrupt another login's scopes.
func TestScopesForRoleReturnsCopy(t *testing.T) {
	t.Parallel()

	first, _ := tenancy.ScopesForRole(tenancy.RoleAdmin)
	if len(first) == 0 {
		t.Fatal("admin scopes empty")
	}

	first[0] = "mutated"

	second, _ := tenancy.ScopesForRole(tenancy.RoleAdmin)
	if second[0] == "mutated" {
		t.Fatal("ScopesForRole leaked the shared table; a caller mutated it")
	}
}
