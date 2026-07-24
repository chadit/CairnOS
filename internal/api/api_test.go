// Integration tests for the Phase 1a API server: a real sqlite store, the
// mock runner, real listeners on ephemeral loopback ports, and both faces
// (gRPC and the generated HTTP gateway) of the one proto source.
package api_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/chadit/CairnOS/internal/api"
	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/internal/vault"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Workflow names the fixture serves from its workflows directory.
const (
	helloName = "hello"
	slowName  = "slow"
)

// Action and status literals the assertions share.
const (
	statusSucceeded = "succeeded"
	statusCanceled  = "canceled"
	statusRunning   = "running"
)

// shutdownTimeout bounds the fixture teardown drain.
const shutdownTimeout = 5 * time.Second

// loopbackEphemeral asks for any free loopback port, so parallel tests never
// collide on a fixed one.
const loopbackEphemeral = "127.0.0.1:0"

// helloTOML is a two-action linear flow over the mock runner, with costs so
// usage assertions have something to count.
const helloTOML = `
[workflow]
name = "hello"

[action.greet]
runner = "mock"
params = { output = "hello world", cost_usd = 0.25 }

[action.reply]
runner = "mock"
depends_on = ["greet"]
params = { output = "hello yourself", cost_usd = 0.25 }
`

// slowTOML naps long enough that CancelRun always lands mid-delay; the mock
// honors cancellation, so nothing waits the full minute.
const slowTOML = `
[workflow]
name = "slow"

[action.nap]
runner = "mock"
params = { delay_ms = 60000 }
`

// fixture is one running API server plus the clients to poke it; db is the
// store underneath the server, exposed for seeding rows directly.
type fixture struct {
	server       *api.Server
	conn         *grpc.ClientConn
	db           *sqlite.Store
	workflowsDir string
	workflow     cairnosv1.WorkflowServiceClient
	memory       cairnosv1.MemoryServiceClient
	vault        cairnosv1.VaultServiceClient
	rules        cairnosv1.LanguageRulesServiceClient
	suggestion   cairnosv1.SuggestionServiceClient
	admin        cairnosv1.AdminServiceClient
	governance   cairnosv1.GovernanceServiceClient
	breakers     *reliability.BreakerSet
	httpBase     string
}

// breakerAdmin adapts a reliability.BreakerSet to the api.BreakerAdmin
// surface, standing in for the engine so tests can trip a breaker open
// directly instead of arranging failing traffic.
type breakerAdmin struct {
	set *reliability.BreakerSet
}

// BreakerStates implements api.BreakerAdmin.
func (b *breakerAdmin) BreakerStates() map[string]reliability.State {
	return b.set.States()
}

// ResetBreaker implements api.BreakerAdmin.
func (b *breakerAdmin) ResetBreaker(name string) bool {
	return b.set.Reset(name)
}

