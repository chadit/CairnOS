package config

import (
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
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

// The dotted path to the block, split the way a decoded document nests it.
const (
	keySecrets     = "secrets"
	keyBitwarden   = "bitwarden"
	bitwardenTable = keySecrets + "." + keyBitwarden
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

// credentialsFileLine matches a plain credentials_file key line, capturing its
// indent so a replacement keeps it.
var credentialsFileLine = regexp.MustCompile(`(?m)^([ \t]*)` + bitwardenKeyCredentialsFile + `[ \t]*=`)

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

// SetBitwardenCredentialsFile returns src with [secrets.bitwarden]
// credentials_file set to path. It edits text rather than re-encoding because
// go-toml cannot write a document back with its comments: the key's line is
// replaced, added under the header, or the table is appended, and every other
// line stays as written. Both versions are decoded, and any difference beyond
// that one key is an error.
func SetBitwardenCredentialsFile(src []byte, path string) ([]byte, error) {
	var before map[string]any

	if err := toml.Unmarshal(src, &before); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	edited := writeCredentialsFileKey(string(src), path)

	if err := verifyCredentialsFileEdit(before, []byte(edited), path); err != nil {
		return nil, err
	}

	return []byte(edited), nil
}

// writeCredentialsFileKey places the key in the text. A form the patterns miss,
// such as a quoted key or a block written as dotted keys under [secrets],
// yields text that fails to decode, which the verify step reports.
func writeCredentialsFileKey(text, path string) string {
	assignment := bitwardenKeyCredentialsFile + " = " + strconv.Quote(path)

	bodyStart, bodyEnd, found := tableSection(text, bitwardenTable)
	if !found {
		return appendBitwardenTable(text, assignment)
	}

	match := credentialsFileLine.FindStringSubmatchIndex(text[bodyStart:bodyEnd])
	if match == nil {
		return insertAfterHeader(text, bodyStart, assignment)
	}

	lineStart := bodyStart + match[0]
	indent := text[bodyStart+match[2] : bodyStart+match[3]]
	line, next := lineAt(text, lineStart)
	tail := valueTail(strings.TrimSuffix(line, "\r")[match[1]-match[0]:])

	return text[:lineStart] + indent + assignment + tail + lineEnding(text) + text[next:]
}

// valueTail returns what follows the old value on its line, such as a
// comment, so the replacement keeps it. Only a single-line quoted value is
// measured; for anything else the tail is dropped, and the verify step refuses
// an edit that broke the line.
func valueTail(afterEquals string) string {
	value := strings.TrimLeft(afterEquals, " \t")
	if value == "" || (value[0] != '"' && value[0] != '\'') || isMultiLineString(value) {
		return ""
	}

	end, closed := stringEnd(value, 0)
	if !closed {
		return ""
	}

	return value[end:]
}

// appendBitwardenTable adds the block at the end, a blank line after whatever
// came before it.
func appendBitwardenTable(text, assignment string) string {
	eol := lineEnding(text)

	var out strings.Builder

	out.WriteString(text)

	if text != "" && !strings.HasSuffix(text, "\n") {
		out.WriteString(eol)
	}

	if text != "" {
		out.WriteString(eol)
	}

	out.WriteString("[" + bitwardenTable + "]")
	out.WriteString(eol)
	out.WriteString(assignment)
	out.WriteString(eol)

	return out.String()
}

// verifyCredentialsFileEdit decodes the edited text and requires it to equal
// the original document with only credentials_file set, which catches an edit
// that landed in the wrong span or produced invalid TOML.
func verifyCredentialsFileEdit(before map[string]any, edited []byte, path string) error {
	var after map[string]any

	if err := toml.Unmarshal(edited, &after); err != nil {
		return fmt.Errorf("%w: %w", ErrCredentialsFileVerify, err)
	}

	if before == nil {
		before = map[string]any{}
	}

	childTable(childTable(before, keySecrets), keyBitwarden)[bitwardenKeyCredentialsFile] = path

	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("%w: a key other than %s changed", ErrCredentialsFileVerify, bitwardenKeyCredentialsFile)
	}

	return nil
}

// childTable returns parent[key] as a table, adding an empty one when it is
// absent so the expected document can be built for a block the edit created.
func childTable(parent map[string]any, key string) map[string]any {
	if table, isTable := parent[key].(map[string]any); isTable {
		return table
	}

	table := map[string]any{}
	parent[key] = table

	return table
}
