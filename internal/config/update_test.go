package config_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
)

// TestLoadUpdateDefaultsOff pins the posture a host gets without an [update]
// block: the loop stays off, and the interval it would use is the quarter hour
// the README documents.
func TestLoadUpdateDefaultsOff(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Update.Auto {
		t.Error("update.auto defaults on; a host must opt in to restarting itself")
	}

	if cfg.Update.Interval != 15*time.Minute {
		t.Errorf("update.interval default = %s, want 15m", cfg.Update.Interval)
	}
}

func TestLoadUpdateBlockFromFile(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, "[update]\nauto = true\ninterval = \"1d\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Update.Auto {
		t.Error("update.auto = false, want the file's true")
	}

	if cfg.Update.Interval != 24*time.Hour {
		t.Errorf("update.interval = %s, want the file's 1d", cfg.Update.Interval)
	}
}

// TestLoadUpdateKeepsTheDefaultInterval covers the block most hosts write: the
// switch alone, with the interval left to the default.
func TestLoadUpdateKeepsTheDefaultInterval(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, "[update]\nauto = true\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Update.Interval != 15*time.Minute {
		t.Errorf("update.interval = %s, want the 15m default", cfg.Update.Interval)
	}
}

func TestLoadUpdateEnvOverridesBeatFile(t *testing.T) {
	t.Parallel()

	overrides := environ.Map(map[string]string{
		"CAIRNOS_UPDATE_AUTO":     "true",
		"CAIRNOS_UPDATE_INTERVAL": "1h",
	})

	cfg, err := config.LoadWith(writeConfig(t, "[update]\nauto = false\ninterval = \"5m\"\n"), overrides)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Update.Auto {
		t.Error("update.auto = false, want the environment's true")
	}

	if cfg.Update.Interval != time.Hour {
		t.Errorf("update.interval = %s, want the environment's 1h", cfg.Update.Interval)
	}
}

// TestLoadRejectsAZeroIntervalFromTheEnvironment pins that validation runs
// after the overrides, so the environment cannot slip in what the file may not.
func TestLoadRejectsAZeroIntervalFromTheEnvironment(t *testing.T) {
	t.Parallel()

	overrides := environ.Map(map[string]string{"CAIRNOS_UPDATE_INTERVAL": "0s"})

	_, err := config.LoadWith(filepath.Join(t.TempDir(), "absent.toml"), overrides)
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("load error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadRejectsInvalidUpdateBlock(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
	}{
		{name: "an unknown key", toml: "[update]\natuo = true\n"},
		{name: "a zero interval", toml: "[update]\ninterval = \"0s\"\n"},
		{name: "a negative interval", toml: "[update]\ninterval = \"-5m\"\n"},
		{name: "an interval that is not a duration", toml: "[update]\ninterval = \"soon\"\n"},
		{name: "an interval written as a number", toml: "[update]\ninterval = 15\n"},
		{name: "auto written as a string", toml: "[update]\nauto = \"yes\"\n"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(writeConfig(t, testCase.toml))
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("load error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
