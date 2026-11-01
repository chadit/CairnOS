package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// [secrets.bitwarden] keys. The parser matches against these and rejects
// anything else, so a typo'd key fails Load rather than leaving a variable
// silently unrendered on the next deploy.
const (
	bitwardenKeyItem            = "item"
	bitwardenKeyCredentialsFile = "credentials_file"
	bitwardenKeyEnvFile         = "env_file"
	bitwardenKeyFields          = "fields"
)

// File names the block falls back to, both relative to the CairnOS home. The
// credentials file is operator-written and holds the Bitwarden API key; the env
// file is generated whole by `secrets pull` and never hand-edited.
const (
	// DefaultBitwardenCredentialsFile is where the block looks for the vault API
	// key when it names no path.
	// #nosec G101 -- a file name, not a credential.
	DefaultBitwardenCredentialsFile = "bitwarden.env"

	// DefaultBitwardenEnvFile is the generated file `secrets pull` writes.
	DefaultBitwardenEnvFile = "secrets.env"
)

// envNamePattern is the grammar for a rendered variable name. It is the shell's
// own, because the generated file is read by systemd's EnvironmentFile and by a
// sourcing shell on macOS, and neither forgives a name they cannot parse.
var envNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Bitwarden is the [secrets.bitwarden] block: one vault item whose custom
// fields become the host's environment file. It is a deployment-time renderer,
// not a runtime backend, because most credentials here are read by bare
// os.Getenv long before any secret source exists to ask.
type Bitwarden struct {
	// Fields maps an environment variable name to the custom field on the item
	// that holds its value. Optional: a field already named like the variable is
	// picked up without a mapping, and this table is for the ones that are not.
	Fields map[string]string

	// Item is the vault item's name, matched exactly rather than by search hit.
	// Optional: the credentials file may carry BW_ITEM instead, and this wins
	// when both are set.
	Item string

	// CredentialsFile holds the vault API key and master password at mode 0600,
	// as BW_ KEY=VALUE lines or, for a path ending in .toml, as a chezmoi
	// config's [data.secret] table. A leading ~/ expands to the operator's home
	// and other relative paths resolve against the CairnOS home, both at use
	// time.
	CredentialsFile string

	// EnvFile is the generated environment file, overwritten whole on each pull.
	EnvFile string
}

// bitwardenEntry is one key of the block, carried together so every error names
// the full path without a wide parameter list.
type bitwardenEntry struct {
	value any
	key   string
}

// parseBitwardenBlock decodes the verbatim [secrets.bitwarden] table, seeding
// the two file defaults so a block that names only an item and its fields is
// complete. A key written blank keeps the blank and fails validation, since an
// operator who typed the key meant to name something.
func parseBitwardenBlock(raw map[string]any) (*Bitwarden, error) {
	block := &Bitwarden{
		CredentialsFile: DefaultBitwardenCredentialsFile,
		EnvFile:         DefaultBitwardenEnvFile,
	}

	for _, key := range slices.Sorted(maps.Keys(raw)) {
		if err := applyBitwardenKey(block, bitwardenEntry{key: key, value: raw[key]}); err != nil {
			return nil, err
		}
	}

	return block, nil
}

// applyBitwardenKey sets one parsed key or returns a naming error. It is split
// from the loop so the switch stays flat and each assertion carries the key path
// into its error.
func applyBitwardenKey(block *Bitwarden, entry bitwardenEntry) error {
	var err error

	switch entry.key {
	case bitwardenKeyItem:
		block.Item, err = bitwardenString(entry)
	case bitwardenKeyCredentialsFile:
		block.CredentialsFile, err = bitwardenString(entry)
	case bitwardenKeyEnvFile:
		block.EnvFile, err = bitwardenString(entry)
	case bitwardenKeyFields:
		block.Fields, err = parseBitwardenFields(entry)
	default:
		err = fmt.Errorf("%w: secrets.bitwarden has unknown key %q", ErrInvalidConfig, entry.key)
	}

	return err
}

// parseBitwardenFields decodes the env-name to field-name table. Its value and
// each entry have to be strings, and a scalar in place of the table is an
// operator mistake worth naming rather than ignoring.
func parseBitwardenFields(entry bitwardenEntry) (map[string]string, error) {
	table, isTable := entry.value.(map[string]any)
	if !isTable {
		return nil, fmt.Errorf("%w: secrets.bitwarden.fields must be a table of field names",
			ErrInvalidConfig)
	}

	fields := make(map[string]string, len(table))

	for _, name := range slices.Sorted(maps.Keys(table)) {
		text, isString := table[name].(string)
		if !isString {
			return nil, fmt.Errorf("%w: secrets.bitwarden.fields.%s must be a string",
				ErrInvalidConfig, name)
		}

		fields[name] = strings.TrimSpace(text)
	}

	return fields, nil
}

// bitwardenString asserts a string value, trimmed so validation and the later
// file lookup judge the same text.
func bitwardenString(entry bitwardenEntry) (string, error) {
	str, ok := entry.value.(string)
	if !ok {
		return "", fmt.Errorf("%w: secrets.bitwarden.%s must be a string", ErrInvalidConfig, entry.key)
	}

	return strings.TrimSpace(str), nil
}

// validateBitwarden checks the block alongside the backend in Load. Field names
// are walked sorted so the first invalid entry is reported reproducibly.
func validateBitwarden(block *Bitwarden) error {
	if block == nil {
		return nil
	}

	if err := requireBitwardenValue(bitwardenKeyCredentialsFile, block.CredentialsFile); err != nil {
		return err
	}

	if err := requireBitwardenValue(bitwardenKeyEnvFile, block.EnvFile); err != nil {
		return err
	}

	return validateBitwardenFields(block.Fields)
}

// requireBitwardenValue rejects a key written blank. The defaults cover an
// absent key, so an empty one is a typo rather than an opt-out.
func requireBitwardenValue(key, value string) error {
	if value == "" {
		return fmt.Errorf("%w: secrets.bitwarden.%s must be set", ErrInvalidConfig, key)
	}

	return nil
}

// validateBitwardenFields holds each mapping to what the generated file can
// carry: a name the shell will parse, and a field the item can actually be
// searched for.
func validateBitwardenFields(fields map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("%w: secrets.bitwarden.fields.%s is not a variable name matching %s",
				ErrInvalidConfig, name, envNamePattern)
		}

		if fields[name] == "" {
			return fmt.Errorf("%w: secrets.bitwarden.fields.%s must name a field on the item",
				ErrInvalidConfig, name)
		}
	}

	return nil
}