// startServer boots a complete server on ephemeral loopback ports.
func startServer(t *testing.T) *fixture {
	t.Helper()

	dir := t.TempDir()
	workflowsDir := filepath.Join(dir, "workflows")

	if err := os.MkdirAll(workflowsDir, 0o750); err != nil {
		t.Fatalf("create workflows dir: %v", err)
	}

	writeWorkflow(t, workflowsDir, "hello.toml", helloTOML)
	writeWorkflow(t, workflowsDir, "slow.toml", slowTOML)
	writeWorkflow(t, workflowsDir, "budgeted.toml", budgetedTOML)

	dbPath := filepath.Join(dir, "cairnos.db")

	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	mem, err := memory.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}

	t.Cleanup(func() { _ = mem.Close() })

	vaultStore, err := vault.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open vault store: %v", err)
	}

	t.Cleanup(func() { _ = vaultStore.Close() })

	rulesStore, err := rules.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open rules store: %v", err)
	}

	t.Cleanup(func() { _ = rulesStore.Close() })

	logger := slog.New(slog.DiscardHandler)
	manager := api.NewManager(logger)

	manager.Bind(engine.New(testEngineConfig(manager), testRegistry(t), db))

	// The breaker set trips on a single failure so the reset test can open
	// a breaker without arranging failing traffic through the engine.
	breakers := reliability.NewBreakerSet(reliability.BreakerConfig{
		FailureThreshold: 1,
		Window:           time.Minute,
		MinRequests:      1,
		Cooldown:         time.Minute,
	})

	server := api.NewServer(&api.Config{
		GRPCAddr:     loopbackEphemeral,
		HTTPAddr:     loopbackEphemeral,
		WorkflowsDir: workflowsDir,
		Store:        db,
		Memory:       mem,
		Vault:        vaultStore,
		Rules:        rulesStore,
		Registry:     testRegistry(t),
		Breakers:     &breakerAdmin{set: breakers},
		Manager:      manager,
		Logger:       logger,
		// Security middleware on for the M1/M4 gateway tests: an explicit CORS
		// origin and a small body cap so the assertions have something to bite.
		CORSOrigins:     []string{corsTestOrigin},
		MaxBodyBytes:    testMaxBodyBytes,
		MaxRecvMsgBytes: testMaxRecvMsgBytes,
	})

	if err := server.Start(t.Context()); err != nil {
		t.Fatalf("start server: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), shutdownTimeout)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown server: %v", err)
		}
	})

	conn, err := grpc.NewClient(server.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial grpc: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return &fixture{
		server:       server,
		conn:         conn,
		db:           db,
		workflowsDir: workflowsDir,
		workflow:     cairnosv1.NewWorkflowServiceClient(conn),
		memory:       cairnosv1.NewMemoryServiceClient(conn),
		vault:        cairnosv1.NewVaultServiceClient(conn),
		rules:        cairnosv1.NewLanguageRulesServiceClient(conn),
		suggestion:   cairnosv1.NewSuggestionServiceClient(conn),
		admin:        cairnosv1.NewAdminServiceClient(conn),
		governance:   cairnosv1.NewGovernanceServiceClient(conn),
		breakers:     breakers,
		httpBase:     "http://" + server.HTTPAddr(),
	}
}

// testEngineConfig mirrors the engine test defaults: a breaker that cannot
// trip on tiny samples and the manager's hooks wired in.
func testEngineConfig(manager *api.Manager) engine.Config {
	return engine.Config{
		MaxConcurrentActions: 4,
		DefaultRetry:         reliability.Policy{MaxAttempts: 1},
		Breaker: reliability.BreakerConfig{
			FailureThreshold: 1,
			Window:           time.Minute,
			MinRequests:      1000,
			Cooldown:         time.Minute,
		},
		NewID:   manager.NewID,
		OnEvent: manager.Dispatch,
	}
}

// testRegistry builds a registry holding just the mock runner.
func testRegistry(t *testing.T) *runner.Registry {
	t.Helper()

	registry := runner.NewRegistry()

	if err := registry.Register(builtins.MockName, builtins.MockFactory); err != nil {
		t.Fatalf("register mock: %v", err)
	}

	if err := registry.BuildAll(nil); err != nil {
		t.Fatalf("build runners: %v", err)
	}

	return registry
}

func writeWorkflow(t *testing.T, dir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write workflow %s: %v", name, err)
	}
}

// collectEvents drains a RunWorkflow stream to EOF.
func collectEvents(t *testing.T, stream grpc.ServerStreamingClient[cairnosv1.RunEvent]) []*cairnosv1.RunEvent {
	t.Helper()

	var events []*cairnosv1.RunEvent

	for {
		evt, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return events
		}

		if err != nil {
			t.Fatalf("recv run event: %v", err)
		}

		events = append(events, evt)
	}
}

func TestRunWorkflowStreamsLifecycleEvents(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	stream, err := fix.workflow.RunWorkflow(t.Context(), &cairnosv1.RunWorkflowRequest{WorkflowName: helloName})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	events := collectEvents(t, stream)
	if len(events) < 3 {
		t.Fatalf("got %d events, want at least run_started, transitions, run_finished", len(events))
	}

	first, last := events[0], events[len(events)-1]

	if first.GetType() != cairnosv1.RunEventType_RUN_EVENT_TYPE_RUN_STARTED {
		t.Fatalf("first event type = %v, want RUN_STARTED", first.GetType())
	}

	if first.GetRunId() == "" {
		t.Fatal("run_started carried no run_id")
	}

	if last.GetType() != cairnosv1.RunEventType_RUN_EVENT_TYPE_RUN_FINISHED {
		t.Fatalf("last event type = %v, want RUN_FINISHED", last.GetType())
	}

	if got := last.GetRun().GetStatus(); got != statusSucceeded {
		t.Fatalf("final run status = %q, want %q (error: %v)", got, statusSucceeded, last.GetError())
	}

	for _, evt := range events[1 : len(events)-1] {
		if evt.GetType() != cairnosv1.RunEventType_RUN_EVENT_TYPE_ACTION_TRANSITION {
			t.Fatalf("middle event = %v, want only action transitions", evt)
		}

		if evt.GetRunId() != first.GetRunId() {
			t.Fatalf("event run_id = %q, want %q", evt.GetRunId(), first.GetRunId())
		}
	}

	assertActionStatuses(t, events, "greet")
	assertActionStatuses(t, events, "reply")
}

