// Package main is the cairnos entrypoint: parse arguments, dispatch through
// the internal cli package, exit with its code
// (.agents/spec/11-deployment-and-config.md, "CLI surface").
package main

import (
	"context"
	"os"

	"github.com/chadit/CairnOS/internal/cli"
)

func main() {
	// context.Background is allowed only here, where no caller context exists.
	ctx, stop := cli.StopContext(context.Background())

	code := cli.New(os.Stdout, os.Stderr).Run(ctx, os.Args[1:])

	// os.Exit skips deferred calls, so release the signal watch explicitly
	// before exiting.
	stop()
	os.Exit(code)
}
