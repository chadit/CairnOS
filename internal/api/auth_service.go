package api

import (
	"context"
	"errors"
	"log/slog"
	"net"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/timestamppb"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/cairnerr"
)

// Revocation target prefixes echoed in RevokeResponse.denied.
const (
	deniedJTIPrefix = "jti:"
	deniedKeyPrefix = "key:"
)

// loginService implements cairnos.v1.LoginService: the access-key-to-token
// exchange plus the revocation RPC feeding the denylist. The auth
// interceptor leaves Login public and gates Revoke behind admin.
type loginService struct {
	cairnosv1.UnimplementedLoginServiceServer

	// authenticator is nil when auth is disabled; Login then answers
	// auth.disabled instead of minting tokens nothing would check.
	authenticator Authenticator

	// limiter caps unauthenticated login cost: a bounded derivation pool and
	// a per-peer rate limit, both guarding only this RPC (spec M1). Nil keeps
	// the login path unbounded, which is the auth-disabled case anyway.
	limiter *auth.LoginLimiter

	// logger records the real login-failure detail server-side; the wire
	// answer stays a single opaque message so a caller cannot tell a wrong
	// secret from a malformed stored hash (spec L3). Nil means slog.Default.
	logger *slog.Logger
}

// Login exchanges a valid, enabled access key for a short-lived token.
// Every credential failure collapses into one UNAUTHENTICATED answer so
// the response does not reveal whether the key id exists, the secret was
// wrong, or the key was disabled.
func (s *loginService) Login(ctx context.Context, req *cairnosv1.LoginRequest) (*cairnosv1.LoginResponse, error) {
	if s.authenticator == nil {
		return nil, errorStatus(codes.FailedPrecondition,
			cairnerr.New(codeAuthDisabled, "auth is disabled on this server; there is nothing to log in to"))
	}

	// Admit through the M1 guard before the argon2id work runs, so a flood is
	// refused cheaply instead of pinning ~64 MiB per concurrent derivation.
	release, err := s.limiter.Acquire(ctx, peerIP(ctx))
	if err != nil {
		return nil, s.loginStatus(err)
	}
	defer release()

	token, expiresAt, err := s.authenticator.Login(ctx, req.GetKey())
	if err != nil {
		return nil, s.loginStatus(err)
	}

	return &cairnosv1.LoginResponse{
		Token:     token,
		ExpiresAt: timestamppb.New(expiresAt),
	}, nil
}

// peerIP pulls the caller's IP from the gRPC peer for the per-peer login rate
// limit (spec M1). The gateway dials the backend over loopback, so HTTP callers
// share one bucket. That's fine: the gateway is the trusted local front and the
// concurrency cap bounds cost. An unknown peer falls back to a shared key rather
// than failing the call.
func peerIP(ctx context.Context) string {
	info, ok := peer.FromContext(ctx)
	if !ok || info.Addr == nil {
		return "unknown"
	}

	host, _, err := net.SplitHostPort(info.Addr.String())
	if err != nil {
		return info.Addr.String()
	}

	return host
}

// Revoke denylists one token by jti or every live token of one key by key
// id; exactly one target must be set. The interceptor has already
// required the admin scope by the time this runs.
func (s *loginService) Revoke(ctx context.Context, req *cairnosv1.RevokeRequest) (*cairnosv1.RevokeResponse, error) {
	if s.authenticator == nil {
		return nil, errorStatus(codes.FailedPrecondition,
			cairnerr.New(codeAuthDisabled, "auth is disabled on this server; there are no tokens to revoke"))
	}

	jti, keyID := req.GetJti(), req.GetKeyId()
	if (jti == "") == (keyID == "") {
		return nil, errorStatus(codes.InvalidArgument,
			cairnerr.New(codeInvalidArgument, "set exactly one of jti or key_id"))
	}

	if jti != "" {
		if err := s.authenticator.RevokeToken(ctx, jti); err != nil {
			return nil, canonicalStatus(err)
		}

		return &cairnosv1.RevokeResponse{Denied: deniedJTIPrefix + jti}, nil
	}

	if err := s.authenticator.RevokeKey(ctx, keyID); err != nil {
		return nil, canonicalStatus(err)
	}

	return &cairnosv1.RevokeResponse{Denied: deniedKeyPrefix + keyID}, nil
}

// loginStatus maps a login failure onto the wire: every credential problem
// collapses into one UNAUTHENTICATED so a caller learns nothing about which
// check tripped (spec L3). A login flood is RESOURCE_EXHAUSTED (M1). Anything
// else is INTERNAL. The real error is logged server-side, never on the wire, so
// the opaque message cannot leak a stored-hash format problem.
func (s *loginService) loginStatus(err error) error {
	switch {
	case isCredentialFailure(err):
		s.resolveLogger().Debug("login rejected", "error", err)

		return errorStatus(codes.Unauthenticated,
			cairnerr.New(codeAuthUnauthenticated, "invalid or disabled access key"))
	case errors.Is(err, auth.ErrTooManyLogins):
		s.resolveLogger().Warn("login rate limited", "error", err)

		return errorStatus(codes.ResourceExhausted,
			cairnerr.New(codeAuthTooManyLogins, "too many login attempts; slow down and retry"))
	default:
		s.resolveLogger().Error("login failed", "error", err)

		return errorStatus(codes.Internal, cairnerr.New(codeInternal, "login failed"))
	}
}

// resolveLogger resolves the configured logger or slog.Default.
func (s *loginService) resolveLogger() *slog.Logger {
	if s.logger == nil {
		return slog.Default()
	}

	return s.logger
}

// isCredentialFailure reports the login errors that must look identical on
// the wire: a malformed key, an unknown or disabled key, a wrong secret, and
// a malformed stored hash, which the timing-decoy path already hides and the
// L3 fix now routes here too so it is indistinguishable from a wrong secret.
func isCredentialFailure(err error) bool {
	return errors.Is(err, auth.ErrKeyMalformed) ||
		errors.Is(err, auth.ErrKeyNotFound) ||
		errors.Is(err, auth.ErrKeyDisabled) ||
		errors.Is(err, auth.ErrKeyMismatch) ||
		errors.Is(err, auth.ErrHashMalformed)
}
