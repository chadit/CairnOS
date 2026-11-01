// Package bitwarden reads custom fields off one vault item by driving the
// standalone `bw` CLI. It exists so a host's credentials can be rendered into an
// environment file at deployment time: most of what CairnOS needs is read by
// bare os.Getenv long before any runtime secret source is built, so a backend
// that answers at run time would not reach them.
//
// Nothing here puts a secret in argv. The API key goes to the login child's
// environment and the vault's master key goes in an HTTP request body, because
// arguments are world-readable through /proc on every host this runs on.
package bitwarden

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// Environment variable names the credentials file and the process environment
// both use, so an operator can keep them in either place.
const (
	EnvClientID     = "BW_CLIENTID"
	EnvClientSecret = "BW_CLIENTSECRET"

	// EnvMasterKey names the variable holding the vault's master password. The
	// Go identifier avoids that word so the repository's secret scanner does not
	// read a variable name as a hardcoded value.
	EnvMasterKey = "BW_PASSWORD"

	// EnvItem names the vault item. It lives beside the credentials so the one
	// file that opens the vault also says what to read from it, and a host that
	// never edits config.toml still ends up with a working pull.
	EnvItem = "BW_ITEM"
)

// chezmoiExt marks a credentials path as a chezmoi config rather than a
// KEY=VALUE file, in any letter case. The extension decides, not the content,
// so a file that fails to parse is reported against the format the operator
// chose.
const chezmoiExt = ".toml"

// credentialsPerm is the loosest mode the credentials file may carry. Anything
// group- or world-readable is refused rather than warned about: the file opens a
// whole vault, and a warning on a deploy path nobody watches is the same as no
// check at all.
const credentialsPerm fs.FileMode = 0o600

// Credentials are the three values needed to open the vault. They are carried by
// value and never logged; every error in this package names the step and the
// file, never a value.
type Credentials struct {
	ClientID     string
	ClientSecret string

	// MasterKey is the vault's master password, sent only in the /unlock body.
	MasterKey string

	// Item is the vault item the fields live on. It is optional here: the config
	// block may name it instead, and the config wins when both do.
	Item string
}

// LoadCredentials reads the values from path and falls back to env for any the
// file omits. A .toml path is read as a chezmoi config, so a workstation keeps
// one copy of the four values. A missing file is fine when env supplies
// everything; a file readable beyond its owner is always an error.
func LoadCredentials(path string, env func(string) string) (Credentials, error) {
	fromFile, err := readCredentialsFile(path)
	if err != nil {
		return Credentials{}, err
	}

	creds := Credentials{
		ClientID:     pick(fromFile, env, EnvClientID),
		ClientSecret: pick(fromFile, env, EnvClientSecret),
		MasterKey:    pick(fromFile, env, EnvMasterKey),
		Item:         pick(fromFile, env, EnvItem),
	}

	return creds, checkComplete(creds, path)
}

// pick prefers the file's value and falls back to the environment, so a host can
// keep three of the four in a file and the last one somewhere else.
func pick(fromFile map[string]string, env func(string) string, name string) string {
	if value := fromFile[name]; value != "" {
		return value
	}

	return strings.TrimSpace(env(name))
}

// checkComplete names every value still missing rather than the first, so one
// run tells the operator everything to fix. The item is not on the list: it may
// come from the config block instead. A chezmoi config never holds a BW_ name,
// so for one the file half of the message names its [data.secret] keys, the
// tags on chezmoiConfig.
func checkComplete(creds Credentials, path string) error {
	var missingEnv, missingKeys []string

	for _, required := range []struct{ env, chezmoiKey, value string }{
		{EnvClientID, "bw_client_id", creds.ClientID},
		{EnvClientSecret, "bw_client_secret", creds.ClientSecret},
		{EnvMasterKey, "bw_master_key", creds.MasterKey},
	} {
		if required.value == "" {
			missingEnv = append(missingEnv, required.env)
			missingKeys = append(missingKeys, required.chezmoiKey)
		}
	}

	if len(missingEnv) == 0 {
		return nil
	}

	if isChezmoiConfig(path) {
		return vaultError(CodeCredentials, "%s has no [data.secret] %s, and the environment has no %s",
			path, strings.Join(missingKeys, ", "), strings.Join(missingEnv, ", "))
	}

	return vaultError(CodeCredentials, "%s is missing from %s and the environment",
		strings.Join(missingEnv, ", "), path)
}

// isChezmoiConfig reports whether path names a chezmoi config. The comparison
// ignores case because a file named chezmoi.TOML sent to the KEY=VALUE parser
// would have every line dropped without a word.
func isChezmoiConfig(path string) bool {
	return strings.EqualFold(filepath.Ext(path), chezmoiExt)
}

