package hive

import (
	"errors"
	"fmt"
)

// Sentinel errors for the hive client and privacy filter.
var (
	// ErrInvalidCategory reports a category outside the fixed shareable set,
	// either in config or on a received tip.
	ErrInvalidCategory = errors.New("category is not in the fixed shareable set")

	// ErrPublishDisabled reports a Publish call while publish_enabled is false.
	// Publish stays off by default and gated on the privacy review (R2) and the
	// hosting decision (R3); subscribe is always available.
	ErrPublishDisabled = errors.New("hive publish is disabled (publish_enabled = false)")

	// ErrBelowMinObservations reports a Publish refused because the node has
	// seen the pattern fewer than the local min_observations floor. The client
	// refuses below its own floor before the server's k-anonymity floor even
	// sees the tip.
	ErrBelowMinObservations = errors.New("hive publish refused: pattern seen fewer than min_observations times")

	// ErrUnshareable reports a suggestion the allowlist transform cannot reduce
	// to a content-free tip (e.g. prompt_refactor, or a missing runner type).
	ErrUnshareable = errors.New("suggestion cannot be reduced to a content-free tip")

	// ErrHiveDisabled reports a client built while [hive] enabled is false.
	ErrHiveDisabled = errors.New("hive is disabled (hive.enabled = false)")

	// ErrNoEndpoint reports a client built without a hive endpoint.
	ErrNoEndpoint = errors.New("hive endpoint is not configured")

	// ErrHiveRequest reports a non-2xx response from the hive API.
	ErrHiveRequest = errors.New("hive request failed")

	// errNilIDMinter reports an Ingester built without an id minter, a
	// programming error caught at construction before it produces empty ids.
	errNilIDMinter = errors.New("ingester requires a non-nil id minter")
)

// errInvalidCategory wraps ErrInvalidCategory with the offending tag.
func errInvalidCategory(tag Category) error {
	return fmt.Errorf("%w: %q", ErrInvalidCategory, tag)
}
