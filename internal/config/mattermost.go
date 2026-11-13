package config

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Mattermost configures the Mattermost bridge: one WebSocket connection and one
// env-only bot token per identity (see MattermostTokenEnv).
//
// The server URL is an operator key here, unlike [Telegram]: Telegram's token
// rides the request path, so a redirectable base URL would leak it, while
// Mattermost sends the token in a header. https is still required
// unless InsecureHTTP is set.
type Mattermost struct {
	// Identity is the declared agent set keyed by identity name. Empty means the
	// bridge is off and serve opens no connection. The map has no env form; it is
	// file-only.
	Identity map[string]MattermostIdentity

	// URL is the Mattermost server root, such as https://mm.example.com. Required
	// once an identity is declared; serve appends the /api/v4 paths itself.
	URL string

	// Team is the team slug #channel-name references resolve against. Required
	// when any allow_bots_in entry or the notify channel names a channel by name
	// rather than by id, because the lookup endpoint is team-scoped.
	Team string

	// AckReaction is the emoji an identity puts on a post it accepted, so the
	// sender sees it was picked up before the reply lands. Empty turns it off.
	AckReaction string

	// SendTimeout bounds each outbound REST call.
	SendTimeout time.Duration

	// ConfirmTTL is how long an unanswered workflow proposal stays pending before it
	// expires as a deny.
	ConfirmTTL time.Duration

	// RatePerSec and RateBurst size the shared client-side token bucket. It is
	// per server rather than per identity because Mattermost keys its own rate
	// limit by source IP, so per-identity buckets would still collide.
	RatePerSec int
	RateBurst  int

	// BotLoop bounds how much bot-to-bot traffic one conversation may generate.
	BotLoop BotLoop

	// InsecureHTTP permits a plain http URL. Off by default: a bearer token over
	// http is readable by anything on the path, so an operator has to say
	// explicitly that the server is reachable only over a trusted network.
	InsecureHTTP bool
}

// MattermostIdentity is one agent account: the Mattermost bot user it posts as,
// the workspace its turns are stamped with, and the two grants that widen what
// it will answer. There is no sender allowlist for chat, so Approvers and
// AllowBotsIn are the only operator-set gates.
type MattermostIdentity struct {
	// Username is the bot account's Mattermost username. It must match what
	// GET /users/me returns for the identity's token; serve cross-checks at
	// connect rather than trusting this value. Required.
	Username string

	// DisplayName is the name advertised on the A2A card. The Mattermost profile
	// name is set in Mattermost itself; CairnOS never modifies a bot account.
	DisplayName string

	// Workspace is the workspace every turn this identity handles is stamped
	// with, exactly as the gRPC interceptor would. Required.
	Workspace string

	// Personality names the chat personality a new session for this identity
	// gets. Empty falls back to [chat] default_personality.
	Personality string

	// TokenEnv names the variable this identity's bot token comes from, for the
	// case one CairnOS talks to a Mattermost server as two different bots. Empty
	// takes the shared default (see MattermostTokenEnvs).
	TokenEnv string

	// Approvers lists the Mattermost usernames allowed to approve a workflow proposal.
	// Empty means nobody can, which leaves every proposal to expire as a deny:
	// chat access does not imply workflow-approval authority.
	Approvers []string

	// AllowBotsIn lists the channels where a bot-authored post still earns a
	// reply, as #channel-name or as a raw channel id. Everywhere else a post with
	// bot provenance is dropped, which is what stops two agents talking forever.
	AllowBotsIn []string
}

// BotLoop bounds bot-to-bot traffic in the channels AllowBotsIn opens up.
// Provenance stops a loop from starting; this catches the loops provenance was
// told to allow.
type BotLoop struct {
	// MaxEvents is how many bot-authored posts one conversation may have answered
	// within Window before the identity stops replying there.
	MaxEvents int

	// Window is the span MaxEvents is counted over.
	Window time.Duration

	// Cooldown is how long a tripped conversation stays unanswered.
	Cooldown time.Duration
}

// NotifyMattermost selects the engine and queue events that fan out to a
// Mattermost channel. The sink is off until both Identity and Channel are set;
// Events then selects which of the fan-out events reach it.
type NotifyMattermost struct {
	// Identity names the bridge identity that posts the line. It must be declared
	// under [bridge.mattermost.identity].
	Identity string

	// Channel is the destination, as #channel-name or a raw channel id.
	Channel string

	// Events is the selected event set. Every value must be one of held,
	// run_failed, dead_lettered, budget_denied, connectivity, source_dirty, or
	// serve_failed.
	Events []string
}

