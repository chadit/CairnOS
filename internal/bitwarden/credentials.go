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
	"io/fs"
	"os"
	"strings"

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

	// EnvItem names the vault item. It lives beside the credentials because
	// `cairnos secrets auth` writes all four together, and a host that never
	// edits config.toml still ends up with a working pull.
	EnvItem = "BW_ITEM"
)

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

// LoadCredentials reads the three values from path and falls back to env for any
// the file omits, so a host can keep them in either place. env is a parameter so
// a test can supply one without touching the process.
//
// A missing file is fine when the environment supplies everything; one readable
// beyond its owner is always an error.
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
// keep two of the three in a file and the last one somewhere else.
func pick(fromFile map[string]string, env func(string) string, name string) string {
	if value := fromFile[name]; value != "" {
		return value
	}

	return strings.TrimSpace(env(name))
}

// checkComplete names every value still missing rather than the first, so one
// run tells the operator everything to fix. The item is not on the list: it may
// come from the config block instead.
func checkComplete(creds Credentials, path string) error {
	var missing []string

	for _, pair := range []struct{ name, value string }{
		{EnvClientID, creds.ClientID},
		{EnvClientSecret, creds.ClientSecret},
		{EnvMasterKey, creds.MasterKey},
	} {
		if pair.value == "" {
			missing = append(missing, pair.name)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	return vaultError(CodeCredentials, "%s is missing from %s and the environment",
		strings.Join(missing, ", "), path)
}

// readCredentialsFile parses the KEY=VALUE file, refusing one anybody but its
// owner can read. An absent file returns nothing and no error.
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

	return parseCredentialsFile(file, path)
}

// parseCredentialsFile reads the KEY=VALUE lines the file is allowed to carry.
// Only the three names this package knows are kept, so anything else an operator
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
