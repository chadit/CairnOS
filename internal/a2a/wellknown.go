package a2a

import (
	"log/slog"
	"net/http"
)

// WellKnownPath is where the agent card is published
// (.agents/spec/09-a2a-and-hive.md, "Agent card"). Discovery is public: the
// handler sits outside the auth interceptor so any consumer can fetch the
// card, and the card's signature is what lets that consumer trust it.
const WellKnownPath = "/.well-known/agent.json"

// cardContentType labels the card response. It is plain JSON, served as the
// signed bytes the card marshaled to.
const cardContentType = "application/json"

// CardHandler serves the pre-signed agent card bytes at the well-known path.
// The bytes are signed and marshaled once at boot, so the handler is a fixed
// write with no per-request crypto: a consumer fetches the card, reads the
// embedded public key, and verifies the signature offline.
func CardHandler(cardJSON []byte, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", cardContentType)

		if _, err := w.Write(cardJSON); err != nil {
			logger.Warn("write agent card", "error", err)
		}
	}
}
