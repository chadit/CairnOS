package plugin

import "errors"

// Construction-time sentinels. Classified runner.Error values cover
// execution failures; these cover config decoding and the startup handshake,
// which happen before any task exists.
var (
	// ErrEmptyCommand reports a plugin config without a command argv.
	ErrEmptyCommand = errors.New("plugin command is empty")

	// ErrConfigValue reports a plugin config key holding the wrong type.
	ErrConfigValue = errors.New("invalid plugin config value")

	// ErrUnnamedPlugin reports a Describe handshake that produced no runner
	// name: the plugin reported none and the config supplied no override.
	ErrUnnamedPlugin = errors.New("plugin reported no name")

	// ErrProcessExited is the cause wrapped into a plugin_down runner.Error
	// when the child process exits before a task's terminal event arrives.
	// Distinct from a transport error: the exit watcher saw the process die,
	// so the run loop never had to infer death from a broken stream.
	ErrProcessExited = errors.New("plugin process exited before result")
)

// errProcessID reports a child whose process tree cannot be named: a Unix stop
// refuses an unrecorded group, because group 0 is the host's own, and a
// Windows process id is 32 bits.
var errProcessID = errors.New("plugin process id is out of range")
