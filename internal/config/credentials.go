package config

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
)

// Credential TOML keys. The parser matches against these and rejects anything
// else, so an unknown or plaintext-secret key fails Load rather than being
// dropped silently.
const (
	credKeyGrant           = "grant"
	credKeyTokenURL        = "token_url"
	credKeyClientID        = "client_id"
	credKeyClientSecretRef = "client_secret_ref"
	credKeyRefreshTokenRef = "refresh_token_ref"
	credKeyScopes          = "scopes"
	credKeyAllowPrivate    = "allow_private_targets"

	// credKeyClientSecret and credKeyRefreshToken are the plaintext-secret keys
	// the parser traps: writing the value directly in the block would otherwise
	// leave a secret on disk. Each points the operator at its _ref alternative.
	credKeyClientSecret = "client_secret"
	credKeyRefreshToken = "refresh_token"
)

// parseCredentialBlocks turns the verbatim [credential.<name>] blocks into typed
// Credentials, rejecting unknown and plaintext-secret keys as it goes. Names are
// walked in sorted order so the first offending block is reported reproducibly.
func parseCredentialBlocks(raw map[string]map[string]any) (map[string]Credential, error) {
	credentials := make(map[string]Credential, len(raw))

	for _, name := range slices.Sorted(maps.Keys(raw)) {
		cred, err := parseCredentialBlock(name, raw[name])
		if err != nil {
			return nil, err
		}

		credentials[name] = cred
	}

	return credentials, nil
}

// parseCredentialBlock decodes one block strictly: every key must be a known
// Credential field, the plaintext-secret keys fail with a pointer at their _ref
// form, and anything else fails as an unknown key. Keys are walked sorted so a
// block with several problems reports the same one every run.
func parseCredentialBlock(name string, block map[string]any) (Credential, error) {
	var cred Credential

	for _, key := range slices.Sorted(maps.Keys(block)) {
		if err := applyCredentialKey(&cred, name, key, block[key]); err != nil {
			return Credential{}, err
		}
	}

	return cred, nil
}

// applyCredentialKey sets one parsed key on cred or returns a naming error. It is
// split from the loop so the switch stays flat and each type assertion carries
// the key name into its error.
func applyCredentialKey(cred *Credential, name, key string, value any) error {
	var err error

	switch key {
	case credKeyGrant:
		cred.Grant, err = credString(name, key, value)
	case credKeyTokenURL:
		cred.TokenURL, err = credString(name, key, value)
	case credKeyClientID:
		cred.ClientID, err = credString(name, key, value)
	case credKeyClientSecretRef:
		cred.ClientSecretRef, err = credString(name, key, value)
	case credKeyRefreshTokenRef:
		cred.RefreshTokenRef, err = credString(name, key, value)
	case credKeyScopes:
		cred.Scopes, err = credStringSlice(name, key, value)
	case credKeyAllowPrivate:
		cred.AllowPrivateTargets, err = credBool(name, key, value)
	case credKeyClientSecret:
		err = &credentialKeyError{name: name, key: key, ref: credKeyClientSecretRef}
	case credKeyRefreshToken:
		err = &credentialKeyError{name: name, key: key, ref: credKeyRefreshTokenRef}
	default:
		err = fmt.Errorf("%w: credential.%s has unknown key %q", ErrInvalidConfig, name, key)
	}

	return err
}

// credString asserts a string value or returns a key-named type error.
func credString(name, key string, value any) (string, error) {
	str, ok := value.(string)
	if !ok {
		return "", &credentialKeyError{name: name, key: key, want: "a string"}
	}

	return str, nil
}

// credBool asserts a bool value or returns a key-named type error.
func credBool(name, key string, value any) (bool, error) {
	flag, ok := value.(bool)
	if !ok {
		return false, &credentialKeyError{name: name, key: key, want: "a boolean"}
	}

	return flag, nil
}

// credStringSlice asserts an array of strings or returns a key-named type error.
func credStringSlice(name, key string, value any) ([]string, error) {
	// Both a non-array and an array holding a non-string fail the same way.
	notStrings := &credentialKeyError{name: name, key: key, want: "a string array"}

	raw, ok := value.([]any)
	if !ok {
		return nil, notStrings
	}

	items := make([]string, len(raw))

	for idx, elem := range raw {
		str, isStr := elem.(string)
		if !isStr {
			return nil, notStrings
		}

		items[idx] = str
	}

	return items, nil
}

