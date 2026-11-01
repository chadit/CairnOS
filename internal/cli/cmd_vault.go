package cli

import (
	"context"
	"flag"
	"strings"

	"github.com/chadit/CairnOS/internal/vault"
)

const vaultUsage = "usage: cairnos vault <put|get|query> [flags]\n"

// vaultListLimit caps query and list output when no --limit is given.
const vaultListLimit = 20

// cmdVault dispatches the vault subcommands.
func (a *App) cmdVault(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", vaultUsage)

		return ExitUsage
	}

	switch args[0] {
	case "put":
		return a.cmdVaultPut(ctx, args[1:])
	case subcmdGet:
		return a.cmdVaultGet(ctx, args[1:])
	case "query":
		return a.cmdVaultQuery(ctx, args[1:])
	default:
		a.printErrf("unknown vault subcommand %q\n", args[0])
		a.printErrf("%s", vaultUsage)

		return ExitUsage
	}
}

func (a *App) cmdVaultPut(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("vault put", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	idFlag := flags.String("id", "", "document id to overwrite; omit to create a new one")
	titleFlag := flags.String("title", "", "document title")
	bodyFlag := flags.String("body", "", "markdown body")
	tagsFlag := flags.String("tags", "", "comma-separated tags")
	linksFlag := flags.String("links", "", "comma-separated outbound document ids")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if *titleFlag == "" {
		a.printErrf("usage: cairnos vault put --title T [--body B] [--tags a,b] [--links id1,id2] [--id ID]\n")

		return ExitUsage
	}

	store, _, code, proceed := openAt(ctx, a, "vault put", *dirFlag, vaultBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(store)

	id, err := store.Put(ctx, &vault.Document{
		ID:    *idFlag,
		Title: *titleFlag,
		Body:  *bodyFlag,
		Tags:  splitTags(*tagsFlag),
		Links: splitTags(*linksFlag),
	})
	if err != nil {
		a.printErrf("vault put: %v\n", err)

		return ExitFailure
	}

	a.printOutf("%s\n", id)

	return ExitOK
}

func (a *App) cmdVaultGet(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("vault get", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	id, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if id == "" {
		a.printErrf("usage: cairnos vault get <id> [--dir D]\n")

		return ExitUsage
	}

	store, _, code, proceed := openAt(ctx, a, "vault get", *dirFlag, vaultBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(store)

	doc, err := store.Get(ctx, id)
	if err != nil {
		a.printErrf("vault get: %v\n", err)

		return ExitFailure
	}

	a.printDocument(&doc)

	return ExitOK
}

func (a *App) cmdVaultQuery(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("vault query", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	tagsFlag := flags.String("tags", "", "comma-separated tags every match must carry")
	projectFlag := flags.String("project", "", "narrow to one project (a project:<name> tag)")
	linkedFromFlag := flags.String("linked-from", "", "traverse links out from this document id")
	hopsFlag := flags.Int("hops", 1, "link traversal depth from --linked-from")
	textFlag := flags.String("text", "", "keyword match over title and body")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	store, _, code, proceed := openAt(ctx, a, "vault query", *dirFlag, vaultBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(store)

	result, err := store.Query(ctx, vault.Query{
		Tags:       splitTags(*tagsFlag),
		Project:    *projectFlag,
		LinkedFrom: *linkedFromFlag,
		Hops:       *hopsFlag,
		Text:       *textFlag,
		Limit:      vaultListLimit,
	})
	if err != nil {
		a.printErrf("vault query: %v\n", err)

		return ExitFailure
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "ID\tTITLE\tTAGS\n")

	for idx := range result.Documents {
		doc := &result.Documents[idx]

		a.tabWritef(tab, "%s\t%s\t%s\n",
			doc.ID, truncate(doc.Title), strings.Join(doc.Tags, ","))
	}

	a.flushTab(tab)

	return ExitOK
}

// printDocument renders one vault document in field: value form.
func (a *App) printDocument(doc *vault.Document) {
	a.printOutf("ID:        %s\n", doc.ID)
	a.printOutf("Title:     %s\n", doc.Title)
	a.printOutf("Tags:      %s\n", strings.Join(doc.Tags, ", "))
	a.printOutf("Links:     %s\n", strings.Join(doc.Links, ", "))
	a.printOutf("Backlinks: %s\n", strings.Join(doc.Backlinks, ", "))
	a.printOutf("Created:   %s\n", formatTableTime(doc.CreatedAt))
	a.printOutf("Updated:   %s\n", formatTableTime(doc.UpdatedAt))
	a.printOutf("Body:      %s\n", doc.Body)
}
