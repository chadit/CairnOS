package service

import (
	"fmt"
	"os"
	"os/user"
	"runtime"
)

// DetectHost gathers the one snapshot of host facts every planner needs.
// Detection happens here and nowhere else, so the planners stay pure.
func DetectHost() (*Host, error) {
	current, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("resolve current user: %w", err)
	}

	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolve config dir: %w", err)
	}

	return &Host{
		GOOS:      runtime.GOOS,
		EUID:      os.Geteuid(),
		Username:  current.Username,
		UID:       current.Uid,
		HomeDir:   current.HomeDir,
		ConfigDir: configDir,
		// /run/systemd/system is the documented liveness marker for systemd;
		// the path check avoids talking to the manager just to detect it.
		HasSystemd: pathExists("/run/systemd/system"),
		HasUser: func(name string) bool {
			_, lookupErr := user.Lookup(name)

			return lookupErr == nil
		},
	}, nil
}

// pathExists reports whether a path is present, the only fact detection
// needs about it.
func pathExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