// assertActionStatuses checks one action's transition events arrive in
// lifecycle order.
func assertActionStatuses(t *testing.T, events []*cairnosv1.RunEvent, action string) {
	t.Helper()

	var statuses []string

	for _, evt := range events {
		if evt.GetAction().GetName() == action {
			statuses = append(statuses, evt.GetAction().GetStatus())
		}
	}

	want := []string{"pending", "ready", statusRunning, statusSucceeded}
	if len(statuses) != len(want) {
		t.Fatalf("action %q statuses = %v, want %v", action, statuses, want)
	}

	for idx, wantStatus := range want {
		if statuses[idx] != wantStatus {
			t.Fatalf("action %q statuses = %v, want %v", action, statuses, want)
		}
	}
}

func TestCancelRunStopsInFlightRun(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	stream, err := fix.workflow.RunWorkflow(t.Context(), &cairnosv1.RunWorkflowRequest{WorkflowName: slowName})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	runID := waitForActionRunning(t, stream)

	ack, err := fix.workflow.CancelRun(t.Context(), &cairnosv1.CancelRunRequest{RunId: runID})
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	if ack.GetRunId() != runID {
		t.Fatalf("cancel ack run_id = %q, want %q", ack.GetRunId(), runID)
	}

	events := collectEvents(t, stream)
	if len(events) == 0 {
		t.Fatal("no events after cancel; want the stream to finish with run_finished")
	}

	last := events[len(events)-1]
	if last.GetType() != cairnosv1.RunEventType_RUN_EVENT_TYPE_RUN_FINISHED {
		t.Fatalf("last event type = %v, want RUN_FINISHED", last.GetType())
	}

	if got := last.GetRun().GetStatus(); got != statusCanceled {
		t.Fatalf("final run status = %q, want %q", got, statusCanceled)
	}

	verifyStoredStatus(t, fix, runID, statusCanceled)
}

// waitForActionRunning reads the stream until the nap action reports
// running, returning the run id. Event-driven: no clock waits anywhere.
func waitForActionRunning(t *testing.T, stream grpc.ServerStreamingClient[cairnosv1.RunEvent]) string {
	t.Helper()

	for {
		evt, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv while waiting for running action: %v", err)
		}

		if evt.GetAction().GetStatus() == statusRunning {
			return evt.GetRunId()
		}
	}
}

func verifyStoredStatus(t *testing.T, fix *fixture, runID, want string) {
	t.Helper()

	got, err := fix.workflow.GetRun(t.Context(), &cairnosv1.GetRunRequest{RunId: runID})
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got.GetRun().GetStatus() != want {
		t.Fatalf("stored run status = %q, want %q", got.GetRun().GetStatus(), want)
	}
}