// Mattermost bridge defaults. The rate bucket matches Mattermost's own
// per-IP default posture closely enough to stay under it, and the bot loop
// tolerates a short agent-to-agent exchange while cutting off a runaway one.
// Timing that has the same meaning as the Telegram bridge's shares its
// constant, so the two transports cannot drift apart by accident.
const (
	defaultMattermostRatePerSec       = 10
	defaultMattermostRateBurst        = 100
	defaultMattermostBotLoopMaxEvents = 20
	defaultMattermostBotLoopWindow    = 5 * time.Minute
	defaultMattermostBotLoopCooldown  = 10 * time.Minute

	// defaultMattermostAckReaction is the eyes emoji, which reads as "seen, on
	// it" without promising an outcome.
	defaultMattermostAckReaction = "eyes"
)

// mattermostIdentityPattern constrains identity names so uppercasing one always
// yields a legal environment variable name. Without it, an identity named
// "ops-1" or "Ops" would derive a token env nobody can export reliably.
const mattermostIdentityPattern = `^[a-z][a-z0-9_]*$`

var mattermostIdentityName = regexp.MustCompile(mattermostIdentityPattern)

// mattermostEmojiPattern is the short-name alphabet Mattermost accepts for an
// emoji, so a typo or a pasted ":eyes:" fails the load rather than every reaction.
const mattermostEmojiPattern = `^[a-z0-9_+-]+$`

var mattermostEmojiName = regexp.MustCompile(mattermostEmojiPattern)

// DefaultMattermostTokenEnv is where a bot token comes from when nothing says
// otherwise. One Mattermost server is the ordinary case and every identity on
// it can post as the same bot, so the token is not per-identity by default.
const DefaultMattermostTokenEnv = envPrefix + "BRIDGE_MATTERMOST_TOKEN" // #nosec G101 -- a variable name, not a credential

// MattermostTokenEnv returns the per-identity variable name. It stays the
// second name tried rather than the first, so a host that already exports one
// keeps working, and a host that does not never has to invent one.
func MattermostTokenEnv(name string) string {
	return envPrefix + "BRIDGE_MATTERMOST_" + strings.ToUpper(name) + "_TOKEN"
}

// MattermostTokenEnvs lists the variables one identity's token may come from,
// most specific first. An explicit token_env is the whole list: an operator who
// named the variable meant that one, and falling back from it would hide a typo
// behind another bot's token. A nil identity declared nothing, so it gets the
// same answer as one that set no token_env.
func MattermostTokenEnvs(identity *MattermostIdentity, name string) []string {
	if identity != nil && identity.TokenEnv != "" {
		return []string{identity.TokenEnv}
	}

	return []string{MattermostTokenEnv(name), DefaultMattermostTokenEnv}
}

// ResolveMattermostToken returns the token and the variable it came from, or
// empty strings when none of the names carries one.
func ResolveMattermostToken(
	identity *MattermostIdentity, name string, getenv func(string) string,
) (string, string) {
	for _, env := range MattermostTokenEnvs(identity, name) {
		if token := getenv(env); token != "" {
			return token, env
		}
	}

	return "", ""
}

// validateMattermost fails fast when the bridge is declared but unusable. With
// no identity there is nothing to connect, so the block is skipped. The chat
// cross-check lives at serve wiring, matching the Telegram bridge.
//
// notifyChannel shares the team-scoped #name lookup, so it decides alongside
// allow_bots_in whether team is required. Names are sorted so the reported
// entry is reproducible.
func validateMattermost(mattermost *Mattermost, notifyChannel string) error {
	if len(mattermost.Identity) == 0 {
		return nil
	}

	if err := validateMattermostURL(mattermost); err != nil {
		return err
	}

	if err := validateMattermostLimits(mattermost); err != nil {
		return err
	}

	if mattermost.AckReaction != "" && !mattermostEmojiName.MatchString(mattermost.AckReaction) {
		return fmt.Errorf("%w: bridge.mattermost.ack_reaction %q must be an emoji short name matching %s",
			ErrInvalidConfig, mattermost.AckReaction, mattermostEmojiPattern)
	}

	names := make([]string, 0, len(mattermost.Identity))
	for name := range mattermost.Identity {
		names = append(names, name)
	}

	slices.Sort(names)

	needsTeam := strings.HasPrefix(notifyChannel, "#")

	for _, name := range names {
		identity := mattermost.Identity[name]
		if err := validateMattermostIdentity(name, &identity); err != nil {
			return err
		}

		needsTeam = needsTeam || namesAChannel(identity.AllowBotsIn)
	}

	if needsTeam && mattermost.Team == "" {
		return fmt.Errorf("%w: bridge.mattermost.team must be set when a channel is named as #name", ErrInvalidConfig)
	}

	return nil
}

