package config_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
)

// telegramUserID is the sample allowlist key the bridge tests bind and assert
// against; a Telegram user ID is a numeric string. chatOnlyWorkspace is the
// second, chat-only user's workspace. The event names are the notify events
// the Discord and Mattermost cases assert on; envEvents is the comma list both
// env overrides carry.
const (
	telegramUserID    = "123456789"
	chatOnlyWorkspace = "ops"

	eventHeld         = "held"
	eventRunFailed    = "run_failed"
	eventDeadLettered = "dead_lettered"
	eventBudgetDenied = "budget_denied"
	envEvents         = "run_failed, budget_denied"
)

func TestLoadBridgeDefaults(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if cfg.Bridge.Telegram.Enabled {
		t.Error("bridge should default off so serve starts no long-poll loop")
	}

	if cfg.Bridge.Telegram.PollTimeout != 50*time.Second {
		t.Errorf("bridge poll_timeout default = %v, want 50s", cfg.Bridge.Telegram.PollTimeout)
	}

	if cfg.Bridge.Telegram.SendTimeout != 10*time.Second {
		t.Errorf("bridge send_timeout default = %v, want 10s", cfg.Bridge.Telegram.SendTimeout)
	}

	if cfg.Bridge.Telegram.ConfirmTTL != 10*time.Minute {
		t.Errorf("bridge confirm_ttl default = %v, want 10m", cfg.Bridge.Telegram.ConfirmTTL)
	}

	if cfg.Bridge.Telegram.Verbose {
		t.Error("bridge verbose should default off")
	}

	if len(cfg.Bridge.Telegram.Users) != 0 {
		t.Errorf("bridge users should default empty: %+v", cfg.Bridge.Telegram.Users)
	}
}

// TestLoadNotifyDefaultEventSetIsHeld pins the default Discord fan-out to exactly
// ["held"], which keeps today's re-authentication hold message byte-identical.
func TestLoadNotifyDefaultEventSetIsHeld(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if !slices.Equal(cfg.Notify.Discord.Events, []string{eventHeld}) {
		t.Errorf("notify.discord.events default = %v, want [held]", cfg.Notify.Discord.Events)
	}
}

