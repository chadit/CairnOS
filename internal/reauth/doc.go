// Package reauth probes a CLI tool's stored OAuth credential to learn whether it
// is currently valid and which account it belongs to, without making a network
// call (.agents/spec/14-open-decisions.md, D12). The scheduler uses it to resume
// a job held on an expired credential once an operator has signed back in, and
// the notifier uses it to name the affected account.
//
// The credential file formats are undocumented and version-sensitive, so every
// reader is defensive: a missing file, a missing field, or a malformed token
// yields a zero value rather than a crash, and a credential whose expiry cannot
// be read is reported as not valid. The safe failure mode is a job that stays
// held (an operator resumes it by hand) rather than one resumed on a credential
// that was never confirmed fresh. The package never logs or returns token
// material, only the expiry time and the account identity.
package reauth
