package api_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/chadit/CairnOS/internal/api"
	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/workflow"
)

// Canonical code the save and inline-run name guard answers with.
const wireNameMismatch = "api.workflow_name_mismatch"

// savedName is the workflow the create-path tests write; it is not seeded by
// the fixture, so a successful save is the only way a file carries it.
const savedName = "saved"

// savedTOML is a one-action flow the builder might emit and save.
const savedTOML = `
[workflow]
name = "saved"

[action.only]
runner = "mock"
params = { output = "saved output" }
`

// helloUpdatedTOML edits the seeded hello flow so save-over-existing has a
// different definition (and hash) to write over the original.
const helloUpdatedTOML = `
[workflow]
name = "hello"

[action.greet]
runner = "mock"
params = { output = "hello again", cost_usd = 0.25 }
`

// inlineName and inlineTOML drive the test-action (inline run) path: a
// definition never written to the workflows directory.
const inlineName = "inline_flow"

const inlineTOML = `
[workflow]
name = "inline_flow"

[action.step]
runner = "mock"
params = { output = "inline output" }
`

func TestSaveWorkflowCreatesFile(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	resp, err := fix.workflow.SaveWorkflow(t.Context(), &cairnosv1.SaveWorkflowRequest{
		WorkflowName: savedName,
		Toml:         []byte(savedTOML),
	})
	if err != nil {
		t.Fatalf("SaveWorkflow: %v", err)
	}

	if !resp.GetCreated() {
		t.Fatal("created = false, want true for a brand-new workflow")
	}

	if resp.GetPriorHash() != "" {
		t.Fatalf("prior_hash = %q, want empty on create", resp.GetPriorHash())
	}

	if resp.GetHash() != workflow.HashBytes([]byte(savedTOML)) {
		t.Fatalf("hash = %q, want the SHA-256 of the written bytes", resp.GetHash())
	}

	// The file lands with owner-only permissions.
	path := filepath.Join(fix.workflowsDir, savedName+".toml")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat saved file: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("saved file mode = %o, want 600", perm)
	}

	// Both the resolver and RunWorkflow-by-name now find it.
	stream, err := fix.workflow.RunWorkflow(t.Context(), &cairnosv1.RunWorkflowRequest{WorkflowName: savedName})
	if err != nil {
		t.Fatalf("RunWorkflow of saved workflow: %v", err)
	}

	events := collectEvents(t, stream)
	if len(events) == 0 {
		t.Fatal("running the saved workflow produced no events")
	}

	last := events[len(events)-1]
	if got := last.GetRun().GetStatus(); got != statusSucceeded {
		t.Fatalf("saved workflow run status = %q, want %q", got, statusSucceeded)
	}
}

func TestSaveWorkflowCapturesPriorVersion(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	resp, err := fix.workflow.SaveWorkflow(t.Context(), &cairnosv1.SaveWorkflowRequest{
		WorkflowName: helloName,
		Toml:         []byte(helloUpdatedTOML),
	})
	if err != nil {
		t.Fatalf("SaveWorkflow over hello: %v", err)
	}

	priorHash := workflow.HashBytes([]byte(helloTOML))

	if resp.GetCreated() {
		t.Fatal("created = true, want false when a file already carried the name")
	}

	if resp.GetPriorHash() != priorHash {
		t.Fatalf("prior_hash = %q, want %q (the replaced definition's hash)", resp.GetPriorHash(), priorHash)
	}

	if resp.GetHash() != workflow.HashBytes([]byte(helloUpdatedTOML)) {
		t.Fatalf("hash = %q, want the hash of the new bytes", resp.GetHash())
	}

	// The prior definition is the newest version-store row: capture happened
	// before the write, so revert can restore it.
	versions, err := fix.db.ListWorkflowVersions(t.Context(), helloName)
	if err != nil {
		t.Fatalf("ListWorkflowVersions: %v", err)
	}

	if len(versions) == 0 {
		t.Fatal("no captured versions; the prior definition was not recorded before the write")
	}

	newest := versions[0]
	if newest.Hash != priorHash {
		t.Fatalf("newest version hash = %q, want the prior hash %q", newest.Hash, priorHash)
	}

	if newest.DefinitionTOML != helloTOML {
		t.Fatalf("newest version bytes = %q, want the original hello definition", newest.DefinitionTOML)
	}
}

