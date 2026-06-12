package cli_test

import (
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// TestWorkspaceCreateAndList creates a workspace and confirms list shows it
// alongside the auto-seeded default workspace.
func TestWorkspaceCreateAndList(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dir := newKeysHome(t)

	code, stdout, stderr := runKeysCLI(ctx, t, "workspace", "create", "acme", "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("workspace create exit = %d, stderr %q", code, stderr)
	}

	if !strings.Contains(stdout, "created workspace") || !strings.Contains(stdout, "acme") {
		t.Fatalf("create output %q missing the workspace", stdout)
	}

	code, listOut, stderr := runKeysCLI(ctx, t, "workspace", "list", "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("workspace list exit = %d, stderr %q", code, stderr)
	}

	if !strings.Contains(listOut, "acme") || !strings.Contains(listOut, "default") {
		t.Fatalf("list output %q missing acme or the default workspace", listOut)
	}
}

// TestWorkspaceCreateRejectsDuplicate proves the unique-name invariant holds
// from the CLI: a second create of the same name fails.
func TestWorkspaceCreateRejectsDuplicate(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dir := newKeysHome(t)

	if code, _, stderr := runKeysCLI(ctx, t, "workspace", "create", "acme", "--dir", dir); code != cli.ExitOK {
		t.Fatalf("first create exit = %d, stderr %q", code, stderr)
	}

	code, _, _ := runKeysCLI(ctx, t, "workspace", "create", "acme", "--dir", dir)
	if code == cli.ExitOK {
		t.Fatal("duplicate workspace create succeeded, want a failure")
	}
}

// TestWorkspaceAddAndListMembers adds a member and confirms members lists it
// with its role.
func TestWorkspaceAddAndListMembers(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dir := newKeysHome(t)

	code, _, stderr := runKeysCLI(ctx, t,
		"workspace", "add-member", "default", "alice@example.com", "editor", "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("add-member exit = %d, stderr %q", code, stderr)
	}

	code, membersOut, stderr := runKeysCLI(ctx, t, "workspace", "members", "default", "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("members exit = %d, stderr %q", code, stderr)
	}

	if !strings.Contains(membersOut, "alice@example.com") || !strings.Contains(membersOut, "editor") {
		t.Fatalf("members output %q missing the added member", membersOut)
	}
}

// TestWorkspaceAddMemberRejectsUnknownRole proves a typo'd role fails at the
// CLI rather than provisioning an empty scope set.
func TestWorkspaceAddMemberRejectsUnknownRole(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dir := newKeysHome(t)

	code, _, stderr := runKeysCLI(ctx, t,
		"workspace", "add-member", "default", "alice@example.com", "superuser", "--dir", dir)
	if code != cli.ExitUsage {
		t.Fatalf("unknown role exit = %d, want usage; stderr %q", code, stderr)
	}
}

// TestKeysAddWorkspaceFlag confirms the keys add --workspace flag is accepted
// and the listing shows the workspace column.
func TestKeysAddWorkspaceFlag(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dir := newKeysHome(t)

	code, stdout, stderr := runKeysCLI(ctx, t,
		"keys", "add", "--label", "scoped", "--scopes", "read:run", "--workspace", "acme", "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("keys add --workspace exit = %d, stderr %q", code, stderr)
	}

	if !strings.Contains(stdout, "created key") {
		t.Fatalf("keys add output %q missing confirmation", stdout)
	}

	code, listOut, stderr := runKeysCLI(ctx, t, "keys", "list", "--dir", dir)
	if code != cli.ExitOK {
		t.Fatalf("keys list exit = %d, stderr %q", code, stderr)
	}

	if !strings.Contains(listOut, "WORKSPACE") || !strings.Contains(listOut, "acme") {
		t.Fatalf("keys list %q missing the workspace column or value", listOut)
	}
}
