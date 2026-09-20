package cli

import "errors"

// Sentinel errors for the CLI.
var (
	// errValueHasNewline reports a vault value the generated environment file
	// cannot carry: both readers parse line by line, so the remainder would be
	// read as a second assignment.
	errValueHasNewline = errors.New("a vault value contains a newline, which " +
		"neither systemd nor a sourcing shell can read back; fix the field on the item")

	// errNoVaultItem reports that neither the config block nor the credentials
	// file names the vault item to read.
	errNoVaultItem = errors.New("no vault item is configured; " +
		"set [secrets.bitwarden] item, or run `cairnos secrets auth` to write")

	// errNoFieldsOnItem reports an item carrying nothing the pull can use: no
	// field named like a variable, and no mapping table to interpret the rest.
	errNoFieldsOnItem = errors.New("no field on the vault item is named like a " +
		"CAIRNOS_ variable, and [secrets.bitwarden.fields] maps none; item")

	// errNotATerminal reports a stdin that is a pipe or a file, where there is
	// no echo to turn off and nothing on screen to hide.
	errNotATerminal = errors.New("stdin is not a terminal")

	// errNoPinnedBitwarden reports a platform this build has no pinned release
	// for, which is every platform but the two CairnOS runs on.
	errNoPinnedBitwarden = errors.New("no Bitwarden CLI release is pinned for")

	// errNoValueForPrompt reports a prompt that had nothing to answer it: no
	// environment value, no default, and a stdin that is done. Failing here beats
	// blocking a deploy on a terminal that will never arrive.
	errNoValueForPrompt = errors.New("no value was given and there is no default; " +
		"set it in the environment or run this on a terminal")

	// errNoBitwardenBinary reports a missing bw CLI on a platform this build
	// pins no release for, where CairnOS has nothing of its own to fall back to.
	errNoBitwardenBinary = errors.New("the bw CLI is not on PATH; " +
		"install it from your package manager or from the Bitwarden releases page")

	// ErrBadInput reports a malformed or uncoercible --input flag value.
	ErrBadInput = errors.New("bad --input value")

	// ErrScheduledRunFailed reports a scheduled fire whose run finished in a
	// non-succeeded status; the daemon logs it and keeps running.
	ErrScheduledRunFailed = errors.New("scheduled run failed")

	// errOAuthNeedsAuth reports an OAuth login configured with auth disabled: a
	// minted token is only useful if the interceptor verifies it.
	errOAuthNeedsAuth = errors.New("oauth requires auth")

	// errFlagValue reports a flag whose value cannot work. Catching it here beats
	// finding out from a server that started on the wrong port: --port 0 makes
	// llama-server bind an ephemeral port while every probe still targets :0, so
	// an upgrade would roll back a runtime that was running fine.
	errFlagValue = errors.New("model: flag value out of range")
)