func TestSaveWorkflowReSaveIsIdempotent(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	// Saving hello's own bytes back captures the prior, then saving the exact
	// same bytes a second time is a store-level no-op: no second version row.
	first, err := fix.workflow.SaveWorkflow(t.Context(), &cairnosv1.SaveWorkflowRequest{
		WorkflowName: helloName,
		Toml:         []byte(helloTOML),
	})
	if err != nil {
		t.Fatalf("first SaveWorkflow: %v", err)
	}

	second, err := fix.workflow.SaveWorkflow(t.Context(), &cairnosv1.SaveWorkflowRequest{
		WorkflowName: helloName,
		Toml:         []byte(helloTOML),
	})
	if err != nil {
		t.Fatalf("second SaveWorkflow of identical bytes: %v", err)
	}

	if first.GetHash() != second.GetHash() {
		t.Fatalf("hashes differ across identical saves: %q vs %q", first.GetHash(), second.GetHash())
	}

	versions, err := fix.db.ListWorkflowVersions(t.Context(), helloName)
	if err != nil {
		t.Fatalf("ListWorkflowVersions: %v", err)
	}

	// Both saves captured the same (hash, workflow), so the store holds exactly
	// one row for it.
	if len(versions) != 1 {
		t.Fatalf("captured %d versions, want 1; the identical re-save should be a no-op", len(versions))
	}
}