func TestGetRunAndListRunsRoundTrip(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	stream, err := fix.workflow.RunWorkflow(t.Context(), &cairnosv1.RunWorkflowRequest{WorkflowName: helloName})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	events := collectEvents(t, stream)
	runID := events[0].GetRunId()

	got, err := fix.workflow.GetRun(t.Context(), &cairnosv1.GetRunRequest{RunId: runID})
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got.GetRun().GetWorkflowName() != helloName || got.GetRun().GetStatus() != statusSucceeded {
		t.Fatalf("GetRun = %v, want succeeded %s run", got.GetRun(), helloName)
	}

	if len(got.GetActions()) != 2 {
		t.Fatalf("GetRun returned %d actions, want 2", len(got.GetActions()))
	}

	list, err := fix.workflow.ListRuns(t.Context(), &cairnosv1.ListRunsRequest{Workflow: helloName})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(list.GetRuns()) != 1 || list.GetRuns()[0].GetRunId() != runID {
		t.Fatalf("ListRuns = %v, want exactly the run %s", list.GetRuns(), runID)
	}

	_, err = fix.workflow.GetRun(t.Context(), &cairnosv1.GetRunRequest{RunId: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetRun(missing) code = %v, want NotFound", status.Code(err))
	}
}

func TestReplayRunRerunsPriorRun(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	stream, err := fix.workflow.RunWorkflow(t.Context(), &cairnosv1.RunWorkflowRequest{WorkflowName: helloName})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	runID := collectEvents(t, stream)[0].GetRunId()

	replayStream, err := fix.workflow.ReplayRun(t.Context(), &cairnosv1.ReplayRunRequest{RunId: runID})
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}

	events := collectEvents(t, replayStream)
	if len(events) == 0 {
		t.Fatal("replay produced no events")
	}

	last := events[len(events)-1]
	if last.GetType() != cairnosv1.RunEventType_RUN_EVENT_TYPE_RUN_FINISHED {
		t.Fatalf("last replay event type = %v, want RUN_FINISHED", last.GetType())
	}

	if got := last.GetRun().GetStatus(); got != statusSucceeded {
		t.Fatalf("replay run status = %q, want %q (error: %v)", got, statusSucceeded, last.GetError())
	}

	newRunID := events[0].GetRunId()
	if newRunID == runID {
		t.Fatal("replay reused the prior run id, want a fresh run")
	}

	// The new run links back to the one it replayed, under trigger "replay".
	replayRun, err := fix.db.GetRun(t.Context(), newRunID)
	if err != nil {
		t.Fatalf("get replay run: %v", err)
	}

	if replayRun.ParentRunID != runID {
		t.Fatalf("replay parent_run_id = %q, want %q", replayRun.ParentRunID, runID)
	}

	if replayRun.Trigger != "replay" {
		t.Fatalf("replay trigger = %q, want replay", replayRun.Trigger)
	}
}

func TestReplayRunRefusesHashMismatch(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	// A run whose recorded hash predates the current hello file: replay must
	// refuse it unless force is set.
	runID := "stale-hash-run"

	err := fix.db.CreateRun(t.Context(), &store.Run{
		ID:           runID,
		WorkflowName: helloName,
		WorkflowHash: "deadbeef",
		Status:       store.RunFailed,
		Trigger:      "cli",
		CreatedAt:    time.Now(),
	})
	if err != nil {
		t.Fatalf("seed stale run: %v", err)
	}

	stream, err := fix.workflow.ReplayRun(t.Context(), &cairnosv1.ReplayRunRequest{RunId: runID})
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}

	if _, err := stream.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("replay of a changed workflow code = %v (%v), want FailedPrecondition", status.Code(err), err)
	}

	forced, err := fix.workflow.ReplayRun(t.Context(), &cairnosv1.ReplayRunRequest{RunId: runID, Force: true})
	if err != nil {
		t.Fatalf("forced ReplayRun: %v", err)
	}

	events := collectEvents(t, forced)
	last := events[len(events)-1]

	if got := last.GetRun().GetStatus(); got != statusSucceeded {
		t.Fatalf("forced replay status = %q, want %q", got, statusSucceeded)
	}
}

func TestRunWorkflowRejectsBadRequests(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	badInputs, err := structpb.NewStruct(map[string]any{"bogus": 1})
	if err != nil {
		t.Fatalf("build inputs: %v", err)
	}

	tests := []struct {
		name     string
		req      *cairnosv1.RunWorkflowRequest
		wantCode codes.Code
	}{
		{
			name:     "unknown workflow name",
			req:      &cairnosv1.RunWorkflowRequest{WorkflowName: "ghost"},
			wantCode: codes.NotFound,
		},
		{
			name:     "undeclared input",
			req:      &cairnosv1.RunWorkflowRequest{WorkflowName: helloName, Inputs: badInputs},
			wantCode: codes.InvalidArgument,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stream, err := fix.workflow.RunWorkflow(t.Context(), tt.req)
			if err != nil {
				t.Fatalf("RunWorkflow: %v", err)
			}

			_, err = stream.Recv()
			if status.Code(err) != tt.wantCode {
				t.Fatalf("recv error code = %v (%v), want %v", status.Code(err), err, tt.wantCode)
			}
		})
	}
}