// Config keys of the bot-loop spans, which both the duration parse and the
// positive-value check name in their errors.
const (
	keyBotLoopWindow   = "bridge.mattermost.bot_loop.window"
	keyBotLoopCooldown = "bridge.mattermost.bot_loop.cooldown"
)

// validateMattermostLimits rejects a non-positive tuning value. NewBucket and
// NewLoopLimiter coerce anything at or below zero back to their own defaults, so
// rate_per_sec = 0 meaning "do not throttle" silently becomes ten per second and
// a typo'd -10 behaves exactly like a correct config.
func validateMattermostLimits(mattermost *Mattermost) error {
	counts := map[string]int{
		"bridge.mattermost.rate_per_sec":        mattermost.RatePerSec,
		"bridge.mattermost.rate_burst":          mattermost.RateBurst,
		"bridge.mattermost.bot_loop.max_events": mattermost.BotLoop.MaxEvents,
	}

	spans := map[string]time.Duration{
		keyBotLoopWindow:   mattermost.BotLoop.Window,
		keyBotLoopCooldown: mattermost.BotLoop.Cooldown,
	}

	for _, key := range slices.Sorted(maps.Keys(counts)) {
		if counts[key] <= 0 {
			return fmt.Errorf("%w: %s must be greater than zero", ErrInvalidConfig, key)
		}
	}

	for _, key := range slices.Sorted(maps.Keys(spans)) {
		if spans[key] <= 0 {
			return fmt.Errorf("%w: %s must be greater than zero", ErrInvalidConfig, key)
		}
	}

	return nil
}

// rejectMattermostToken fails a config file that puts a bot token beside the
// identity's other keys, where it looks like it belongs. Silence is the worst of
// the three possible answers: the operator gets a clean start, the bridge then
// reports the token env as unset, and a live token sits in a file that is
// probably committed.
func rejectMattermostToken(identities map[string]fileMattermostIdentity) error {
	for _, name := range slices.Sorted(maps.Keys(identities)) {
		if identities[name].Token != nil {
			return fmt.Errorf("%w: bridge.mattermost.identity.%s.token is not a config key; "+
				"set %s, or %s for this identity alone",
				ErrInvalidConfig, name, DefaultMattermostTokenEnv, MattermostTokenEnv(name))
		}
	}

	return nil
}

// validateMattermostURL rejects a URL the client cannot use or should not trust.
func validateMattermostURL(mattermost *Mattermost) error {
	if mattermost.URL == "" {
		return fmt.Errorf("%w: bridge.mattermost.url must be set when an identity is declared", ErrInvalidConfig)
	}

	parsed, err := url.Parse(mattermost.URL)
	if err != nil {
		return fmt.Errorf("%w: bridge.mattermost.url %q is not a URL: %w", ErrInvalidConfig, mattermost.URL, err)
	}

	if parsed.Host == "" {
		return fmt.Errorf("%w: bridge.mattermost.url %q names no host", ErrInvalidConfig, mattermost.URL)
	}

	if parsed.Scheme == schemeHTTPS {
		return nil
	}

	if parsed.Scheme == schemeHTTP && mattermost.InsecureHTTP {
		return nil
	}

	return fmt.Errorf("%w: bridge.mattermost.url scheme %q must be https, or http with insecure_http = true",
		ErrInvalidConfig, parsed.Scheme)
}

// validateMattermostIdentity checks one identity entry: a name the token env is
// derivable from, the two required fields, and well-formed grant lists.
func validateMattermostIdentity(name string, identity *MattermostIdentity) error {
	if !mattermostIdentityName.MatchString(name) {
		return fmt.Errorf("%w: bridge.mattermost.identity.%s: the name must match %s so the token env name is derivable",
			ErrInvalidConfig, name, mattermostIdentityPattern)
	}

	if identity.Username == "" {
		return fmt.Errorf("%w: bridge.mattermost.identity.%s.username must be set", ErrInvalidConfig, name)
	}

	if identity.Workspace == "" {
		return fmt.Errorf("%w: bridge.mattermost.identity.%s.workspace must be set", ErrInvalidConfig, name)
	}

	if identity.TokenEnv != "" && !envNamePattern.MatchString(identity.TokenEnv) {
		return fmt.Errorf("%w: bridge.mattermost.identity.%s.token_env %q is not a variable name matching %s",
			ErrInvalidConfig, name, identity.TokenEnv, envNamePattern)
	}

	for index, approver := range identity.Approvers {
		if strings.TrimSpace(approver) == "" {
			return fmt.Errorf("%w: bridge.mattermost.identity.%s.approvers[%d] must be a Mattermost username",
				ErrInvalidConfig, name, index)
		}
	}

	for index, channel := range identity.AllowBotsIn {
		if !validChannelRef(channel) {
			return fmt.Errorf("%w: bridge.mattermost.identity.%s.allow_bots_in[%d] %q must be a #channel-name or a channel id",
				ErrInvalidConfig, name, index, channel)
		}
	}

	return nil
}

