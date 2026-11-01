package builtins_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/source"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Fixture values for the trusted-source gate: an action no allow list names,
// the workflow paths inside the checkout, and the prompt one workflow feeds to
// its command through stdin_file.
const (
	unlistedAction  = "deploy"
	committedRel    = "engines/garmin/cairnos/sync.toml"
	editedRel       = "engines/mail/cairnos/digest.toml"
	plantedRel      = "engines/planted/cairnos/run.toml"
	localRel        = "engines/local/cairnos/run.toml"
	escapeRel       = "engines/escape/cairnos/run.toml"
	includingRel    = "engines/shell/cairnos/run.toml"
	scriptRel       = "engines/shell/cairnos/script.sh"
	scriptBody      = "echo committed\n"
	trustedFileMode = 0o600
	trustedDirMode  = 0o750
	trustedRef      = "main"
)

// workflowBody runs one exec action named unlistedAction. The command goes
// through PATH, since true lives in /usr/bin on macOS and /bin elsewhere.
const workflowBody = `[workflow]
name = "sync"

[action.deploy]
runner = "exec"
params = { command = "true" }
`

// includingBody is workflowBody feeding scriptRel to the command's stdin.
const includingBody = `[workflow]
name = "shell"

[action.deploy]
runner = "exec"
params = { command = "true", stdin_file = "script.sh" }
`

// isolatedGit runs git under an empty global and system config, so the
// developer's own signing or hooks cannot change what the fixture commits, with
// an identity in the environment so a commit needs no config at all.
func isolatedGit() source.Git {
	return source.GitCommandWith([]string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_AUTHOR_NAME=Fixture",
		"GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Fixture",
		"GIT_COMMITTER_EMAIL=fixture@example.invalid",
	})
}

// runGit runs one fixture command and stops the case on failure.
func runGit(t *testing.T, dir string, argv ...string) {
	t.Helper()

	if _, err := isolatedGit()(t.Context(), dir, argv...); err != nil {
		t.Fatalf("git %v: %v", argv, err)
	}
}

// writeTrustedFile writes one file under root, creating its directory.
func writeTrustedFile(t *testing.T, root, rel, body string) string {
	t.Helper()

	path := filepath.Join(root, filepath.FromSlash(rel))

	if err := os.MkdirAll(filepath.Dir(path), trustedDirMode); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(body), trustedFileMode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}

// trustedCheckout pushes three workflows and a script to an origin and clones
// it. The clone then drifts the ways a host can drift: one workflow edited in
// place, one only on disk, one committed locally and never pushed, the script
// edited, and a symlink pointing outside the checkout at identical bytes.
func trustedCheckout(t *testing.T) source.Spec {
	t.Helper()

	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	dest := filepath.Join(root, "checkout")

	runGit(t, root, "init", "--bare", "--initial-branch=main", bare)
	runGit(t, root, "init", "--initial-branch=main", work)
	writeTrustedFile(t, work, committedRel, workflowBody)
	writeTrustedFile(t, work, editedRel, workflowBody)
	writeTrustedFile(t, work, includingRel, includingBody)
	writeTrustedFile(t, work, scriptRel, scriptBody)
	runGit(t, work, "add", "--", committedRel, editedRel, includingRel, scriptRel)
	runGit(t, work, "commit", "-m", "workflows")
	runGit(t, work, "push", bare, "main")
	runGit(t, root, "clone", "--", bare, dest)

	writeTrustedFile(t, dest, localRel, workflowBody)
	runGit(t, dest, "add", "--", localRel)
	runGit(t, dest, "commit", "-m", "planted")

	writeTrustedFile(t, dest, editedRel, workflowBody+"# edited on the host\n")
	writeTrustedFile(t, dest, plantedRel, workflowBody)

	outside := writeTrustedFile(t, t.TempDir(), "run.toml", workflowBody)
	link := filepath.Join(dest, filepath.FromSlash(escapeRel))

	if err := os.MkdirAll(filepath.Dir(link), trustedDirMode); err != nil {
		t.Fatalf("create link dir: %v", err)
	}

	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	return source.Spec{Name: "automation", Dest: dest, Ref: trustedRef}
}

// trustedExec builds an exec runner from cfg whose trust covers specs.
func trustedExec(t *testing.T, cfg map[string]any, git source.Git, specs ...source.Spec) *builtins.Exec {
	t.Helper()

	built, err := builtins.ExecFactoryWithHostEnv(nil, nil, builtins.ExecTrust{Git: git, Sources: specs})(cfg)
	if err != nil {
		t.Fatalf("build exec: %v", err)
	}

	gate, isExec := built.(*builtins.Exec)
	if !isExec {
		t.Fatalf("exec factory built %T, want *builtins.Exec", built)
	}

	return gate
}

// loadedTask loads the workflow at path the way a run does and returns the
// task for its unlistedAction, carrying the definition Load recorded.
func loadedTask(t *testing.T, path string) *runner.Task {
	t.Helper()

	flow, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}

	action := flow.Actions[unlistedAction]

	return &runner.Task{
		RunID:      testRunID,
		ActionName: unlistedAction,
		Definition: action.Definition,
		Params:     action.Params,
		Resolved:   true,
	}
}

