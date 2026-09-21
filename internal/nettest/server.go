package nettest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/test/bufconn"
)

// memBufferBytes sizes one in-memory pipe. A writer blocks until the reader
// drains, so this only bounds how far ahead a sender may run.
const memBufferBytes = 256 * 1024

// headerTimeout bounds how long the fixture server waits for request headers.
// Nothing dials it, so this only catches a caller that opens a connection and
// then writes nothing.
const headerTimeout = 30 * time.Second

// Server runs a real http.Server over an in-memory listener. Reach for it when
// a fixture needs the parts a recorder cannot give: a hijacked connection for a
// WebSocket upgrade, chunked transfer, or a body read while the handler is
// still writing it. Plain request and response fixtures want Transport.
type Server struct {
	listener *bufconn.Listener

	// URL is the origin to aim requests at. It resolves to nothing, so a client
	// built without Client or DialContext fails at DNS instead of leaving the
	// process.
	URL string
}

// NewServer starts handler on an in-memory listener and stops it when the test
// ends.
func NewServer(t *testing.T, handler http.Handler) *Server {
	t.Helper()

	listener := bufconn.Listen(memBufferBytes)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: headerTimeout}

	go func() { _ = server.Serve(listener) }()

	t.Cleanup(func() { _ = server.Close() })

	return &Server{listener: listener, URL: BaseURL}
}

// DialContext reaches this server whatever address it is handed, since one
// listener sits behind it. It is the shape http.Transport.DialContext wants.
func (s *Server) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	conn, err := s.listener.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial in-memory listener: %w", err)
	}

	return conn, nil
}

// Transport reaches this server and nothing else. Keep-alives are off so a
// pooled connection never outlives the test that made it.
func (s *Server) Transport() *http.Transport {
	return &http.Transport{DialContext: s.DialContext, DisableKeepAlives: true}
}

// Client is an http.Client bound to this server.
func (s *Server) Client() *http.Client {
	return &http.Client{Transport: s.Transport(), Timeout: clientTimeout}
}
