package cli_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"

	"github.com/chadit/CairnOS/internal/nettest"
)

// serveFixtureAddr is the --addr every in-memory fixture reports. Nothing
// listens there: a fixture's dial answers any address, so this only decides
// what the command under test sees, a loopback host.
const serveFixtureAddr = "127.0.0.1:7777"

// serveFixture is a gRPC server on an in-memory pipe. An App reaches it only
// through WithServeDialer(fixture.dial), because a loopback dial in this suite
// can land on a port connection tracking still holds stale state for and come
// back EHOSTUNREACH (see internal/nettest).
type serveFixture struct {
	dial func(ctx context.Context, address string) (net.Conn, error)
	addr string
}

// serveInMemory runs a gRPC server for register over an in-memory pipe until
// the test ends.
func serveInMemory(t *testing.T, register func(*grpc.Server)) *serveFixture {
	t.Helper()

	pipe := nettest.NewPipe()
	nettest.ServeGRPC(t, pipe, register)

	return &serveFixture{dial: pipe.DialAddress, addr: serveFixtureAddr}
}
