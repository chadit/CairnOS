// Package webhook holds the webhook-ingress registration store. One
// registration per workflow opts that workflow into a stable POST URL; the
// store keeps only hex SHA-256 hashes of the two path tokens, so a dumped
// table can never be replayed against the endpoint. The two backends mirror
// the scheduler's dual-backend pattern and are held to one behavioral
// contract by webhooktest.
package webhook

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// tokenBytes is the entropy of a minted path token: 32 crypto/rand bytes,
// hex-encoded to 64 characters. The handler rejects anything but exactly 64
// hex chars before a lookup, so the minted shape and the accepted shape match.
const tokenBytes = 32

// Token-hash column names, shared by both backends' UpdateTokenHash so the
// Endpoint-to-column mapping lives in one place (columnForEndpoint).
const (
	colProdHash = "prod_token_hash" // #nosec G101 -- column name, not a credential
	colTestHash = "test_token_hash" // #nosec G101 -- column name, not a credential
)

// Endpoint distinguishes the two independent tokens a registration mints. One
// hash lookup answers both which registration matched and which semantics the
// hit runs under, so production and test share no capability: rotating one
// leaves the other valid, and a leaked test token never opens production.
type Endpoint int

// The two webhook endpoints. Production runs async and answers 202; test waits
// (bounded) for the terminal run. Independent tokens back each.
const (
	EndpointProd Endpoint = iota
	EndpointTest
)

// Registration is one workflow's webhook opt-in. The store persists only the
// token hashes; the raw tokens exist just once, at mint time, and are never
// recoverable from here.
type Registration struct {
	CreatedAt     time.Time // stamped at Add when the caller leaves it zero
	ID            string    // stable id; the only registration identifier logs may carry
	WorkflowName  string    // unique per store
	WorkflowPath  string    // loaded fresh per hit (FireRunner precedent)
	ProdTokenHash string    // hex SHA-256 of the production path token
	TestTokenHash string    // hex SHA-256 of the test path token
	HMACSecretRef string    // secrets.Source name; empty disables HMAC
	PayloadInput  string    // raw-body mode: deliver the whole body into this declared input
	RatePerMinute int       // 0 means the [webhook] rate_per_minute default
}

// Store persists webhook registrations. Lookup is by token hash, never by the
// token itself, so the database holds no usable capability. Both backends
// satisfy this contract, proven by the webhooktest conformance suite. It has
// no Close: the opener returns its own closer, because the Postgres backend
// borrows a pool it must not close.
type Store interface {
	RegistrationReader
	RegistrationWriter
}

// RegistrationReader finds registrations. The ingress handler needs only
// this half, so a request path can never mint, rotate, or remove a token.
type RegistrationReader interface {
	// GetByTokenHash finds the registration whose production or test hash
	// equals hash and reports which endpoint matched. Unknown hash is
	// ErrNotFound with no state minted.
	GetByTokenHash(ctx context.Context, hash string) (*Registration, Endpoint, error)

	// GetByWorkflow returns the registration for a workflow name, or
	// ErrNotFound.
	GetByWorkflow(ctx context.Context, workflowName string) (*Registration, error)

	// List returns every registration, oldest first.
	List(ctx context.Context) ([]*Registration, error)
}

// RegistrationWriter creates, rotates, and removes registrations, the
// operator-side half the webhook CLI drives.
type RegistrationWriter interface {
	// Add persists a registration, stamping ID and CreatedAt when zero. A
	// second registration for the same workflow is ErrDuplicate.
	Add(ctx context.Context, reg *Registration) error

	// UpdateTokenHash rotates one endpoint's hash for a workflow, so the old
	// token dies at that moment while the sibling endpoint's token survives.
	// An unknown workflow is ErrNotFound.
	UpdateTokenHash(ctx context.Context, workflowName string, endpoint Endpoint, newHash string) error

	// Remove deletes a workflow's registration, or ErrNotFound when none
	// exists.
	Remove(ctx context.Context, workflowName string) error
}

// MintToken returns a fresh path token and its stored hash, in that order. The
// token is 32 crypto/rand bytes hex-encoded (64 chars), shown to the operator
// exactly once; only the hash is ever persisted.
func MintToken() (string, string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("mint webhook token: %w", err)
	}

	token := hex.EncodeToString(raw)

	return token, HashToken(token), nil
}

// HashToken returns the hex SHA-256 of a token string. The handler hashes the
// path token the same way and looks up by the result, so the stored value is
// never the token and a leaked table yields no working capability.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// columnForEndpoint maps an endpoint to its token-hash column so both backends
// share one mapping rather than each re-deriving it. An out-of-range endpoint
// is ErrUnknownEndpoint, which only a corrupt caller can trigger.
func columnForEndpoint(endpoint Endpoint) (string, error) {
	switch endpoint {
	case EndpointProd:
		return colProdHash, nil
	case EndpointTest:
		return colTestHash, nil
	default:
		return "", fmt.Errorf("%w: %d", ErrUnknownEndpoint, endpoint)
	}
}

// Compile-time proof both backends satisfy the store contract, so a signature
// drift surfaces here rather than at the wiring call site.
var (
	_ Store = (*SQLiteWebhooks)(nil)
	_ Store = (*PostgresWebhooks)(nil)
)
