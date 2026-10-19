package workflow_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/workflow"
)

// promptText is the sibling prompt file the include cases load. It ends with an
// INPUT section reading an upstream output, the shape a long LLM prompt takes.
const promptText = "Summarize the report.\n\n## INPUT\n\n${{ fetch.output }}\n"

// includeLimit mirrors the package's 256 KiB cap so the oversize case writes
// one byte past it.
const includeLimit = 256 << 10 // 256 KiB

const (
	promptFile  = "prompt.md"
	promptParam = `"` + promptFile + `"`
	notRelative = "relative to the workflow file's directory"
)

// includeFlow is a workflow whose exec summarize action pulls stdin from
// stdinFile.
func includeFlow(stdinFile string) string {
	return includeFlowOn("exec", stdinFile)
}

// includeFlowOn is includeFlow with the summarize action on runnerName.
func includeFlowOn(runnerName, stdinFile string) string {
	return `
[workflow]
name = "report"

[action.fetch]
runner = "mock"

[action.summarize]
runner     = "` + runnerName + `"
depends_on = ["fetch"]
params     = { command = "cat", stdin_file = ` + stdinFile + ` }
`
}

// badIncludeMessage returns the message of the one bad_include entry in err,
// with the workflow and action prefix every such message carries stripped, so
// a case compares only the problem sentence.
func badIncludeMessage(t *testing.T, err error) string {
	t.Helper()

	list, isList := errors.AsType[cairnerr.Errors](err)
	if !isList {
		t.Fatalf("error = %v (%T), want cairnerr.Errors", err, err)
	}

	var messages []string

	for _, entry := range list {
		if entry.Code == workflow.CodeBadInclude {
			messages = append(messages, entry.Message)
		}
	}

	if len(messages) != 1 {
		t.Fatalf("bad_include entries = %q, want exactly one in %v", messages, err)
	}

	const prefix = `workflow "report" action "summarize": stdin_file: `

	problem, hasPrefix := strings.CutPrefix(messages[0], prefix)
	if !hasPrefix {
		t.Fatalf("message = %q, want the %q prefix", messages[0], prefix)
	}

	return problem
}

// stageWorkflow writes the workflow and any extra files into a fresh directory
// and returns the workflow's path.
func stageWorkflow(t *testing.T, flowText string, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}

		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	path := filepath.Join(dir, "report.toml")
	if err := os.WriteFile(path, []byte(flowText), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	return path
}

func TestLoadReadsStdinFile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		files map[string]string
		name  string
		param string
		want  string
	}{
		{name: "sibling file", param: promptParam, files: map[string]string{promptFile: promptText}, want: promptText},
		{name: "file in a subdirectory", param: `"prompts/summary.md"`, files: map[string]string{"prompts/summary.md": promptText}, want: promptText},
		{name: "dot-slash prefix", param: `"./prompt.md"`, files: map[string]string{promptFile: promptText}, want: promptText},
		{name: "empty file", param: promptParam, files: map[string]string{promptFile: ""}, want: ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			flow, err := workflow.Load(stageWorkflow(t, includeFlow(testCase.param), testCase.files))
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			params := flow.Actions["summarize"].Params

			if params["stdin"] != testCase.want {
				t.Fatalf("stdin = %q, want the file text %q", params["stdin"], testCase.want)
			}

			if _, kept := params["stdin_file"]; kept {
				t.Fatal("stdin_file is still in params; the runner would see a key it does not read")
			}
		})
	}
}

func TestLoadStdinFileRejectsBadIncludes(t *testing.T) {
	t.Parallel()

	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte(promptText), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	const linkName = "link.md"

	const symlinkLeaves = `"` + linkName + `" is a symlink that leaves the workflow directory (absolute symlink targets are refused)`

	cases := []struct {
		files map[string]string
		name  string
		// runner is the summarize action's runner; empty means exec.
		runner string
		param  string
		// link names a symlink to create beside the workflow, pointing at
		// outside, or at the sibling prompt file's absolute path when
		// linkInside is set.
		link        string
		wantProblem string
		linkInside  bool
	}{
		{name: "runner that does not read stdin", runner: "anthropic", param: promptParam, files: map[string]string{promptFile: promptText}, wantProblem: `only the exec runner reads stdin, and this action runs on "anthropic"`},
		{name: "sandbox_exec runner", runner: "sandbox_exec", param: promptParam, files: map[string]string{promptFile: promptText}, wantProblem: `only the exec runner reads stdin, and this action runs on "sandbox_exec"`},
		{name: "parent escape", param: `"../outside.md"`, wantProblem: `"../outside.md" must be a path ` + notRelative},
		{name: "absolute path", param: `"` + outside + `"`, wantProblem: `"` + outside + `" must be a path ` + notRelative},
		{name: "dot-dot inside the path", param: `"sub/../prompt.md"`, files: map[string]string{promptFile: promptText}, wantProblem: `"sub/../prompt.md" must not contain a .. element`},
		{name: "symlink out of the directory", param: `"` + linkName + `"`, link: linkName, wantProblem: symlinkLeaves},
		{name: "absolute symlink to a file inside", param: `"` + linkName + `"`, link: linkName, linkInside: true, files: map[string]string{promptFile: promptText}, wantProblem: symlinkLeaves},
		{name: "empty path", param: `""`, wantProblem: "path is empty"},
		{name: "expression in the path", param: `"${{ fetch.output }}.md"`, wantProblem: `"${{ fetch.output }}.md" must be a literal path; ${{ is not expanded here`},
		{name: "not a string", param: `7`, wantProblem: "must be a path string, got int64"},
		{name: "directory", param: `"prompts"`, files: map[string]string{"prompts/a.md": "x"}, wantProblem: `"prompts" is not a regular file`},
		{name: "over the size cap", param: promptParam, files: map[string]string{promptFile: strings.Repeat("a", includeLimit+1)}, wantProblem: `"prompt.md" is 262145 bytes, over the 262144 byte limit`},
		{name: "not UTF-8", param: promptParam, files: map[string]string{promptFile: "\xff\xfe"}, wantProblem: `"prompt.md" is not UTF-8 text`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			runnerName := testCase.runner
			if runnerName == "" {
				runnerName = "exec"
			}

			path := stageWorkflow(t, includeFlowOn(runnerName, testCase.param), testCase.files)

			if testCase.link != "" {
				target := outside
				if testCase.linkInside {
					target = filepath.Join(filepath.Dir(path), promptFile)
				}

				if err := os.Symlink(target, filepath.Join(filepath.Dir(path), testCase.link)); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			}

			_, err := workflow.Load(path)
			if err == nil {
				t.Fatal("load succeeded, want a bad_include error")
			}

			if got := badIncludeMessage(t, err); got != testCase.wantProblem {
				t.Fatalf("problem = %q\nwant      %q", got, testCase.wantProblem)
			}
		})
	}
}

