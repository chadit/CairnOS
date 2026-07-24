package cli

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

// approveMaxArgs caps the approve command's positionals: a run id and an
// optional action name.
const approveMaxArgs = 2

// approveLabel prefixes the approve command's messages and store-open label.
const approveLabel = "runs approve"

const runsUsage = "usage: cairnos runs <list|show|replay|approve> [flags]\n"

func (a *App) cmdRuns(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", runsUsage)

		return ExitUsage
	}

	switch args[0] {
	case subcmdList:
		return a.cmdRunsList(ctx, args[1:])
	case "show":
		return a.cmdRunsShow(ctx, args[1:])
	case "replay":
		return a.cmdRunsReplay(ctx, args[1:])
	case "approve":
		return a.cmdRunsApprove(ctx, args[1:])
	default:
		a.printErrf("unknown runs subcommand %q\n", args[0])
		a.printErrf("%s", runsUsage)

		return ExitUsage
	}
}

func (a *App) cmdRunsList(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("runs list", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	workflowFlag := flags.String("workflow", "", "only runs of this workflow")
	statusFlag := flags.String("status", "", "only runs in this status (pending|running|succeeded|failed|canceled|held)")
	limitFlag := flags.Int("limit", defaultListLimit, "maximum rows")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	db, _, code, proceed := a.openStoreAt(ctx, "runs list", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	runs, err := db.ListRuns(ctx, store.ListFilter{
		WorkflowName: *workflowFlag,
		Status:       store.RunStatus(*statusFlag),
		Limit:        *limitFlag,
	})
	if err != nil {
		a.printErrf("runs list: %v\n", err)

		return ExitFailure
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "ID\tWORKFLOW\tSTATUS\tTRIGGER\tCREATED\tFINISHED\n")

	for _, run := range runs {
		a.tabWritef(tab, "%s\t%s\t%s\t%s\t%s\t%s\n",
			run.ID, run.WorkflowName, run.Status, run.Trigger,
			formatTableTime(run.CreatedAt), formatTableTime(run.FinishedAt))
	}

	a.flushTab(tab)

	return ExitOK
}

func (a *App) cmdRunsShow(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("runs show", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	runID, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if runID == "" {
		a.printErrf("usage: cairnos runs show <run-id> [--dir D]\n")

		return ExitUsage
	}

	db, _, code, proceed := a.openStoreAt(ctx, "runs show", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	run, err := db.GetRun(ctx, runID)
	if err != nil {
		a.printErrf("runs show: %v\n", err)

		return ExitFailure
	}

	results, err := db.ListActionResults(ctx, runID)
	if err != nil {
		a.printErrf("runs show: %v\n", err)

		return ExitFailure
	}

	a.printRunDetail(run)
	a.printOutf("\n")
	a.writeActionTable(results)

	return ExitOK
}

// cmdRunsReplay reruns a prior run from its first failed action: prior
// succeeded actions keep their outputs and the rest re-execute. It resolves
// the workflow by name from the workflows directory, refuses a changed
// workflow file unless --force is given, and warns when a needed upstream
// output was truncated by retention (.agents/spec/parity/workflow-language.md,
// "Replay from failed action").
func (a *App) cmdRunsReplay(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("runs replay", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	forceFlag := flags.Bool("force", false, "replay even when the workflow file changed since the run")
	dirFlag := dirFlagOn(flags)

	runID, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if runID == "" {
		a.printErrf("usage: cairnos runs replay <run-id> [--force] [--dir D]\n")

		return ExitUsage
	}

	dir, err := resolveDir(*dirFlag)
	if err != nil {
		a.printErrf("runs replay: %v\n", err)

		return ExitFailure
	}

	db, cfg, code, proceed := a.openStoreAt(ctx, "runs replay", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	prior, results, flow, ok := a.loadReplaySources(ctx, db, dir, runID, *forceFlag)
	if !ok {
		return ExitFailure
	}

	mem, code, proceed := a.openMemoryAt(ctx, "runs replay", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	return a.executeReplay(ctx, &cfg, db, mem, prior, results, flow, filepath.Join(dir, workflowsDirName))
}

// loadReplaySources gathers everything a replay needs from the store and the
// workflows directory: the prior run, its action results, and the current
// workflow. It enforces the hash guard (unless force) and prints the retention
// preflight warnings, returning ok=false with the failure already reported.
func (a *App) loadReplaySources(ctx context.Context, db store.Store, dir, runID string, force bool) (*store.Run, []*store.ActionResult, *workflow.Workflow, bool) {
	prior, err := db.GetRun(ctx, runID)
	if err != nil {
		a.printErrf("runs replay: %v\n", err)

		return nil, nil, nil, false
	}

	results, err := db.ListActionResults(ctx, runID)
	if err != nil {
		a.printErrf("runs replay: %v\n", err)

		return nil, nil, nil, false
	}

	flow, err := api.NewWorkflowResolver(filepath.Join(dir, workflowsDirName), a.logger).Resolve(prior.WorkflowName)
	if err != nil {
		a.printErrf("runs replay: %v\n", err)

		return nil, nil, nil, false
	}

	if !force && flow.Hash != prior.WorkflowHash {
		a.printErrf("runs replay: workflow %q changed since run %s (stored hash %s, current %s); rerun with --force to replay anyway\n",
			prior.WorkflowName, runID, prior.WorkflowHash, flow.Hash)

		return nil, nil, nil, false
	}

	for _, warning := range replayRetentionWarnings(flow, results) {
		a.printErrf("runs replay: warning: %s\n", warning)
	}

	return prior, results, flow, true
}

// replayRetentionWarnings flags upstream outputs a replay will need but that
// retention truncated: an action that re-executes and depends on a prior
// succeeded action whose output_json was nulled will fail resolving that
// reference at launch. The warning is advisory; the replay still runs.
func replayRetentionWarnings(flow *workflow.Workflow, results []*store.ActionResult) []string {
	byName := make(map[string]*store.ActionResult, len(results))
	for _, result := range results {
		byName[result.ActionName] = result
	}

	var warnings []string

	warned := map[string]bool{}

	for _, name := range slices.Sorted(maps.Keys(flow.Actions)) {
		if !willReplay(name, byName) {
			continue
		}

		warnings = append(warnings, truncatedDepWarnings(flow.Actions[name], byName, warned)...)
	}

	return warnings
}

// willReplay reports whether an action re-executes on replay: it either never
// ran (no prior row) or ended in a non-succeeded state. A succeeded action
// seeds forward instead.
func willReplay(name string, byName map[string]*store.ActionResult) bool {
	prior, ran := byName[name]

	return !ran || prior.Status != store.ActionSucceeded
}

// truncatedDepWarnings collects one warning per not-yet-warned dependency of a
// re-executing action whose prior output was truncated by retention.
func truncatedDepWarnings(action *workflow.Action, byName map[string]*store.ActionResult, warned map[string]bool) []string {
	var warnings []string

	for _, dep := range action.DependsOn {
		if warned[dep] || !outputTruncated(byName[dep]) {
			continue
		}

		warned[dep] = true
		warnings = append(warnings, fmt.Sprintf(
			"upstream output for %q was not retained; replay may fail resolving references to it", dep,
		))
	}

	return warnings
}

// outputTruncated reports whether a prior action succeeded but has no retained
// output, the retention-truncation shape a downstream replay ref would fail on.
func outputTruncated(prior *store.ActionResult) bool {
	return prior != nil && prior.Status == store.ActionSucceeded && prior.Output == nil
}

// approveRequest bundles one approve invocation's decision so executeApprove
// keeps a small signature.
type approveRequest struct {
	runID        string
	actionName   string // empty means resolve the single pending gate
	deny         bool
	comment      string
	workflowsDir string
}

// cmdRunsApprove decides one human-approval gate and resumes the parked run.
// With the action name given it decides that gate; omitted, it decides the
// single pending gate or errors listing the pending action names when zero or
// more than one wait (.agents/spec/parity/workflow-language.md, "Human
// approval").
func (a *App) cmdRunsApprove(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet(approveLabel, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	denyFlag := flags.Bool("deny", false, "deny the approval instead of approving it")
	commentFlag := flags.String("comment", "", "optional decision note recorded on the gate")
	dirFlag := dirFlagOn(flags)

	positionals, code, proceed := parseThreePositionals(flags, args)
	if !proceed {
		return code
	}

	if len(positionals) == 0 || len(positionals) > approveMaxArgs {
		a.printErrf("usage: cairnos runs approve <run-id> [<action-name>] [--deny] [--comment TEXT] [--dir D]\n")

		return ExitUsage
	}

	req := approveRequest{runID: positionals[0], deny: *denyFlag, comment: *commentFlag}
	if len(positionals) == approveMaxArgs {
		req.actionName = positionals[1]
	}

	db, cfg, code, proceed := a.openStoreAt(ctx, approveLabel, *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	mem, code, proceed := a.openMemoryAt(ctx, approveLabel, *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	dir, err := resolveDir(*dirFlag)
	if err != nil {
		a.printErrf("%s: %v\n", approveLabel, err)

		return ExitFailure
	}

	req.workflowsDir = filepath.Join(dir, workflowsDirName)

	return a.executeApprove(ctx, &cfg, db, mem, req)
}

// executeApprove records the decision, then resumes the run so the loop drives
// the approval action to a terminal state: approve succeeds it, deny fails it
// with action.approval_denied under normal error handling. A run that is no
// longer held (a failure or cancel orphaned its gate) is refused before any
// decision is written, so the terminal run is never resurrected.
func (a *App) executeApprove(ctx context.Context, cfg *config.Config, db store.Store, mem memory.Store, req approveRequest) int {
	held, err := db.GetRun(ctx, req.runID)
	if err != nil {
		a.printErrf("%s: %v\n", approveLabel, err)

		return ExitFailure
	}

	// A run that already finished can still own a pending gate: when a failure or
	// cancel beat the approval branch, the branch's detached write left the gate
	// behind while the run finalized failed or canceled. Deciding that orphaned
	// gate must not drive ResumeRun on a terminal run, so refuse before touching
	// it (.agents/spec/parity/workflow-language.md, "Human approval").
	if held.Status != store.RunHeld {
		a.printErrf("%s: run %s is not awaiting approval (status %s)\n", approveLabel, req.runID, held.Status)

		return ExitFailure
	}

	actionName, ok := a.resolveApprovalAction(ctx, db, req.runID, req.actionName)
	if !ok {
		return ExitFailure
	}

	status := store.ApprovalApproved
	if req.deny {
		status = store.ApprovalDenied
	}

	if err := db.DecideApproval(ctx, req.runID, actionName, status, actorCLI, req.comment); err != nil {
		a.printErrf("%s: %v\n", approveLabel, err)

		return ExitFailure
	}

	a.printOutf("%s %s on run %s\n", decisionVerb(req.deny), actionName, req.runID)

	return a.resumeApprovedRun(ctx, cfg, db, mem, held, req.workflowsDir)
}

// resolveApprovalAction picks the gate to decide: the named one, or the single
// pending gate when the name is omitted. Zero or several pending gates with no
// name is an error that lists the pending action names, so the operator reruns
// naming one.
func (a *App) resolveApprovalAction(ctx context.Context, db store.Store, runID, actionName string) (string, bool) {
	if actionName != "" {
		return actionName, true
	}

	pending, err := db.ListPendingApprovals(ctx, runID)
	if err != nil {
		a.printErrf("%s: %v\n", approveLabel, err)

		return "", false
	}

	switch len(pending) {
	case 0:
		a.printErrf("%s: run %s has no pending approvals\n", approveLabel, runID)

		return "", false
	case 1:
		return pending[0].ActionName, true
	default:
		a.printErrf("%s: run %s has %d pending approvals; rerun naming one of: %s\n",
			approveLabel, runID, len(pending), strings.Join(approvalActionNames(pending), ", "))

		return "", false
	}
}

// resumeApprovedRun resolves the held run's workflow and drives it forward
// through ResumeRun so the decided approval settles and downstream work runs.
// The caller has already loaded the run and confirmed it is held.
func (a *App) resumeApprovedRun(ctx context.Context, cfg *config.Config, db store.Store, mem memory.Store, held *store.Run, workflowsDir string) int {
	results, err := db.ListActionResults(ctx, held.ID)
	if err != nil {
		a.printErrf("%s: %v\n", approveLabel, err)

		return ExitFailure
	}

	flow, err := api.NewWorkflowResolver(workflowsDir, a.logger).Resolve(held.WorkflowName)
	if err != nil {
		a.printErrf("%s: %v\n", approveLabel, err)

		return ExitFailure
	}

	eng, cleanup, code, proceed := a.buildEngine(ctx, approveLabel, workflowsDir, cfg, db, mem)
	if !proceed {
		return code
	}
	defer cleanup()

	run, err := eng.ResumeRun(ctx, held, results, flow)
	if err != nil {
		a.printErrf("%s: %v\n", approveLabel, err)

		return ExitFailure
	}

	a.printRunSummary(ctx, db, run)

	// A resumed run that succeeds or parks again on another branch's approval is
	// a clean outcome; only a real failure (a denial, a downstream error) fails.
	if run.Status == store.RunSucceeded || run.Status == store.RunHeld {
		return ExitOK
	}

	return ExitFailure
}

// decisionVerb renders the past-tense decision word for the confirmation line.
func decisionVerb(deny bool) string {
	if deny {
		return "denied"
	}

	return "approved"
}

// approvalActionNames lists the action names of pending gates for the ambiguous
// decision error.
func approvalActionNames(pending []*store.Approval) []string {
	names := make([]string, len(pending))
	for i, approval := range pending {
		names[i] = approval.ActionName
	}

	return names
}

// printRunDetail renders the run header fields.
func (a *App) printRunDetail(run *store.Run) {
	a.printOutf("Run:      %s\n", run.ID)
	a.printOutf("Workflow: %s\n", run.WorkflowName)

	if run.WorkflowHash != "" {
		a.printOutf("Hash:     %s\n", run.WorkflowHash)
	}

	a.printOutf("Status:   %s\n", run.Status)
	a.printOutf("Trigger:  %s\n", run.Trigger)
	a.printOutf("Created:  %s\n", formatTableTime(run.CreatedAt))
	a.printOutf("Finished: %s\n", formatTableTime(run.FinishedAt))

	if run.Error != nil {
		a.printOutf("Error:    %s: %s\n", run.Error.Code, run.Error.Message)
	}
}
