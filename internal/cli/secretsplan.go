package cli

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/runner/builtins"
)

// inferredFieldPattern is the convention a field opts into by its own name. A
// field named exactly like the variable it fills needs no mapping, which is what
// keeps a host's config from listing the same string twice.
var inferredFieldPattern = regexp.MustCompile(`^CAIRNOS_[A-Z0-9_]+$`)

// resolveItem picks the vault item: the config block wins, the credentials file
// is the fallback. Two places on purpose, because the credentials file already
// has to exist on the host, and a host that never edits config.toml still gets
// a working pull out of it.
func resolveItem(block *config.Bitwarden, creds bitwarden.Credentials) (string, error) {
	if block.Item != "" {
		return block.Item, nil
	}

	if creds.Item != "" {
		return creds.Item, nil
	}

	return "", errNoVaultItem
}

// planValues decides what the generated file holds. Fields named like the
// variable they fill are picked up on their own; the config table adds the ones
// that are not, and wins a collision because an operator who wrote a mapping
// meant it.
func planValues(cfg *config.Config, item bitwarden.Item, table map[string]string) (map[string]string, error) {
	values := inferredValues(cfg, item)

	mapped, err := bitwarden.Select(item, slices.Sorted(maps.Values(table)))
	if err != nil {
		return nil, fmt.Errorf("read the mapped fields: %w", err)
	}

	for name, field := range table {
		values[name] = mapped[field]
	}

	if len(values) == 0 {
		return nil, fmt.Errorf("%w: %q", errNoFieldsOnItem, item.Name)
	}

	return values, nil
}

// inferredValues is everything the item offers without a mapping. A field named
// after a bot loses to one named after the variable, because a name that says
// which variable it fills is the more specific of the two.
func inferredValues(cfg *config.Config, item bitwarden.Item) map[string]string {
	values := inferUsernameValues(cfg, item)

	maps.Copy(values, inferValues(item))

	return values
}

// usernameTokenEnvs maps each Mattermost bot's username to the token variable
// its identity reads first, so one vault item can hold every bot's token under
// the bot's own name instead of a mapping per bot. Identities are walked in
// sorted order, so two that share a username land the same way on every host
// rather than following map order.
func usernameTokenEnvs(cfg *config.Config) map[string]string {
	identities := cfg.Bridge.Mattermost.Identity
	envs := make(map[string]string, len(identities))

	for _, name := range slices.Sorted(maps.Keys(identities)) {
		identity := identities[name]
		if identity.Username == "" {
			continue
		}

		if _, taken := envs[identity.Username]; !taken {
			envs[identity.Username] = config.MattermostTokenEnvs(&identity, name)[0]
		}
	}

	return envs
}

// inferUsernameValues keeps the fields named after a bot, each under the
// variable that bot's identity reads. An empty value is skipped for the reason
// inferValues skips one.
func inferUsernameValues(cfg *config.Config, item bitwarden.Item) map[string]string {
	envs := usernameTokenEnvs(cfg)
	values := make(map[string]string, len(envs))

	for username, env := range envs {
		if value := item.Fields[username]; value != "" {
			values[env] = value
		}
	}

	return values
}

// inferValues keeps the fields whose names are already variable names. An empty
// value is skipped for the same reason a mapped one fails: it would look
// configured in the generated file and behave as if it were not.
func inferValues(item bitwarden.Item) map[string]string {
	values := make(map[string]string, len(item.Fields))

	for name, value := range item.Fields {
		if value != "" && inferredFieldPattern.MatchString(name) {
			values[name] = value
		}
	}

	return values
}

// missingMattermost lists the token variables no pulled value satisfies. An
// identity is satisfied by any of the names it would read, and the one reported
// is the one it prefers, so the warning names the variable to add rather than
// the one a host happens to have.
func missingMattermost(cfg *config.Config, values map[string]string) []string {
	var missing []string

	for name := range cfg.Bridge.Mattermost.Identity {
		identity := cfg.Bridge.Mattermost.Identity[name]

		tried := config.MattermostTokenEnvs(&identity, name)
		if anyPresent(values, tried) {
			continue
		}

		missing = append(missing, tokenEnvToAdd(tried))
	}

	return missing
}

// tokenEnvToAdd names the variable a warning should ask for. The last name an
// identity reads is the shared default, which is what an operator should set;
// the per-identity names above it are overrides.
func tokenEnvToAdd(tried []string) string {
	return tried[len(tried)-1]
}

// missingRunnerKeys lists the API keys no pulled value satisfies, one per model
// runner pointed at an endpoint somebody else operates. A local model server
// needs no key, which is the same line the runner itself draws.
func missingRunnerKeys(cfg *config.Config, values map[string]string) []string {
	var missing []string

	for name := range cfg.Runners {
		tried := runnerKeyEnvs(cfg.Runners[name], name)
		if len(tried) == 0 || anyPresent(values, tried) {
			continue
		}

		missing = append(missing, tried[0])
	}

	return missing
}

// anyPresent reports whether the generated file already carries one of the
// names.
func anyPresent(values map[string]string, names []string) bool {
	for _, name := range names {
		if _, found := values[name]; found {
			return true
		}
	}

	return false
}

// runnerKeyEnvs returns the API-key variables one runner block reads, most
// specific first, or nothing when it reads none. The block's own name comes
// first, which is what lets two blocks of one type hold different vendors'
// keys, and the variable named after the type stays as the fallback.
func runnerKeyEnvs(block map[string]any, name string) []string {
	kind, _ := block["type"].(string)
	if kind == "" {
		kind = name
	}

	baseURL, _ := block["base_url"].(string)
	if baseURL != "" && builtins.LocalEndpoint(baseURL) {
		return nil
	}

	switch kind {
	case builtins.OpenAIName:
		return config.RunnerKeyEnvs(name, builtins.OpenAIKeyEnv)
	case builtins.AnthropicName:
		return config.RunnerKeyEnvs(name, builtins.AnthropicKeyEnv)
	default:
		return nil
	}
}

// missingImplied lists the credentials this configuration will want at run time
// that the vault item did not supply. It is a warning and never a failure: a
// host may well hold one of these in its hand-written env file, and a deploy
// that refused to proceed over that would be wrong more often than right.
func missingImplied(cfg *config.Config, values map[string]string) []string {
	missing := append(missingMattermost(cfg, values), missingRunnerKeys(cfg, values)...)

	slices.Sort(missing)

	return slices.Compact(missing)
}

// mattermostOverrideApplies reports whether any identity that is missing its
// token could take one from a per-identity variable instead. The warning says
// so, because a host that already exports the per-identity name needs no change
// at all.
func mattermostOverrideApplies(cfg *config.Config, values map[string]string) bool {
	for name := range cfg.Bridge.Mattermost.Identity {
		identity := cfg.Bridge.Mattermost.Identity[name]

		tried := config.MattermostTokenEnvs(&identity, name)
		if len(tried) > 1 && !anyPresent(values, tried) {
			return true
		}
	}

	return false
}

// joinNames renders a list for one output line.
func joinNames(names []string) string {
	return strings.Join(names, " ")
}
