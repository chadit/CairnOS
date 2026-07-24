package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Execer runs one supervisor command and returns its combined output, so
// tests swap in a recorder and Apply never talks to a real supervisor.
type Execer func(ctx context.Context, argv []string) (string, error)

// ExecCommand is the production Execer over os/exec. Argv comes from the
// compiled-in planners, never from workflow or network input.
func ExecCommand(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", ErrEmptyCommand
	}

	// #nosec G204 -- argv comes from the compiled-in planners (systemctl,
	// launchctl, useradd invocations), never from workflow or network input.
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)

	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("run %s: %w", argv[0], err)
	}

	return string(output), nil
}

// Apply materializes a plan in its fixed order (dirs, files, commands,
// removals, after) and returns the notices the operator should read:
// the plan's own plus one per optional command that failed.
func Apply(ctx context.Context, plan *Plan, run Execer) ([]string, error) {
	if err := applyDirs(plan.Dirs); err != nil {
		return nil, err
	}

	if err := applyFiles(plan.Files); err != nil {
		return nil, err
	}

	notices, err := runCommands(ctx, plan.Commands, run)
	if err != nil {
		return nil, err
	}

	if err := applyRemovals(plan.Removals); err != nil {
		return nil, err
	}

	afterNotices, err := runCommands(ctx, plan.After, run)
	if err != nil {
		return nil, err
	}

	notices = append(notices, afterNotices...)

	return append(notices, plan.Notices...), nil
}

// Describe renders a plan for --dry-run: every path, mode, and command,
// nothing executed.
func Describe(plan *Plan) string {
	var out strings.Builder

	fmt.Fprintf(&out, "plan: %s\n", plan.Label)

	for _, dir := range plan.Dirs {
		fmt.Fprintf(&out, "dir:  %s (%O)\n", dir.Path, dir.Perm)
	}

	for _, file := range plan.Files {
		var kept string
		if file.KeepExisting {
			kept = ", kept if present"
		}

		fmt.Fprintf(&out, "file: %s (%O%s)\n", file.Path, file.Perm, kept)
	}

	for _, command := range append(plan.Commands, plan.After...) {
		var optional string
		if command.Optional {
			optional = " (failure tolerated)"
		}

		fmt.Fprintf(&out, "run:  %s%s\n", strings.Join(command.Argv, " "), optional)
	}

	for _, path := range plan.Removals {
		fmt.Fprintf(&out, "rm:   %s\n", path)
	}

	return out.String()
}

// applyDirs creates every plan directory, parents included.
func applyDirs(dirs []Dir) error {
	for _, dir := range dirs {
		if err := os.MkdirAll(dir.Path, dir.Perm); err != nil {
			return fmt.Errorf("create %s: %w", dir.Path, err)
		}
	}

	return nil
}

// applyFiles writes the plan files, honoring KeepExisting so operator-edited
// content (the env file) survives a re-run untouched, permissions included.
func applyFiles(files []File) error {
	for _, file := range files {
		if file.KeepExisting {
			if _, err := os.Stat(file.Path); err == nil {
				continue
			}
		}

		if err := os.MkdirAll(filepath.Dir(file.Path), dirPerm); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(file.Path), err)
		}

		if err := os.WriteFile(file.Path, []byte(file.Content), file.Perm); err != nil {
			return fmt.Errorf("write %s: %w", file.Path, err)
		}
	}

	return nil
}

// applyRemovals deletes the plan's files, treating already-gone as done so
// uninstall stays idempotent.
func applyRemovals(paths []string) error {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	return nil
}

// runCommands executes one command list. A required failure stops the plan
// with the supervisor's output attached; an optional failure becomes a
// notice built from the command's hint.
func runCommands(ctx context.Context, commands []Command, run Execer) ([]string, error) {
	var notices []string

	for _, command := range commands {
		output, err := run(ctx, command.Argv)
		if err == nil {
			continue
		}

		if !command.Optional {
			return nil, fmt.Errorf("%w: %s: %w: %s",
				ErrCommandFailed, strings.Join(command.Argv, " "), err, strings.TrimSpace(output))
		}

		if command.Hint != "" {
			notices = append(notices, command.Hint)
		}
	}

	return notices, nil
}
