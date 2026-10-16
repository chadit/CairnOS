package a2a_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/a2a"
)

// nodeName and nodeVersion are shared literals the card tests build cards from.
const (
	nodeName    = "cairnos-test-node"
	nodeVersion = "1.2.3"
)

// Tamper values a forger writes into a signed card; verification must reject
// every one of them.
const (
	evilEndpoint = "grpc://evil:9999"
	imposterName = "imposter"
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
		Channels: fixtureChannels(),
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
		mutate func(card *a2a.Card)
		name   string
	}{
		{
			name:   "endpoint changed",
			mutate: func(card *a2a.Card) { card.Endpoint = evilEndpoint },
		},
		{
			name:   "name changed",
			mutate: func(card *a2a.Card) { card.Name = imposterName },
		},
		{
			name:   "skill appended",
			mutate: func(card *a2a.Card) { card.Skills = append(card.Skills, a2a.Skill{Name: "exfiltrate"}) },
		},
		{
			name:   "login changed",
			mutate: func(card *a2a.Card) { card.Auth.Login = "http://evil/login" },
		},
		{
			name:   "channel url changed",
			mutate: func(card *a2a.Card) { card.Channels[0].URL = "https://evil.example.test" },
		},
		{
			name: "channel appended",
			mutate: func(card *a2a.Card) {
				card.Channels = append(card.Channels, a2a.Channel{Type: channelTypeMattermost, Username: imposterName})
			},
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

	card.Endpoint = evilEndpoint

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

// TestCardChannelsSurviveJSONRoundTrip proves the channel block is carried by
// the served card and is covered by the signature: a consumer decoding the card
// reads the same chat surfaces the node signed.
func TestCardChannelsSurviveJSONRoundTrip(t *testing.T) {
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
		t.Fatalf("Verify a card carrying channels: %v", err)
	}

	want := fixtureChannels()
	if !slices.Equal(decoded.Channels, want) {
		t.Fatalf("decoded channels = %+v, want %+v", decoded.Channels, want)
	}
}

// TestCardWithoutChannelsOmitsTheKey pins the empty case. A nil channel list
// and an empty one must render identically, because the builder produces either
// depending on how identities resolved and that choice must never change the
// bytes Sign covers. Omitting the key also leaves a chat-less node's card
// byte-identical to the one it published before channels existed.
func TestCardWithoutChannelsOmitsTheKey(t *testing.T) {
	t.Parallel()

	nilChannels := &a2a.Card{Name: nodeName, Protocol: a2a.ProtocolVersion}
	emptyChannels := &a2a.Card{Name: nodeName, Protocol: a2a.ProtocolVersion, Channels: []a2a.Channel{}}

	nilJSON, err := json.Marshal(nilChannels)
	if err != nil {
		t.Fatalf("marshal card with nil channels: %v", err)
	}

	emptyJSON, err := json.Marshal(emptyChannels)
	if err != nil {
		t.Fatalf("marshal card with empty channels: %v", err)
	}

	if !bytes.Equal(nilJSON, emptyJSON) {
		t.Fatalf("nil channels rendered %s, empty channels rendered %s; the two must agree", nilJSON, emptyJSON)
	}

	if strings.Contains(string(nilJSON), `"channels"`) {
		t.Fatalf("card with no channels carried the key: %s", nilJSON)
	}
}
