package builtins

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// cfgKeyAllowPrivateTargets is the per-runner opt-out for the egress IP guard.
// Default false: the http, webhook, and github runners refuse loopback,
// link-local, and private destinations so a tenant-supplied target cannot reach
// cloud metadata (169.254.169.254) or internal services. An operator that
// legitimately calls internal endpoints sets it true, which keeps the scheme
// guard but drops the IP guard.
const cfgKeyAllowPrivateTargets = "allow_private_targets"

// codeSuffixBlockedTarget names the permanent error a runner returns when the
// egress guard refuses a destination. It is permanent because the same target
// resolves to the same blocked address on every retry.
const codeSuffixBlockedTarget = ".blocked_target"

// The only URL schemes the egress guard lets a target or a redirect hop use.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// safeHTTPClient builds an http.Client that refuses connections to loopback,
// link-local, private, unspecified, and multicast addresses. The check sits in
// the dialer's Control callback, after DNS, so a rebind to an internal IP is
// still blocked. allowPrivate drops the IP guard for operators who call
// internal endpoints deliberately, and keeps the redirect scheme guard
// (.agents/spec/08-auth-and-access.md, "Runner egress guard").
func safeHTTPClient(name string, timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{}

	if !allowPrivate {
		dialer.Control = guardedControl
	}

	return &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: redirectSchemeGuard(name),
	}
}

// credentialsClientName labels the egress guard's redirect check for the
// credentials token client, so a refused hop reads the same as the runners'.
const credentialsClientName = "credentials"

// GuardedHTTPClient exposes the shared egress guard to callers outside the
// runner factories, namely the credentials token client that POSTs client
// secrets to OAuth2 token endpoints. It is the same dial-time IP guard and
// redirect scheme guard as safeHTTPClient. timeout bounds one token mint or
// refresh, since x/oauth2 TokenSources take no per-call context.
func GuardedHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	return safeHTTPClient(credentialsClientName, timeout, allowPrivate)
}

// guardedControl is the net.Dialer Control callback that rejects a blocked
// address. address is the resolved "host:port" the connection will use, so
// parsing the host yields the IP the socket connects to, not a name that could
// resolve elsewhere later. A blocked IP fails the dial, surfacing to the runner
// as a transport error the caller maps to a blocked_target.
func guardedControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("egress guard: parse dial address %q: %w", address, err)
	}

	resolved := net.ParseIP(host)
	if resolved == nil {
		return fmt.Errorf("%w: dial address %q is not an IP", errBlockedTarget, host)
	}

	if isBlockedIP(resolved) {
		return fmt.Errorf("%w: %s", errBlockedTarget, resolved)
	}

	return nil
}

// isBlockedIP reports whether ip is one a runner must not reach: loopback
// (127/8, ::1), link-local (169.254.0.0/16, fe80::/10, which is where cloud
// metadata lives), private (10/8, 172.16/12, 192.168/16, fc00::/7),
// unspecified (0.0.0.0, ::), and multicast. The stdlib classifiers cover every
// case except the IPv4-mapped form, which Unmap normalizes first so a
// ::ffff:127.0.0.1 cannot slip past the v4 checks.
func isBlockedIP(addr net.IP) bool {
	addr = ipUnmap(addr)

	return addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsPrivate() ||
		addr.IsUnspecified() ||
		addr.IsMulticast()
}

// isLocalNetworkIP reports whether addr sits on a network only this machine or
// its operators reach: loopback, RFC 1918 space, and the 100.64.0.0/10
// carrier-grade NAT block a tailnet hands its nodes.
//
// It is not isBlockedIP inverted: that predicate answers a security question,
// this one decides whether a hosted provider's credential is required.
func isLocalNetworkIP(addr net.IP) bool {
	addr = ipUnmap(addr)

	return addr.IsLoopback() || addr.IsPrivate() || isCarrierGradeNAT(addr)
}

// isCarrierGradeNAT reports whether addr falls in 100.64.0.0/10, the range
// RFC 6598 reserves for carrier-grade NAT and Tailscale assigns to tailnet
// nodes.
func isCarrierGradeNAT(addr net.IP) bool {
	parsed, ok := netip.AddrFromSlice(ipUnmap(addr))
	if !ok {
		return false
	}

	return netip.MustParsePrefix("100.64.0.0/10").Contains(parsed.Unmap())
}

// ipUnmap returns the 4-byte form of an IPv4-mapped IPv6 address so the v4
// classifiers (private, loopback) apply, and the address unchanged otherwise.
func ipUnmap(addr net.IP) net.IP {
	if v4 := addr.To4(); v4 != nil {
		return v4
	}

	return addr
}

// redirectSchemeGuard re-validates every redirect hop's scheme. The dialer's
// Control already covers each hop's IP, so only the scheme, which a dial never
// sees, needs checking here. A refused hop wraps errBlockedTarget so the
// classifier reports a blocked_target; errors.Is sees through the *url.Error
// http.Client puts around it. http.ErrUseLastResponse would hand back the 3xx
// silently, so it is never returned.
func redirectSchemeGuard(_ string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, _ []*http.Request) error {
		switch req.URL.Scheme {
		case schemeHTTP, schemeHTTPS:
			return nil
		default:
			return fmt.Errorf("%w: redirect to scheme %q", errBlockedTarget, req.URL.Scheme)
		}
	}
}

// validateScheme enforces the http/https allowlist on a target URL, rejecting
// file://, gopher://, and the like before a request is built or a redirect is
// followed. It returns a permanent blocked_target error so the engine does not
// retry a scheme that will never be allowed.
func validateScheme(name string, target *url.URL) error {
	switch target.Scheme {
	case schemeHTTP, schemeHTTPS:
		return nil
	default:
		return taskError(runner.ClassPermanent, name+codeSuffixBlockedTarget,
			"scheme %q is not allowed; only http and https targets are permitted", target.Scheme)
	}
}

// guardTarget runs the request-build-time half of the egress guard on a raw
// target string: the IP guard lives in the dialer, so this covers only the URL
// parse and the scheme allowlist. A runner calls it before building a request
// so an unparseable or wrong-scheme target fails as a permanent blocked_target
// rather than reaching the transport.
func guardTarget(name, target string) error {
	parsed, err := url.Parse(target)
	if err != nil {
		return runner.Wrapf(err, runner.ClassPermanent, name+codeSuffixBlockedTarget,
			"parse target %q: %v", target, err)
	}

	return validateScheme(name, parsed)
}

// classifyBlockedDial maps a transport error to a blocked_target when the
// dialer's Control refused the destination, and returns nil otherwise so the
// caller falls back to its normal transport classification. The guard wraps
// errBlockedTarget, which survives the net package's error chain, so errors.Is
// finds it under the dial error the client surfaces.
func classifyBlockedDial(name string, err error) error {
	if !errors.Is(err, errBlockedTarget) {
		return nil
	}

	return taskError(runner.ClassPermanent, name+codeSuffixBlockedTarget,
		"target resolves to a blocked address (loopback, link-local, or private); set allow_private_targets = true on the runner to reach internal endpoints")
}
