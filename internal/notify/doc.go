// Package notify posts operator alerts for events that need a human, starting
// with the re-authentication hold: when a scheduled job parks because a CLI
// tool's credential expired, the worker calls a notifier so an operator learns
// which tool to sign back in. The webhook URL is a credential, so it is
// sourced from the environment, never a config file.
//
// Fanout is the shared producer surface: engine, queue, and budget producers
// emit one rendered line per event and the fan-out delivers it to every sink
// that asked for that event. Sinks are a list rather than one poster so an
// operator can run the Discord webhook and a Mattermost channel at once, each
// subscribed to its own slice of the four event names, without producers
// holding two handles.
package notify