// credentialKeyError rejects one credential key by naming the block and key,
// never the value, so a secret written in the wrong place stays out of logs. A
// set ref marks a plaintext secret and names the _ref key that replaces it;
// otherwise want names the type the key must hold.
type credentialKeyError struct {
	name string
	key  string
	want string
	ref  string
}

func (e *credentialKeyError) Error() string {
	if e.ref != "" {
		return fmt.Sprintf("%v: credential.%s.%s is a plaintext secret; store the value and reference it with %s",
			ErrInvalidConfig, e.name, e.key, e.ref)
	}

	return fmt.Sprintf("%v: credential.%s.%s must be %s", ErrInvalidConfig, e.name, e.key, e.want)
}

// Unwrap keeps errors.Is(err, ErrInvalidConfig) true for every rejected key.
func (*credentialKeyError) Unwrap() error {
	return ErrInvalidConfig
}

// validateCredentials checks every parsed credential's grant, token_url, and
// secret refs, alongside validateAuth in Load. Names are walked sorted so the
// first invalid block is reported reproducibly.
func validateCredentials(credentials map[string]Credential) error {
	for _, name := range slices.Sorted(maps.Keys(credentials)) {
		cred := credentials[name]
		if err := validateCredential(name, &cred); err != nil {
			return err
		}
	}

	return nil
}

// validateCredential fails fast on a credential that would break the first token
// mint: an unknown grant, a missing client id, an unusable token_url, or a secret
// ref set that does not match the grant.
func validateCredential(name string, cred *Credential) error {
	if cred.Grant != GrantClientCredentials && cred.Grant != GrantRefreshToken {
		return fmt.Errorf("%w: credential.%s.grant %q is not %s or %s",
			ErrInvalidConfig, name, cred.Grant, GrantClientCredentials, GrantRefreshToken)
	}

	if cred.ClientID == "" {
		return fmt.Errorf("%w: credential.%s.client_id must be set", ErrInvalidConfig, name)
	}

	if err := validateTokenURL(name, cred); err != nil {
		return err
	}

	return validateCredentialRefs(name, cred)
}

// validateTokenURL enforces the static half of the token-endpoint egress guard:
// token_url must be set, parse, and use https, unless allow_private_targets lets
// an operator reach an internal http endpoint on purpose.
func validateTokenURL(name string, cred *Credential) error {
	if cred.TokenURL == "" {
		return fmt.Errorf("%w: credential.%s.token_url must be set", ErrInvalidConfig, name)
	}

	parsed, err := url.Parse(cred.TokenURL)
	if err != nil {
		return fmt.Errorf("%w: credential.%s.token_url is not a valid URL", ErrInvalidConfig, name)
	}

	switch parsed.Scheme {
	case schemeHTTPS:
		return nil
	case schemeHTTP:
		if cred.AllowPrivateTargets {
			return nil
		}

		return fmt.Errorf("%w: credential.%s.token_url must use https unless allow_private_targets = true",
			ErrInvalidConfig, name)
	default:
		return fmt.Errorf("%w: credential.%s.token_url scheme %q is not http or https",
			ErrInvalidConfig, name, parsed.Scheme)
	}
}

// validateCredentialRefs enforces the per-grant secret-ref rules: a
// client_credentials block needs a client_secret_ref and forbids a
// refresh_token_ref; a refresh_token block needs a refresh_token_ref and may
// carry a client_secret_ref.
func validateCredentialRefs(name string, cred *Credential) error {
	switch cred.Grant {
	case GrantClientCredentials:
		if cred.ClientSecretRef == "" {
			return fmt.Errorf("%w: credential.%s.client_secret_ref must be set for the %s grant",
				ErrInvalidConfig, name, GrantClientCredentials)
		}

		if cred.RefreshTokenRef != "" {
			return fmt.Errorf("%w: credential.%s.refresh_token_ref is not valid for the %s grant",
				ErrInvalidConfig, name, GrantClientCredentials)
		}

		return nil
	case GrantRefreshToken:
		if cred.RefreshTokenRef == "" {
			return fmt.Errorf("%w: credential.%s.refresh_token_ref must be set for the %s grant",
				ErrInvalidConfig, name, GrantRefreshToken)
		}

		return nil
	default:
		return nil
	}
}
