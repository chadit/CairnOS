package a2a

import "errors"

// Sentinel errors for the A2A adapter, card signing, and the mTLS transport.
var (
	// ErrSigningKeyInvalid reports an ed25519 private key whose public half
	// does not type-assert; a well-formed key always yields an
	// ed25519.PublicKey, so this is a programming error, not a request one.
	ErrSigningKeyInvalid = errors.New("a2a card signing key is not a valid ed25519 private key")

	// ErrSigningKeyFileInvalid reports a card_signing_key_file that does not
	// parse as a PKCS#8 PEM ed25519 private key.
	ErrSigningKeyFileInvalid = errors.New("a2a card signing key file is not a PEM-encoded ed25519 private key")

	// ErrCardUnsigned reports an attempt to marshal a card that was never
	// signed; serving an unsigned card would hand a consumer something it
	// cannot trust.
	ErrCardUnsigned = errors.New("agent card is not signed")

	// ErrCardAlgUnknown reports a card whose signing algorithm is not EdDSA;
	// the adapter signs and verifies only ed25519.
	ErrCardAlgUnknown = errors.New("agent card signing algorithm is not supported")

	// ErrCardMalformed reports a card whose public key or signature is not
	// valid base64.
	ErrCardMalformed = errors.New("agent card signing block is malformed")

	// ErrCardKeyMismatch reports a card whose embedded public key differs
	// from the one the consumer already trusts; the signature may verify
	// against the card's own key, but the card is not from the expected node.
	ErrCardKeyMismatch = errors.New("agent card public key does not match the expected key")

	// ErrCardSignatureBad reports a card whose signature does not verify
	// against its public key: the card was tampered with in transit.
	ErrCardSignatureBad = errors.New("agent card signature does not verify")

	// ErrTLSConfigIncomplete reports an [a2a] mTLS block that sets some but
	// not all of cert_file, key_file, and client_ca_file; mTLS needs the full
	// trio or none of it.
	ErrTLSConfigIncomplete = errors.New("a2a mTLS config needs cert_file, key_file, and client_ca_file together")

	// ErrClientCAUnreadable reports a client CA bundle that does not parse as
	// PEM certificates.
	ErrClientCAUnreadable = errors.New("a2a client CA file is not a readable PEM certificate bundle")
)