func TestValidateWorkflowReportsCanonicalCodes(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	tests := []struct {
		name     string
		toml     string
		wantCode string
	}{
		{
			name: "dependency cycle",
			toml: `
[workflow]
name = "loop"

[action.a]
runner = "mock"
depends_on = ["b"]

[action.b]
runner = "mock"
depends_on = ["a"]
`,
			wantCode: "workflow.cycle_detected",
		},
		{
			name: "unknown dependency",
			toml: `
[workflow]
name = "dangling"

[action.a]
runner = "mock"
depends_on = ["nowhere"]
`,
			wantCode: "workflow.unknown_dependency",
		},
		{
			name:     "not toml at all",
			toml:     "definitely not toml {{{",
			wantCode: "workflow.invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resp, err := fix.workflow.ValidateWorkflow(t.Context(), &cairnosv1.ValidateWorkflowRequest{Toml: []byte(tt.toml)})
			if err != nil {
				t.Fatalf("ValidateWorkflow: %v", err)
			}

			if resp.GetValid() {
				t.Fatal("valid = true, want validation failure")
			}

			if !hasErrorCode(resp.GetErrors(), tt.wantCode) {
				t.Fatalf("errors = %v, want code %q", resp.GetErrors(), tt.wantCode)
			}
		})
	}
}

func hasErrorCode(errs []*cairnosv1.Error, code string) bool {
	for _, item := range errs {
		if item.GetCode() == code {
			return true
		}
	}

	return false
}

func TestValidateWorkflowAcceptsGoodFile(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	resp, err := fix.workflow.ValidateWorkflow(t.Context(), &cairnosv1.ValidateWorkflowRequest{Toml: []byte(helloTOML)})
	if err != nil {
		t.Fatalf("ValidateWorkflow: %v", err)
	}

	if !resp.GetValid() || len(resp.GetErrors()) != 0 {
		t.Fatalf("response = %v, want valid with no errors", resp)
	}
}

func TestAdminHealthAndRunners(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	health, err := fix.admin.Health(t.Context(), &cairnosv1.HealthRequest{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	if health.GetStatus() != statusOK {
		t.Fatalf("health status = %q, want ok", health.GetStatus())
	}

	runners, err := fix.admin.ListRunners(t.Context(), &cairnosv1.ListRunnersRequest{})
	if err != nil {
		t.Fatalf("ListRunners: %v", err)
	}

	if len(runners.GetRunners()) != 1 || runners.GetRunners()[0].GetName() != builtins.MockName {
		t.Fatalf("runners = %v, want just %q", runners.GetRunners(), builtins.MockName)
	}

	// The mock has taken no traffic in this fixture, so it has no breaker
	// yet; reset answers NOT_FOUND instead of pretending to act.
	_, err = fix.admin.ResetBreaker(t.Context(), &cairnosv1.ResetBreakerRequest{Runner: builtins.MockName})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("ResetBreaker code = %v, want NotFound for a runner with no breaker", status.Code(err))
	}
}

func TestMemoryCRUDOverGRPC(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	added, err := fix.memory.AddMemory(t.Context(), &cairnosv1.AddMemoryRequest{
		Entry: &cairnosv1.MemoryEntry{Scope: memoryScopeGlobal, Content: "prefer tabs", Importance: 80},
	})
	if err != nil {
		t.Fatalf("AddMemory: %v", err)
	}

	got, err := fix.memory.GetMemory(t.Context(), &cairnosv1.GetMemoryRequest{Id: added.GetId()})
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}

	if got.GetEntry().GetContent() != "prefer tabs" {
		t.Fatalf("entry content = %q, want the added content", got.GetEntry().GetContent())
	}

	found, err := fix.memory.SearchMemory(t.Context(), &cairnosv1.SearchMemoryRequest{Scope: memoryScopeGlobal, Text: "tabs"})
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}

	if len(found.GetEntries()) != 1 {
		t.Fatalf("search found %d entries, want 1", len(found.GetEntries()))
	}

	if _, err := fix.memory.DeleteMemory(t.Context(), &cairnosv1.DeleteMemoryRequest{Id: added.GetId()}); err != nil {
		t.Fatalf("DeleteMemory: %v", err)
	}

	_, err = fix.memory.GetMemory(t.Context(), &cairnosv1.GetMemoryRequest{Id: added.GetId()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetMemory after delete code = %v, want NotFound", status.Code(err))
	}
}
