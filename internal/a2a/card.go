// Package a2a implements the Phase 4b Agent-to-Agent protocol adapter
// (.agents/spec/09-a2a-and-hive.md). It is a thin protocol shim over the
// existing run lifecycle, not a second engine: an A2A "task" is a CairnOS
// "run", so CreateTask starts a run, GetTask/ListTasks read run history,
// CancelTask stops a run, and TaskUpdates streams a run's events. The package
// also serves the signed agent card at /.well-known/agent.json and builds the
// mTLS transport credentials when the operator configures them.
//
// Run/task vocabulary mapping (.agents/spec/09-a2a-and-hive.md, "Core
// methods"): task == run, skill == workflow name, params == workflow inputs,
// task state == run status, task update == run event. The adapter reuses
// internal/api's run manager and workflow resolver, so an A2A task and a
// native API run are the same persisted record.
package a2a

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the A2A protocol identifier the card advertises
// (.agents/spec/09-a2a-and-hive.md, "Agent card").
const ProtocolVersion = "a2a/1"

// SigningAlg names the card signature algorithm. EdDSA (ed25519) is the only
// algorithm: the card carries the same asymmetric public key the cross-org
// tokens verify under, so a consumer holding the public key can trust the
// card without being able to forge one (.agents/spec/08-auth-and-access.md,
// Change 3).
const SigningAlg = "EdDSA"

// AuthTypeJWT is the card's auth type: A2A reuses the bearer JWT exchanged at
// the login endpoint (.agents/spec/09-a2a-and-hive.md, "Agent card").
const AuthTypeJWT = "jwt"

// Skill is one capability the node advertises on its card.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AuthInfo tells a consumer how to authenticate: the scheme and the login
// endpoint where an access key is exchanged for a bearer token.
type AuthInfo struct {
	Type  string `json:"type"`
	Login string `json:"login"`
}

// SigningInfo carries the card's signature and the public key that verifies
// it. PublicKey is base64 (standard, padded) ed25519 public key bytes;
// Signature is base64 of the ed25519 signature over the card's canonical
// bytes. Signature is empty in the bytes that get signed, so the key is bound
// into the signature without the signature signing itself.
type SigningInfo struct {
	Alg       string `json:"alg"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature,omitempty"`
}

// Card is the agent card published at /.well-known/agent.json
// (.agents/spec/09-a2a-and-hive.md, "Agent card"): what the node is, where to
// reach it, what it can do, how to authenticate, and the signature that lets a
// consumer trust the card came from this node untampered.
type Card struct {
	Name     string      `json:"name"`
	Version  string      `json:"version"`
	Protocol string      `json:"protocol"`
	Endpoint string      `json:"endpoint"`
	Skills   []Skill     `json:"skills"`
	Auth     AuthInfo    `json:"auth"`
	Signing  SigningInfo `json:"signing"`
}

// Sign fills the card's signing block: it records the algorithm and public
// key, computes the ed25519 signature over the card's canonical bytes (with
// an empty signature field), and stores the signature. The public key is
// derived from the private key so the card and the signature can never name
// mismatched halves.
func (c *Card) Sign(priv ed25519.PrivateKey) error {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return ErrSigningKeyInvalid
	}

	c.Signing.Alg = SigningAlg
	c.Signing.PublicKey = base64.StdEncoding.EncodeToString(pub)
	c.Signing.Signature = ""

	payload, err := c.canonicalBytes()
	if err != nil {
		return err
	}

	c.Signing.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))

	return nil
}

// Verify checks a card's signature against the public key embedded in its
// signing block, then confirms that key matches the expected one the consumer
// already trusts. Passing the expected key is what makes verification mean
// "this card came from the node I think it did": a forged card carrying an
// attacker's key verifies against itself but fails the expected-key check.
func Verify(card *Card, expected ed25519.PublicKey) error {
	if card.Signing.Alg != SigningAlg {
		return fmt.Errorf("%w: card alg %q, want %s", ErrCardAlgUnknown, card.Signing.Alg, SigningAlg)
	}

	pub, err := base64.StdEncoding.DecodeString(card.Signing.PublicKey)
	if err != nil {
		return fmt.Errorf("%w: decode card public key: %w", ErrCardMalformed, err)
	}

	if len(expected) > 0 && !ed25519.PublicKey(pub).Equal(expected) {
		return ErrCardKeyMismatch
	}

	sig, err := base64.StdEncoding.DecodeString(card.Signing.Signature)
	if err != nil {
		return fmt.Errorf("%w: decode card signature: %w", ErrCardMalformed, err)
	}

	unsigned := *card
	unsigned.Signing.Signature = ""

	payload, err := unsigned.canonicalBytes()
	if err != nil {
		return err
	}

	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		return ErrCardSignatureBad
	}

	return nil
}

// Marshal renders the signed card as the JSON bytes served at the well-known
// path. It refuses an unsigned card so a consumer never receives a card it
// cannot trust.
func (c *Card) Marshal() ([]byte, error) {
	if c.Signing.Signature == "" {
		return nil, ErrCardUnsigned
	}

	out, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal agent card: %w", err)
	}

	return out, nil
}

// canonicalBytes renders the card deterministically for signing and verifying.
// encoding/json sorts struct fields by declaration order and map keys
// lexically, and the card holds no maps, so the same card always produces the
// same bytes: signer and verifier agree without a second canonicalization
// pass.
func (c *Card) canonicalBytes() ([]byte, error) {
	out, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal card for signing: %w", err)
	}

	return out, nil
}
