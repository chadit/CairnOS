package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// StopContext returns a context that SIGINT, a service manager's SIGTERM, or a
// terminal's SIGHUP cancels, so a workflow finalizes its run record and serve
// kills each CLI runner's process group. It is exported so the signal tests
// trap exactly what main traps.
func StopContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	// A command blocked in a stdin read never sees ctx, so the first signal
	// releases the trap and a second one ends the process the default way.
	context.AfterFunc(ctx, stop)

	return ctx, stop
}
