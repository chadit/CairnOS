package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/config"
)

// bwItem is the vault item every fixture names, and bwTokenVar one rendered
// variable. Both are spelled the way a host config would spell them.
const (
	bwItem     = "chezmoi"
	bwTokenVar = "CAIRNOS_RUNNER_OPENAI_API_KEY"

	// wantNameRefusal is the grammar guard's message, asserted for three shapes
	// a shell could not parse back out of the generated file.
	wantNameRefusal = "is not a variable name matching"
)

// bitwardenTOML builds a [secrets.bitwarden] block plus a fields table from the
// lines a case wants, so each test states only the keys it is about.
func bitwardenTOML(blockLines, fieldLines []string) string {
	toml := "[secrets.bitwarden]\n" + strings.Join(blockLines, "\n") + "\n"
	if len(fieldLines) > 0 {
		toml += "\n[secrets.bitwarden.fields]\n" + strings.Join(fieldLines, "\n") + "\n"
	}

	return toml
}

func TestLoadBitwardenParsesBlockAndDefaults(t *testing.T) {
	t.Parallel()

	toml := bitwardenTOML(
		[]string{`item = "` + bwItem + `"`},
		[]string{bwTokenVar + ` = "openai key"`, `CAIRNOS_SECRET_WEBHOOK = "webhook secret"`},
	)

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	block := cfg.Secrets.Bitwarden
	if block == nil {
		t.Fatal("secrets.bitwarden missing from parsed config")
	}

	if block.Item != bwItem {
		t.Fatalf("item = %q, want %q", block.Item, bwItem)
	}

	if block.CredentialsFile != config.DefaultBitwardenCredentialsFile {
		t.Fatalf("credentials_file = %q, want the default", block.CredentialsFile)
	}

	if block.EnvFile != config.DefaultBitwardenEnvFile {
		t.Fatalf("env_file = %q, want the default", block.EnvFile)
	}

	if block.Fields[bwTokenVar] != "openai key" {
		t.Fatalf("fields = %+v, want the openai mapping", block.Fields)
	}
}

func TestLoadBitwardenKeepsConfiguredPaths(t *testing.T) {
	t.Parallel()

	toml := bitwardenTOML(
		[]string{
			`item             = "` + bwItem + `"`,
			`credentials_file = "/etc/cairnos/bw.env"`,
			`env_file         = "/etc/cairnos/secrets.env"`,
		},
		[]string{bwTokenVar + ` = "openai key"`},
	)

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	block := cfg.Secrets.Bitwarden
	if block.CredentialsFile != "/etc/cairnos/bw.env" || block.EnvFile != "/etc/cairnos/secrets.env" {
		t.Fatalf("block = %+v, want both configured paths kept", block)
	}
}

// TestLoadWithoutBitwardenLeavesTheBlockNil proves the absent case stays the
// no-op every host without a vault depends on.
func TestLoadWithoutBitwardenLeavesTheBlockNil(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, "[secrets]\nbackend = \"env\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Secrets.Bitwarden != nil {
		t.Fatalf("bitwarden = %+v, want nil when no block is written", cfg.Secrets.Bitwarden)
	}
}

// TestLoadBitwardenAcceptsABareBlock covers FR18 and FR19 together. A host whose
// item name lives in the credentials file, and whose fields are named after the
// variables they fill, has nothing left to write here, so the header alone has
// to load. An absent block means something else: the pull does nothing at all.
func TestLoadBitwardenAcceptsABareBlock(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, "[secrets.bitwarden]\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	block := cfg.Secrets.Bitwarden
	if block == nil {
		t.Fatal("a written [secrets.bitwarden] header parsed as no block at all")
	}

	if block.Item != "" || len(block.Fields) != 0 {
		t.Fatalf("block = %+v, want no item and no mappings", block)
	}

	if block.CredentialsFile != config.DefaultBitwardenCredentialsFile ||
		block.EnvFile != config.DefaultBitwardenEnvFile {
		t.Fatalf("block = %+v, want both file defaults", block)
	}
}

// TestLoadBitwardenAcceptsFieldsWithoutAnItem covers the other half of FR18: the
// mappings are in config and the item name comes from the credentials file.
func TestLoadBitwardenAcceptsFieldsWithoutAnItem(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, bitwardenTOML(nil, []string{bwTokenVar + ` = "openai key"`})))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Secrets.Bitwarden.Fields[bwTokenVar] != "openai key" {
		t.Fatalf("fields = %+v, want the openai mapping", cfg.Secrets.Bitwarden.Fields)
	}
}

func TestLoadBitwardenRejectsBadBlocks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "unknown key",
			toml: bitwardenTOML(
				[]string{`item = "` + bwItem + `"`, `vault = "personal"`},
				[]string{bwTokenVar + ` = "openai key"`},
			),
			want: `secrets.bitwarden has unknown key "vault"`,
		},
		{
			name: "non-string item",
			toml: bitwardenTOML([]string{`item = 7`}, []string{bwTokenVar + ` = "openai key"`}),
			want: "secrets.bitwarden.item must be a string",
		},
		{
			name: "non-string field value",
			toml: bitwardenTOML([]string{`item = "` + bwItem + `"`}, []string{bwTokenVar + ` = 7`}),
			want: "secrets.bitwarden.fields." + bwTokenVar + " must be a string",
		},
		{
			name: "scalar where the fields table belongs",
			toml: bitwardenTOML([]string{`item = "` + bwItem + `"`, `fields = "openai"`}, nil),
			want: "secrets.bitwarden.fields must be a table",
		},
		{
			name: "blank credentials file",
			toml: bitwardenTOML(
				[]string{`item = "` + bwItem + `"`, `credentials_file = "  "`},
				[]string{bwTokenVar + ` = "openai key"`},
			),
			want: "secrets.bitwarden.credentials_file must be set",
		},
		{
			name: "lower-case variable name",
			toml: bitwardenTOML([]string{`item = "` + bwItem + `"`}, []string{`openai_key = "openai key"`}),
			want: wantNameRefusal,
		},
		{
			name: "variable name starting with a digit",
			toml: bitwardenTOML([]string{`item = "` + bwItem + `"`}, []string{`"2FA_TOKEN" = "totp"`}),
			want: wantNameRefusal,
		},
		{
			name: "variable name with a dash",
			toml: bitwardenTOML([]string{`item = "` + bwItem + `"`}, []string{`"API-KEY" = "openai key"`}),
			want: wantNameRefusal,
		},
		{
			name: "blank field name",
			toml: bitwardenTOML([]string{`item = "` + bwItem + `"`}, []string{bwTokenVar + ` = "   "`}),
			want: "must name a field on the item",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(writeConfig(t, testCase.toml))
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("load error = %v, want one wrapping ErrInvalidConfig", err)
			}

			message := err.Error()
			if !strings.Contains(message, testCase.want) {
				t.Fatalf("load error %q does not mention %q", message, testCase.want)
			}
		})
	}
}
