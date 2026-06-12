package autonomic

import "errors"

// Sentinel errors for the gated auto-apply path. Each names a gate or a
// missing precondition, so a caller can tell a refused apply from a real
// failure.
var (
	// ErrAutoApplyDisabled reports an Apply call while auto_apply_enabled is
	// false. Auto-apply is off by default; the operator opts in deliberately.
	ErrAutoApplyDisabled = errors.New("auto-apply is disabled (autonomic.auto_apply_enabled = false)")

	// ErrKindNotAutoApplicable reports an Apply against a suggestion kind that
	// changes output. Only caching and batching auto-apply; param tuning and
	// prompt refactor stay manual because output equivalence is unsolved (R1).
	ErrKindNotAutoApplicable = errors.New("only output-neutral kinds (caching, batching) auto-apply")

	// ErrSuggestionNotApproved reports an Apply against a suggestion that is not
	// approved. Auto-apply never touches an open or rejected suggestion.
	ErrSuggestionNotApproved = errors.New("suggestion is not approved")

	// ErrNoWorkflowTarget reports a suggestion whose evidence and target name no
	// workflow, leaving apply nothing to edit.
	ErrNoWorkflowTarget = errors.New("suggestion names no workflow")

	// ErrWorkflowFileNotFound reports no TOML file in the workflows directory
	// whose [workflow] name matches the suggestion's workflow.
	ErrWorkflowFileNotFound = errors.New("no workflow file matches the suggestion's workflow")

	// ErrNoVersionToRevert reports a Revert against a workflow with no captured
	// version, so there is no rollback point to restore.
	ErrNoVersionToRevert = errors.New("no captured version to revert to")
)
