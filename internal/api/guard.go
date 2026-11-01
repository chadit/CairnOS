package api

import (
	"fmt"
	"net"
	"strings"
)

// Network names normalizeBind can answer with.
const (
	networkTCP  = "tcp"
	networkUnix = "unix"
)

// unixPrefix marks a bind address as a unix socket path.
const unixPrefix = "unix:"

// localhostName is the one hostname accepted without resolution; anything
// else could resolve off-box, so it only passes when the policy allows
// network serving anyway.
const localhostName = "localhost"

// openServeWarning is the loud line logged when the operator combines
// auth.enabled=false with serve_without_auth_on_network=true: the bind is
// honored, but nothing on this listener checks any credential.
const openServeWarning = "SERVING WITHOUT AUTHENTICATION ON A NETWORK INTERFACE: " +
	"auth.enabled=false with auth.serve_without_auth_on_network=true means every " +
	"workflow, memory entry, and admin operation is open to anyone who can reach this address"

// BindPolicy is the Phase 4a network-bind decision input: whether auth guards
// the listeners, whether a TLS-terminating proxy fronts them, and whether the
// operator explicitly accepted serving an open API.
type BindPolicy struct {
	// AuthEnabled says the auth interceptor guards every method.
	AuthEnabled bool

	// TLSTerminated asserts a TLS-terminating proxy fronts non-loopback
	// traffic. /login carries the long-lived access key, so with auth on,
	// a plaintext non-loopback bind is refused without this claim; CairnOS
	// itself serves plaintext and delegates TLS to the proxy.
	TLSTerminated bool

	// ServeWithoutAuthOnNetwork is the spec's second explicit flag: with
	// auth off, a non-loopback bind is refused unless this is also set.
	ServeWithoutAuthOnNetwork bool
}

// normalizeBind applies the bind-address network guard, returning network,
// address, and a warning when an allowed bind is unprotected. Loopback,
// localhost, and unix sockets pass; an empty host rewrites to 127.0.0.1.
// Otherwise:
//
//	auth on,  tls_terminated -> allowed
//	auth on,  no TLS claim   -> refused (plaintext /login)
//	auth off, serve_without_auth_on_network -> allowed, loud warning
//	auth off, otherwise      -> refused
func normalizeBind(addr string, policy BindPolicy) (string, string, string, error) {
	if path, isUnix := strings.CutPrefix(addr, unixPrefix); isUnix {
		return networkUnix, path, "", nil
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", "", fmt.Errorf("parse bind address %q: %w", addr, err)
	}

	if host == "" {
		return networkTCP, net.JoinHostPort("127.0.0.1", port), "", nil
	}

	if host == localhostName {
		return networkTCP, addr, "", nil
	}

	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return networkTCP, addr, "", nil
	}

	warn, err := networkBindAllowed(addr, policy)
	if err != nil {
		return "", "", "", err
	}

	return networkTCP, addr, warn, nil
}

// networkBindAllowed runs the policy matrix for a non-loopback address and
// returns the warning to log when the bind is allowed without protection.
func networkBindAllowed(addr string, policy BindPolicy) (string, error) {
	if policy.AuthEnabled {
		if policy.TLSTerminated {
			return "", nil
		}

		return "", fmt.Errorf("%w: %q", ErrPlaintextNetworkBind, addr)
	}

	if policy.ServeWithoutAuthOnNetwork {
		return openServeWarning, nil
	}

	return "", fmt.Errorf("%w: %q", ErrNetworkBind, addr)
}
