package api

import (
	"context"
	"log/slog"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

// eventBufferSize bounds each watcher's event channel. The engine hook never
// blocks (Dispatch drops on overflow), so the buffer absorbs bursts from
// parallel actions while a stream consumer catches up.
const eventBufferSize = 256

// triggerAPI marks runs started over the API in run history.
const triggerAPI = "api"

// RunOutcome is the terminal result of one managed run: the finished run
// record, or the rejection error when the engine refused the run before
// creating any record.
type RunOutcome struct {
	Run *store.Run
	Err error
}

// RunHandle is one managed run as its starter sees it. Events is nil unless
// the run was started with a watcher; the manager closes it after the final
// event, so consumers range until close and then read the outcome.
type RunHandle struct {
	ID      string
	Events  <-chan engine.Event
	Outcome <-chan RunOutcome

	// workspace is the tenant the run started in. Watch checks it against the
	// caller's workspace so a handle cannot be attached to from another tenant.
	workspace string
}

// reservation pre-mints the next run ID so a starter can subscribe to its
// run's events before the engine emits any. taken closes when the engine
// consumes the ID, which is the signal that the next start may proceed.
type reservation struct {
	id    string
	taken chan struct{}
}

// runState is the per-run routing the manager owns: the cancel func plus the
// workspace the run was started in. Cancel and Watch check the caller's
// workspace against this one, so a tenant cannot reach another tenant's run by
// id before the store's own workspace filter would catch it.
type runState struct {
	cancel    context.CancelFunc
	workspace string
}

// Manager owns every run the serve process starts: it pre-mints run IDs
// (wired into engine.Config.NewID), fans engine events out to per-run
// watchers (wired into engine.Config.OnEvent), and keeps the runID-to-cancel
// map that CancelRun and shutdown act on.
//
// Correlation works because StartRun serializes on startMu from "reservation
// placed" to "engine consumed the ID": with every run on this engine started
// through the manager, no other run can claim a reservation that is not its
// own.
type Manager struct {
	logger *slog.Logger

	startMu sync.Mutex

	mu      sync.Mutex
	eng     *engine.Engine
	pending *reservation
	cancels map[string]*runState
	subs    map[string]chan engine.Event

	// handles retains watched run handles started through StartWatched so a
	// later Watch can attach to a run's live event stream. The A2A adapter
	// needs this because CreateTask and TaskUpdates are two calls: the run
	// starts on the first and a consumer streams it on the second. release
	// drops a handle once its run can produce no more events, so a Watch that
	// arrives after completion misses and falls back to the stored record.
	handles map[string]*RunHandle
}

// NewManager builds an unbound manager; call Bind once the engine exists.
// The two-step dance breaks the construction cycle: the engine's config
// needs NewID and Dispatch, and the manager needs the engine to start runs.
func NewManager(logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}

	return &Manager{
		logger:  logger,
		cancels: map[string]*runState{},
		subs:    map[string]chan engine.Event{},
		handles: map[string]*RunHandle{},
	}
}

// StartWatched starts a watched run and retains its handle by run id so a
// later Watch can stream its events, returning the run id once the engine
// accepts the run. It is the A2A CreateTask path: the run begins here and a
// consumer attaches with TaskUpdates afterward. Like StartRun, the run's
// lifetime detaches from ctx, so a dropped client does not end the run.
func (m *Manager) StartWatched(ctx context.Context, flow *workflow.Workflow, inputs map[string]any, trigger string) (string, error) {
	handle, err := m.StartRun(ctx, flow, inputs, true, trigger)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	m.handles[handle.ID] = handle
	m.mu.Unlock()

	return handle.ID, nil
}

