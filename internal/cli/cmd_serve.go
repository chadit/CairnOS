package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/chadit/CairnOS/internal/agentloop"
	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/hive"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/sweep"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/internal/vault"
)

const serveUsage = "usage: cairnos serve [--dir D]\n"

// serveShutdownTimeout bounds the graceful drain: the HTTP listener, the
// canceled runs, and the gRPC GracefulStop all have to finish inside it
// before the hard stop fires.
const serveShutdownTimeout = 10 * time.Second

// cmdServe is the Phase 1a long-lived process: the gRPC and HTTP API plus
// the scheduler daemon in one process, stopping cleanly on interrupt.
func (a *App) cmdServe(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if flags.NArg() > 0 {
		a.printErrf("%s", serveUsage)

		return ExitUsage
	}

	dir, err := resolveDir(*dirFlag)
	if err != nil {
		a.printErrf("serve: %v\n", err)

		return ExitFailure
	}

	db, cfg, code, proceed := a.openStoreAt(ctx, "serve", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	mem, code, proceed := a.openMemoryAt(ctx, "serve", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	vaultDB, code, proceed := a.openVaultAt(ctx, "serve", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(vaultDB)

	rulesSvc, code, proceed := a.openRulesAt(ctx, "serve", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(rulesSvc)

	scheds, code, proceed := a.openSchedulesAt(ctx, "serve", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(scheds)

	// The auth store only opens when auth is on; a nil store keeps the
	// whole token path out of the process.
	var keyStore authStore

	if cfg.Auth.Enabled {
		keyStore, _, code, proceed = a.openAuthAt(ctx, "serve", *dirFlag)
		if !proceed {
			return code
		}
		defer a.closeStore(keyStore)
	}

	// The tenancy store only opens when OAuth human login is on; the
	// access-key path needs no workspace membership lookup.
	var tenants tenancyStore

	if cfg.OAuth.Enabled {
		tenants, code, proceed = a.openTenancyAt(ctx, "serve", *dirFlag)
		if !proceed {
			return code
		}
		defer a.closeStore(tenants)
	}

	return a.runServe(ctx, &cfg, dir, &serveStores{
		db:       db,
		mem:      mem,
		vault:    vaultDB,
		rules:    rulesSvc,
		scheds:   scheds,
		keyStore: keyStore,
		tenants:  tenants,
	})
}

// serveStores groups the stores the serve process opens, so runServe takes one
// argument instead of a long parameter list (gocritic paramTypeCombine,
// varnamelen on a wide signature).
type serveStores struct {
	db       store.Store
	mem      memory.Store
	vault    vault.Store
	rules    rules.Service
	scheds   scheduleStore
	keyStore authStore
	tenants  tenancyStore
}

// runServe wires telemetry, the manager, the engine, the API server, the
// retention sweep, and the scheduler daemon, then blocks until ctx cancels
// and drains everything. A non-nil authStore turns on Phase 4a token
// enforcement; signing material loads from the environment before anything
// heavier starts, so a missing secret fails fast.
func (a *App) runServe(ctx context.Context, cfg *config.Config, dir string, stores *serveStores) int {
	db, mem, scheds := stores.db, stores.mem, stores.scheds

	authn, err := buildAuthenticator(cfg, stores.keyStore)
	if err != nil {
		a.printErrf("serve: auth: %v\n", err)

		return ExitFailure
	}

	reconciled, err := db.ReconcileInterrupted(ctx)
	if err != nil {
		a.printErrf("serve: reconcile interrupted runs: %v\n", err)

		return ExitFailure
	}

	if reconciled > 0 {
		a.logger.Info("reconciled interrupted runs", "count", reconciled)
	}

	tel, err := telemetry.Setup(ctx, cfg.Telemetry, appVersion())
	if err != nil {
		a.printErrf("serve: telemetry: %v\n", err)

		return ExitFailure
	}

	queue, closeQueue, err := openWorkQueue(ctx, dir, cfg, scheds)
	if err != nil {
		return a.failServe(ctx, tel, "work queue", err)
	}
	defer func() { _ = closeQueue() }()

	// Every serve component logs through the trace-correlating handler, so
	// records emitted inside a span carry its trace and span IDs.
	logger := slog.New(telemetry.NewLogHandler(a.logger.Handler()))

	registry, err := newRegistry(ctx, cfg.Runners, mem, stores.rules)
	if err != nil {
		return a.failServe(ctx, tel, "runners", err)
	}
	defer a.closeRegistry(registry)

	// The manager owns run IDs and event fan-out, so its hooks ride the
	// engine config; every run in this process starts through the manager.
	manager := api.NewManager(logger)

	engCfg := engineConfig(cfg)
	engCfg.NewID = manager.NewID
	engCfg.OnEvent = manager.Dispatch
	engCfg.Instrumentation = tel.Recorder()
	engCfg.AgentExecutor = agentloop.New(registry, mem, db, tel.Recorder())
	// Governance enforcement reads the store's spend and pause state. A
	// warn-mode budget breach raises the budget metric through the recorder
	// and, with no notifier wired here, an engine-side slog warning; routing
	// warn alerts to a workflow's notify runners needs the workflow's notify
	// targets at the breach point, which the warn-budget callback does not yet
	// carry, so that path stays a follow-up (.agents/spec/12-ui-and-governance.md,
	// "Budgets").
	engCfg.Budget = db
	engCfg.Pause = db

	eng := engine.New(engCfg, registry, db)
	manager.Bind(eng)

	if err := wireBreakerTelemetry(ctx, eng, tel.Recorder()); err != nil {
		return a.failServe(ctx, tel, "telemetry", err)
	}

	workflowsDir := filepath.Join(dir, workflowsDirName)

	apiCfg := &api.Config{
		GRPCAddr:     cfg.API.GRPC,
		HTTPAddr:     cfg.API.HTTP,
		WorkflowsDir: workflowsDir,
		Store:        db,
		Memory:       mem,
		Vault:        stores.vault,
		Rules:        stores.rules,
		Registry:     registry,
		Breakers:     eng,
		Manager:      manager,
		Logger:       logger,
		BindPolicy: api.BindPolicy{
			AuthEnabled:               cfg.Auth.Enabled,
			TLSTerminated:             cfg.Auth.TLSTerminated,
			ServeWithoutAuthOnNetwork: cfg.Auth.ServeWithoutAuthOnNetwork,
		},
		CORSOrigins:     cfg.API.CORSOrigins,
		MaxBodyBytes:    cfg.API.MaxBodyBytes,
		MaxRecvMsgBytes: cfg.API.MaxRecvMsgBytes,
	}

	if code, mounted := a.attachChat(ctx, cfg, tel, apiCfg, &chatDeps{
		store:        db,
		memory:       mem,
		vault:        stores.vault,
		registry:     registry,
		manager:      manager,
		workflowsDir: workflowsDir,
		logger:       logger,
	}); !mounted {
		return code
	}

	// Assigned only when non-nil: a typed-nil *auth.Authenticator inside
	// the interface field would read as "auth on" to the server.
	if authn != nil {
		apiCfg.Auth = authn
		apiCfg.LoginLimiter = auth.NewLoginLimiter(auth.LoginLimitConfig{
			MaxConcurrent: cfg.Auth.MaxConcurrentLogins,
			RatePerMinute: cfg.Auth.LoginRatePerMinute,
		})
		apiCfg.StreamRecheck = cfg.Auth.StreamRecheck
	}

	bundle, err := buildA2ABundle(cfg, manager, api.NewWorkflowResolver(workflowsDir, logger), db, logger)
	if err != nil {
		a.printErrf("serve: %v\n", err)
		a.closeTelemetry(ctx, tel)

		return ExitFailure
	}

	if bundle != nil {
		if err := bundle.Attach(apiCfg); err != nil {
			a.printErrf("serve: a2a: %v\n", err)
			a.closeTelemetry(ctx, tel)

			return ExitFailure
		}

		a.printOutf("a2a card published at %s%s\n", "http://"+cfg.API.HTTP, a2aWellKnownPath)
	}

	// OAuth human login mounts when configured and auth is on: a minted
	// CairnOS token is only useful if the interceptor verifies it, which needs
	// authn. tenants is the membership store the callback resolves roles in.
	if cfg.OAuth.Enabled {
		ext, oauthErr := buildOAuthExtension(ctx, cfg, authn, stores.tenants, logger)
		if oauthErr != nil {
			a.printErrf("serve: oauth: %v\n", oauthErr)
			a.closeTelemetry(ctx, tel)

			return ExitFailure
		}

		apiCfg.Extensions = append(apiCfg.Extensions, ext)

		a.printOutf("oauth login available at %s%s\n", "http://"+cfg.API.HTTP, oauthloginPath)
	}

	// The MCP http transport mounts on the same mux as A2A and OAuth: the MCP
	// handlers enforce auth themselves (the same authn the interceptor uses), so
	// they sit on a root HTTP route outside the gateway interceptor
	// (.agents/spec/10-mcp-integration.md, "MCP Server").
	mcpPath, mcpErr := attachMCP(cfg, &mcpServeDeps{
		manager:      manager,
		store:        db,
		memory:       mem,
		rules:        stores.rules,
		workflowsDir: workflowsDir,
		authn:        authn,
		logger:       logger,
	}, apiCfg)
	if mcpErr != nil {
		a.printErrf("serve: mcp: %v\n", mcpErr)
		a.closeTelemetry(ctx, tel)

		return ExitFailure
	}

	if mcpPath != "" {
		a.printOutf("mcp server available at %s%s\n", "http://"+cfg.API.HTTP, mcpPath)
	}

	server := api.NewServer(apiCfg)

	if err := server.Start(ctx); err != nil {
		a.printErrf("serve: %v\n", err)
		a.closeTelemetry(ctx, tel)

		return ExitFailure
	}

	a.printOutf("gRPC listening on %s\n", server.GRPCAddr())
	a.printOutf("HTTP listening on %s\n", server.HTTPAddr())

	if addr := tel.PrometheusAddr(); addr != "" {
		a.printOutf("metrics listening on %s\n", addr)
	}

	a.printOutf("serving; interrupt to stop\n")

	sweepDone := startSweep(ctx, cfg, db, stores.keyStore, logger)
	hiveDone := startHiveSubscribe(ctx, cfg, db, scheds, logger)

	a.announceHiveSubscribe(hiveDone)

	code := a.serveUntilCanceled(ctx, server, &serveSchedulerDeps{
		scheds:  scheds,
		manager: manager,
		pauses:  db,
		queue:   queue,
		cfg:     &cfg.Scheduler,
		metrics: &queueMetricsAdapter{rec: tel.Recorder()},
	})

	<-sweepDone

	if hiveDone != nil {
		<-hiveDone
	}

	a.closeTelemetry(ctx, tel)

	return code
}

// announceHiveSubscribe prints the hive-subscribe banner when the loop is
// running (a non-nil done channel), staying quiet when hive is off.
func (a *App) announceHiveSubscribe(hiveDone <-chan struct{}) {
	if hiveDone != nil {
		a.printOutf("hive subscribe loop running every %s\n", hiveSubscribeInterval)
	}
}

// hiveSubscribeInterval is how often the serve process pulls hive tips into the
// suggestion queue. It is a quiet background pull, so the cadence is slow.
const hiveSubscribeInterval = time.Hour

// startHiveSubscribe runs a periodic hive subscribe on its own goroutine when
// [hive] is enabled, landing received tips in the suggestion queue so they
// flow through the same human review as local suggestions
// (.agents/spec/09-a2a-and-hive.md). It returns nil (and starts nothing) when
// the hive is disabled, so the serve loop adds no overhead for the common case.
// Subscribing is receive-only and never publishes, so the periodic loop is safe
// regardless of the publish gate.
func startHiveSubscribe(ctx context.Context, cfg *config.Config, db store.Store, scheds scheduleStore, logger *slog.Logger) <-chan struct{} {
	if !cfg.Hive.Enabled {
		return nil
	}

	// The hive is a private cluster over shared state, so it enables only on
	// Postgres; on SQLite there is no shared backend for the elected primary or
	// the sealed multi-node model, so serve refuses rather than running a
	// single-node hive that cannot coordinate (R2/R3 in 14-open-decisions.md).
	if cfg.HiveNeedsPostgres() {
		logger.Warn("hive subscribe loop not started: the hive requires the postgres store backend",
			"store_backend", cfg.Store.Backend)

		return nil
	}

	// One elected node in the cluster runs the hive loop, so a multi-node cluster
	// presents as one hive participant rather than N. The primary election is the
	// scheduler-leader advisory-lock pattern on its own key (R3).
	coord, closeCoord := newHiveCoordinator(scheds)
	if coord == nil {
		logger.Warn("hive subscribe loop not started: no postgres coordinator for the hive primary")

		return nil
	}

	client, err := buildHiveClient(ctx, cfg)
	if err != nil {
		logger.Warn("hive subscribe loop not started", "error", err)
		closeCoord(ctx)

		return nil
	}

	ingester, err := hive.NewIngester(db, store.NewID)
	if err != nil {
		logger.Warn("hive subscribe loop not started", "error", err)
		closeCoord(ctx)

		return nil
	}

	done := make(chan struct{})
	categories := hiveCategories(cfg.Hive.Categories)

	go func() {
		defer close(done)
		// Release the primary lock on shutdown so another node takes over
		// promptly; a detached context because ctx is already canceled by then.
		defer closeCoord(context.WithoutCancel(ctx))

		runHiveSubscribeLoop(ctx, coord, client, ingester, categories, logger)
	}()

	return done
}

// runHiveSubscribeLoop pulls tips on the interval until ctx cancels. A failed
// pull logs and waits for the next tick rather than tearing down serve:
// network tips are a convenience, never load-bearing. Only the elected primary
// pulls; a follower node skips the tick, so the cluster ingests each tip once.
func runHiveSubscribeLoop(ctx context.Context, coord scheduler.Coordinator, client *hive.HTTPHiveClient, ingester *hive.Ingester, categories []hive.Category, logger *slog.Logger) {
	ticker := time.NewTicker(hiveSubscribeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			primary, err := coord.Leader(ctx)
			if err != nil {
				logger.Warn("hive primary check", "error", err)

				continue
			}

			if !primary {
				continue
			}

			tips, err := client.Subscribe(ctx, categories)
			if err != nil {
				logger.Warn("hive subscribe", "error", err)

				continue
			}

			saved, ingestErr := ingester.Ingest(ctx, tips)
			if ingestErr != nil {
				logger.Warn("hive ingest", "error", ingestErr)

				continue
			}

			if saved > 0 {
				logger.Info("hive tips ingested into suggestion queue", "count", saved)
			}
		}
	}
}

// startSweep runs the retention prune loop on its own goroutine, closing
// the returned channel once the loop exits so shutdown can wait for an
// in-flight pass to finish its writes. With auth on, the same pass clears
// expired denylist rows.
func startSweep(ctx context.Context, cfg *config.Config, db store.Store, keyStore authStore, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})

	sweepCfg := &sweep.Config{
		Store:    db,
		Interval: cfg.Retention.Sweep,
		Runs:     cfg.Retention.Runs,
		Outputs:  cfg.Retention.Outputs,
		Logger:   logger,
	}

	// Assigned only when non-nil so a nil interface never reaches the
	// Denylist field, which keeps sweep's own nil check meaningful.
	if keyStore != nil {
		sweepCfg.Denylist = keyStore
	}

	go func() {
		defer close(done)

		sweep.Run(ctx, sweepCfg)
	}()

	return done
}

// buildAuthenticator wires the Phase 4a authenticator from the config and
// the signing material in the environment. A nil store (auth disabled)
// yields a nil authenticator, which keeps the API serving without token
// checks.
func buildAuthenticator(cfg *config.Config, keyStore authStore) (*auth.Authenticator, error) {
	if keyStore == nil {
		return nil, nil //nolint:nilnil // nil authenticator is the documented "auth disabled" state, not an error
	}

	material, err := auth.LoadMaterial(cfg.Auth.SigningAlgorithm, os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("load signing material: %w", err)
	}

	return auth.New(keyStore, material, auth.Config{
		Issuer:     cfg.Auth.Issuer,
		Audience:   cfg.Auth.Audience,
		TTL:        cfg.Auth.AccessTokenTTL,
		ClockSkew:  cfg.Auth.ClockSkew,
		RequireExp: cfg.Auth.RequireExp,
		RequireJTI: cfg.Auth.RequireJTI,
	}), nil
}

// serveUntilCanceled runs the scheduler daemon alongside the API and drains
// both on cancellation. The daemon runs in a goroutine because a scheduled
// fire blocks inside the daemon loop until its run finishes; shutdown
// cancels active runs through the manager, which is what unblocks a daemon
// stuck mid-fire. The coordinator is the advisory-lock PostgresCoordinator on
// a Postgres backend, so only one serving node fires schedules, and the
// SoloCoordinator on sqlite (.agents/spec/14-open-decisions.md, S2).
func (a *App) serveUntilCanceled(ctx context.Context, server *api.Server, sched *serveSchedulerDeps) int {
	coord, closeCoord := newCoordinator(sched.scheds)
	if closeCoord != nil {
		defer closeCoord(context.WithoutCancel(ctx))
	}

	// The daemon enqueues each fire; the worker drains the queue and runs the
	// workflow through the manager fire runner. A fire no worker can take right
	// away waits in the queue rather than being skipped (S8).
	daemon := scheduler.New(scheduler.Config{
		Store:       sched.scheds,
		Runner:      &scheduleEnqueueRunner{queue: sched.queue, logger: a.logger},
		Coordinator: coord,
		Logger:      a.logger,
	})

	fire := api.NewFireRunner(sched.manager, sched.pauses, a.logger)
	worker := newQueueWorker(sched.queue, &queueRunnerAdapter{fire: fire}, sched.cfg, sched.metrics, a.logger)

	daemonDone := make(chan error, 1)
	workerDone := make(chan error, 1)

	go func() { daemonDone <- daemon.Run(ctx) }()
	go func() { workerDone <- worker.Run(ctx) }()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveShutdownTimeout)
	defer cancel()

	code := ExitOK

	if err := server.Shutdown(shutdownCtx); err != nil {
		a.printErrf("serve: shutdown: %v\n", err)

		code = ExitFailure
	}

	if err := <-daemonDone; err != nil {
		a.printErrf("serve: scheduler: %v\n", err)

		code = ExitFailure
	}

	if err := <-workerDone; err != nil {
		a.printErrf("serve: worker: %v\n", err)

		code = ExitFailure
	}

	return code
}

// serveSchedulerDeps bundles the scheduler and work-queue wiring serve runs: the
// schedule store and coordinator inputs, the manager fire path the worker runs
// jobs through, the queue both share, and the tuning plus metrics for the worker.
type serveSchedulerDeps struct {
	scheds  scheduleStore
	manager *api.Manager
	pauses  api.SchedulePauseChecker
	queue   scheduler.WorkQueue
	cfg     *config.Scheduler
	metrics scheduler.QueueMetrics
}
