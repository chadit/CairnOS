package a2a_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/a2a"
)

// nodeName and nodeVersion are shared literals the card tests build cards from.
const (
	nodeName    = "cairnos-test-node"
	nodeVersion = "1.2.3"
)

// newSignedCard mints a key pair, builds a card, and signs it, returning the
// card and the public half a verifier would hold.
func newSignedCard(t *testing.T) (*a2a.Card, ed25519.PublicKey) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	card := &a2a.Card{
		Name:     nodeName,
		Version:  nodeVersion,
		Protocol: a2a.ProtocolVersion,
		Endpoint: "grpc://localhost:7777",
		Skills:   []a2a.Skill{{Name: "run_workflow", Description: "Run a named workflow."}},
		Auth:     a2a.AuthInfo{Type: a2a.AuthTypeJWT, Login: "http://localhost:8080/v1/login"},
	}

	if err := card.Sign(priv); err != nil {
		t.Fatalf("sign card: %v", err)
	}

	return card, pub
}

func TestCardSignVerifyRoundTrip(t *testing.T) {
	t.Parallel()

	card, pub := newSignedCard(t)

	if err := a2a.Verify(card, pub); err != nil {
		t.Fatalf("Verify a freshly signed card: %v", err)
	}

	if card.Signing.Alg != a2a.SigningAlg {
		t.Fatalf("signing alg = %q, want %q", card.Signing.Alg, a2a.SigningAlg)
	}

	if card.Signing.Signature == "" {
		t.Fatal("signed card carried no signature")
	}
}

func TestCardVerifyWithoutExpectedKey(t *testing.T) {
	t.Parallel()

	card, _ := newSignedCard(t)

	// A nil expected key skips the pin check and verifies the signature
	// against the card's own embedded key: useful for a first fetch before a
	// consumer has pinned anything.
	if err := a2a.Verify(card, nil); err != nil {
		t.Fatalf("Verify with no pinned key: %v", err)
	}
}

func TestCardTamperDetected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(card *a2a.Card)
	}{
		{
			name:   "endpoint changed",
			mutate: func(card *a2a.Card) { card.Endpoint = "grpc://evil:9999" },
		},
		{
			name:   "name changed",
			mutate: func(card *a2a.Card) { card.Name = "imposter" },
		},
		{
			name:   "skill appended",
			mutate: func(card *a2a.Card) { card.Skills = append(card.Skills, a2a.Skill{Name: "exfiltrate"}) },
		},
		{
			name:   "login changed",
			mutate: func(card *a2a.Card) { card.Auth.Login = "http://evil/login" },
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			card, pub := newSignedCard(t)
			tt.mutate(card)

			err := a2a.Verify(card, pub)
			if !errors.Is(err, a2a.ErrCardSignatureBad) {
				t.Fatalf("Verify a tampered card = %v, want ErrCardSignatureBad", err)
			}
		})
	}
}

func TestCardForgedKeyRejected(t *testing.T) {
	t.Parallel()

	// An attacker re-signs a doctored card with their own key, then swaps the
	// embedded public key to match. The signature verifies against that key,
	// so only the pin check against the trusted key catches the forgery.
	card, trusted := newSignedCard(t)

	attackerPub, attackerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate attacker key: %v", err)
	}

	card.Endpoint = "grpc://evil:9999"

	if err := card.Sign(attackerPriv); err != nil {
		t.Fatalf("attacker re-sign: %v", err)
	}

	// Sanity: the forged card verifies against its own (attacker) key.
	if err := a2a.Verify(card, attackerPub); err != nil {
		t.Fatalf("forged card should verify against attacker key: %v", err)
	}

	// Against the trusted key, the pin check rejects it.
	if err := a2a.Verify(card, trusted); !errors.Is(err, a2a.ErrCardKeyMismatch) {
		t.Fatalf("Verify forged card against trusted key = %v, want ErrCardKeyMismatch", err)
	}
}

func TestCardMarshalRefusesUnsigned(t *testing.T) {
	t.Parallel()

	card := &a2a.Card{Name: nodeName, Protocol: a2a.ProtocolVersion}

	if _, err := card.Marshal(); !errors.Is(err, a2a.ErrCardUnsigned) {
		t.Fatalf("Marshal unsigned card = %v, want ErrCardUnsigned", err)
	}
}

func TestCardMarshalRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()

	card, pub := newSignedCard(t)

	raw, err := card.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded a2a.Card
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal card JSON: %v", err)
	}

	if err := a2a.Verify(&decoded, pub); err != nil {
		t.Fatalf("Verify a card that round-tripped through JSON: %v", err)
	}

	if decoded.Protocol != a2a.ProtocolVersion {
		t.Fatalf("decoded protocol = %q, want %q", decoded.Protocol, a2a.ProtocolVersion)
	}
}