// Watch hands off the retained handle for one watched run to a single
// consumer, reporting whether a live handle was found for the caller's
// workspace. A handle started in another workspace reads as a miss, the same
// answer a finished or unknown run gives, so a tenant cannot attach to another
// tenant's live event stream by id (.agents/spec/08-auth-and-access.md, "Run
// manager workspace scoping"). A miss means the run already finished (release
// dropped its handle), was never started here, or belongs to another tenant, so
// the caller falls back to the stored terminal record, which the store filters
// by workspace anyway.
func (m *Manager) Watch(ctx context.Context, runID string) (*RunHandle, bool) {
	workspace := store.WorkspaceFromContext(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	handle, found := m.handles[runID]
	if !found || handle.workspace != workspace {
		return nil, false
	}

	delete(m.handles, runID)

	return handle, true
}

// Bind attaches the engine the manager starts runs on.
func (m *Manager) Bind(eng *engine.Engine) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.eng = eng
}

// NewID feeds engine.Config.NewID: it hands out the pending reservation when
// one is waiting, and mints a fresh ULID otherwise.
func (m *Manager) NewID() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.pending != nil {
		id := m.pending.id
		close(m.pending.taken)
		m.pending = nil

		return id
	}

	return ulid.Make().String()
}

// Dispatch feeds engine.Config.OnEvent: it routes one event to the run's
// watcher, if any. The send never blocks; a full buffer drops the event and
// logs, because a slow stream consumer must not stall the scheduler.
//
//nolint:gocritic // hugeParam: the signature is fixed by the engine.Config.OnEvent hook contract
func (m *Manager) Dispatch(evt engine.Event) {
	m.mu.Lock()
	sub, watched := m.subs[evt.RunID]
	m.mu.Unlock()

	if !watched {
		return
	}

	select {
	case sub <- evt:
	default:
		m.logger.Warn("dropping run event: watcher buffer full",
			"run_id", evt.RunID, "type", string(evt.Type), "action", evt.Action)
	}
}

// Cancel signals one active run to stop, reporting whether the run was active
// here for the caller's workspace. A run started in another workspace reads as
// not active, the same answer an unknown id gives, so a tenant cannot cancel
// another tenant's run by id before the store's own workspace filter would
// catch it (.agents/spec/08-auth-and-access.md, "Run manager workspace
// scoping"). The run finalizes asynchronously as canceled.
func (m *Manager) Cancel(ctx context.Context, runID string) bool {
	workspace := store.WorkspaceFromContext(ctx)

	m.mu.Lock()
	state, active := m.cancels[runID]
	m.mu.Unlock()

	if !active || state.workspace != workspace {
		return false
	}

	state.cancel()

	return true
}

// Shutdown cancels every active run so streams drain and the engine
// finalizes each record as canceled before the process exits.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(m.cancels))

	for _, state := range m.cancels {
		cancels = append(cancels, state.cancel)
	}
	m.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

// launcher runs one workflow on the engine and returns its terminal record.
// StartRun and StartReplay differ only in this closure: RunWorkflow for a
// fresh run, ReplayRun for a replay of a prior one. Everything around it (the
// reservation, the watcher wiring, the workspace scoping) is identical, so the
// two share launchRun.
type launcher func(ctx context.Context, eng *engine.Engine) (*store.Run, error)

// StartRun launches one run on the bound engine in its own goroutine and
// returns once the engine has accepted it (the run record exists) or
// rejected it (validation failed; the error returns here). The run's
// lifetime detaches from ctx: a dropped client does not kill a run, only
// Cancel, Shutdown, or completion ends it.
func (m *Manager) StartRun(ctx context.Context, flow *workflow.Workflow, inputs map[string]any, watch bool, trigger string) (*RunHandle, error) {
	return m.launchRun(ctx, watch, func(runCtx context.Context, eng *engine.Engine) (*store.Run, error) {
		return eng.RunWorkflow(runCtx, flow, inputs, trigger)
	})
}

