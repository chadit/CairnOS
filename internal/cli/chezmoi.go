package cli

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pelletier/go-toml/v2"

	"github.com/chadit/CairnOS/internal/environ"
)

// chezmoiConfigPath is where chezmoi keeps its own config on a workstation. The
// operator already stores the same four values there to unlock the vault during
// a dotfile apply, so `secrets auth` offers them rather than asking for a second
// copy of what is already on the machine.
const chezmoiConfigPath = ".config/chezmoi/chezmoi.toml"

// homeEnvKey is the variable os.UserHomeDir reads on this platform. The
// injected environment has to be asked the same question the process
// environment would be.
func homeEnvKey() string {
	switch runtime.GOOS {
	case goosWindows:
		return "USERPROFILE"
	case "plan9":
		return "home"
	default:
		return "HOME"
	}
}

// homeDir resolves the operator's home from the injected environment, so a case
// can point one command at a staged home without moving the whole process.
func homeDir(lookup environ.Lookup) (string, error) {
	key := homeEnvKey()

	if home := lookup.Get(key); home != "" {
		return home, nil
	}

	return "", fmt.Errorf("%w: %s", errNoHomeDir, key)
}

// chezmoiDefaults holds the values chezmoi's config can supply. Empty strings
// mean chezmoi had nothing to say, not that the value is empty.
type chezmoiDefaults struct {
	itemName     string
	clientID     string
	clientSecret string
	masterKey    string
}

// hasSecrets reports whether any of the three credentials can be offered as a
// default, which decides whether the prompt explains where the defaults came
// from.
func (d chezmoiDefaults) hasSecrets() bool {
	return d.clientID != "" || d.clientSecret != "" || d.masterKey != ""
}

// chezmoiFile mirrors only the table `secrets auth` reads. Every other key in
// chezmoi's config is left alone, so a config that grows new sections still
// parses here.
type chezmoiFile struct {
	Data struct {
		Secret struct {
			ItemName     string `toml:"bw_item_name"`
			ClientID     string `toml:"bw_client_id"`
			ClientSecret string `toml:"bw_client_secret"`
			MasterKey    string `toml:"bw_master_key"`
		} `toml:"secret"`
	} `toml:"data"`
}

// readChezmoiDefaults returns what chezmoi's config offers, or nothing at all.
// A missing or unparsable file is not an error: the prompts still work, they
// just have nothing to pre-fill, and failing here would block an operator whose
// workstation tooling is unrelated to this host.
func readChezmoiDefaults(lookup environ.Lookup, logger *slog.Logger) chezmoiDefaults {
	home, err := homeDir(lookup)
	if err != nil {
		logger.Debug("no home directory for chezmoi defaults", "error", err)

		return chezmoiDefaults{}
	}

	path := filepath.Join(home, chezmoiConfigPath)

	data, err := os.ReadFile(path) // #nosec G304 -- a fixed path under the operator's own home
	if err != nil {
		logger.Debug("no chezmoi config to read defaults from", "path", path, "error", err)

		return chezmoiDefaults{}
	}

	var parsed chezmoiFile

	if err := toml.Unmarshal(data, &parsed); err != nil {
		logger.Debug("chezmoi config did not parse", "path", path, "error", err)

		return chezmoiDefaults{}
	}

	return chezmoiDefaults{
		itemName:     parsed.Data.Secret.ItemName,
		clientID:     parsed.Data.Secret.ClientID,
		clientSecret: parsed.Data.Secret.ClientSecret,
		masterKey:    parsed.Data.Secret.MasterKey,
	}
}
