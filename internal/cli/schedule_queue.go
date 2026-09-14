package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/notify"
	"github.com/chadit/CairnOS/internal/poll"
	"github.com/chadit/CairnOS/internal/reach"
	"github.com/chadit/CairnOS/internal/reauth"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// NotifyDiscordWebhookEnv holds the Discord webhook URL re-auth alerts post to.
// The webhook URL grants posting rights, so it is a secret read from the
// environment, never a config file (D12). Unset disables re-auth notifications.
const NotifyDiscordWebhookEnv = "CAIRNOS_NOTIFY_DISCORD_WEBHOOK" // #nosec G101 -- environment variable name, not a credential

// queueMetricsAdapter forwards the worker's queue metrics to the telemetry
// recorder, so the work-queue panels (depth, runs, dead-letters, reclaimed
// leases, oldest age) land on the same Prometheus endpoint the rest of the
// metrics use (.agents/spec/06-observability-and-tuning.md). Metric recording is
// fire-and-forget, so it carries no request context.
type queueMetricsAdapter struct {
	rec *telemetry.Recorder
}

// ItemRun records one queued run by outcome.
func (m *queueMetricsAdapter) ItemRun(ok bool) {
	m.rec.QueueItemRun(context.Background(), ok)
}

// ItemDeadLettered records one work item exhausting its deliveries.
func (m *queueMetricsAdapter) ItemDeadLettered() {
	m.rec.QueueDeadLettered(context.Background())
}

// ItemHeld records one work item parked for re-authentication.
func (m *queueMetricsAdapter) ItemHeld() {
	m.rec.QueueHeld(context.Background())
}

// ItemResumed records one held work item returned to ready after re-auth.
func (m *queueMetricsAdapter) ItemResumed() {
	m.rec.QueueResumed(context.Background())
}

// LeaseReclaimed records leases the sweep returned to ready.
func (m *queueMetricsAdapter) LeaseReclaimed(count int) {
	m.rec.QueueLeaseReclaimed(context.Background(), count)
}

// Depth records the queue-depth snapshot.
func (m *queueMetricsAdapter) Depth(stats scheduler.QueueStats) {
	m.rec.QueueDepth(context.Background(), stats.Ready, stats.Leased, stats.Held, stats.Dead, stats.OldestReadyAge.Seconds())
}

// triggerPoll marks runs a poll fire enqueues, so run history tells a polled run
// apart from a plain scheduled one. It rides the work item's payload, not the
// dedup key.
const triggerPoll = "poll"

// SchedulePayload is what a cron fire stores on a queued work item. The queue
// treats it as opaque bytes; this is the CLI's agreed shape on both ends. Inputs
// and Trigger are omitempty so rows queued before the poll feature still
// unmarshal: absent Trigger means "schedule", absent Inputs means the workflow's
// declared defaults.
type SchedulePayload struct {
	WorkflowPath string         `json:"workflow_path"`
	WorkflowName string         `json:"workflow_name"`
	Inputs       map[string]any `json:"inputs,omitempty"`
	Trigger      string         `json:"trigger,omitempty"`
}

// scheduleEnqueueRunner is the scheduler's Runner in the queue model: a cron
// fire enqueues a work item instead of running the workflow inline. Enqueue is
// exactly-once on the (schedule, fire-time) key, so a fire no worker can take
// right away waits in the queue rather than being dropped
// (.agents/spec/14-open-decisions.md, S8).
type scheduleEnqueueRunner struct {
	queue  scheduler.WorkQueue
	logger *slog.Logger
}

// Fire enqueues one scheduled occurrence. A dedup hit (another node already
// enqueued this occurrence) is a clean no-op, logged at debug so it does not
// look like a fire was lost.
func (r *scheduleEnqueueRunner) Fire(ctx context.Context, sched *scheduler.Schedule) error {
	payload, err := json.Marshal(SchedulePayload{
		WorkflowPath: sched.WorkflowPath,
		WorkflowName: sched.WorkflowName,
	})
	if err != nil {
		return fmt.Errorf("encode work item payload: %w", err)
	}

	inserted, err := r.queue.Enqueue(ctx, &scheduler.WorkItem{
		DedupKey: scheduler.DedupKeyFor(sched.ID, sched.NextFire),
		Payload:  payload,
	})
	if err != nil {
		return fmt.Errorf("enqueue scheduled occurrence: %w", err)
	}

	if inserted {
		r.logger.Info("scheduled occurrence enqueued",
			"schedule", sched.ID, "workflow", sched.WorkflowName, "fire", sched.NextFire)

		return nil
	}

	r.logger.Debug("scheduled occurrence already queued",
		"schedule", sched.ID, "workflow", sched.WorkflowName, "fire", sched.NextFire)

	return nil
}