// StartReplay replays a prior run on the bound engine: its succeeded actions
// seed forward and the rest re-execute, watchable and cancelable like any run
// (.agents/spec/parity/workflow-language.md, "Replay from failed action"). It
// always watches, since the replay RPC streams the new run's events.
func (m *Manager) StartReplay(ctx context.Context, prior *store.Run, priorResults []*store.ActionResult, flow *workflow.Workflow) (*RunHandle, error) {
	return m.launchRun(ctx, true, func(runCtx context.Context, eng *engine.Engine) (*store.Run, error) {
		return eng.ReplayRun(runCtx, prior, priorResults, flow)
	})
}

// launchRun runs the reservation handshake shared by StartRun and StartReplay:
// it reserves the next run ID, wires the watcher before the engine emits, and
// returns once the engine accepts the run (the record exists) or rejects it
// (the error returns here). The run's lifetime detaches from ctx: a dropped
// client does not kill a run, only Cancel, Shutdown, or completion ends it.
func (m *Manager) launchRun(ctx context.Context, watch bool, launch launcher) (*RunHandle, error) {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	eng := m.engine()
	if eng == nil {
		return nil, ErrManagerUnbound
	}

	// The run's workspace is the caller's, lifted from ctx the same way the
	// engine stamps run.WorkspaceID (engine.go), so the manager records the run
	// under the same tenant the store will. Cancel and Watch compare against it.
	workspace := store.WorkspaceFromContext(ctx)

	res := &reservation{id: ulid.Make().String(), taken: make(chan struct{})}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	events := m.register(res, cancel, workspace, watch)

	outcome := make(chan RunOutcome, 1)

	go m.execute(runCtx, res.id, eng, launch, outcome)

	select {
	case <-res.taken:
		return &RunHandle{ID: res.id, Events: events, Outcome: outcome, workspace: workspace}, nil
	case out := <-outcome:
		// The run goroutine finished before the start handshake resolved. A
		// consumed reservation means the run was accepted and simply finished
		// fast: the outcome goes back on the buffered channel for the caller.
		// An unconsumed one means the engine rejected the run before creating
		// a record, so the rejection returns as the start error.
		if reservationTaken(res) {
			outcome <- out

			return &RunHandle{ID: res.id, Events: events, Outcome: outcome, workspace: workspace}, nil
		}

		m.release(res.id)

		return nil, out.Err
	}
}

// reservationTaken reports whether the engine consumed the reserved ID.
func reservationTaken(res *reservation) bool {
	select {
	case <-res.taken:
		return true
	default:
		return false
	}
}

// engine returns the bound engine under the lock.
func (m *Manager) engine() *engine.Engine {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.eng
}

// register installs the reservation, the cancel func with its workspace, and
// (when watched) the event channel, all before the engine goroutine starts, so
// no event can fire without a route.
func (m *Manager) register(res *reservation, cancel context.CancelFunc, workspace string, watch bool) chan engine.Event {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pending = res
	m.cancels[res.id] = &runState{cancel: cancel, workspace: workspace}

	var events chan engine.Event
	if watch {
		events = make(chan engine.Event, eventBufferSize)
		m.subs[res.id] = events
	}

	return events
}

// execute runs the launcher to completion and releases the run's routing
// state. Closing the events channel is safe here: every engine emission for
// the run happens before the launcher returns.
func (m *Manager) execute(ctx context.Context, id string, eng *engine.Engine, launch launcher, outcome chan<- RunOutcome) {
	run, err := launch(ctx, eng)

	m.release(id)

	outcome <- RunOutcome{Run: run, Err: err}
}

// release tears down one run's routing state once it can produce no more
// events.
func (m *Manager) release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if state, exists := m.cancels[id]; exists {
		state.cancel()
		delete(m.cancels, id)
	}

	if sub, exists := m.subs[id]; exists {
		close(sub)
		delete(m.subs, id)
	}

	// Drop the retained handle once the run can produce no more events: a
	// Watch arriving after this point misses and falls back to the stored
	// terminal record rather than blocking on a channel that already closed.
	delete(m.handles, id)

	if m.pending != nil && m.pending.id == id {
		m.pending = nil
	}
}
