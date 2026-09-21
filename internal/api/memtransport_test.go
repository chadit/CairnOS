package api_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/chadit/CairnOS/internal/api"
)

// memBufferBytes sizes one in-memory pipe. A writer blocks until the reader
// drains, so this only bounds how far ahead a sender may run.
const memBufferBytes = 256 * 1024

// passthroughScheme sends a gRPC target straight to the dialer instead of
// through a name resolver, which is what an in-memory address needs.
const passthroughScheme = "passthrough:///"

// errNoMemListener marks a dial for an address the transport never handed
// out, which means the fixture is wired wrong.
var errNoMemListener = errors.New("no in-memory listener")

// memTransport serves one fixture's server on in-memory pipes. A suite that
// churns through listeners eventually binds an ephemeral port whose conntrack
// entries have not aged out, and the SYN comes back EHOSTUNREACH on a port
// this process is listening on. The table is nowhere near full then, so
// raising its limit fixes nothing.
type memTransport struct {
	listeners map[string]*bufconn.Listener
	mu        sync.Mutex
	next      atomic.Uint64
}

// newMemTransport builds an empty transport for one fixture.
func newMemTransport() *memTransport {
	return &memTransport{listeners: make(map[string]*bufconn.Listener)}
}

// memAddr is the address a memListener reports. It parses as a URL host so
// the HTTP fixtures can build request URLs from it, and it resolves to
// nothing, so a client without the in-memory dialer fails loudly instead of
// leaving the process.
type memAddr string

// Network names the pipe rather than a real socket family.
func (memAddr) Network() string { return "mem" }

func (a memAddr) String() string { return string(a) }

// memListener gives a bufconn listener the address its transport registered
// it under, which is what Dial later looks up.
type memListener struct {
	*bufconn.Listener

	addr memAddr
}

func (l *memListener) Addr() net.Addr { return l.addr }

// Listen implements api.Transport. The requested network and address only
// decide which bind the guard checked; every listener here is a pipe.
func (m *memTransport) Listen(_ context.Context, _, _ string) (net.Listener, error) {
	addr := memAddr(fmt.Sprintf("mem-%d.invalid:80", m.next.Add(1)))
	lis := bufconn.Listen(memBufferBytes)

	m.mu.Lock()
	m.listeners[string(addr)] = lis
	m.mu.Unlock()

	return &memListener{Listener: lis, addr: addr}, nil
}

// Dial implements api.Transport, reaching the listener registered under the
// address. An unknown address is a fixture wiring mistake, not a condition
// worth retrying.
func (m *memTransport) Dial(ctx context.Context, address string) (net.Conn, error) {
	m.mu.Lock()
	lis, ok := m.listeners[address]
	m.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("%w: %s", errNoMemListener, address)
	}

	conn, err := lis.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial in-memory listener %s: %w", address, err)
	}

	return conn, nil
}

// dialNetwork adapts Dial to the shape http.Transport expects; the network
// argument is ignored because there is only one kind of pipe.
func (m *memTransport) dialNetwork(ctx context.Context, _, address string) (net.Conn, error) {
	return m.Dial(ctx, address)
}

// httpClient builds a client that reaches this transport's HTTP listener.
// Keep-alives are off so a spare dialed connection is closed rather than
// pooled; a pooled one that never sent a request holds Server.Shutdown for
// its grace period, past the fixture's shutdown budget.
func (m *memTransport) httpClient() *http.Client {
	return &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext:       m.dialNetwork,
		},
	}
}

// dialGRPC connects to the server's in-memory gRPC listener by the address
// GRPCAddr reported, closing the connection when the test ends.
func (m *memTransport) dialGRPC(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient(passthroughScheme+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(m.Dial))
	if err != nil {
		t.Fatalf("dial grpc: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// compile-time check that the test transport satisfies the server's contract.
var _ api.Transport = (*memTransport)(nil)