func TestSaveWorkflowRejectsInvalidTOML(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	const badName = "broken"

	badTOML := `
[workflow]
name = "broken"

[action.a]
runner = "mock"
depends_on = ["ghost"]
`

	_, err := fix.workflow.SaveWorkflow(t.Context(), &cairnosv1.SaveWorkflowRequest{
		WorkflowName: badName,
		Toml:         []byte(badTOML),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("SaveWorkflow code = %v (%v), want InvalidArgument", status.Code(err), err)
	}

	// The workflow.* problems ride in details.errors, same envelope as
	// ValidateWorkflow's error list.
	if problems := detailErrorCodes(t, err); !slices.Contains(problems, workflow.CodeUnknownDependency) {
		t.Fatalf("details.errors codes = %v, want %q", problems, workflow.CodeUnknownDependency)
	}

	// Nothing touched disk: no broken.toml exists.
	if _, statErr := os.Stat(filepath.Join(fix.workflowsDir, badName+".toml")); !os.IsNotExist(statErr) {
		t.Fatalf("stat broken.toml = %v, want not-exist (a rejected save must not write)", statErr)
	}
}

func TestSaveWorkflowRejectsNameMismatch(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	// The path param says "elsewhere" but the definition names itself "saved".
	_, err := fix.workflow.SaveWorkflow(t.Context(), &cairnosv1.SaveWorkflowRequest{
		WorkflowName: "elsewhere",
		Toml:         []byte(savedTOML),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("SaveWorkflow code = %v (%v), want InvalidArgument", status.Code(err), err)
	}

	if got := canonicalCode(t, err); got != wireNameMismatch {
		t.Fatalf("canonical code = %q, want %q", got, wireNameMismatch)
	}

	if _, statErr := os.Stat(filepath.Join(fix.workflowsDir, "elsewhere.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("stat elsewhere.toml = %v, want not-exist", statErr)
	}
}

func TestResolvePathFindsSkipsAndReportsMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	writeWorkflow(t, dir, "good.toml", savedTOML)
	writeWorkflow(t, dir, "broken.toml", "definitely not toml {{{")

	resolver := api.NewWorkflowResolver(dir, slog.New(slog.DiscardHandler))

	// The good file resolves to its path and loaded flow despite the broken
	// sibling in the same directory.
	path, flow, err := resolver.ResolvePath(savedName)
	if err != nil {
		t.Fatalf("ResolvePath(%q): %v", savedName, err)
	}

	if path != filepath.Join(dir, "good.toml") {
		t.Fatalf("resolved path = %q, want good.toml", path)
	}

	if flow.Name != savedName {
		t.Fatalf("resolved flow name = %q, want %q", flow.Name, savedName)
	}

	// A name no file carries answers the canonical NotFound.
	_, _, err = resolver.ResolvePath("nowhere")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("ResolvePath(missing) code = %v, want NotFound", status.Code(err))
	}
}

func TestInlineRunExecutesWithoutWritingFile(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	stream, err := fix.workflow.RunWorkflow(t.Context(), &cairnosv1.RunWorkflowRequest{
		WorkflowName: inlineName,
		Toml:         []byte(inlineTOML),
	})
	if err != nil {
		t.Fatalf("inline RunWorkflow: %v", err)
	}

	events := collectEvents(t, stream)
	if len(events) < 3 {
		t.Fatalf("got %d events, want run_started, transitions, run_finished", len(events))
	}

	if events[0].GetType() != cairnosv1.RunEventType_RUN_EVENT_TYPE_RUN_STARTED {
		t.Fatalf("first event type = %v, want RUN_STARTED", events[0].GetType())
	}

	last := events[len(events)-1]
	if last.GetType() != cairnosv1.RunEventType_RUN_EVENT_TYPE_RUN_FINISHED {
		t.Fatalf("last event type = %v, want RUN_FINISHED", last.GetType())
	}

	if got := last.GetRun().GetStatus(); got != statusSucceeded {
		t.Fatalf("inline run status = %q, want %q (error: %v)", got, statusSucceeded, last.GetError())
	}

	// The run lands in history under trigger "api" with the definition's real
	// name and the hash of the request bytes.
	run, err := fix.db.GetRun(t.Context(), events[0].GetRunId())
	if err != nil {
		t.Fatalf("GetRun for inline run: %v", err)
	}

	if run.Trigger != "api" {
		t.Fatalf("inline run trigger = %q, want api", run.Trigger)
	}

	if run.WorkflowName != inlineName {
		t.Fatalf("inline run workflow name = %q, want %q", run.WorkflowName, inlineName)
	}

	if run.WorkflowHash != workflow.HashBytes([]byte(inlineTOML)) {
		t.Fatalf("inline run hash = %q, want HashBytes of the request bytes", run.WorkflowHash)
	}

	// No file was written for the inline definition.
	if _, statErr := os.Stat(filepath.Join(fix.workflowsDir, inlineName+".toml")); !os.IsNotExist(statErr) {
		t.Fatalf("stat %s.toml = %v, want not-exist; inline runs write no files", inlineName, statErr)
	}
}

func TestInlineRunRejectsBadDefinitions(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	cases := []struct {
		req      *cairnosv1.RunWorkflowRequest
		name     string
		wantCode codes.Code
	}{
		{
			name: "invalid inline toml",
			req: &cairnosv1.RunWorkflowRequest{
				WorkflowName: "whatever",
				Toml:         []byte("definitely not toml {{{"),
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "inline name mismatch",
			req: &cairnosv1.RunWorkflowRequest{
				WorkflowName: "wrong",
				Toml:         []byte(inlineTOML),
			},
			wantCode: codes.InvalidArgument,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stream, err := fix.workflow.RunWorkflow(t.Context(), tt.req)
			if err != nil {
				t.Fatalf("open inline RunWorkflow stream: %v", err)
			}

			// The parse and name checks run before any stream event, so the
			// first Recv carries the error.
			_, err = stream.Recv()
			if status.Code(err) != tt.wantCode {
				t.Fatalf("first recv code = %v (%v), want %v", status.Code(err), err, tt.wantCode)
			}
		})
	}
}

// detailErrorCodes digs the canonical code list out of a status error's
// details.errors, the envelope SaveWorkflow reuses from the validation path.
func detailErrorCodes(t *testing.T, err error) []string {
	t.Helper()

	for _, detail := range status.Convert(err).Details() {
		cerr, isCanonical := detail.(*cairnosv1.Error)
		if !isCanonical {
			continue
		}

		values := cerr.GetDetails().GetFields()["errors"].GetListValue().GetValues()

		out := make([]string, 0, len(values))
		for _, item := range values {
			out = append(out, item.GetStructValue().GetFields()["code"].GetStringValue())
		}

		return out
	}

	return nil
}

// promptedName names the stdin_file fixture workflow.
const promptedName = "prompted"

// stdinFileTOML is a saved workflow whose action reads stdin from a prompt
// file in the workflows directory.
const stdinFileTOML = `
[workflow]
name = "prompted"

[action.ask]
runner = "exec"
params = { command = "cat", stdin_file = "prompted.md" }
`

// A definition sent over the API names its prompt file relative to the
// workflows directory, the place a save writes it, so validation and save both
// resolve there and fail the same way when the file is absent.
func TestWorkflowAPIResolvesStdinFileInWorkflowsDir(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	request := &cairnosv1.ValidateWorkflowRequest{Toml: []byte(stdinFileTOML)}

	missing, err := fix.workflow.ValidateWorkflow(t.Context(), request)
	if err != nil {
		t.Fatalf("ValidateWorkflow without the prompt: %v", err)
	}

	if missing.GetValid() || len(missing.GetErrors()) != 1 || missing.GetErrors()[0].GetCode() != workflow.CodeBadInclude {
		t.Fatalf("response = %v, want one %s error", missing, workflow.CodeBadInclude)
	}

	if writeErr := os.WriteFile(filepath.Join(fix.workflowsDir, "prompted.md"), []byte("hello"), 0o600); writeErr != nil {
		t.Fatalf("write prompt: %v", writeErr)
	}

	present, err := fix.workflow.ValidateWorkflow(t.Context(), request)
	if err != nil {
		t.Fatalf("ValidateWorkflow with the prompt: %v", err)
	}

	if !present.GetValid() {
		t.Fatalf("response = %v, want valid once the prompt exists", present)
	}

	saved, err := fix.workflow.SaveWorkflow(t.Context(), &cairnosv1.SaveWorkflowRequest{
		WorkflowName: promptedName,
		Toml:         []byte(stdinFileTOML),
	})
	if err != nil {
		t.Fatalf("SaveWorkflow: %v", err)
	}

	if saved.GetHash() == workflow.HashBytes([]byte(stdinFileTOML)) {
		t.Fatal("saved hash ignores the prompt file")
	}
}

// An inline run resolves stdin_file in the workflows directory as well, and the
// run record's hash covers the prompt text, the identity a replay checks.
func TestInlineRunResolvesStdinFileInWorkflowsDir(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	request := &cairnosv1.RunWorkflowRequest{WorkflowName: promptedName, Toml: []byte(stdinFileTOML)}

	refused, err := fix.workflow.RunWorkflow(t.Context(), request)
	if err != nil {
		t.Fatalf("open inline RunWorkflow stream without the prompt: %v", err)
	}

	_, err = refused.Recv()
	if status.Code(err) != codes.InvalidArgument || !slices.Contains(detailErrorCodes(t, err), workflow.CodeBadInclude) {
		t.Fatalf("first recv without the prompt = %v, want InvalidArgument carrying %s", err, workflow.CodeBadInclude)
	}

	if writeErr := os.WriteFile(filepath.Join(fix.workflowsDir, "prompted.md"), []byte("hello"), 0o600); writeErr != nil {
		t.Fatalf("write prompt: %v", writeErr)
	}

	stream, err := fix.workflow.RunWorkflow(t.Context(), request)
	if err != nil {
		t.Fatalf("open inline RunWorkflow stream with the prompt: %v", err)
	}

	events := collectEvents(t, stream)
	if len(events) == 0 || events[0].GetType() != cairnosv1.RunEventType_RUN_EVENT_TYPE_RUN_STARTED {
		t.Fatalf("events = %v, want the run to start once the prompt exists", events)
	}

	run, err := fix.db.GetRun(t.Context(), events[0].GetRunId())
	if err != nil {
		t.Fatalf("GetRun for the inline run: %v", err)
	}

	flow, err := workflow.ParseInDir([]byte(stdinFileTOML), fix.workflowsDir)
	if err != nil {
		t.Fatalf("parse the definition for its expected hash: %v", err)
	}

	if want := flow.DefinitionHash([]byte(stdinFileTOML)); run.WorkflowHash != want {
		t.Fatalf("inline run hash = %q, want %q, the hash that covers the prompt file", run.WorkflowHash, want)
	}
}