// inputsRunner fires a scheduled workflow with explicit inputs and a trigger
// label, so the queue adapter carries a poll fire's mapped inputs and "poll"
// trigger through to the run. Both the manager fire runner (serve) and the
// engine fire runner (schedule run) satisfy it; a plain scheduled item decodes
// to nil inputs and the schedule trigger.
type inputsRunner interface {
	FireRun(ctx context.Context, sched *scheduler.Schedule, inputs map[string]any, trigger string) error
}

// QueueRunnerAdapter turns an inputsRunner into a QueueRunner, so a claimed work
// item drives a fire. It decodes the payload back into the schedule fields the
// inner runner reads, so the queue model reuses the engine and manager fire
// paths unchanged. The reach monitor gates the run on the workflow's
// [workflow.requires] hosts.
type QueueRunnerAdapter struct {
	fire  inputsRunner
	reach *reach.Monitor
}

// Run decodes the work item and delegates to the inner runner. A failure goes
// back to the worker for backoff or dead-letter, unless the required host is
// down: that is an UnreachableError, which the worker holds. The host is
// re-probed after a failure so a mid-run outage is caught on the run that hit it.
func (a *QueueRunnerAdapter) Run(ctx context.Context, item *scheduler.WorkItem) error {
	var payload SchedulePayload
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		return fmt.Errorf("decode work item payload: %w", err)
	}

	// Registered here as well as on the daemon's fire, because a held item
	// resumed after a restart reaches the worker before any fire has told the
	// fresh monitor what the workflow needs. A load failure is left for FireRun
	// to report; an unregistered workflow is merely ungated.
	if flow, err := workflow.Load(payload.WorkflowPath); err == nil {
		a.reach.Register(payload.WorkflowName, flow.Requires)
	}

	if reason, blocked := a.reach.Blocked(ctx, payload.WorkflowName); blocked {
		return reach.NewUnreachable(payload.WorkflowName, reason, nil)
	}

	trigger := payload.Trigger
	if trigger == "" {
		trigger = triggerSchedule
	}

	sched := &scheduler.Schedule{
		WorkflowPath: payload.WorkflowPath,
		WorkflowName: payload.WorkflowName,
	}

	err := a.fire.FireRun(ctx, sched, payload.Inputs, trigger)
	if err == nil {
		return nil
	}

	// A run cut short by shutdown failed because of the shutdown; probing on
	// the dead context would only report the cancellation as an outage.
	if ctx.Err() != nil {
		return fmt.Errorf("run queued workflow %q: %w", payload.WorkflowName, err)
	}

	if reason, blocked := a.reach.Check(ctx, payload.WorkflowName); blocked {
		return reach.NewUnreachable(payload.WorkflowName, reason, err)
	}

	return fmt.Errorf("run queued workflow %q: %w", payload.WorkflowName, err)
}

// openWorkQueue opens the durable work-queue for the configured backend. On
// Postgres it borrows the schedule store's pool (one pool per node), so the
// returned closer is a no-op; on sqlite it opens the same database file the run
// store uses, sharing it through a disjoint table, and the closer releases that
// handle.
func openWorkQueue(ctx context.Context, dir string, cfg *config.Config, scheds scheduleStore) (scheduler.WorkQueue, func() error, error) {
	if pgScheds, ok := scheds.(*scheduler.Postgres); ok {
		queue, err := scheduler.NewPostgresQueue(ctx, pgScheds.Pool())
		if err != nil {
			return nil, nil, fmt.Errorf("open work queue: %w", err)
		}

		return queue, func() error { return nil }, nil
	}

	path := storePath(dir, cfg.Store.DSN)
	if err := ensureDataDir(path); err != nil {
		return nil, nil, err
	}

	//nolint:contextcheck // OpenSQLiteQueue carries no ctx by contract: schema setup is process-lifetime initialization, not request work
	queue, err := scheduler.OpenSQLiteQueue(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open work queue: %w", err)
	}

	return queue, queue.Close, nil
}

