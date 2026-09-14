// Package autonomic is the Phase 6 gated auto-apply path
// (.agents/spec/13-roadmap.md, Phase 6; .agents/spec/06-observability-and-tuning.md,
// "Review flow"). It applies an approved, output-neutral suggestion to a
// workflow file, recording the prior definition in the version store first so
// the change is reversible, and writing an audit row so the action is on the
// record. It is the narrow, reversible counterpart to the tuning engine, which
// only ever proposes.
//
// The gates are the point of this package, not an afterthought:
//
//   - Auto-apply is default off (config [autonomic] auto_apply_enabled = false).
//   - Only caching and batching apply. Param tuning and prompt refactor never
//     do, because verifying they kept output quality is the open R1 problem
//     (.agents/spec/14-open-decisions.md). Apply refuses any other kind.
//   - A suggestion must be approved first. Apply never touches an open or
//     rejected suggestion.
//   - The prior file version is captured before the edit, so Revert can restore
//     it.
//
// What "apply" does in this slice: the engine has no result-cache or batching
// feature for a workflow to honor yet, so apply does not invent one. It records
// the rollback point in the version store, appends a clearly-marked annotation
// to the workflow TOML naming the change to make and that it is unapplied, and
// writes an audit row. The annotation is a TOML comment, so the file still
// parses and runs unchanged; the real cache/batch honoring is a later slice.
// This keeps the reversible mechanism and its gates real without faking an
// engine feature.
package autonomic

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

// applyFilePerm is the permission the rewritten workflow file keeps: owner
// read/write, matching the CLI scaffold's filePerm.
const applyFilePerm = 0o600

// AuditActionApply and AuditActionRevert name the two autonomic actions in the
// audit log, alongside the governance actions already there. Every apply and
// revert writes one audit row so the change is on the record.
const (
	AuditActionApply  = "auto_apply"
	AuditActionRevert = "auto_revert"
)

// annotationMarker brackets the block apply appends to a workflow file, so a
// later apply or a human can find and replace it, and so the file still parses
// (every line is a TOML comment).
const (
	annotationBegin = "# >>> cairnos auto-apply"
	annotationEnd   = "# <<< cairnos auto-apply"
)

// Store is the slice of store.Store the applier needs: the version store for
// rollback points, the suggestion store to read the approved suggestion, and
// the audit log for the record. store.Store satisfies it.
type Store interface {
	store.WorkflowVersionStore
	store.SuggestionStore
	store.GovernanceStore
}

// Config gates the applier (.agents/spec/06-observability-and-tuning.md, Phase
// 6 auto-apply). AutoApplyEnabled is the master switch and defaults off; even
// on, the kind and approval gates still apply.
type Config struct {
	// Now anchors the version-snapshot and audit timestamps so a test can use a
	// fixed clock. Nil means time.Now.
	Now func() time.Time

	// WorkflowsDir is where workflow TOML files live, so apply can find the
	// file a suggestion's workflow names.
	WorkflowsDir string

	// Actor labels the audit rows apply and revert write; the CLI passes "cli"
	// and the served path passes the auth subject.
	Actor string

	// AutoApplyEnabled is the [autonomic] master switch. Default false: nothing
	// auto-applies until the operator opts in, and even then only the
	// output-neutral kinds.
	AutoApplyEnabled bool
}

// Applier applies approved output-neutral suggestions and reverts them. It
// holds the store and its config; one value serves the whole node.
type Applier struct {
	store Store
	cfg   Config
}

// New builds an applier over a store and config. A nil Now falls back to
// time.Now, so callers only set it in tests.
func New(database Store, cfg Config) *Applier {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &Applier{store: database, cfg: cfg}
}

// ApplyResult reports what an Apply did: the workflow it touched, the hash of
// the prior version it captured (the rollback point), and the hash after the
// edit.
type ApplyResult struct {
	WorkflowName string
	PriorHash    string
	NewHash      string
}

// Apply applies one approved, output-neutral suggestion to its workflow file,
// after every gate. The order is deliberate: gates first, then capture the
// prior version as the rollback point, then edit the file, then audit. A
// failure after the version is captured leaves the rollback point in place, so
// Revert still works.
func (a *Applier) Apply(ctx context.Context, suggestionID string) (ApplyResult, error) {
	if !a.cfg.AutoApplyEnabled {
		return ApplyResult{}, ErrAutoApplyDisabled
	}

	suggestion, err := a.store.GetSuggestion(ctx, suggestionID)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("auto-apply: load suggestion: %w", err)
	}

	if err := guardApplicable(suggestion); err != nil {
		return ApplyResult{}, err
	}

	workflowName := workflowOf(suggestion)
	if workflowName == "" {
		return ApplyResult{}, fmt.Errorf("%w: suggestion %s names no workflow", ErrNoWorkflowTarget, suggestionID)
	}

	return a.applyToFile(ctx, suggestion, workflowName)
}