// The message has to name both the workflow and the action, since a scheduler
// log line is often all an operator sees of a load failure.
func TestLoadStdinFileErrorNamesWorkflowAndAction(t *testing.T) {
	t.Parallel()

	path := stageWorkflow(t, includeFlow(promptParam), nil)

	_, err := workflow.Load(path)
	if err == nil {
		t.Fatal("load succeeded, want a missing-file error")
	}

	list, isList := errors.AsType[cairnerr.Errors](err)
	if !isList || len(list) != 1 {
		t.Fatalf("error = %v (%T), want exactly one cairnerr entry", err, err)
	}

	want := `workflow "report" action "summarize": stdin_file: "prompt.md" not found in ` + filepath.Dir(path)
	if list[0].Code != workflow.CodeBadInclude || list[0].Message != want {
		t.Fatalf("entry = %s: %q\nwant    %s: %q", list[0].Code, list[0].Message, workflow.CodeBadInclude, want)
	}
}

func TestStdinFileTextGetsLoadTimeRefChecks(t *testing.T) {
	t.Parallel()

	// The prompt reads an action the summarize step does not depend on, which
	// an inline stdin would fail on too.
	path := stageWorkflow(t, includeFlow(promptParam), map[string]string{
		promptFile: "## INPUT\n${{ other.output }}\n",
	})

	_, err := workflow.Load(path)
	if err == nil {
		t.Fatal("load succeeded, want the included reference rejected")
	}

	if codes := codesOf(t, err); !slices.Contains(codes, workflow.CodeBadReference) {
		t.Fatalf("codes = %v, want %s", codes, workflow.CodeBadReference)
	}
}

func TestStdinFileRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		parse       func(t *testing.T) error
		name        string
		wantProblem string
	}{
		{
			name: "bytes with no directory",
			parse: func(*testing.T) error {
				_, err := workflow.Parse([]byte(includeFlow(promptParam)))

				return err
			},
			wantProblem: promptParam + " needs the workflow loaded from a file, since the path is relative to that file's directory",
		},
		{
			name: "stdin and stdin_file together",
			parse: func(t *testing.T) error {
				t.Helper()

				text := strings.Replace(includeFlow(promptParam), `command = "cat",`, `command = "cat", stdin = "inline",`, 1)
				_, err := workflow.ParseInDir([]byte(text), t.TempDir())

				return err
			},
			wantProblem: promptParam + " cannot be set alongside stdin; pick one",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := testCase.parse(t)
			if err == nil {
				t.Fatal("parse succeeded, want a bad_include error")
			}

			if got := badIncludeMessage(t, err); got != testCase.wantProblem {
				t.Fatalf("problem = %q\nwant      %q", got, testCase.wantProblem)
			}
		})
	}
}

// A run record proves which definition ran, so a prompt edit has to change the
// hash even though the workflow bytes did not.
func TestLoadHashCoversIncludedFile(t *testing.T) {
	t.Parallel()

	flowText := includeFlow(promptParam)
	path := stageWorkflow(t, flowText, map[string]string{promptFile: promptText})

	first, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	if first.Hash == workflow.HashBytes([]byte(flowText)) {
		t.Fatal("hash ignores the included file")
	}

	promptPath := filepath.Join(filepath.Dir(path), promptFile)
	if writeErr := os.WriteFile(promptPath, []byte(promptText+"Be brief.\n"), 0o600); writeErr != nil {
		t.Fatalf("edit prompt: %v", writeErr)
	}

	second, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if second.Hash == first.Hash {
		t.Fatal("hash did not change after the prompt file changed")
	}
}