// newQueueWorker builds the queue worker from the [scheduler] config block. It
// wires runner.IsReauth as the hold predicate, so a NeedsReauth failure parks
// the job for a human rather than dead-lettering it (D12). The fan-out routes
// held and dead_lettered alerts to Discord when those events are selected; a
// nil fan-out leaves both callbacks unset
// (.agents/spec/parity/messaging-bridge.md, decision 7).
func newQueueWorker(queue scheduler.WorkQueue, queueRunner scheduler.QueueRunner, cfg *config.Scheduler, metrics scheduler.QueueMetrics, fanout *notify.Fanout, monitor *reach.Monitor, logger *slog.Logger) *scheduler.Worker {
	return scheduler.NewWorker(&scheduler.WorkerConfig{
		Queue:         queue,
		Runner:        queueRunner,
		Concurrency:   cfg.WorkerConcurrency,
		Prefetch:      cfg.Prefetch,
		AckWait:       cfg.AckWait,
		Backoff:       cfg.Backoff,
		MaxDeliveries: cfg.MaxDeliveries,
		SweepInterval: cfg.SweepInterval,
		MaxAge:        cfg.MaxAge,
		Metrics:       metrics,
		Holdable:      Holdable,
		HoldKey:       HoldKey,
		OnHold:        SkipReachHolds(NewReauthOnHold(fanout, logger)),
		OnDeadLetter:  NewDeadLetterFanout(fanout),
		ResumeProbe:   ResumeProbe(monitor, newReauthResumeProbe(logger)),
		Logger:        logger,
	})
}

// Holdable is the worker's hold predicate: a run that needs re-auth waits for
// a human, and a run whose required host is down waits for the host. Both are
// failures a redelivery cannot fix and a dead-letter would misreport.
func Holdable(runErr error) bool {
	return runner.IsReauth(runErr) || reach.IsUnreachable(runErr)
}

// HoldKey names what a held item waits on: the credential provider for a
// re-auth hold, the workflow's reach key for a host hold.
func HoldKey(runErr error) string {
	if reach.IsUnreachable(runErr) {
		return reach.HoldKey(runErr)
	}

	return reauthHoldKey(runErr)
}

// SkipReachHolds wraps the re-auth hold alert so a host hold posts nothing
// from the worker: the reach monitor already announced the outage, and one
// line per outage is the whole point. A nil inner stays nil.
func SkipReachHolds(inner func(context.Context, *scheduler.WorkItem, error)) func(context.Context, *scheduler.WorkItem, error) {
	if inner == nil {
		return nil
	}

	return func(ctx context.Context, item *scheduler.WorkItem, runErr error) {
		if reach.IsUnreachable(runErr) {
			return
		}

		inner(ctx, item, runErr)
	}
}

// ResumeProbe routes a held item's key to whichever prober owns it: the reach
// monitor for a host hold, the credential probe for a re-auth hold. A nil
// credential probe (no resolvable home) still lets host holds resume.
func ResumeProbe(monitor *reach.Monitor, reauthProbe func(string, time.Time) bool) func(string, time.Time) bool {
	return func(key string, now time.Time) bool {
		if strings.HasPrefix(key, reach.HoldPrefix) {
			return monitor.Resumable(key)
		}

		return reauthProbe != nil && reauthProbe(key, now)
	}
}

// newReachMonitor builds the reachability monitor the daemon gates fires on
// and the worker holds items on, posting its lines through the fan-out under
// the connectivity event. A nil fan-out or a deselected event leaves the gate
// working and the operator untold, the same shape as the other producers.
func newReachMonitor(fanout *notify.Fanout, logger *slog.Logger) *reach.Monitor {
	return reach.New(reach.Config{
		Notify: func(ctx context.Context, line string) {
			fanout.Emit(ctx, config.NotifyEventConnectivity, line)
		},
		Logger: logger,
	})
}

// newReauthResumeProbe builds the worker's resume probe: it reports whether a
// held item's credential is valid again by re-reading the tool's creds file and
// checking its expiry, never a real call. It returns nil when the user home
// cannot be resolved, which disables auto-resume so held items wait for a manual
// resume (D12).
func newReauthResumeProbe(logger *slog.Logger) func(string, time.Time) bool {
	home := reauthHomeDir(logger)
	if home == "" {
		return nil
	}

	return func(holdKey string, now time.Time) bool {
		provider := reauth.Provider(holdKey)
		if !reauth.Known(provider) {
			return false
		}

		cred, err := reauth.ReadCredential(provider, home)
		if err != nil {
			// A missing or unreadable credential is the held state's normal case:
			// the operator has not signed back in yet. Debug, not error.
			logger.Debug("re-auth resume probe could not read credential",
				"provider", holdKey, "error", err)

			return false
		}

		return cred.Valid(now)
	}
}

