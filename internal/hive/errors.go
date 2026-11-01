package hive

import "errors"

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

	// ErrKeyWrongSize reports a shared hive key that is not the 32 bytes
	// AES-256 needs. The key is the private cluster's only privacy mechanism
	// (R2/R3), so a wrong-sized key is a hard failure, never a silent
	// downgrade.
	ErrKeyWrongSize = errors.New("hive key must be 32 bytes (AES-256)")

	// ErrBadKeyEncoding reports a shared hive key secret that is not valid
	// standard base64. The operator distributes 32 random bytes base64-encoded
	// through SecretSource; a malformed value fails at construction.
	ErrBadKeyEncoding = errors.New("hive key must be base64-encoded 32 bytes")

	// ErrOpenSealed reports a received envelope that did not decrypt and
	// authenticate under the shared key: a wrong key, a truncated nonce, or a
	// tampered ciphertext. The tip is dropped, never queued from ciphertext.
	ErrOpenSealed = errors.New("hive sealed tip did not open under the shared key")
)
