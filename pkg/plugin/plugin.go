// Package plugin is the SDK an out-of-process runner imports. A plugin
// author implements pkg/runner.Runner (plus any of Streamer and Describer)
// and hands it to Serve; everything else, the socket, the gRPC service, the
// proto translation, and stopping on SIGTERM or a closed stdin pipe, lives
// here.
package plugin

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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

// Serve runs impl as a CairnOS runner plugin over the unix socket the host
// supplied (SocketEnv first, then SocketFlag) until SIGTERM, SIGINT, or the
// end of a pipe on stdin. A pipe on stdin is the host's liveness lease and the
// SDK reads it, so a plugin runner must not pass os.Stdin to its own
// subprocesses.
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

	hostGone := make(chan struct{})
	if hostLease(os.Stdin) {
		go watchHost(os.Stdin, hostGone)
	}

	go stopServer(ctx, hostGone, server)

	if err := server.Serve(listener); err != nil {
		return fmt.Errorf("serve plugin: %w", err)
	}

	return nil
}

// hostLease reports whether stdin is a pipe, the only stdin a CairnOS host
// hands a plugin. A terminal, the null device, a file, or a socket comes from
// some other launcher, and its end says nothing about whether a host is alive.
func hostLease(stdin *os.File) bool {
	info, err := stdin.Stat()
	if err != nil {
		slog.Warn("cannot stat plugin stdin, not watching for host exit", "error", err)

		return false
	}

	return info.Mode()&os.ModeNamedPipe != 0
}

// watchHost reads stdin to its end, then closes hostGone. The host holds the
// write end until it has reaped this process, so EOF or a read error means the
// host died without stopping the plugin. A blocked stdin read cannot be
// interrupted, so after a signal stop this goroutine lasts until exit.
func watchHost(stdin io.Reader, hostGone chan<- struct{}) {
	defer close(hostGone)

	if _, err := io.Copy(io.Discard, stdin); err != nil {
		slog.Warn("plugin stdin read failed, stopping", "error", err)
	}
}

// stopServer ends the server on the first stop. A signal drains in-flight
// calls, because the host sent it and is waiting for their results. A lost
// host stops at once: nobody can receive a result, and a runner that ignores
// its context would otherwise keep the orphaned plugin alive forever.
func stopServer(ctx context.Context, hostGone <-chan struct{}, server *grpc.Server) {
	select {
	case <-ctx.Done():
		server.GracefulStop()
	case <-hostGone:
		server.Stop()
	}
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
