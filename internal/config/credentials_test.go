package config_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/config"
)

// credentialTOML builds a valid client_credentials block plus one extra line, so
// a test can prove that the extra line (an unknown or plaintext-secret key) fails
// Load while the rest of the block is well-formed.
func credentialTOML(extraLine string) string {
	return "[credential.saas]\n" +
		"grant = \"client_credentials\"\n" +
		"token_url = \"https://login.example.com/oauth/token\"\n" +
		"client_id = \"cairnos-prod\"\n" +
		"client_secret_ref = \"saas_client_secret\"\n" +
		extraLine + "\n"
}

func TestLoadCredentialClientCredentials(t *testing.T) {
	t.Parallel()

	toml := credentialTOML("scopes = [\"read\", \"write\"]")

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	cred, ok := cfg.Credentials["saas"]
	if !ok {
		t.Fatal("credential saas missing from parsed config")
	}

	if cred.Grant != config.GrantClientCredentials {
		t.Fatalf("grant = %q, want %q", cred.Grant, config.GrantClientCredentials)
	}

	if cred.ClientID != "cairnos-prod" {
		t.Fatalf("client_id = %q", cred.ClientID)
	}

	if cred.ClientSecretRef != "saas_client_secret" {
		t.Fatalf("client_secret_ref = %q", cred.ClientSecretRef)
	}

	if !slices.Equal(cred.Scopes, []string{"read", "write"}) {
		t.Fatalf("scopes = %v", cred.Scopes)
	}
}

func TestLoadCredentialRefreshToken(t *testing.T) {
	t.Parallel()

	toml := "[credential.rt]\n" +
		"grant = \"refresh_token\"\n" +
		"token_url = \"https://login.example.com/oauth/token\"\n" +
		"client_id = \"cairnos-prod\"\n" +
		"refresh_token_ref = \"saas_refresh\"\n" +
		"client_secret_ref = \"saas_client_secret\"\n"

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	cred, ok := cfg.Credentials["rt"]
	if !ok {
		t.Fatal("credential rt missing from parsed config")
	}

	if cred.Grant != config.GrantRefreshToken {
		t.Fatalf("grant = %q, want %q", cred.Grant, config.GrantRefreshToken)
	}

	if cred.RefreshTokenRef != "saas_refresh" {
		t.Fatalf("refresh_token_ref = %q", cred.RefreshTokenRef)
	}
}

func TestLoadCredentialRejectsPlaintextSecrets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		line    string
		pointer string
	}{
		{name: "client_secret", line: "client_secret = \"PLAINTEXT-LEAK\"", pointer: "client_secret_ref"},
		{name: "refresh_token", line: "refresh_token = \"PLAINTEXT-LEAK\"", pointer: "refresh_token_ref"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(writeConfig(t, credentialTOML(tt.line)))
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}

			message := err.Error()
			if !strings.Contains(message, tt.pointer) {
				t.Fatalf("error should point at %s: %v", tt.pointer, err)
			}

			if strings.Contains(message, "PLAINTEXT-LEAK") {
				t.Fatalf("error leaked the secret value: %v", err)
			}
		})
	}
}

func TestLoadCredentialRejectsUnknownKeys(t *testing.T) {
	t.Parallel()

	// Each subtest is named for the key it writes. cliennt_id is a typo of
	// client_id, so a misspelling fails the same way a secret-shaped key does.
	keys := []string{"token", "access_token", "api_key", "private_key", "cliennt_id"}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(writeConfig(t, credentialTOML(key+" = \"LEAK\"")))
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}

			message := err.Error()
			if !strings.Contains(message, key) {
				t.Fatalf("error should name the unknown key %q: %v", key, err)
			}

			if strings.Contains(message, "LEAK") {
				t.Fatalf("error leaked the value: %v", err)
			}
		})
	}
}

func TestLoadCredentialTokenURLScheme(t *testing.T) {
	t.Parallel()

	httpNoOptOut := "[credential.saas]\n" +
		"grant = \"client_credentials\"\n" +
		"token_url = \"http://login.example.com/oauth/token\"\n" +
		"client_id = \"cairnos-prod\"\n" +
		"client_secret_ref = \"saas_client_secret\"\n"

	if _, err := config.Load(writeConfig(t, httpNoOptOut)); !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("http token_url without opt-out: error = %v, want ErrInvalidConfig", err)
	}

	httpOptOut := httpNoOptOut + "allow_private_targets = true\n"
	if _, err := config.Load(writeConfig(t, httpOptOut)); err != nil {
		t.Fatalf("http token_url with allow_private_targets should load: %v", err)
	}
}

func TestLoadCredentialValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
	}{
		{name: "unknown grant", toml: "[credential.c]\ngrant = \"password\"\ntoken_url = \"https://x/t\"\nclient_id = \"c\"\nclient_secret_ref = \"r\"\n"},
		{name: "missing client_id", toml: "[credential.c]\ngrant = \"client_credentials\"\ntoken_url = \"https://x/t\"\nclient_secret_ref = \"r\"\n"},
		{name: "missing token_url", toml: "[credential.c]\ngrant = \"client_credentials\"\nclient_id = \"c\"\nclient_secret_ref = \"r\"\n"},
		{name: "cc missing secret ref", toml: "[credential.c]\ngrant = \"client_credentials\"\ntoken_url = \"https://x/t\"\nclient_id = \"c\"\n"},
		{name: "cc with refresh ref", toml: "[credential.c]\ngrant = \"client_credentials\"\ntoken_url = \"https://x/t\"\nclient_id = \"c\"\nclient_secret_ref = \"r\"\nrefresh_token_ref = \"rt\"\n"},
		{name: "rt missing refresh ref", toml: "[credential.c]\ngrant = \"refresh_token\"\ntoken_url = \"https://x/t\"\nclient_id = \"c\"\n"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := config.Load(writeConfig(t, tt.toml)); !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