// reauthAccount reads which account a tool's credential belongs to, best-effort:
// an unresolved home, unknown provider, or unreadable file yields "", so the
// alert simply omits the account line.
func reauthAccount(home, tool string) string {
	provider := reauth.Provider(tool)
	if home == "" || !reauth.Known(provider) {
		return ""
	}

	cred, err := reauth.ReadCredential(provider, home)
	if err != nil {
		return ""
	}

	return cred.Account
}

// reauthHomeDir resolves the user home the credential probes read under, or ""
// when it cannot be determined.
func reauthHomeDir(logger *slog.Logger) string {
	home, err := os.UserHomeDir()
	if err != nil {
		logger.Warn("re-auth credential probe disabled: cannot resolve home dir", "error", err)

		return ""
	}

	return home
}

// reauthHoldKey maps a re-auth run error to the credential provider the held job
// waits on, stored on the item so the resume probe knows which credential to
// check (D12). It returns "" when the error is not a runner error or its tool is
// not a provider the prober knows, leaving the job held until an operator
// resumes it.
func reauthHoldKey(runErr error) string {
	rerr, ok := errors.AsType[*runner.Error](runErr)
	if !ok {
		return ""
	}

	provider := reauth.Provider(reauthTool(rerr.Code))
	if !reauth.Known(provider) {
		return ""
	}

	return string(provider)
}

// NewReauthOnHold builds the worker's hold callback, routing alerts through the
// Discord fan-out when "held" is selected. Returns nil when the fan-out is off
// or "held" is deselected, leaving holds logged and counted. The alert matches
// the direct notifier's content, keeping the default ["held"] set byte-identical
// (.agents/spec/parity/messaging-bridge.md, decision 7). Emit is
// fire-and-forget: a dropped alert never changes the job's fate (D12).
func NewReauthOnHold(fanout *notify.Fanout, logger *slog.Logger) func(context.Context, *scheduler.WorkItem, error) {
	if !fanout.Selects(config.NotifyEventHeld) {
		return nil
	}

	home := reauthHomeDir(logger)

	return func(ctx context.Context, item *scheduler.WorkItem, runErr error) {
		alert := notify.ReauthAlert{DedupKey: item.DedupKey}

		if rerr, ok := errors.AsType[*runner.Error](runErr); ok {
			alert.Tool = reauthTool(rerr.Code)
			alert.Remediation = rerr.Message
			alert.Account = reauthAccount(home, alert.Tool)
		}

		fanout.Emit(ctx, config.NotifyEventHeld, alert.Message())
	}
}

// NewDeadLetterFanout builds the worker's dead-letter callback, emitting a
// dead_lettered line when that event is selected. It returns nil when the
// fan-out is off or the event is deselected, so the worker's OnDeadLetter stays
// unset and the dead-letter path is only logged and counted
// (.agents/spec/parity/messaging-bridge.md, decision 7).
func NewDeadLetterFanout(fanout *notify.Fanout) func(context.Context, *scheduler.WorkItem, error) {
	if !fanout.Selects(config.NotifyEventDeadLettered) {
		return nil
	}

	return func(ctx context.Context, item *scheduler.WorkItem, runErr error) {
		fanout.Emit(ctx, config.NotifyEventDeadLettered, deadLetteredLine(item, runErr))
	}
}

// deadLetteredLine renders a dead-lettered work item as one Discord line, naming
// the dedup key and the delivery count so an operator can find the job.
func deadLetteredLine(item *scheduler.WorkItem, runErr error) string {
	return fmt.Sprintf("CairnOS: work item %s dead-lettered after %d deliveries: %v",
		item.DedupKey, item.Deliveries, runErr)
}

// reauthTool pulls the CLI tool name out of a runner error code like
// "codex.needs_reauth", returning the part before the last dot. A code with no
// dot comes back unchanged.
func reauthTool(code string) string {
	before, _, found := strings.CutLast(code, ".")
	if !found || before == "" {
		return code
	}

	return before
}

// pollQueue is what a poll fire needs from the work queue: enqueue one work item
// per new event and persist the poll cursor between fires. Both queue backends
// satisfy it, so the poll fire path shares the one handle it enqueues on.
type pollQueue interface {
	Enqueue(ctx context.Context, item *scheduler.WorkItem) (bool, error)
	scheduler.PollCursors
}

