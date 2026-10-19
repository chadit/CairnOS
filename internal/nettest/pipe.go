package nettest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

// memBufferBytes sizes one in-memory pipe. A writer blocks until the reader
// drains, so this only bounds how far ahead a sender may run.
const memBufferBytes = 256 << 10 // 256 KiB

// Pipe is an in-memory listener a fixture serves on in place of a loopback
// port, for the connection tracking reason the package doc gives.
type Pipe struct {
	*bufconn.Listener
}

// NewPipe opens an empty pipe. Closing it, directly or by stopping the server
// that accepts on it, is the caller's job.
func NewPipe() *Pipe {
	return &Pipe{Listener: bufconn.Listen(memBufferBytes)}
}

// DialAddress reaches this pipe whatever address it is handed, since one
// listener sits behind it. It is the shape grpc.WithContextDialer wants.
func (p *Pipe) DialAddress(ctx context.Context, _ string) (net.Conn, error) {
	conn, err := p.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial in-memory listener: %w", err)
	}

	return conn, nil
}

// ServeGRPC serves register's services on listener and stops the server when
// the test ends, failing the test if Serve returned for any reason other than
// that stop.
func ServeGRPC(t *testing.T, listener net.Listener, register func(*grpc.Server)) {
	t.Helper()

	server := grpc.NewServer()
	register(server)

	served := make(chan error, 1)

	go func() { served <- server.Serve(listener) }()

	t.Cleanup(func() {
		server.Stop()

		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("serve gRPC fixture: %v", err)
		}
	})
}
