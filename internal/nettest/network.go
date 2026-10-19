package nettest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// passthroughScheme sends a gRPC target straight to the dialer instead of
// through a name resolver, which is what an in-memory address needs.
const passthroughScheme = "passthrough:///"

// Network stands in for the host's sockets under a server that binds more
// than one listener, such as api.Server's gRPC and HTTP pair. Listen hands
// out pipes under addresses that resolve to nothing, and Dial reaches a pipe
// by the address its listener reports.
type Network struct {
	listeners map[string]*Pipe
	mu        sync.Mutex
	next      atomic.Uint64
}

// NewNetwork builds an empty network for one fixture.
func NewNetwork() *Network {
	return &Network{listeners: make(map[string]*Pipe)}
}

// networkListener reports the name Dial looks a pipe up by. net.UnixAddr is
// used only because it carries a free-form name. The name parses as a URL host
// for request URLs and resolves to nothing, so a client without this network's
// dialer fails loudly instead of leaving the process.
type networkListener struct {
	*Pipe

	addr *net.UnixAddr
}

func (l *networkListener) Addr() net.Addr { return l.addr }

// Listen hands out a fresh pipe whatever network and address are asked for,
// so a bind guard upstream still sees and checks the configured address. A
// canceled context fails the bind the way a host listen would.
func (n *Network) Listen(ctx context.Context, network, address string) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("listen %s %s: %w", network, address, err)
	}

	name := fmt.Sprintf("mem-%d.invalid:80", n.next.Add(1))
	pipe := NewPipe()

	n.mu.Lock()
	n.listeners[name] = pipe
	n.mu.Unlock()

	return &networkListener{Pipe: pipe, addr: &net.UnixAddr{Name: name, Net: "mem"}}, nil
}

// Dial reaches the listener registered under address. An unknown address is
// a fixture wiring mistake, not a condition worth retrying.
func (n *Network) Dial(ctx context.Context, address string) (net.Conn, error) {
	n.mu.Lock()
	pipe, ok := n.listeners[address]
	n.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("%w: %s", errNoListener, address)
	}

	conn, err := pipe.DialAddress(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", address, err)
	}

	return conn, nil
}

// DialContext is Dial in the shape http.Transport wants. The network argument
// is ignored because there is only one kind of pipe.
func (n *Network) DialContext(ctx context.Context, _, address string) (net.Conn, error) {
	return n.Dial(ctx, address)
}

// Client reaches this network's HTTP listeners. Keep-alives are off so a
// spare dialed connection is closed rather than pooled; a pooled one that
// never sent a request holds Server.Shutdown for its grace period.
func (n *Network) Client() *http.Client {
	return &http.Client{
		Timeout: clientTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext:       n.DialContext,
		},
	}
}

// DialGRPC connects to the gRPC listener registered under addr, closing the
// connection when the test ends.
func (n *Network) DialGRPC(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient(passthroughScheme+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(n.Dial))
	if err != nil {
		t.Fatalf("dial grpc %s: %v", addr, err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}