// wantDenied asserts err is the gate's denial with the given retry class.
func wantDenied(t *testing.T, err error, class runner.ErrorClass) {
	t.Helper()

	classified := classifiedError(t, err)
	if classified.Code != codeExecDenied || classified.Class != class {
		t.Fatalf("error = %s/%v, want %s/%v (%v)", classified.Code, classified.Class, codeExecDenied, class, err)
	}
}

func TestExecTrustedSourceGate(t *testing.T) {
	t.Parallel()

	spec := trustedCheckout(t)
	outside := writeTrustedFile(t, t.TempDir(), committedRel, workflowBody)
	under := func(rel string) string { return filepath.Join(spec.Dest, filepath.FromSlash(rel)) }

	cases := []struct {
		name    string
		path    string
		trusted []source.Spec
		allowed bool
	}{
		{name: "file origin holds, unchanged", path: under(committedRel), trusted: []source.Spec{spec}, allowed: true},
		{name: "stdin_file origin holds, unchanged", path: under(includingRel), trusted: []source.Spec{spec}, allowed: true},
		{name: "file edited in place", path: under(editedRel), trusted: []source.Spec{spec}},
		{name: "file only on disk", path: under(plantedRel), trusted: []source.Spec{spec}},
		{name: "file committed locally, never pushed", path: under(localRel), trusted: []source.Spec{spec}},
		{name: "file outside every trusted dest", path: outside, trusted: []source.Spec{spec}},
		{name: "symlink under dest escaping it", path: under(escapeRel), trusted: []source.Spec{spec}},
		{name: "file origin holds, no trusted source", path: under(committedRel)},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			gate := trustedExec(t, execConfig(nil), isolatedGit(), testCase.trusted...)
			err := gate.Validate(loadedTask(t, testCase.path))

			if testCase.allowed {
				if err != nil {
					t.Fatalf("validate refused a file origin holds: %v", err)
				}

				return
			}

			wantDenied(t, err, runner.ClassPermanent)
		})
	}
}

// TestExecTrustJudgesTheBytesLoaded covers the window between load and check:
// the gate compares what Load read, so restoring the disk afterwards admits
// nothing, and an edit to a stdin_file after commit is caught like an edit to
// the workflow.
func TestExecTrustJudgesTheBytesLoaded(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		workflowRel string
		editedRel   string
		before      string
		restore     string
	}{
		{
			name: "workflow edited, loaded, then restored", workflowRel: committedRel, editedRel: committedRel,
			before: workflowBody + "# edit\n", restore: workflowBody,
		},
		{
			name: "stdin_file edited, loaded, then restored", workflowRel: includingRel, editedRel: scriptRel,
			before: "echo planted\n", restore: scriptBody,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			spec := trustedCheckout(t)
			path := filepath.Join(spec.Dest, filepath.FromSlash(testCase.workflowRel))

			writeTrustedFile(t, spec.Dest, testCase.editedRel, testCase.before)
			task := loadedTask(t, path)
			writeTrustedFile(t, spec.Dest, testCase.editedRel, testCase.restore)

			wantDenied(t, trustedExec(t, execConfig(nil), isolatedGit(), spec).Validate(task), runner.ClassPermanent)
		})
	}
}

// TestExecTrustCheckFailureIsTransient proves a git read that fails denies the
// action as retryable: it says nothing about the file, and the next attempt
// may get an answer.
func TestExecTrustCheckFailureIsTransient(t *testing.T) {
	t.Parallel()

	spec := trustedCheckout(t)
	broken := func(context.Context, string, ...string) (string, error) {
		return "", errors.New("fatal: the fixture broke git")
	}

	task := loadedTask(t, filepath.Join(spec.Dest, filepath.FromSlash(committedRel)))

	wantDenied(t, trustedExec(t, execConfig(nil), broken, spec).Validate(task), runner.ClassTransient)
}

// TestExecTrustedRunsWithoutAnExecBlock covers a host whose only route through
// the gate is a trusted source: with no [runner.exec] block, the action still
// gets a deadline it can finish inside.
func TestExecTrustedRunsWithoutAnExecBlock(t *testing.T) {
	t.Parallel()

	spec := trustedCheckout(t)
	task := loadedTask(t, filepath.Join(spec.Dest, filepath.FromSlash(committedRel)))

	result, err := trustedExec(t, map[string]any{}, isolatedGit(), spec).Execute(t.Context(), task)
	if err != nil {
		t.Fatalf("execute a trusted action with no exec block: %v", err)
	}

	if result.Meta["exit_code"] != "0" {
		t.Fatalf("exit code = %q, want 0", result.Meta["exit_code"])
	}
}

// TestExecAllowListStillAppliesWithTrust proves trust only widens the gate: a
// listed action from an uncommitted file still runs.
func TestExecAllowListStillAppliesWithTrust(t *testing.T) {
	t.Parallel()

	spec := trustedCheckout(t)

	task := execTask(map[string]any{cmdKey: binTrue})
	task.Definition = loadedTask(t, filepath.Join(spec.Dest, filepath.FromSlash(plantedRel))).Definition

	if err := trustedExec(t, execConfig(nil), isolatedGit(), spec).Validate(task); err != nil {
		t.Fatalf("validate refused an allow-listed action: %v", err)
	}
}