func TestLoadBridgeBlockFromFile(t *testing.T) {
	t.Parallel()

	bridgeTOML := `
[bridge.telegram]
enabled = true
poll_timeout = "40s"
send_timeout = "8s"
confirm_ttl = "5m"
verbose = true

[bridge.telegram.users.123456789]
workspace = "default"
personality = "assistant"
allow_confirm = true

[bridge.telegram.users.987654321]
workspace = "ops"

[notify.discord]
events = ["held", "run_failed"]
`

	cfg, err := config.Load(writeConfig(t, bridgeTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Bridge.Telegram.Enabled || !cfg.Bridge.Telegram.Verbose {
		t.Fatalf("bridge flags not overlaid: %+v", cfg.Bridge.Telegram)
	}

	if cfg.Bridge.Telegram.PollTimeout != 40*time.Second || cfg.Bridge.Telegram.SendTimeout != 8*time.Second {
		t.Fatalf("bridge timeouts not overlaid: %+v", cfg.Bridge.Telegram)
	}

	if cfg.Bridge.Telegram.ConfirmTTL != 5*time.Minute {
		t.Fatalf("bridge confirm_ttl not overlaid: %v", cfg.Bridge.Telegram.ConfirmTTL)
	}

	granted, exists := cfg.Bridge.Telegram.Users[telegramUserID]
	if !exists {
		t.Fatalf("user %s missing: %+v", telegramUserID, cfg.Bridge.Telegram.Users)
	}

	if granted.Workspace != "default" || granted.Personality != "assistant" || !granted.AllowConfirm {
		t.Fatalf("granted user overlay wrong: %+v", granted)
	}

	// The second entry sets only workspace, so personality and allow_confirm keep
	// their zero-value defaults.
	chatOnly, exists := cfg.Bridge.Telegram.Users["987654321"]
	if !exists {
		t.Fatalf("second user missing: %+v", cfg.Bridge.Telegram.Users)
	}

	if chatOnly.Workspace != chatOnlyWorkspace || chatOnly.Personality != "" || chatOnly.AllowConfirm {
		t.Fatalf("chat-only user defaults wrong: %+v", chatOnly)
	}

	if !slices.Equal(cfg.Notify.Discord.Events, []string{eventHeld, eventRunFailed}) {
		t.Fatalf("notify.discord.events not overlaid: %v", cfg.Notify.Discord.Events)
	}
}

func TestLoadBridgeEnvOverridesBeatFile(t *testing.T) {
	t.Parallel()

	envOverrides := environ.Map(map[string]string{
		"CAIRNOS_BRIDGE_TELEGRAM_ENABLED":      envOn,
		"CAIRNOS_BRIDGE_TELEGRAM_POLL_TIMEOUT": envShortWait,
		"CAIRNOS_BRIDGE_TELEGRAM_SEND_TIMEOUT": envSendTimeout,
		"CAIRNOS_BRIDGE_TELEGRAM_CONFIRM_TTL":  envConfirmTTL,
		"CAIRNOS_BRIDGE_TELEGRAM_VERBOSE":      envOn,
		"CAIRNOS_NOTIFY_DISCORD_EVENTS":        envEvents,
	})

	// The file carries different values and one allowlisted user so the enabled
	// config still validates; the environment must then win over every key it sets.
	fileTOML := `
[bridge.telegram]
enabled = false
poll_timeout = "50s"
send_timeout = "10s"
confirm_ttl = "10m"
verbose = false

[bridge.telegram.users.123456789]
workspace = "default"

[notify.discord]
events = ["held"]
`

	cfg, err := config.LoadWith(writeConfig(t, fileTOML), envOverrides)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Bridge.Telegram.Enabled {
		t.Error("CAIRNOS_BRIDGE_TELEGRAM_ENABLED override did not win over the file")
	}

	if cfg.Bridge.Telegram.PollTimeout != 20*time.Second || cfg.Bridge.Telegram.SendTimeout != 3*time.Second {
		t.Fatalf("bridge timeout env overrides missing: %+v", cfg.Bridge.Telegram)
	}

	if cfg.Bridge.Telegram.ConfirmTTL != 2*time.Minute || !cfg.Bridge.Telegram.Verbose {
		t.Fatalf("bridge ttl/verbose env overrides missing: %+v", cfg.Bridge.Telegram)
	}

	if !slices.Equal(cfg.Notify.Discord.Events, []string{eventRunFailed, eventBudgetDenied}) {
		t.Fatalf("notify.discord.events env override missing: %v", cfg.Notify.Discord.Events)
	}

	// The users table is file-only, so the file binding survives the env overlay.
	if _, ok := cfg.Bridge.Telegram.Users[telegramUserID]; !ok {
		t.Fatalf("file user lost under env overlay: %+v", cfg.Bridge.Telegram.Users)
	}
}

// TestLoadBridgeSkipsValidationWhenDisabled keeps the secure-default off path
// usable: a bridge-disabled config with no users still loads, since the poll
// loop never starts.
func TestLoadBridgeSkipsValidationWhenDisabled(t *testing.T) {
	t.Parallel()

	if _, err := config.Load(writeConfig(t, "[bridge.telegram]\nenabled = false")); err != nil {
		t.Fatalf("bridge-off config with no users should load: %v", err)
	}
}

// TestLoadRejectsInvalidBridgeAndNotify covers the fail-fast validation: an
// enabled bridge with no users, a user with an empty workspace, and an unknown
// Discord event name are each config.invalid at load.
func TestLoadRejectsInvalidBridgeAndNotify(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
	}{
		{
			name: "enabled with zero users",
			toml: "[bridge.telegram]\nenabled = true",
		},
		{
			name: "empty workspace",
			toml: "[bridge.telegram]\nenabled = true\n\n[bridge.telegram.users.123456789]\nworkspace = \"\"",
		},
		{
			name: "unknown discord event",
			toml: "[notify.discord]\nevents = [\"held\", \"bogus\"]",
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

func TestLoadAcceptsEveryNotifyEvent(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t,
		"[notify.discord]\nevents = [\"held\", \"run_failed\", \"dead_lettered\", \"budget_denied\"]"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	want := []string{eventHeld, eventRunFailed, eventDeadLettered, eventBudgetDenied}
	if !slices.Equal(cfg.Notify.Discord.Events, want) {
		t.Fatalf("notify.discord.events = %v, want %v", cfg.Notify.Discord.Events, want)
	}
}

// TestLoadBridgeTokenIsNotAConfigKey documents the env-only posture: the bot
// token and a base_url are never config keys, so a file carrying
// them still loads with unknown keys ignored rather than binding a credential or
// an exfiltration-capable base URL into the config tree.
func TestLoadBridgeTokenIsNotAConfigKey(t *testing.T) {
	t.Parallel()

	tokenTOML := `
[bridge.telegram]
enabled = true
token = "super-secret-bot-token"
base_url = "https://attacker.example"

[bridge.telegram.users.123456789]
workspace = "default"
`

	cfg, err := config.Load(writeConfig(t, tokenTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Bridge.Telegram.Enabled {
		t.Fatalf("bridge should still enable with the unknown keys ignored: %+v", cfg.Bridge.Telegram)
	}
}
