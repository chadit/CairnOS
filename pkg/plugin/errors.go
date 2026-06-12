package plugin

import "errors"

// Serve-time sentinels.
var (
	// ErrNoSocket reports a plugin started without a socket path: neither the
	// CAIRNOS_PLUGIN_SOCKET environment variable nor the --socket flag was
	// supplied, which usually means the binary ran outside a CairnOS host.
	ErrNoSocket = errors.New("plugin socket path not set: expected CAIRNOS_PLUGIN_SOCKET or --socket")

	// ErrNilRunner reports Serve called with a nil implementation.
	ErrNilRunner = errors.New("plugin runner is nil")
)
