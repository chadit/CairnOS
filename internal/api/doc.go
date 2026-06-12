// Package api serves the CairnOS gRPC services and their generated HTTP
// gateway from one proto source (.agents/spec/07-api-protocols.md). The gRPC
// server is the source of truth; grpc-gateway derives every HTTP route from
// the google.api.http annotations, success responses are the typed messages
// themselves, and failures share the canonical error envelope
// {"status":"error","error":{code,message,details}} built by a custom
// gateway error handler from the engine's cairnerr codes.
//
// Engine and workflow failures keep their canonical dotted codes end to end.
// Failures the API layer itself produces (lookup misses, cancel races,
// unimplemented admin calls) use the api.* prefix, since the engine error
// families in .agents/spec/04-execution-engine.md do not cover transport
// concerns.
//
// Phase 4a adds authentication (.agents/spec/08-auth-and-access.md): with
// auth enabled, an interceptor pair verifies the bearer token (algorithm,
// signature, exp, jti, denylist) and the method's scope before any handler
// runs; Health and Login stay public. Non-loopback binds need either auth
// plus a TLS-terminating proxy (auth.tls_terminated, because /login carries
// the long-lived access key) or the explicit
// auth.serve_without_auth_on_network acceptance, which logs a loud warning.
package api
