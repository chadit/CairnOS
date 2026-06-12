// Package plugin is the SDK an out-of-process runner imports. A plugin
// author implements pkg/runner.Runner (plus any of Streamer and Describer)
// and hands it to Serve; everything else, the socket, the gRPC service, the
// proto translation, and the SIGTERM handling, lives here
// (.agents/spec/03-runner-interface.md, "Custom runners and plugins").
package plugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"google.golang.org/grpc"

	pluginv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/plugin/v1"
	"github.com/chadit/CairnOS/pkg/runner"
)

// SocketEnv names the environment variable through which the host passes the
// unix socket path. The host side keeps its own copy of this contract in
// internal/runner/plugin.
const SocketEnv = "CAIRNOS_PLUGIN_SOCKET"

// SocketFlag is the command-line fallback for the socket path, useful when
// launching a plugin by hand for debugging: `my-plugin --socket /tmp/p.sock`.
const SocketFlag = "--socket"

// Serve runs impl as a CairnOS runner plugin: it reads the socket path the
// host supplied (SocketEnv first, then SocketFlag), serves the RunnerPlugin
// gRPC service over that unix socket, and returns once a SIGTERM or SIGINT
// arrives and in-flight calls drain. A plugin main is just
// `plugin.Serve(myRunner{})` plus error reporting.
func Serve(impl runner.Runner) error {
	if impl == nil {
		return ErrNilRunner
	}

	path, err := socketPath(os.Args[1:], os.Getenv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	var listenCfg net.ListenConfig

	listener, err := listenCfg.Listen(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("listen on plugin socket %q: %w", path, err)
	}

	server := grpc.NewServer()
	pluginv1.RegisterRunnerPluginServer(server, &service{impl: impl})

	go func() {
		<-ctx.Done()
		// GracefulStop lets an in-flight Execute finish; a plugin that hangs
		// here is covered by the host's SIGKILL escalation, not by us.
		server.GracefulStop()
	}()

	if err := server.Serve(listener); err != nil {
		return fmt.Errorf("serve plugin: %w", err)
	}

	return nil
}

// socketPath resolves the socket location: the environment variable wins,
// then a --socket flag in either `--socket path` or `--socket=path` form.
func socketPath(args []string, getenv func(string) string) (string, error) {
	if path := getenv(SocketEnv); path != "" {
		return path, nil
	}

	for idx, arg := range args {
		if arg == SocketFlag && idx+1 < len(args) {
			return args[idx+1], nil
		}

		if path, found := strings.CutPrefix(arg, SocketFlag+"="); found && path != "" {
			return path, nil
		}
	}

	return "", ErrNoSocket
}
