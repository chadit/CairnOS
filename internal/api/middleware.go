package api

import (
	"net/http"
	"slices"
)

// Security header values the gateway sets on every response (spec M1). The CSP
// is the strictest baseline that still serves a JSON API: default-src 'none'
// blocks every subresource, which the API never loads. The SPA is served
// separately, so the API needs no script-src.
const (
	headerContentTypeOptions = "X-Content-Type-Options"
	headerFrameOptions       = "X-Frame-Options"
	headerCSP                = "Content-Security-Policy"

	valueNoSniff = "nosniff"
	valueDeny    = "DENY"
	valueCSPNone = "default-src 'none'"
)

// securityHeaders wraps a handler so every response carries the baseline
// hardening headers (spec M1): nosniff stops content-type sniffing, DENY blocks
// framing (clickjacking), and the CSP blocks every subresource an API response
// has no reason to load.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set(headerContentTypeOptions, valueNoSniff)
		header.Set(headerFrameOptions, valueDeny)
		header.Set(headerCSP, valueCSPNone)

		next.ServeHTTP(w, r)
	})
}

// corsAllowlist applies an explicit-origin CORS policy (spec M1). Only a
// listed origin is echoed back, never a wildcard, and the response varies on
// Origin so a cache cannot leak one origin's headers to another. An empty
// allowlist adds no CORS headers, leaving the browser to enforce same-origin.
// A preflight from an allowed origin is answered here.
func corsAllowlist(allowed []string, next http.Handler) http.Handler {
	if len(allowed) == 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(allowed, origin) {
			applyCORSHeaders(w, origin)

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)

				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// applyCORSHeaders writes the per-origin CORS response headers for one allowed
// origin. Vary: Origin keeps a shared cache from serving one origin's allow
// header to another.
func applyCORSHeaders(w http.ResponseWriter, origin string) {
	header := w.Header()
	header.Set("Access-Control-Allow-Origin", origin)
	header.Add("Vary", "Origin")
	header.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	header.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	header.Set("Access-Control-Allow-Credentials", "true")
}

// limitBody caps each request body (spec M4). A declared Content-Length over
// the cap is rejected with 413 before the gateway reads a byte; a body with no
// declared length is bounded by http.MaxBytesReader so a chunked upload cannot
// run away. A non-positive limit leaves the body uncapped. It guards the
// gateway mux handlers, where tenants stream arbitrary bodies.
func limitBody(maxBytes int64, next http.Handler) http.Handler {
	if maxBytes <= 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)

			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

		next.ServeHTTP(w, r)
	})
}