// pollPauseChecker gates a poll fire on the workflow's pause state, the same
// pause seam the scheduler fire runner and the webhook handler read. The store
// satisfies it.
type pollPauseChecker interface {
	GetWorkflowPause(ctx context.Context, workflowName string) (*store.WorkflowPause, bool, error)
}

// PollAwareRunner wraps the enqueue runner so a [workflow.poll] fire turns one
// cron occurrence into zero or more enqueued events, deduped through the queue's
// PollDedupKeyFor key. A non-poll workflow delegates straight to the wrapped
// runner, byte-for-byte today's behavior
// (.agents/spec/parity/triggers-webhooks.md, "Firing path").
type PollAwareRunner struct {
	inner   scheduler.Runner
	queue   pollQueue
	sources poll.Sources
	pauses  pollPauseChecker
	reach   *reach.Monitor
	logger  *slog.Logger
}

// newPollAwareRunner wraps inner with poll awareness and the reach gate. Both
// shipped work queues persist poll cursors, so the wrapper reads the queue as a
// pollQueue. A queue without cursor support falls back to inner, so poll
// workflows fail at fire time rather than panicking the process; in practice
// that branch is unreachable.
func newPollAwareRunner(inner scheduler.Runner, queue scheduler.WorkQueue, sources poll.Sources, pauses pollPauseChecker, monitor *reach.Monitor, logger *slog.Logger) scheduler.Runner {
	cursors, ok := queue.(pollQueue)
	if !ok {
		logger.Warn("work queue does not persist poll cursors; poll workflows disabled")

		return inner
	}

	return &PollAwareRunner{inner: inner, queue: cursors, sources: sources, pauses: pauses, reach: monitor, logger: logger}
}

// A workflow whose required host is down is skipped without enqueueing, so an
// outage leaves no backlog to run when it ends. A paused workflow polls nothing
// and holds its cursor, so events wait for resume instead of being dropped. A
// source error returns without advancing the cursor, so the next poll re-reads
// the same window.

// Fire loads the workflow fresh (FireRunner precedent). A plain scheduled
// workflow delegates to the wrapped enqueue runner. A poll workflow polls its
// source for events newer than the stored cursor, enqueues one deduped item per
// event with the event's mapped inputs, then advances the cursor.
func (r *PollAwareRunner) Fire(ctx context.Context, sched *scheduler.Schedule) error {
	flow, err := workflow.Load(sched.WorkflowPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", sched.WorkflowPath, err)
	}

	r.reach.Register(sched.WorkflowName, flow.Requires)

	if reason, blocked := r.reach.Blocked(ctx, sched.WorkflowName); blocked {
		r.logger.Info("skipping fire while a required host is unreachable",
			"schedule", sched.ID, "workflow", flow.Name, "reason", reason)

		return nil
	}

	if flow.Poll == nil {
		if fireErr := r.inner.Fire(ctx, sched); fireErr != nil {
			return fmt.Errorf("fire schedule %s: %w", sched.ID, fireErr)
		}

		return nil
	}

	source, err := r.sources.Get(flow.Poll.Source)
	if err != nil {
		return fmt.Errorf("poll %s: %w", flow.Name, err)
	}

	paused := r.paused(ctx, flow.Name)
	if paused {
		r.logger.Info("skipping poll for paused workflow", "schedule", sched.ID, "workflow", flow.Name)
	}

	// Dispatch owns the observable firing behavior (pause holds the cursor, a
	// source error holds it, the cursor advances only after every event
	// enqueues); this wrapper supplies the store-backed cursor and the
	// payload-marshaling enqueue closure.
	if err := poll.Dispatch(ctx, poll.DispatchInput{
		ScheduleID: sched.ID,
		Source:     source,
		Params:     flow.Poll.Params,
		Paused:     paused,
		Cursors:    queueCursors{queue: r.queue},
		Enqueue:    r.enqueueFor(sched, flow),
	}); err != nil {
		return fmt.Errorf("poll %s: %w", flow.Name, err)
	}

	return nil
}

// paused reports whether the workflow is paused. A nil checker never pauses; a
// read failure logs and proceeds rather than silently dropping a poll.
func (r *PollAwareRunner) paused(ctx context.Context, workflowName string) bool {
	if r.pauses == nil {
		return false
	}

	_, paused, err := r.pauses.GetWorkflowPause(ctx, workflowName)
	if err != nil {
		r.logger.Error("poll pause check", "workflow", workflowName, "error", err)

		return false
	}

	return paused
}