// validateNotifyMattermost checks the fan-out sink. Identity and channel are
// declared together or not at all: either one alone is a half-configured sink
// that would silently post nowhere.
func validateNotifyMattermost(notify *NotifyMattermost, identities map[string]MattermostIdentity) error {
	if notify.Identity == "" && notify.Channel == "" {
		return nil
	}

	if notify.Identity == "" {
		return fmt.Errorf("%w: notify.mattermost.identity must be set alongside notify.mattermost.channel", ErrInvalidConfig)
	}

	if notify.Channel == "" {
		return fmt.Errorf("%w: notify.mattermost.channel must be set alongside notify.mattermost.identity", ErrInvalidConfig)
	}

	if _, declared := identities[notify.Identity]; !declared {
		return fmt.Errorf("%w: notify.mattermost.identity %q is not declared under bridge.mattermost.identity",
			ErrInvalidConfig, notify.Identity)
	}

	if !validChannelRef(notify.Channel) {
		return fmt.Errorf("%w: notify.mattermost.channel %q must be a #channel-name or a channel id",
			ErrInvalidConfig, notify.Channel)
	}

	return nil
}

// namesAChannel reports whether any entry uses the #name form, which is the
// form that needs a team to resolve.
func namesAChannel(refs []string) bool {
	for _, ref := range refs {
		if strings.HasPrefix(ref, "#") {
			return true
		}
	}

	return false
}

// validChannelRef accepts either a #channel-name or a raw channel id. It does
// not check an id's length: Mattermost's 26-character id format is a server
// detail, and a wrong-length id fails the lookup with a clear 404 rather than a
// confusing config error.
func validChannelRef(ref string) bool {
	if strings.ContainsAny(ref, " \t\r\n") {
		return false
	}

	return strings.TrimPrefix(ref, "#") != ""
}

// fileMattermost mirrors the [bridge.mattermost] table. Scalars are pointers so
// an unset key keeps its default; the identity table is a map of plain-value
// entries, matching fileBridgeUser. There is no token field: the token is
// env-only.
type fileMattermost struct {
	URL          *string                           `toml:"url"`
	Team         *string                           `toml:"team"`
	InsecureHTTP *bool                             `toml:"insecure_http"`
	SendTimeout  *string                           `toml:"send_timeout"`
	ConfirmTTL   *string                           `toml:"confirm_ttl"`
	RatePerSec   *int                              `toml:"rate_per_sec"`
	RateBurst    *int                              `toml:"rate_burst"`
	AckReaction  *string                           `toml:"ack_reaction"`
	BotLoop      fileBotLoop                       `toml:"bot_loop"`
	Identity     map[string]fileMattermostIdentity `toml:"identity"`
}

type fileBotLoop struct {
	MaxEvents *int    `toml:"max_events"`
	Window    *string `toml:"window"`
	Cooldown  *string `toml:"cooldown"`
}

// fileMattermostIdentity mirrors one [bridge.mattermost.identity.<name>] table.
// Each entry replaces wholesale (there is no per-identity default to merge
// with), so plain values stand in for the pointer-optional pattern.
type fileMattermostIdentity struct {
	// Token exists only to be rejected. pelletier/go-toml drops an unknown key
	// without a word, and turning on DisallowUnknownFields would break every
	// other block in this file, so a declared field is the only way a token
	// written beside username fails the load rather than being ignored.
	Token *string `toml:"token"`

	Username    string   `toml:"username"`
	DisplayName string   `toml:"display_name"`
	Workspace   string   `toml:"workspace"`
	Personality string   `toml:"personality"`
	TokenEnv    string   `toml:"token_env"`
	Approvers   []string `toml:"approvers"`
	AllowBotsIn []string `toml:"allow_bots_in"`
}

type fileNotifyMattermost struct {
	Identity *string  `toml:"identity"`
	Channel  *string  `toml:"channel"`
	Events   []string `toml:"events"`
}

// convertMattermostIdentities turns the file layout's identity tables into the
// resolved map.
func convertMattermostIdentities(identities map[string]fileMattermostIdentity) map[string]MattermostIdentity {
	resolved := make(map[string]MattermostIdentity, len(identities))
	for name := range identities {
		identity := identities[name]

		resolved[name] = MattermostIdentity{
			Username:    identity.Username,
			DisplayName: identity.DisplayName,
			Workspace:   identity.Workspace,
			Personality: identity.Personality,
			TokenEnv:    strings.TrimSpace(identity.TokenEnv),
			Approvers:   identity.Approvers,
			AllowBotsIn: identity.AllowBotsIn,
		}
	}

	return resolved
}