// Revert restores a workflow file to its most recent captured version, the
// rollback the version store exists for (.agents/spec/13-roadmap.md, Phase 6).
// It writes the prior definition back, writes an audit row, and reports the
// hash it restored. A workflow with no captured version cannot be reverted.
func (a *Applier) Revert(ctx context.Context, workflowName string) (string, error) {
	versions, err := a.store.ListWorkflowVersions(ctx, workflowName)
	if err != nil {
		return "", fmt.Errorf("auto-revert: list versions for %s: %w", workflowName, err)
	}

	if len(versions) == 0 {
		return "", fmt.Errorf("%w: %s", ErrNoVersionToRevert, workflowName)
	}

	latest := versions[0]

	path, err := a.findWorkflowPath(workflowName)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(path, []byte(latest.DefinitionTOML), applyFilePerm); err != nil {
		return "", fmt.Errorf("auto-revert: write workflow %s: %w", workflowName, err)
	}

	detail := "reverted to version " + latest.Hash
	if err := a.audit(ctx, AuditActionRevert, workflowName, detail); err != nil {
		return "", err
	}

	return latest.Hash, nil
}

// applyToFile loads the workflow file, captures its current bytes as the
// rollback point, appends the annotation, writes the file, and records the
// audit row.
func (a *Applier) applyToFile(ctx context.Context, suggestion *store.Suggestion, workflowName string) (ApplyResult, error) {
	path, priorBytes, priorFlow, err := a.loadWorkflowFile(workflowName)
	if err != nil {
		return ApplyResult{}, err
	}

	if captureVersionErr := a.captureVersion(ctx, workflowName, priorFlow.Hash, priorBytes); captureVersionErr != nil {
		return ApplyResult{}, captureVersionErr
	}

	updated := annotate(priorBytes, suggestion)

	if writeFileErr := os.WriteFile(path, updated, applyFilePerm); writeFileErr != nil {
		return ApplyResult{}, fmt.Errorf("auto-apply: write workflow %s: %w", workflowName, writeFileErr)
	}

	newFlow, err := workflow.Load(path)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("auto-apply: reload workflow %s after edit: %w", workflowName, err)
	}

	detail := fmt.Sprintf("applied %s suggestion %s; rollback point %s", suggestion.Kind, suggestion.ID, priorFlow.Hash)
	if err := a.audit(ctx, AuditActionApply, workflowName, detail); err != nil {
		return ApplyResult{}, err
	}

	if err := a.store.UpdateSuggestionStatus(ctx, suggestion.ID, store.SuggestionApplied); err != nil {
		return ApplyResult{}, fmt.Errorf("auto-apply: mark suggestion applied: %w", err)
	}

	return ApplyResult{WorkflowName: workflowName, PriorHash: priorFlow.Hash, NewHash: newFlow.Hash}, nil
}

// captureVersion records the prior file bytes as a rollback point. Saving the
// same hash twice is a no-op in the store, so re-applying after a revert does
// not duplicate the snapshot.
func (a *Applier) captureVersion(ctx context.Context, workflowName, hash string, bytes []byte) error {
	err := a.store.SaveWorkflowVersion(ctx, &store.WorkflowVersion{
		Hash:           hash,
		WorkflowName:   workflowName,
		DefinitionTOML: string(bytes),
		CreatedAt:      a.cfg.Now(),
	})
	if err != nil {
		return fmt.Errorf("auto-apply: capture rollback point for %s: %w", workflowName, err)
	}

	return nil
}

// audit writes one autonomic audit row.
func (a *Applier) audit(ctx context.Context, action, target, detail string) error {
	err := a.store.AppendAudit(ctx, &store.AuditEntry{
		Actor:     a.actor(),
		Action:    action,
		Target:    target,
		Detail:    detail,
		CreatedAt: a.cfg.Now(),
	})
	if err != nil {
		return fmt.Errorf("auto-apply: write audit row: %w", err)
	}

	return nil
}

// actor returns the configured actor, defaulting to "autonomic" so an audit
// row always names a source.
func (a *Applier) actor() string {
	if a.cfg.Actor == "" {
		return "autonomic"
	}

	return a.cfg.Actor
}

// loadWorkflowFile finds the file the workflow name lives in and returns its
// path, raw bytes, and parsed form (for the hash). It scans the workflows
// directory because a file's name on disk need not match the workflow name
// inside it.
func (a *Applier) loadWorkflowFile(workflowName string) (string, []byte, *workflow.Workflow, error) {
	path, err := a.findWorkflowPath(workflowName)
	if err != nil {
		return "", nil, nil, err
	}

	data, err := os.ReadFile(path) // #nosec G304 -- the path comes from the workflows directory scan, not external input
	if err != nil {
		return "", nil, nil, fmt.Errorf("auto-apply: read workflow %s: %w", workflowName, err)
	}

	flow, err := workflow.Load(path)
	if err != nil {
		return "", nil, nil, fmt.Errorf("auto-apply: parse workflow %s: %w", workflowName, err)
	}

	return path, data, flow, nil
}

