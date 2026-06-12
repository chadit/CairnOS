package hive

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// pseudonymBytes is the random width of a node pseudonym. Sixteen bytes (128
// bits) makes a collision between two nodes' pseudonyms vanishingly unlikely
// without carrying any node-identifying structure.
const pseudonymBytes = 16

// NewPseudonym mints a fresh random node pseudonym, the rotating id a node
// presents to the hive (.agents/spec/09-a2a-and-hive.md, "No raw identifiers":
// "Node ids in the Hive are rotating pseudonyms, not stable instance names").
// It is built from crypto/rand alone, so it carries nothing about the host:
// not the hostname, not the instance name, not the MAC. Rotating it (a new
// call per publish window) is what keeps two publishes from linking to one
// node.
func NewPseudonym() (string, error) {
	buf := make([]byte, pseudonymBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint node pseudonym: %w", err)
	}

	return "node-" + hex.EncodeToString(buf), nil
}
