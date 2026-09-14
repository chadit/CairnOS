package reauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Provider names a subscription CLI tool whose OAuth credential CairnOS can probe
// for expiry. The value matches the conventional creds-directory name under the
// user home (~/.codex, ~/.qwen, ~/.claude).
type Provider string

// Known providers, the subscription CLI tools D12 sanctions
// (.agents/spec/14-open-decisions.md, S5/D12).
const (
	ProviderCodex  Provider = "codex"
	ProviderQwen   Provider = "qwen"
	ProviderClaude Provider = "claude"
)

// jwtSegments is the header.payload.signature part count of a well-formed JWT.
const jwtSegments = 3

// Known reports whether provider is one this package can probe, so a caller can
// reject an unrecognized tool before storing it as a hold key (D12).
func Known(provider Provider) bool {
	switch provider {
	case ProviderCodex, ProviderQwen, ProviderClaude:
		return true
	default:
		return false
	}
}

// Credential is the expiry-relevant slice of a CLI tool's stored OAuth creds:
// when it expires and which account it belongs to. Both fields are best-effort.
// A field the reader could not resolve is left zero rather than guessed.
type Credential struct {
	ExpiresAt time.Time
	Account   string
}

// Valid reports whether the credential is present and not yet expired at now. A
// zero ExpiresAt (the reader could not confirm an expiry) is treated as not
// valid, so a job is never resumed on a credential that was not confirmed fresh.
func (c Credential) Valid(now time.Time) bool {
	return !c.ExpiresAt.IsZero() && now.Before(c.ExpiresAt)
}

// ReadCredential reads the stored credential for provider under homeDir, where
// homeDir is the user home (os.UserHomeDir). A missing file returns
// ErrNoCredential; a present-but-unreadable expiry returns a Credential whose
// Valid is false rather than an error, so the resume loop simply keeps the job
// held.
func ReadCredential(provider Provider, homeDir string) (Credential, error) {
	switch provider {
	case ProviderCodex:
		return readCodex(homeDir)
	case ProviderQwen:
		return readQwen(homeDir)
	case ProviderClaude:
		return readClaude(homeDir)
	default:
		return Credential{}, fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
}

// codexAuth is the slice of ~/.codex/auth.json the probe reads: the expiry lives
// in the access token's JWT and the account in the id token's email claim.
type codexAuth struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	} `json:"tokens"`
}

// readCodex reads ~/.codex/auth.json. The expiry comes from the access token's
// exp claim; the account from the id token's email claim, falling back to the id
// token's exp when the access token carries none.
func readCodex(homeDir string) (Credential, error) {
	raw, err := readCredFile(filepath.Join(homeDir, ".codex", "auth.json"))
	if err != nil {
		return Credential{}, err
	}

	var auth codexAuth
	if err := json.Unmarshal(raw, &auth); err != nil {
		return Credential{}, fmt.Errorf("parse codex auth: %w", err)
	}

	cred := Credential{ExpiresAt: jwtExpiry(auth.Tokens.AccessToken)}

	if claims, claimErr := jwtClaims(auth.Tokens.IDToken); claimErr == nil {
		cred.Account = stringClaim(claims, "email")

		if cred.ExpiresAt.IsZero() {
			cred.ExpiresAt = expFromClaims(claims)
		}
	}

	return cred, nil
}

// qwenCreds is the slice of ~/.qwen/oauth_creds.json the probe reads: qwen stores
// an explicit expiry_date in epoch milliseconds.
type qwenCreds struct {
	Email      string `json:"email"`
	ExpiryDate int64  `json:"expiry_date"`
}

// readQwen reads ~/.qwen/oauth_creds.json, taking the expiry from the explicit
// expiry_date (epoch ms) and the account from an email field when present.
func readQwen(homeDir string) (Credential, error) {
	raw, err := readCredFile(filepath.Join(homeDir, ".qwen", "oauth_creds.json"))
	if err != nil {
		return Credential{}, err
	}

	var creds qwenCreds
	if err := json.Unmarshal(raw, &creds); err != nil {
		return Credential{}, fmt.Errorf("parse qwen creds: %w", err)
	}

	return Credential{Account: creds.Email, ExpiresAt: millisToTime(creds.ExpiryDate)}, nil
}

// claudeCreds is the slice of ~/.claude/.credentials.json the probe reads: the
// OAuth block carries an expiresAt in epoch milliseconds. The JSON keys are
// Claude's own camelCase, not CairnOS naming, so they keep their external form.
type claudeCreds struct {
	OAuth struct {
		ExpiresAt int64 `json:"expiresAt"` //nolint:tagliatelle // external Claude credentials.json key
	} `json:"claudeAiOauth"` //nolint:tagliatelle // external Claude credentials.json key
}

// readClaude reads ~/.claude/.credentials.json, taking the expiry from the OAuth
// block's expiresAt (epoch ms). The file carries no plain account field, so
// Account is left empty.
func readClaude(homeDir string) (Credential, error) {
	raw, err := readCredFile(filepath.Join(homeDir, ".claude", ".credentials.json"))
	if err != nil {
		return Credential{}, err
	}

	var creds claudeCreds
	if err := json.Unmarshal(raw, &creds); err != nil {
		return Credential{}, fmt.Errorf("parse claude creds: %w", err)
	}

	return Credential{ExpiresAt: millisToTime(creds.OAuth.ExpiresAt)}, nil
}

// readCredFile reads a credential file, mapping "not found" to ErrNoCredential so
// the caller distinguishes a missing login from a read or parse failure.
func readCredFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed per-provider credential file under the user home
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoCredential, path)
	}

	if err != nil {
		return nil, fmt.Errorf("read credential file: %w", err)
	}

	return raw, nil
}

// millisToTime converts an epoch-millisecond timestamp to a time, mapping the
// zero (a missing field) to the zero time so Valid reports it as not fresh.
func millisToTime(millis int64) time.Time {
	if millis == 0 {
		return time.Time{}
	}

	return time.UnixMilli(millis)
}

// jwtExpiry returns the exp claim of a JWT as a time, or the zero time when the
// token is missing or unreadable.
func jwtExpiry(token string) time.Time {
	claims, err := jwtClaims(token)
	if err != nil {
		return time.Time{}
	}

	return expFromClaims(claims)
}

// jwtClaims decodes a JWT payload without verifying the signature: the probe only
// reads the expiry and account, it does not authenticate the token. The middle
// segment is base64url with no padding (RFC 7519).
func jwtClaims(token string) (map[string]any, error) {
	if token == "" {
		return nil, ErrMalformedJWT
	}

	parts := strings.Split(token, ".")
	if len(parts) != jwtSegments {
		return nil, ErrMalformedJWT
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode jwt payload: %w", err)
	}

	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse jwt claims: %w", err)
	}

	return claims, nil
}

// expFromClaims pulls the exp claim (epoch seconds, decoded as a JSON float) out
// of a JWT claim set, returning the zero time when it is absent or not a number.
func expFromClaims(claims map[string]any) time.Time {
	exp, ok := claims["exp"].(float64)
	if !ok {
		return time.Time{}
	}

	return time.Unix(int64(exp), 0)
}

// stringClaim reads a string claim, returning "" when it is absent or not a
// string.
func stringClaim(claims map[string]any, key string) string {
	value, ok := claims[key].(string)
	if !ok {
		return ""
	}

	return value
}
