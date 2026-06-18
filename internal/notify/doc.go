// Package notify posts operator alerts for events that need a human, starting
// with the re-authentication hold: when a scheduled job parks because a CLI
// tool's credential expired, the worker calls a notifier so an operator learns
// which tool to sign back in (.agents/spec/14-open-decisions.md, D12). The
// webhook URL is a credential, so it is sourced from the environment, never a
// config file.
package notify
