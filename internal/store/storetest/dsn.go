package storetest

import (
	"net"
	"strconv"
	"testing"
)

// RefusedPostgresDSN returns a DSN no listener answers. The port comes from a
// listener this helper opens and closes, so the kernel has just released it
// and a connect gets ECONNREFUSED instead of hanging on a firewalled port.
// Both the run store and the auth store use it to prove Open reports a
// connection failure with no Postgres running.
func RefusedPostgresDSN(t *testing.T) string {
	t.Helper()

	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not TCP", listener.Addr())
	}

	if closeErr := listener.Close(); closeErr != nil {
		t.Fatalf("release the loopback port: %v", closeErr)
	}

	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))

	return "postgres://user:pass@" + host + "/nope?sslmode=disable&connect_timeout=2"
}
