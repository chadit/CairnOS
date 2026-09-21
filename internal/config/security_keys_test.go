package config_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
)

// oneMiB is the body and message default the security keys fall back to.
const oneMiB = 1 << 20

func TestLoadSecurityKeyDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.API.MaxBodyBytes != oneMiB || cfg.API.MaxRecvMsgBytes != oneMiB {
		t.Fatalf("request-size defaults wrong: %+v", cfg.API)
	}

	if len(cfg.API.CORSOrigins) != 0 {
		t.Fatalf("cors_origins must default empty (same-origin), got %v", cfg.API.CORSOrigins)
	}

	if cfg.OAuth.AutoProvision {
		t.Fatal("oauth auto_provision must default false: provisioning fails closed")
	}

	if len(cfg.OAuth.AllowedEmailDomains) != 0 || len(cfg.OAuth.AllowedSubjects) != 0 {
		t.Fatalf("oauth allowlists must default empty: %+v", cfg.OAuth)
	}
}

func TestLoadSecurityKeysFromFile(t *testing.T) {
	t.Parallel()

	securityTOML := `
[api]
cors_origins = ["https://app.example"]
max_body_bytes = 2048
max_recv_msg_bytes = 4096

[oauth]
auto_provision = true
allowed_email_domains = ["example.com", "corp.example"]
allowed_subjects = ["sub-123"]
`

	cfg, err := config.Load(writeConfig(t, securityTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !slices.Equal(cfg.API.CORSOrigins, []string{"https://app.example"}) {
		t.Fatalf("cors_origins overlay missing: %v", cfg.API.CORSOrigins)
	}

	if cfg.API.MaxBodyBytes != 2048 || cfg.API.MaxRecvMsgBytes != 4096 {
		t.Fatalf("request-size overlay missing: %+v", cfg.API)
	}

	if !cfg.OAuth.AutoProvision {
		t.Fatal("auto_provision overlay missing")
	}

	if !slices.Equal(cfg.OAuth.AllowedEmailDomains, []string{"example.com", "corp.example"}) {
		t.Fatalf("allowed_email_domains overlay missing: %v", cfg.OAuth.AllowedEmailDomains)
	}

	if !slices.Equal(cfg.OAuth.AllowedSubjects, []string{"sub-123"}) {
		t.Fatalf("allowed_subjects overlay missing: %v", cfg.OAuth.AllowedSubjects)
	}
}

func TestLoadSecurityKeysFromEnv(t *testing.T) {
	t.Parallel()

	envOverrides := environ.Map(map[string]string{
		"CAIRNOS_API_CORS_ORIGINS":            "https://a.example, https://b.example",
		"CAIRNOS_API_MAX_BODY_BYTES":          "8192",
		"CAIRNOS_API_MAX_RECV_MSG_BYTES":      "16384",
		"CAIRNOS_OAUTH_AUTO_PROVISION":        envOn,
		"CAIRNOS_OAUTH_ALLOWED_EMAIL_DOMAINS": "example.com",
		"CAIRNOS_OAUTH_ALLOWED_SUBJECTS":      "sub-a, sub-b",
	})

	cfg, err := config.LoadWith(filepath.Join(t.TempDir(), "absent.toml"), envOverrides)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !slices.Equal(cfg.API.CORSOrigins, []string{"https://a.example", "https://b.example"}) {
		t.Fatalf("cors_origins env override missing: %v", cfg.API.CORSOrigins)
	}

	if cfg.API.MaxBodyBytes != 8192 || cfg.API.MaxRecvMsgBytes != 16384 {
		t.Fatalf("request-size env override missing: %+v", cfg.API)
	}

	if !cfg.OAuth.AutoProvision {
		t.Fatal("auto_provision env override missing")
	}

	if !slices.Equal(cfg.OAuth.AllowedSubjects, []string{"sub-a", "sub-b"}) {
		t.Fatalf("allowed_subjects env override missing: %v", cfg.OAuth.AllowedSubjects)
	}
}
