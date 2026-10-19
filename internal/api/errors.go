package api

import "errors"

// Sentinel errors for the API server.
var (
	// ErrNetworkBind reports a non-loopback bind with auth disabled and no
	// explicit acceptance of an open API
	// (.agents/spec/08-auth-and-access.md, serve_without_auth_on_network).
	ErrNetworkBind = errors.New("refusing to bind a non-loopback address with auth disabled: enable [auth] or set auth.serve_without_auth_on_network = true to serve an open API deliberately")

	// ErrPlaintextNetworkBind reports a non-loopback bind with auth on but
	// no TLS in front: /login carries the long-lived access key, and the
	// spec refuses to put it on a network in plaintext
	// (.agents/spec/08-auth-and-access.md, "Model"). A TLS-terminating
	// proxy satisfies the requirement via auth.tls_terminated = true.
	ErrPlaintextNetworkBind = errors.New("refusing a plaintext non-loopback bind: /login carries access keys and requires TLS; front CairnOS with a TLS-terminating proxy and set auth.tls_terminated = true")

	// ErrManagerUnbound reports a StartRun before Bind wired the engine.
	ErrManagerUnbound = errors.New("run manager has no engine bound")

	// ErrManagerShutdown reports a start after Shutdown. Shutdown cancels only
	// the runs it saw, so a later start would drive a loop nothing cancels while
	// the server stops and the store closes.
	ErrManagerShutdown = errors.New("run manager is shut down")

	// ErrRunActive reports a StartResume for a run this manager is already
	// driving, so two loops can never execute one run.
	ErrRunActive = errors.New("run is already active on this server")

	// ErrScheduledRunFailed reports a scheduled fire whose run finished in a
	// non-succeeded status; the daemon logs it and keeps running.
	ErrScheduledRunFailed = errors.New("scheduled run failed")

	// ErrServerNotStarted reports address accessors called before Start.
	ErrServerNotStarted = errors.New("server not started")
)
