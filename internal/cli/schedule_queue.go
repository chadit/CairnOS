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
	"github.com/chadit/CairnOS/internal/reauth"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/pkg/runner"
)

// notifyDiscordWebhookEnv holds the Discord webhook URL re-auth alerts post to.
// The webhook URL grants posting rights, so it is a secret read from the
// environment, never a config file (D12). Unset disables re-auth notifications.
const notifyDiscordWebhookEnv = "CAIRNOS_NOTIFY_DISCORD_WEBHOOK" // #nosec G101 -- environment variable name, not a credential

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

// schedulePayload is what a cron fire stores on a queued work item: enough to
// load and run the workflow when a worker claims it. The queue treats the
// payload as opaque bytes; this is the CLI's agreed shape on both ends.
type schedulePayload struct {
	WorkflowPath string `json:"workflow_path"`
	WorkflowName string `json:"workflow_name"`
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
	payload, err := json.Marshal(schedulePayload{
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

// queueRunnerAdapter turns an existing scheduler.Runner (one that runs a
// workflow given a Schedule) into a QueueRunner (one that runs a workflow given
// a claimed work item). It decodes the payload back into the schedule fields the
// inner runner reads, so the queue model reuses the engine and manager fire
// paths unchanged.
type queueRunnerAdapter struct {
	fire scheduler.Runner
}

// Run decodes the work item and delegates to the inner runner. A failure returns
// to the worker, which naks the item for backoff retry or dead-letters it.
func (a *queueRunnerAdapter) Run(ctx context.Context, item *scheduler.WorkItem) error {
	var payload schedulePayload
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		return fmt.Errorf("decode work item payload: %w", err)
	}

	if err := a.fire.Fire(ctx, &scheduler.Schedule{
		WorkflowPath: payload.WorkflowPath,
		WorkflowName: payload.WorkflowName,
	}); err != nil {
		return fmt.Errorf("run queued workflow %q: %w", payload.WorkflowName, err)
	}

	return nil
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

// newQueueWorker builds the queue worker from the scheduler config block, mapping
// the [scheduler] knobs onto the worker's tuning. It wires runner.IsReauth as the
// hold predicate, so a NeedsReauth failure parks the job for a human rather than
// dead-lettering it (D12).
func newQueueWorker(queue scheduler.WorkQueue, queueRunner scheduler.QueueRunner, cfg *config.Scheduler, metrics scheduler.QueueMetrics, logger *slog.Logger) *scheduler.Worker {
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
		Holdable:      runner.IsReauth,
		HoldKey:       reauthHoldKey,
		OnHold:        newReauthOnHold(logger),
		ResumeProbe:   newReauthResumeProbe(logger),
		Logger:        logger,
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
// not a provider the prober recognizes, in which case the job is held but only an
// operator can resume it.
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

// newReauthOnHold builds the worker's hold callback. When a job parks for re-auth
// and a Discord webhook is configured, it posts an operator alert naming the tool
// and the fix; with no webhook set it returns nil, so holds are only logged and
// counted. A failed post is logged, never propagated: a missed alert must not
// change the held job's fate (D12).
func newReauthOnHold(logger *slog.Logger) func(context.Context, *scheduler.WorkItem, error) {
	webhook := os.Getenv(notifyDiscordWebhookEnv)
	if webhook == "" {
		return nil
	}

	notifier := notify.NewDiscordNotifier(webhook)
	home := reauthHomeDir(logger)

	return func(ctx context.Context, item *scheduler.WorkItem, runErr error) {
		alert := notify.ReauthAlert{DedupKey: item.DedupKey}

		if rerr, ok := errors.AsType[*runner.Error](runErr); ok {
			alert.Tool = reauthTool(rerr.Code)
			alert.Remediation = rerr.Message
			alert.Account = reauthAccount(home, alert.Tool)
		}

		if err := notifier.Notify(ctx, alert); err != nil {
			logger.Error("re-auth notification failed", "item", item.ID, "error", err)
		}
	}
}

// reauthTool pulls the CLI tool name out of a runner error code like
// "codex.needs_reauth", returning the part before the last dot. A code with no
// dot comes back unchanged.
func reauthTool(code string) string {
	if idx := strings.LastIndex(code, "."); idx > 0 {
		return code[:idx]
	}

	return code
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