// enqueueFor builds the poll.EnqueueEvent closure for one schedule: it marshals
// the schedule payload with the event's mapped inputs and the poll trigger, then
// enqueues under the poll dedup key. A dedup hit (an overlapping poll window or a
// concurrent leader already enqueued this event) is a clean no-op.
func (r *PollAwareRunner) enqueueFor(sched *scheduler.Schedule, flow *workflow.Workflow) poll.EnqueueEvent {
	return func(ctx context.Context, event poll.Event) error {
		payload, err := json.Marshal(SchedulePayload{
			WorkflowPath: sched.WorkflowPath,
			WorkflowName: sched.WorkflowName,
			Inputs:       filterPollInputs(event.Inputs, flow.Inputs),
			Trigger:      triggerPoll,
		})
		if err != nil {
			return fmt.Errorf("encode poll work item for schedule %s: %w", sched.ID, err)
		}

		inserted, err := r.queue.Enqueue(ctx, &scheduler.WorkItem{
			DedupKey: scheduler.PollDedupKeyFor(sched.ID, event.ID),
			Payload:  payload,
		})
		if err != nil {
			return fmt.Errorf("enqueue poll event %s for schedule %s: %w", event.ID, sched.ID, err)
		}

		if !inserted {
			r.logger.Debug("poll event already queued", "schedule", sched.ID, "workflow", flow.Name, "event", event.ID)

			return nil
		}

		r.logger.Info("poll event enqueued", "schedule", sched.ID, "workflow", flow.Name, "event", event.ID)

		return nil
	}
}

// queueCursors adapts the work queue's PollCursors to poll.Cursors, mapping the
// never-polled sentinel to the empty first-poll cursor.
type queueCursors struct {
	queue scheduler.PollCursors
}

// Get returns the schedule's stored cursor, or "" when it has never polled.
func (c queueCursors) Get(ctx context.Context, scheduleID string) (string, error) {
	cursor, err := c.queue.GetCursor(ctx, scheduleID)
	if err != nil {
		if errors.Is(err, scheduler.ErrCursorNotFound) {
			return "", nil
		}

		return "", fmt.Errorf("read poll cursor for schedule %s: %w", scheduleID, err)
	}

	return cursor, nil
}

// Set upserts the schedule's cursor.
func (c queueCursors) Set(ctx context.Context, scheduleID, cursor string) error {
	if err := c.queue.SetCursor(ctx, scheduleID, cursor); err != nil {
		return fmt.Errorf("write poll cursor for schedule %s: %w", scheduleID, err)
	}

	return nil
}

// filterPollInputs keeps only the event input keys the workflow declares, the
// documented trigger-payload mapping rule; extra source fields drop here, and
// engine.validateInputs still has final say at run time. It returns nil for an
// empty result so the payload omits the inputs field.
func filterPollInputs(eventInputs map[string]any, declared map[string]workflow.InputDecl) map[string]any {
	filtered := make(map[string]any, len(eventInputs))

	for key, value := range eventInputs {
		if _, ok := declared[key]; ok {
			filtered[key] = value
		}
	}

	if len(filtered) == 0 {
		return nil
	}

	return filtered
}

// buildPollSources maps each poll source name to its adapter over the built
// runner registry. github_issues drives the github builtin's list_issues op, so
// it is available whenever the registry is built (BuildAll builds every
// registered runner). A [workflow.poll] naming an unbuilt source fails at fire
// time with poll.ErrUnknownSource.
func buildPollSources(registry *runner.Registry) poll.Sources {
	sources := poll.Sources{}

	if gh, ok := registry.Get(builtins.GitHubName); ok {
		sources[poll.GitHubSourceName] = poll.NewGitHubIssues(gh)
	}

	return sources
}

// runProducerAndWorker runs the scheduler daemon (the producer) and the queue
// worker (the consumer) until ctx cancels, returning the first error either
// reports. Both return nil on a clean ctx-cancel shutdown.
func runProducerAndWorker(ctx context.Context, daemon *scheduler.Scheduler, worker *scheduler.Worker) error {
	errs := make(chan error, 2)

	go func() { errs <- daemon.Run(ctx) }()
	go func() { errs <- worker.Run(ctx) }()

	var firstErr error

	for range 2 {
		if err := <-errs; err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}
