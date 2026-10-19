package cli_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

// serveFixtureBufferBytes sizes one in-memory pipe. A writer blocks until the
// reader drains, so this only bounds how far ahead a sender may run.
const serveFixtureBufferBytes = 256 << 10 // 256 KiB

// serveFixtureAddr is the --addr every in-memory fixture reports. Nothing
// listens there: a fixture's dial answers any address, so this only decides
// what the command under test sees, a loopback host.
const serveFixtureAddr = "127.0.0.1:7777"

// serveFixture is a gRPC server on an in-memory pipe. An App reaches it only
// through WithServeDialer(fixture.dial), because a loopback dial in this suite
// can land on a port connection tracking still holds stale state for and come
// back EHOSTUNREACH (see internal/nettest).
type serveFixture struct {
	listener *bufconn.Listener
	addr     string
}

// serveInMemory runs a gRPC server for register over an in-memory pipe until
// the test ends.
func serveInMemory(t *testing.T, register func(*grpc.Server)) *serveFixture {
	t.Helper()

	listener := bufconn.Listen(serveFixtureBufferBytes)
	serveGRPC(t, listener, register)

	return &serveFixture{listener: listener, addr: serveFixtureAddr}
}

// dial is the App serve dialer that reaches this fixture whatever address the
// command was given.
func (f *serveFixture) dial(ctx context.Context, _ string) (net.Conn, error) {
	conn, err := f.listener.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial in-memory fixture: %w", err)
	}

	return conn, nil
}

// serveGRPC serves register's services on listener until the test ends.
func serveGRPC(t *testing.T, listener net.Listener, register func(*grpc.Server)) {
	t.Helper()

	server := grpc.NewServer()
	register(server)

	served := make(chan error, 1)

	go func() { served <- server.Serve(listener) }()

	t.Cleanup(func() {
		server.Stop()

		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("serve: %v", err)
		}
	})
}
