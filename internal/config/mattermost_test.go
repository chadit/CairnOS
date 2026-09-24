package config_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
)

// identityOps is the sample identity name these tests declare and assert
// against; workspaceDefault is the workspace every sample identity stamps.
const (
	identityOps       = "ops"
	workspaceDefault  = "default"
	eventBudgetDenied = "budget_denied"
)

// Both fixtures declare one usable identity and end inside its table, so a case
// that breaks exactly one rule appends a single key instead of restating the
// block. teamedMattermostTOML is the variant for cases that name a #channel,
// since TOML will not let a case reopen [bridge.mattermost] to add the team.
const (
	validMattermostTOML = `
[bridge.mattermost]
url = "https://mm.example.com"

[bridge.mattermost.identity.ops]
username = "ops-agent"
workspace = "default"
`

	teamedMattermostTOML = `
[bridge.mattermost]
url  = "https://mm.example.com"
team = "main"

[bridge.mattermost.identity.ops]
username = "ops-agent"
workspace = "default"
`
)

func TestLoadMattermostDefaults(t *testing.T) {
	t.Parallel()

	mattermost := config.Default().Bridge.Mattermost

	if mattermost.URL != "" || mattermost.Team != "" || mattermost.InsecureHTTP {
		t.Errorf("mattermost server settings should default empty: %+v", mattermost)
	}

	if len(mattermost.Identity) != 0 {
		t.Errorf("mattermost identities should default empty so serve connects nothing: %+v", mattermost.Identity)
	}

	if mattermost.SendTimeout != 10*time.Second {
		t.Errorf("mattermost send_timeout default = %v, want 10s", mattermost.SendTimeout)
	}

	if mattermost.ConfirmTTL != 10*time.Minute {
		t.Errorf("mattermost confirm_ttl default = %v, want 10m", mattermost.ConfirmTTL)
	}

	if mattermost.RatePerSec != 10 || mattermost.RateBurst != 100 {
		t.Errorf("mattermost rate defaults = %d/%d, want 10/100", mattermost.RatePerSec, mattermost.RateBurst)
	}

	if mattermost.BotLoop.MaxEvents != 20 {
		t.Errorf("bot_loop max_events default = %d, want 20", mattermost.BotLoop.MaxEvents)
	}

	if mattermost.BotLoop.Window != 5*time.Minute || mattermost.BotLoop.Cooldown != 10*time.Minute {
		t.Errorf("bot_loop window/cooldown defaults = %v/%v, want 5m/10m", mattermost.BotLoop.Window, mattermost.BotLoop.Cooldown)
	}

	if mattermost.AckReaction != "eyes" {
		t.Errorf("ack_reaction default = %q, want eyes", mattermost.AckReaction)
	}
}

// TestLoadNotifyMattermostDefaults pins the sink to unconfigured with the same
// ["held"] event set Discord defaults to, so naming an identity and a channel
// is the only step between an operator and today's hold notice.
func TestLoadNotifyMattermostDefaults(t *testing.T) {
	t.Parallel()

	notify := config.Default().Notify.Mattermost

	if notify.Identity != "" || notify.Channel != "" {
		t.Errorf("notify.mattermost should default unconfigured: %+v", notify)
	}

	if !slices.Equal(notify.Events, []string{eventHeld}) {
		t.Errorf("notify.mattermost.events default = %v, want [held]", notify.Events)
	}
}

// fullMattermostTOML sets every key the block accepts, so the two tests below
// can each assert one half of the overlay without restating it.
const fullMattermostTOML = `
[bridge.mattermost]
url           = "https://mm.example.com"
team          = "main"
insecure_http = false
send_timeout  = "8s"
confirm_ttl   = "5m"
rate_per_sec  = 4
rate_burst    = 40
ack_reaction  = "white_check_mark"

[bridge.mattermost.bot_loop]
max_events = 7
window     = "90s"
cooldown   = "3m"

[bridge.mattermost.identity.ops]
username      = "ops-agent"
display_name  = "Ops Agent"
workspace     = "default"
personality   = "ops"
approvers     = ["alice"]
allow_bots_in = ["#agents"]

[bridge.mattermost.identity.release]
username  = "release-agent"
workspace = "releases"

[notify.mattermost]
identity = "ops"
channel  = "#alerts"
events   = ["held", "run_failed"]
`