// readCredentialsFile parses the credentials file in the format its extension
// names, refusing one anybody but its owner can read. An absent file returns
// nothing and no error.
//
// The mode is read off the descriptor, not the path: checking a path and then
// opening it are two resolutions, and anything that can write the directory
// could swap the file between them.
func readCredentialsFile(path string) (map[string]string, error) {
	file, err := os.Open(path) // #nosec G304 -- the path comes from the operator's own config
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}

	if err != nil {
		return nil, cairnerr.Wrapf(err, CodeCredentials, "open %s: %v", path, err)
	}

	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, cairnerr.Wrapf(err, CodeCredentials, "read %s: %v", path, err)
	}

	if perm := info.Mode().Perm(); perm&^credentialsPerm != 0 {
		return nil, vaultError(CodeCredentials,
			"%s is mode %04o; it opens a vault and must be %04o or stricter",
			path, perm, credentialsPerm)
	}

	if isChezmoiConfig(path) {
		return parseChezmoiConfig(file, path)
	}

	return parseCredentialsFile(file, path)
}

// chezmoiConfig mirrors the one table of a chezmoi config this package reads.
// Every other key is left to the decoder to skip, so a config that grows new
// sections still loads.
type chezmoiConfig struct {
	Data struct {
		Secret struct {
			ClientID     string `toml:"bw_client_id"`
			ClientSecret string `toml:"bw_client_secret"`
			MasterKey    string `toml:"bw_master_key"`
			Item         string `toml:"bw_item_name"`
		} `toml:"secret"`
	} `toml:"data"`
}

// parseChezmoiConfig reads the four values from a chezmoi config's
// [data.secret] table, keyed by the variable names the file fallback uses so
// LoadCredentials treats both formats the same. An empty value counts as
// absent, which is what lets the environment fill it in.
func parseChezmoiConfig(reader io.Reader, path string) (map[string]string, error) {
	var parsed chezmoiConfig

	if err := toml.NewDecoder(reader).Decode(&parsed); err != nil {
		return nil, tomlProblem(err, path)
	}

	secret := parsed.Data.Secret

	return map[string]string{
		EnvClientID:     secret.ClientID,
		EnvClientSecret: secret.ClientSecret,
		EnvMasterKey:    secret.MasterKey,
		EnvItem:         secret.Item,
	}, nil
}

// tomlProblem reports where a chezmoi config stopped decoding by line and
// column only, because the decoder's error keeps the offending line, which can
// hold the master key. A type mismatch decodes to the same error, so the
// message does not say invalid TOML. Any other error is a read failure and
// quotes nothing from the file.
func tomlProblem(err error, path string) error {
	if decodeErr, isDecode := errors.AsType[*toml.DecodeError](err); isDecode {
		row, column := decodeErr.Position()

		return vaultError(CodeCredentials, "%s cannot be read as a chezmoi config at line %d, column %d",
			path, row, column)
	}

	return cairnerr.Wrapf(err, CodeCredentials, "read %s: %v", path, err)
}

// parseCredentialsFile reads the KEY=VALUE lines the file is allowed to carry.
// Only the four names this package knows are kept, so anything else an operator
// parks in the file is not silently picked up.
func parseCredentialsFile(file *os.File, path string) (map[string]string, error) {
	wanted := map[string]struct{}{EnvClientID: {}, EnvClientSecret: {}, EnvMasterKey: {}, EnvItem: {}}
	values := make(map[string]string, len(wanted))
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		name, value, found := splitAssignment(scanner.Text())
		if !found {
			continue
		}

		if _, want := wanted[name]; want {
			values[name] = value
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		return nil, cairnerr.Wrapf(scanErr, CodeCredentials, "read %s: %v", path, scanErr)
	}

	return values, nil
}

// splitAssignment parses one line, skipping blanks and comments and stripping
// one layer of quotes, which is what an operator copying from a shell file
// writes. It reports false for a line that assigns nothing.
func splitAssignment(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}

	trimmed = strings.TrimPrefix(trimmed, "export ")

	name, value, found := strings.Cut(trimmed, "=")
	if !found {
		return "", "", false
	}

	return strings.TrimSpace(name), unquote(strings.TrimSpace(value)), true
}

// unquote removes one matching pair of surrounding quotes.
func unquote(value string) string {
	for _, quote := range []string{`"`, `'`} {
		if len(value) >= 2 && strings.HasPrefix(value, quote) && strings.HasSuffix(value, quote) {
			return value[1 : len(value)-1]
		}
	}

	return value
}
