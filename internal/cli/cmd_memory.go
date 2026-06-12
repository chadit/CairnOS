package cli

import (
	"context"
	"flag"
	"strings"

	"github.com/chadit/CairnOS/internal/memory"
)

const memoryUsage = "usage: cairnos memory <add|get|rm|search> [flags]\n"

// defaultImportance matches the memory schema's importance default.
const defaultImportance = 50

func (a *App) cmdMemory(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", memoryUsage)

		return ExitUsage
	}

	switch args[0] {
	case subcmdAdd:
		return a.cmdMemoryAdd(ctx, args[1:])
	case subcmdGet:
		return a.cmdMemoryGet(ctx, args[1:])
	case "rm":
		return a.cmdMemoryRemove(ctx, args[1:])
	case "search":
		return a.cmdMemorySearch(ctx, args[1:])
	default:
		a.printErrf("unknown memory subcommand %q\n", args[0])
		a.printErrf("%s", memoryUsage)

		return ExitUsage
	}
}

func (a *App) cmdMemoryAdd(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("memory add", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	scopeFlag := flags.String("scope", "", "entry scope, e.g. global or project:name")
	contentFlag := flags.String("content", "", "entry text")
	importanceFlag := flags.Int("importance", defaultImportance, "0..100 ordering weight")
	tagsFlag := flags.String("tags", "", "comma-separated tags")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if *scopeFlag == "" || *contentFlag == "" {
		a.printErrf("usage: cairnos memory add --scope S --content C [--importance N] [--tags a,b]\n")

		return ExitUsage
	}

	mem, code, proceed := a.openMemoryAt(ctx, "memory add", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	entry := &memory.Entry{
		Scope:      *scopeFlag,
		Content:    *contentFlag,
		Importance: *importanceFlag,
		Tags:       splitTags(*tagsFlag),
	}

	id, err := mem.Add(ctx, entry)
	if err != nil {
		a.printErrf("memory add: %v\n", err)

		return ExitFailure
	}

	a.printOutf("%s\n", id)

	return ExitOK
}

func (a *App) cmdMemoryGet(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("memory get", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	id, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if id == "" {
		a.printErrf("usage: cairnos memory get <id> [--dir D]\n")

		return ExitUsage
	}

	mem, code, proceed := a.openMemoryAt(ctx, "memory get", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	entry, err := mem.Get(ctx, id)
	if err != nil {
		a.printErrf("memory get: %v\n", err)

		return ExitFailure
	}

	a.printEntry(&entry)

	return ExitOK
}

func (a *App) cmdMemoryRemove(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("memory rm", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	id, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if id == "" {
		a.printErrf("usage: cairnos memory rm <id> [--dir D]\n")

		return ExitUsage
	}

	mem, code, proceed := a.openMemoryAt(ctx, "memory rm", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	if err := mem.Delete(ctx, id); err != nil {
		a.printErrf("memory rm: %v\n", err)

		return ExitFailure
	}

	a.printOutf("deleted %s\n", id)

	return ExitOK
}

func (a *App) cmdMemorySearch(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("memory search", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	textFlag := flags.String("text", "", "text to search for")
	scopeFlag := flags.String("scope", "", "restrict results to one scope")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if *textFlag == "" {
		a.printErrf("usage: cairnos memory search --text T [--scope S] [--dir D]\n")

		return ExitUsage
	}

	mem, code, proceed := a.openMemoryAt(ctx, "memory search", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	result, err := mem.Search(ctx, memory.Query{Scope: *scopeFlag, Text: *textFlag, Limit: defaultListLimit})
	if err != nil {
		a.printErrf("memory search: %v\n", err)

		return ExitFailure
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "ID\tSCOPE\tIMPORTANCE\tCONTENT\n")

	for idx := range result.Entries {
		entry := &result.Entries[idx]

		a.tabWritef(tab, "%s\t%s\t%d\t%s\n",
			entry.ID, entry.Scope, entry.Importance, truncate(entry.Content))
	}

	a.flushTab(tab)

	return ExitOK
}

// printEntry renders one memory entry in field: value form.
func (a *App) printEntry(entry *memory.Entry) {
	a.printOutf("ID:         %s\n", entry.ID)
	a.printOutf("Scope:      %s\n", entry.Scope)
	a.printOutf("Importance: %d\n", entry.Importance)
	a.printOutf("Tags:       %s\n", strings.Join(entry.Tags, ", "))
	a.printOutf("Created:    %s\n", formatTableTime(entry.CreatedAt))
	a.printOutf("Updated:    %s\n", formatTableTime(entry.UpdatedAt))
	a.printOutf("Content:    %s\n", entry.Content)
}

// splitTags turns a comma-separated flag into clean tag values.
func splitTags(raw string) []string {
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	tags := make([]string, 0, len(parts))

	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			tags = append(tags, trimmed)
		}
	}

	return tags
}
