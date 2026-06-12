package cli

import (
	"context"
	"flag"

	"github.com/chadit/CairnOS/internal/tenancy"
)

const workspaceUsage = `usage: cairnos workspace <create|list|add-member|members> [flags]

  create      <name>                       Create a workspace
  list                                      List workspaces
  add-member  <workspace> <subject> <role>  Add or update a member (role: admin|editor|viewer)
  members     <workspace>                   List a workspace's members
`

// cmdWorkspace dispatches the Phase 5b workspace-management subcommands
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func (a *App) cmdWorkspace(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", workspaceUsage)

		return ExitUsage
	}

	switch args[0] {
	case "create":
		return a.cmdWorkspaceCreate(ctx, args[1:])
	case subcmdList:
		return a.cmdWorkspaceList(ctx, args[1:])
	case "add-member":
		return a.cmdWorkspaceAddMember(ctx, args[1:])
	case "members":
		return a.cmdWorkspaceMembers(ctx, args[1:])
	default:
		a.printErrf("unknown workspace subcommand %q\n", args[0])
		a.printErrf("%s", workspaceUsage)

		return ExitUsage
	}
}

// cmdWorkspaceCreate creates one workspace; the name must be unique.
func (a *App) cmdWorkspaceCreate(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("workspace create", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	name, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if name == "" {
		a.printErrf("usage: cairnos workspace create <name> [--dir D]\n")

		return ExitUsage
	}

	tenants, code, proceed := a.openTenancyAt(ctx, "workspace create", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(tenants)

	workspace := &tenancy.Workspace{Name: name}
	if err := tenants.CreateWorkspace(ctx, workspace); err != nil {
		a.printErrf("workspace create: %v\n", err)

		return ExitFailure
	}

	a.printOutf("created workspace %s (%s)\n", workspace.ID, workspace.Name)

	return ExitOK
}

// cmdWorkspaceList prints every workspace, oldest first.
func (a *App) cmdWorkspaceList(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("workspace list", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	tenants, code, proceed := a.openTenancyAt(ctx, "workspace list", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(tenants)

	workspaces, err := tenants.ListWorkspaces(ctx)
	if err != nil {
		a.printErrf("workspace list: %v\n", err)

		return ExitFailure
	}

	if len(workspaces) == 0 {
		a.printOutf("no workspaces\n")

		return ExitOK
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "ID\tNAME\tCREATED\n")

	for _, workspace := range workspaces {
		a.tabWritef(tab, "%s\t%s\t%s\n",
			workspace.ID, workspace.Name, formatTableTime(workspace.CreatedAt))
	}

	a.flushTab(tab)

	return ExitOK
}

// cmdWorkspaceAddMember adds or updates one member's role in a workspace.
func (a *App) cmdWorkspaceAddMember(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("workspace add-member", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	positionals, code, proceed := parseThreePositionals(flags, args)
	if !proceed {
		return code
	}

	if len(positionals) != memberArgCount {
		a.printErrf("usage: cairnos workspace add-member <workspace> <subject> <role> [--dir D]\n")

		return ExitUsage
	}

	workspaceID, subject, role := positionals[0], positionals[1], positionals[2]
	if !tenancy.ValidRole(role) {
		a.printErrf("workspace add-member: unknown role %q (want admin, editor, or viewer)\n", role)

		return ExitUsage
	}

	tenants, code, proceed := a.openTenancyAt(ctx, "workspace add-member", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(tenants)

	if err := tenants.AddMember(ctx, workspaceID, subject, role); err != nil {
		a.printErrf("workspace add-member: %v\n", err)

		return ExitFailure
	}

	a.printOutf("added %s to %s as %s\n", subject, workspaceID, role)

	return ExitOK
}

// memberArgCount is the positional-argument count for add-member: workspace,
// subject, and role.
const memberArgCount = 3

// cmdWorkspaceMembers lists one workspace's members.
func (a *App) cmdWorkspaceMembers(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("workspace members", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	workspaceID, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if workspaceID == "" {
		a.printErrf("usage: cairnos workspace members <workspace> [--dir D]\n")

		return ExitUsage
	}

	tenants, code, proceed := a.openTenancyAt(ctx, "workspace members", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(tenants)

	members, err := tenants.ListMembers(ctx, workspaceID)
	if err != nil {
		a.printErrf("workspace members: %v\n", err)

		return ExitFailure
	}

	if len(members) == 0 {
		a.printOutf("no members in %s\n", workspaceID)

		return ExitOK
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "SUBJECT\tROLE\tADDED\n")

	for _, member := range members {
		a.tabWritef(tab, "%s\t%s\t%s\n",
			member.Subject, member.Role, formatTableTime(member.CreatedAt))
	}

	a.flushTab(tab)

	return ExitOK
}