func TestLoadMattermostServerBlockFromFile(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, fullMattermostTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	mattermost := cfg.Bridge.Mattermost

	if mattermost.URL != "https://mm.example.com" || mattermost.Team != "main" || mattermost.InsecureHTTP {
		t.Fatalf("mattermost server settings not overlaid: %+v", mattermost)
	}

	if mattermost.SendTimeout != 8*time.Second || mattermost.ConfirmTTL != 5*time.Minute {
		t.Fatalf("mattermost durations not overlaid: %+v", mattermost)
	}

	if mattermost.RatePerSec != 4 || mattermost.RateBurst != 40 {
		t.Fatalf("mattermost rate settings not overlaid: %+v", mattermost)
	}

	if mattermost.AckReaction != "white_check_mark" {
		t.Fatalf("mattermost ack_reaction = %q, want white_check_mark", mattermost.AckReaction)
	}

	if mattermost.BotLoop.MaxEvents != 7 {
		t.Fatalf("bot_loop max_events not overlaid: %+v", mattermost.BotLoop)
	}

	if mattermost.BotLoop.Window != 90*time.Second || mattermost.BotLoop.Cooldown != 3*time.Minute {
		t.Fatalf("bot_loop durations not overlaid: %+v", mattermost.BotLoop)
	}
}

func TestLoadMattermostIdentitiesFromFile(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, fullMattermostTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	ops, exists := cfg.Bridge.Mattermost.Identity[identityOps]
	if !exists {
		t.Fatalf("identity %s missing: %+v", identityOps, cfg.Bridge.Mattermost.Identity)
	}

	if ops.Username != "ops-agent" || ops.DisplayName != "Ops Agent" {
		t.Fatalf("identity names not overlaid: %+v", ops)
	}

	if ops.Workspace != workspaceDefault || ops.Personality != "ops" {
		t.Fatalf("identity workspace/personality not overlaid: %+v", ops)
	}

	if !slices.Equal(ops.Approvers, []string{"alice"}) || !slices.Equal(ops.AllowBotsIn, []string{"#agents"}) {
		t.Fatalf("identity lists not overlaid: %+v", ops)
	}

	// The second entry sets only the two required keys, so the rest keep their
	// zero values: a map entry replaces wholesale, it does not merge.
	release, exists := cfg.Bridge.Mattermost.Identity["release"]
	if !exists {
		t.Fatalf("second identity missing: %+v", cfg.Bridge.Mattermost.Identity)
	}

	if release.Workspace != "releases" || release.Personality != "" || len(release.Approvers) != 0 {
		t.Fatalf("second identity defaults wrong: %+v", release)
	}

	if cfg.Notify.Mattermost.Identity != identityOps || cfg.Notify.Mattermost.Channel != "#alerts" {
		t.Fatalf("notify.mattermost not overlaid: %+v", cfg.Notify.Mattermost)
	}

	if !slices.Equal(cfg.Notify.Mattermost.Events, []string{eventHeld, eventRunFailed}) {
		t.Fatalf("notify.mattermost.events not overlaid: %v", cfg.Notify.Mattermost.Events)
	}
}

