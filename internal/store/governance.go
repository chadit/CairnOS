package store

import (
	"context"
	"time"
)

// Governance audit actions (.agents/spec/12-ui-and-governance.md, "Control
// actions"). Every admin-scoped control writes one audit row naming the
// action it performed.
const (
	AuditActionPause     = "pause"
	AuditActionResume    = "resume"
	AuditActionTerminate = "terminate"
)

// AuditEntry is one append-only governance audit row: who did what to which
// target, when, with optional structured detail. Audit rows outlive the runs
// they reference; the retention sweep leaves them alone
// (.agents/spec/12-ui-and-governance.md, "Control actions").
type AuditEntry struct {
	CreatedAt time.Time
	ID        string
	Actor     string // the auth subject when present, else "cli"/"local"
	Action    string // AuditActionPause, AuditActionResume, or AuditActionTerminate
	Target    string // the workflow name or run id the action applied to
	Detail    string // free-form context, e.g. the pause reason
}

// AuditFilter narrows ListAudit.
type AuditFilter struct {
	// Limit caps the result count; zero means the caller's default.
	Limit int
}

// WorkflowPause is the persisted pause flag for one workflow name. Presence
// means paused; PausedAt, Actor, and Reason record who paused it and why
// (.agents/spec/12-ui-and-governance.md, "Control actions").
type WorkflowPause struct {
	PausedAt     time.Time
	WorkflowName string
	Actor        string
	Reason       string
}

// GovernanceStore is the governance slice of the store
// (.agents/spec/12-ui-and-governance.md, "Governance"): the append-only audit
// log, the per-workflow pause flag, and the per-period spend query budgets
// read. Split out so the engine and the governance API depend on this narrow
// surface instead of all of Store.
type GovernanceStore interface {
	AuditLog
	WorkflowPauseStore
	SpendLedger
}

// AuditLog is the append-only record of governance actions. It has no update
// or delete, so a written row is never rewritten.
type AuditLog interface {
	// AppendAudit persists one audit row. A zero ID is stamped with a fresh
	// ULID and a zero CreatedAt with the current time.
	AppendAudit(ctx context.Context, entry *AuditEntry) error

	// ListAudit returns audit rows newest-first, narrowed by the filter.
	ListAudit(ctx context.Context, filter AuditFilter) ([]*AuditEntry, error)
}

// WorkflowPauseStore holds the per-workflow pause flags the engine checks
// before it starts a run.
type WorkflowPauseStore interface {
	// SetWorkflowPause records (or overwrites) the pause flag for one
	// workflow name.
	SetWorkflowPause(ctx context.Context, pause *WorkflowPause) error

	// ClearWorkflowPause removes the pause flag for one workflow name,
	// reporting whether a flag was present to clear.
	ClearWorkflowPause(ctx context.Context, workflowName string) (bool, error)

	// GetWorkflowPause fetches one workflow's pause flag, reporting whether
	// the workflow is paused.
	GetWorkflowPause(ctx context.Context, workflowName string) (*WorkflowPause, bool, error)

	// ListWorkflowPauses returns every paused workflow, newest pause first.
	ListWorkflowPauses(ctx context.Context) ([]*WorkflowPause, error)
}

// SpendLedger answers the budget question: how much a workflow has spent in
// the current period.
type SpendLedger interface {
	// SpendSince sums the estimated cost_usd of every action result of the
	// named workflow's runs created at or after since. It is the budget
	// accumulator: a soft ceiling on the estimated cost runners report, not a
	// billed total (.agents/spec/12-ui-and-governance.md, "Budgets").
	SpendSince(ctx context.Context, workflowName string, since time.Time) (float64, error)
}