// findWorkflowPath scans the workflows directory for the TOML file whose
// [workflow] name matches workflowName, returning its path. A workflow with no
// matching file cannot be applied or reverted.
func (a *Applier) findWorkflowPath(workflowName string) (string, error) {
	entries, err := filepath.Glob(filepath.Join(a.cfg.WorkflowsDir, "*.toml"))
	if err != nil {
		return "", fmt.Errorf("auto-apply: scan workflows directory: %w", err)
	}

	for _, path := range entries {
		flow, loadErr := workflow.Load(path)
		if loadErr != nil {
			continue
		}

		if flow.Name == workflowName {
			return path, nil
		}
	}

	return "", fmt.Errorf("%w: %s", ErrWorkflowFileNotFound, workflowName)
}

// guardApplicable enforces the kind and approval gates: only an approved
// caching or batching suggestion may apply. Param tuning and prompt refactor
// never auto-apply, because R1 (output equivalence) is unsolved.
func guardApplicable(suggestion *store.Suggestion) error {
	if !OutputNeutral(suggestion.Kind) {
		return fmt.Errorf("%w: %s is not an output-neutral kind", ErrKindNotAutoApplicable, suggestion.Kind)
	}

	if suggestion.Status != store.SuggestionApproved {
		return fmt.Errorf("%w: suggestion %s is %s, not approved", ErrSuggestionNotApproved, suggestion.ID, suggestion.Status)
	}

	return nil
}

// OutputNeutral reports whether a suggestion kind changes action output.
// Caching and batching do not (a cache hit and a batched call return the same
// result), so they are the only kinds auto-apply touches. Param tuning and
// prompt refactor change output and stay manual.
func OutputNeutral(kind store.SuggestionKind) bool {
	switch kind {
	case store.SuggestionCaching, store.SuggestionBatching:
		return true
	case store.SuggestionParamTuning:
		return false
	default:
		return false
	}
}

// workflowOf reads the workflow name a suggestion targets, preferring the
// evidence's workflow key and falling back to the part of Target before the
// first dot (the "workflow.action" convention).
func workflowOf(suggestion *store.Suggestion) string {
	if name, ok := suggestion.Evidence[evidenceWorkflowKey].(string); ok && name != "" {
		return name
	}

	if before, _, found := strings.Cut(suggestion.Target, "."); found {
		return before
	}

	return suggestion.Target
}

// evidenceWorkflowKey is the evidence key the tuning detectors stamp the
// workflow name under (mirrors tuning.EvidenceWorkflow; named here so this
// package does not import tuning just for one string constant).
const evidenceWorkflowKey = "workflow"

// annotate appends the auto-apply annotation block to the workflow bytes,
// describing the change to make as a set of TOML comment lines so the file
// still parses. It replaces any prior block so re-applying does not stack
// annotations.
func annotate(original []byte, suggestion *store.Suggestion) []byte {
	stripped := stripAnnotation(string(original))

	var block strings.Builder

	block.WriteString(annotationBegin + "\n")
	fmt.Fprintf(&block, "# kind: %s\n", suggestion.Kind)
	fmt.Fprintf(&block, "# target: %s\n", suggestion.Target)
	fmt.Fprintf(&block, "# from suggestion: %s\n", suggestion.ID)
	fmt.Fprintf(&block, "# action: %s\n", applyHint(suggestion.Kind))
	block.WriteString("# status: recorded, not yet honored by the engine (see the version store for rollback)\n")
	block.WriteString(annotationEnd + "\n")

	if !strings.HasSuffix(stripped, "\n") {
		stripped += "\n"
	}

	return []byte(stripped + "\n" + block.String())
}

// applyHint names the change each output-neutral kind recommends, in one line
// for the annotation.
func applyHint(kind store.SuggestionKind) string {
	switch kind {
	case store.SuggestionCaching:
		return "add a result cache with a TTL for this action once the engine honors a cache hint"
	case store.SuggestionBatching:
		return "restructure the burst of same-runner calls into one batched action"
	case store.SuggestionParamTuning:
		return "no action (param tuning never auto-applies)"
	default:
		return "no action"
	}
}

// stripAnnotation removes a previously appended annotation block so re-applying
// replaces rather than stacks. It keeps everything before the begin marker,
// trimming trailing blank lines so the rewritten file does not accumulate them.
func stripAnnotation(content string) string {
	before, _, found := strings.Cut(content, annotationBegin)
	if !found {
		return content
	}

	return strings.TrimRight(before, "\n") + "\n"
}