// TestLoadMattermostEnvOverridesBeatFile walks every scalar override one at a
// time: each case starts from a file that sets a different value, so a binding
// missing from the env table shows up as the file value surviving.
func TestLoadMattermostEnvOverridesBeatFile(t *testing.T) {
	t.Parallel()

	fileTOML := `
[bridge.mattermost]
url           = "https://file.example.com"
team          = "file-team"
insecure_http = false
send_timeout  = "10s"
confirm_ttl   = "10m"
rate_per_sec  = 10
rate_burst    = 100

[bridge.mattermost.bot_loop]
max_events = 20
window     = "5m"
cooldown   = "10m"

[bridge.mattermost.identity.ops]
username  = "ops-agent"
workspace = "default"

[notify.mattermost]
identity = "ops"
channel  = "#alerts"
events   = ["held"]
`

	cases := []struct {
		got   func(config.Config) any
		want  any
		name  string
		env   string
		value string
	}{
		{
			name: "url", env: "CAIRNOS_BRIDGE_MATTERMOST_URL", value: envURL,
			got:  func(c config.Config) any { return c.Bridge.Mattermost.URL },
			want: envURL,
		},
		{
			name: "team", env: "CAIRNOS_BRIDGE_MATTERMOST_TEAM", value: "env-team",
			got:  func(c config.Config) any { return c.Bridge.Mattermost.Team },
			want: "env-team",
		},
		{
			name: "insecure http", env: "CAIRNOS_BRIDGE_MATTERMOST_INSECURE_HTTP", value: envOn,
			got:  func(c config.Config) any { return c.Bridge.Mattermost.InsecureHTTP },
			want: true,
		},
		{
			name: "send timeout", env: "CAIRNOS_BRIDGE_MATTERMOST_SEND_TIMEOUT", value: "3s",
			got:  func(c config.Config) any { return c.Bridge.Mattermost.SendTimeout },
			want: 3 * time.Second,
		},
		{
			name: "confirm ttl", env: "CAIRNOS_BRIDGE_MATTERMOST_CONFIRM_TTL", value: "2m",
			got:  func(c config.Config) any { return c.Bridge.Mattermost.ConfirmTTL },
			want: 2 * time.Minute,
		},
		{
			name: "rate per sec", env: "CAIRNOS_BRIDGE_MATTERMOST_RATE_PER_SEC", value: "3",
			got:  func(c config.Config) any { return c.Bridge.Mattermost.RatePerSec },
			want: 3,
		},
		{
			name: "rate burst", env: "CAIRNOS_BRIDGE_MATTERMOST_RATE_BURST", value: "33",
			got:  func(c config.Config) any { return c.Bridge.Mattermost.RateBurst },
			want: 33,
		},
		{
			name: "ack reaction", env: "CAIRNOS_BRIDGE_MATTERMOST_ACK_REACTION", value: "thumbsup",
			got:  func(c config.Config) any { return c.Bridge.Mattermost.AckReaction },
			want: "thumbsup",
		},
		{
			name: "bot loop max events", env: "CAIRNOS_BRIDGE_MATTERMOST_BOT_LOOP_MAX_EVENTS", value: "5",
			got:  func(c config.Config) any { return c.Bridge.Mattermost.BotLoop.MaxEvents },
			want: 5,
		},
		{
			name: "bot loop window", env: "CAIRNOS_BRIDGE_MATTERMOST_BOT_LOOP_WINDOW", value: "45s",
			got:  func(c config.Config) any { return c.Bridge.Mattermost.BotLoop.Window },
			want: 45 * time.Second,
		},
		{
			name: "bot loop cooldown", env: "CAIRNOS_BRIDGE_MATTERMOST_BOT_LOOP_COOLDOWN", value: "4m",
			got:  func(c config.Config) any { return c.Bridge.Mattermost.BotLoop.Cooldown },
			want: 4 * time.Minute,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.LoadWith(writeConfig(t, fileTOML), environ.Map(map[string]string{tt.env: tt.value}))
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			if got := tt.got(cfg); got != tt.want {
				t.Fatalf("%s = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

func TestLoadNotifyMattermostEventsEnvOverridesFile(t *testing.T) {
	t.Parallel()

	envOverrides := environ.Map(map[string]string{
		"CAIRNOS_NOTIFY_MATTERMOST_EVENTS": "run_failed, budget_denied",
	})

	fileTOML := teamedMattermostTOML + `
[notify.mattermost]
identity = "ops"
channel  = "#alerts"
events   = ["held"]
`

	cfg, err := config.LoadWith(writeConfig(t, fileTOML), envOverrides)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !slices.Equal(cfg.Notify.Mattermost.Events, []string{eventRunFailed, eventBudgetDenied}) {
		t.Fatalf("notify.mattermost.events env override missing: %v", cfg.Notify.Mattermost.Events)
	}
}

// twoIdentityTOML declares a second identity so a test can point the notify sink
// somewhere the file did not name.
const twoIdentityTOML = teamedMattermostTOML + `
[bridge.mattermost.identity.release]
username  = "release-agent"
workspace = "releases"

[notify.mattermost]
identity = "ops"
channel  = "#alerts"
`

// TestLoadNotifyMattermostTargetEnvOverridesFile moves the sink to a different
// identity and channel than the file names, which is how a deployment redirects
// alerts without editing the config file.
func TestLoadNotifyMattermostTargetEnvOverridesFile(t *testing.T) {
	t.Parallel()

	envOverrides := environ.Map(map[string]string{
		"CAIRNOS_NOTIFY_MATTERMOST_IDENTITY": "release",
		"CAIRNOS_NOTIFY_MATTERMOST_CHANNEL":  "#releases",
	})

	cfg, err := config.LoadWith(writeConfig(t, twoIdentityTOML), envOverrides)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Notify.Mattermost.Identity != "release" {
		t.Errorf("notify.mattermost.identity = %q, want release", cfg.Notify.Mattermost.Identity)
	}

	if cfg.Notify.Mattermost.Channel != "#releases" {
		t.Errorf("notify.mattermost.channel = %q, want #releases", cfg.Notify.Mattermost.Channel)
	}
}

// TestLoadRejectsUndeclaredNotifyIdentityFromEnv pins the overlay order: the
// override lands before validation, so an env pointing at an identity nobody
// declared fails at load rather than at the first fan-out.
func TestLoadRejectsUndeclaredNotifyIdentityFromEnv(t *testing.T) {
	t.Parallel()

	envOverrides := environ.Map(map[string]string{
		"CAIRNOS_NOTIFY_MATTERMOST_IDENTITY": "nope",
	})

	_, err := config.LoadWith(writeConfig(t, twoIdentityTOML), envOverrides)
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("error = %v, want ErrInvalidConfig", err)
	}
}

// TestLoadMattermostIdentitiesAreFileOnly documents the map's shape: a table
// has no single env form, so the file binding survives an env overlay exactly
// as the Telegram users table does.
func TestLoadMattermostIdentitiesAreFileOnly(t *testing.T) {
	t.Parallel()

	envOverrides := environ.Map(map[string]string{
		"CAIRNOS_BRIDGE_MATTERMOST_URL": envURL,
	})

	cfg, err := config.LoadWith(writeConfig(t, validMattermostTOML), envOverrides)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if _, exists := cfg.Bridge.Mattermost.Identity[identityOps]; !exists {
		t.Fatalf("file identity lost under env overlay: %+v", cfg.Bridge.Mattermost.Identity)
	}
}

// TestLoadMattermostSkipsValidationWithNoIdentity keeps the off-by-default path
// usable: with no identity declared serve connects nothing, so a missing url is
// not yet a problem.
func TestLoadMattermostSkipsValidationWithNoIdentity(t *testing.T) {
	t.Parallel()

	if _, err := config.Load(writeConfig(t, "[bridge.mattermost]\nteam = \"main\"")); err != nil {
		t.Fatalf("config with no identity should load: %v", err)
	}
}

func TestLoadRejectsInvalidMattermost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
	}{
		{
			name: "identity without url",
			toml: "[bridge.mattermost.identity.ops]\nusername = \"ops-agent\"\nworkspace = \"default\"",
		},
		{
			name: "url with no host",
			toml: "[bridge.mattermost]\nurl = \"https://\"\n\n[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"",
		},
		{
			name: "unsupported scheme",
			toml: "[bridge.mattermost]\nurl = \"ftp://mm.example.com\"\n\n[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"",
		},
		{
			name: "plain http without insecure_http",
			toml: "[bridge.mattermost]\nurl = \"http://mm.example.com\"\n\n[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"",
		},
		{
			name: "identity name outside the token-env pattern",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n[bridge.mattermost.identity.\"Ops-1\"]\nusername = \"a\"\nworkspace = \"w\"",
		},
		{
			// The name has to be one a shell can export, or the token it points
			// at can never be set.
			name: "token_env that is not a variable name",
			toml: validMattermostTOML + "token_env = \"bw token\"",
		},
		{
			name: "missing username",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n[bridge.mattermost.identity.ops]\nworkspace = \"w\"",
		},
		{
			name: "missing workspace",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n[bridge.mattermost.identity.ops]\nusername = \"a\"",
		},
		{
			name: "blank approver",
			toml: validMattermostTOML + "approvers = [\"alice\", \"  \"]",
		},
		{
			name: "blank allow_bots_in entry",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\nteam = \"main\"\n\n" +
				"[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"\nallow_bots_in = [\"\"]",
		},
		{
			name: "bare hash allow_bots_in entry",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\nteam = \"main\"\n\n" +
				"[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"\nallow_bots_in = [\"#\"]",
		},
		{
			name: "team missing while allow_bots_in names a channel",
			toml: validMattermostTOML + "allow_bots_in = [\"#agents\"]",
		},
		{
			name: "team missing while the notify channel names a channel",
			toml: validMattermostTOML + "\n[notify.mattermost]\nidentity = \"ops\"\nchannel = \"#alerts\"",
		},
		{
			name: "zero rate_per_sec",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\nrate_per_sec = 0\n\n" +
				"[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"",
		},
		{
			name: "negative rate_burst",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\nrate_burst = -10\n\n" +
				"[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"",
		},
		{
			name: "zero bot_loop.max_events",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n" +
				"[bridge.mattermost.bot_loop]\nmax_events = 0\n\n" +
				"[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"",
		},
		{
			name: "zero bot_loop.window",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n" +
				"[bridge.mattermost.bot_loop]\nwindow = \"0s\"\n\n" +
				"[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"",
		},
		{
			name: "ack_reaction wrapped in colons",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\nack_reaction = \":eyes:\"\n\n" +
				"[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"",
		},
		{
			name: "ack_reaction with uppercase and a space",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\nack_reaction = \"Thumbs Up\"\n\n" +
				"[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"",
		},
		{
			name: "zero bot_loop.cooldown",
			toml: "[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n" +
				"[bridge.mattermost.bot_loop]\ncooldown = \"0s\"\n\n" +
				"[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\"",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(writeConfig(t, tt.toml))
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// TestLoadMattermostEmptyAckReactionDisables proves an operator can switch the
// reaction off with an empty value rather than having it rejected as malformed.
func TestLoadMattermostEmptyAckReactionDisables(t *testing.T) {
	t.Parallel()

	offTOML := "[bridge.mattermost]\nurl = \"https://mm.example.com\"\nack_reaction = \"\"\n\n" +
		"[bridge.mattermost.identity.ops]\nusername = \"a\"\nworkspace = \"w\""

	cfg, err := config.Load(writeConfig(t, offTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Bridge.Mattermost.AckReaction != "" {
		t.Fatalf("ack_reaction = %q, want empty", cfg.Bridge.Mattermost.AckReaction)
	}
}

// TestLoadMattermostAcceptsOptedInHTTP covers the one escape hatch: an operator
// who sets insecure_http may point at a plain-http server.
func TestLoadMattermostAcceptsOptedInHTTP(t *testing.T) {
	t.Parallel()

	httpTOML := `
[bridge.mattermost]
url           = "http://mm.internal"
insecure_http = true

[bridge.mattermost.identity.ops]
username  = "ops-agent"
workspace = "default"
`

	cfg, err := config.Load(writeConfig(t, httpTOML))
	if err != nil {
		t.Fatalf("insecure_http config should load: %v", err)
	}

	if cfg.Bridge.Mattermost.URL != "http://mm.internal" {
		t.Fatalf("url = %q, want the http url", cfg.Bridge.Mattermost.URL)
	}
}

// TestLoadMattermostChannelIDsNeedNoTeam separates the two allow_bots_in forms:
// a raw channel id resolves without a team, so only a #name pulls team in.
func TestLoadMattermostChannelIDsNeedNoTeam(t *testing.T) {
	t.Parallel()

	idTOML := validMattermostTOML + "allow_bots_in = [\"jz5rf3k9ytfj5x9t8m3q4h1w7c\"]"

	if _, err := config.Load(writeConfig(t, idTOML)); err != nil {
		t.Fatalf("channel-id allow_bots_in should load without a team: %v", err)
	}
}

func TestLoadRejectsInvalidNotifyMattermost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
	}{
		{
			name: "unknown event",
			toml: "[notify.mattermost]\nevents = [\"held\", \"bogus\"]",
		},
		{
			name: "identity without channel",
			toml: validMattermostTOML + "\n[notify.mattermost]\nidentity = \"ops\"",
		},
		{
			// The team is set here so the missing identity is the only rule the
			// case breaks; without it the team check would fire first.
			name: "channel without identity",
			toml: teamedMattermostTOML + "\n[notify.mattermost]\nchannel = \"#alerts\"",
		},
		{
			name: "identity not declared",
			toml: teamedMattermostTOML + "\n[notify.mattermost]\nidentity = \"nope\"\nchannel = \"#alerts\"",
		},
		{
			name: "malformed channel",
			toml: teamedMattermostTOML + "\n[notify.mattermost]\nidentity = \"ops\"\nchannel = \"#\"",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(writeConfig(t, tt.toml))
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestLoadAcceptsEveryNotifyMattermostEvent(t *testing.T) {
	t.Parallel()

	eventsTOML := teamedMattermostTOML +
		"\n[notify.mattermost]\nidentity = \"ops\"\nchannel = \"#alerts\"\n" +
		"events = [\"held\", \"run_failed\", \"dead_lettered\", \"budget_denied\"]"

	cfg, err := config.Load(writeConfig(t, eventsTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	want := []string{eventHeld, eventRunFailed, "dead_lettered", eventBudgetDenied}
	if !slices.Equal(cfg.Notify.Mattermost.Events, want) {
		t.Fatalf("notify.mattermost.events = %v, want %v", cfg.Notify.Mattermost.Events, want)
	}
}

// TestLoadMattermostTokenIsNotAConfigKey proves a token written beside username
// fails the load and names the env var instead. Ignoring it gives the operator a
// clean start, a bridge reporting the env as unset, and a live token in a file
// that is probably committed (FR1).
func TestLoadMattermostTokenIsNotAConfigKey(t *testing.T) {
	t.Parallel()

	tokenTOML := validMattermostTOML + "token = \"super-secret-bot-token\""

	_, err := config.Load(writeConfig(t, tokenTOML))
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("error = %v, want ErrInvalidConfig", err)
	}
}

func TestMattermostTokenEnv(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		identity string
		want     string
	}{
		{name: "single word", identity: identityOps, want: "CAIRNOS_BRIDGE_MATTERMOST_OPS_TOKEN"},
		{name: "underscored", identity: "release_bot", want: "CAIRNOS_BRIDGE_MATTERMOST_RELEASE_BOT_TOKEN"},
		{name: "digits", identity: "ops2", want: "CAIRNOS_BRIDGE_MATTERMOST_OPS2_TOKEN"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := config.MattermostTokenEnv(tt.identity); got != tt.want {
				t.Fatalf("MattermostTokenEnv(%q) = %q, want %q", tt.identity, got, tt.want)
			}
		})
	}
}

// TestLoadMattermostKeepsTokenEnv covers the config key itself: the name an
// identity states is the one serve reads, with no fallback behind it.
func TestLoadMattermostKeepsTokenEnv(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, validMattermostTOML+"token_env = \""+secondBotTokenEnv+"\""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	identity := cfg.Bridge.Mattermost.Identity["ops"]
	if identity.TokenEnv != secondBotTokenEnv {
		t.Fatalf("token_env = %q, want the configured name", identity.TokenEnv)
	}

	envs := config.MattermostTokenEnvs(&identity, "ops")
	if len(envs) != 1 || envs[0] != secondBotTokenEnv {
		t.Fatalf("token envs = %v, want only the configured name", envs)
	}
}

// TestLoadMattermostWithoutTokenEnvTakesTheDefault is the ordinary host: one
// Mattermost server, one bot token, nothing to write in config.
func TestLoadMattermostWithoutTokenEnvTakesTheDefault(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, validMattermostTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	identity := cfg.Bridge.Mattermost.Identity["ops"]

	envs := config.MattermostTokenEnvs(&identity, "ops")
	if len(envs) != 2 || envs[1] != config.DefaultMattermostTokenEnv {
		t.Fatalf("token envs = %v, want the shared default last", envs)
	}
}
