package cli

import (
	"context"
	"flag"

	"github.com/chadit/CairnOS/internal/store"
)

func (a *App) cmdHealth(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("health", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	db, _, code, proceed := openAt(ctx, a, "health", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	if _, err := db.ListRuns(ctx, store.ListFilter{Limit: 1}); err != nil {
		a.printErrf("health: query store: %v\n", err)

		return ExitFailure
	}

	a.printOutf("ok\n")

	return ExitOK
}
