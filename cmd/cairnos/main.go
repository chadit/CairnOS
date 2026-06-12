// Package main is the cairnos entrypoint: parse arguments, dispatch through
// the internal cli package, exit with its code
// (.agents/spec/11-deployment-and-config.md, "CLI surface").
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/chadit/CairnOS/internal/cli"
)

func main() {
	// Interrupt cancels the context so an in-flight workflow finalizes its
	// run record instead of dying mid-write. context.Background is allowed
	// only here, where no caller context exists.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)

	code := cli.New(os.Stdout, os.Stderr).Run(ctx, os.Args[1:])

	// os.Exit skips deferred calls, so release the signal watch explicitly
	// before exiting.
	stop()
	os.Exit(code)
}
