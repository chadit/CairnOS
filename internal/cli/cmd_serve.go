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
	"github.com/chadit/CairnOS/internal/messaging"
	"github.com/chadit/CairnOS/internal/notify"
	"github.com/chadit/CairnOS/internal/poll"
	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/secrets"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/sweep"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/internal/vault"
	"github.com/chadit/CairnOS/pkg/runner"
)

const serveUsage = "usage: cairnos serve [--dir D]\n"

// serveShutdownTimeout bounds the graceful drain: the HTTP listener, the
// canceled runs, and the gRPC GracefulStop all have to finish inside it
// before the hard stop fires.
const serveShutdownTimeout = 10 * time.Second

// cmdServe is the Phase 1a long-lived process: the gRPC and HTTP API plus
// the scheduler daemon in one process, stopping cleanly on interrupt. The auth
// store opens only with auth on, and the tenancy store only with OAuth login on,
// so a nil store keeps that whole path out of the process.
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

	var keyStore authStore

	if cfg.Auth.Enabled {
		keyStore, _, code, proceed = a.openAuthAt(ctx, "serve", *dirFlag)
		if !proceed {
			return code
		}
		defer a.closeStore(keyStore)
	}

	var tenants tenancyStore

	if cfg.OAuth.Enabled {
		tenants, code, proceed = a.openTenancyAt(ctx, "serve", *dirFlag)
		if !proceed {
			return code
		}
		defer a.closeStore(tenants)
	}

	source, closeSource, code, proceed := a.openServeSecretSourceAt(ctx, &cfg, dir)
	if !proceed {
		return code
	}
	defer func() { _ = closeSource() }()

	return a.runServe(ctx, &cfg, dir, &serveStores{
		db:       db,
		mem:      mem,
		vault:    vaultDB,
		rules:    rulesSvc,
		scheds:   scheds,
		keyStore: keyStore,
		tenants:  tenants,
		secrets:  source,
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
	// secrets is the shared secret source for the whole serve process: the
	// engine and the webhook ingress both resolve through it, so a store secret
	// is not visible to one and hidden from the other.
	secrets secrets.Source
}

// runServe wires telemetry, the manager, the engine, the API server, the
// retention sweep, and the scheduler daemon, then blocks until ctx cancels
// and drains everything. A non-nil authStore turns on Phase 4a token
// enforcement; signing material loads from the environment before anything
// heavier starts, so a missing secret fails fast.
func (a *App) runServe(ctx context.Context, cfg *config.Config, dir string, stores *serveStores) int {
	db, mem, scheds := stores.db, stores.mem, stores.scheds

	a.serveFail = newServeFailNotifier(cfg, dir, a.logger)

	authn, err := buildAuthenticator(cfg, stores.keyStore)
	if err != nil {
		a.printErrf("serve: auth: %v\n", err)

		return ExitFailure
	}

	credManager, proceed := a.buildServeCredentials(ctx, cfg, stores.secrets)
	if !proceed {
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

	specs, code, proceed := a.prepareSources(ctx, cfg, tel)
	if !proceed {
		return code
	}

	queue, closeQueue, err := openWorkQueue(ctx, dir, cfg, scheds)
	if err != nil {
		return a.failServe(ctx, tel, "work queue", err)
	}
	defer func() { _ = closeQueue() }()

	logger := slog.New(telemetry.NewLogHandler(a.logger.Handler()))

	msgRegistry := messaging.NewRegistry()

	registry, sourceNotifier, err := newServeRegistry(ctx, cfg, specs, mem, stores.rules, credManager,
		&messageSenderAdapter{registry: msgRegistry, logger: logger}, logger)
	if err != nil {
		return a.failServe(ctx, tel, "runners", err)
	}
	defer a.closeRegistry(registry)

	manager := api.NewManager(logger)

	workflowsDir := filepath.Join(dir, workflowsDirName)

	fanout := BuildFanout(cfg, msgRegistry, logger)
	sourceNotifier.SetFanout(fanout)

	eng, err := buildServeEngine(ctx, cfg, &serveEngineDeps{
		manager:      manager,
		tel:          tel,
		registry:     registry,
		db:           db,
		mem:          mem,
		workflowsDir: workflowsDir,
		fanout:       fanout,
		secrets:      stores.secrets,
		logger:       logger,
	})
	if err != nil {
		return a.failServe(ctx, tel, "engine", err)
	}

	apiCfg := newServeAPIConfig(cfg, stores, &serveAPIDeps{
		registry:     registry,
		breakers:     eng,
		manager:      manager,
		workflowsDir: workflowsDir,
		logger:       logger,
	})

	chatAgent, personalities, code, mounted := a.attachChat(ctx, cfg, tel, apiCfg, &chatDeps{
		store:        db,
		memory:       mem,
		vault:        stores.vault,
		registry:     registry,
		manager:      manager,
		workflowsDir: workflowsDir,
		skillsDir:    filepath.Join(dir, skillsDirName),
		contextDir:   filepath.Join(dir, contextDirName),
		logger:       logger,
	})
	if !mounted {
		return code
	}

	bridges, code, bridgesMounted := a.buildServeBridges(ctx, cfg, tel, msgRegistry, &bridgeDeps{
		chat:    chatAgent,
		store:   db,
		metrics: &bridgeMetricsAdapter{rec: tel.Recorder()},
		logger:  logger,
	})
	if !bridgesMounted {
		return code
	}

	a.clearServeFailure()

	attachMessagingServe(apiCfg, msgRegistry, tel)

	applyServeAuth(apiCfg, cfg, authn)

	if attachA2AServeErr, a2aMounted := a.attachA2AServe(ctx, cfg, tel, apiCfg, &a2aServeDeps{
		manager:      manager,
		store:        db,
		core:         bridges.core,
		identities:   msgRegistry,
		workflowsDir: workflowsDir,
		logger:       logger,
	}); !a2aMounted {
		return attachA2AServeErr
	}

	if attachOAuthServeErr, oauthMounted := a.attachOAuthServe(ctx, cfg, tel, authn, stores.tenants, apiCfg, logger); !oauthMounted {
		return attachOAuthServeErr
	}

	closeHooks, code, hooksMounted := a.attachWebhookServe(ctx, cfg, tel, dir, apiCfg, &webhookDeps{
		scheds:  scheds,
		manager: manager,
		pauses:  db,
		secrets: stores.secrets,
		logger:  logger,
	})
	if !hooksMounted {
		return code
	}
	defer func() { _ = closeHooks() }()

	if code, mcpMounted := a.attachMCPServe(ctx, cfg, tel, apiCfg, &mcpServeDeps{
		manager:      manager,
		store:        db,
		memory:       mem,
		rules:        stores.rules,
		workflowsDir: workflowsDir,
		authn:        authn,
		logger:       logger,
	}); !mcpMounted {
		return code
	}

	if code, compatMounted := a.attachOpenAICompatServe(ctx, cfg, tel, apiCfg, &openaiCompatDeps{
		agent:         chatAgent,
		personalities: personalities,
		store:         db,
		authn:         authn,
		logger:        logger,
	}); !compatMounted {
		return code
	}

	server := api.NewServer(apiCfg)

	if err := server.Start(ctx); err != nil {
		a.printErrf("serve: %v\n", err)
		a.closeTelemetry(ctx, tel)

		return ExitFailure
	}

	a.printOutf("gRPC listening on %s\n", server.GRPCAddr())
	a.printOutf("HTTP listening on %s\n", server.HTTPAddr())
	a.printOutf("messaging endpoint at http://%s%s\n", server.HTTPAddr(), messagingRoute)

	if addr := tel.PrometheusAddr(); addr != "" {
		a.printOutf("metrics listening on %s\n", addr)
	}

	return a.serveAndDrain(ctx, cfg, stores, logger, &serveRuntime{
		server:   server,
		manager:  manager,
		queue:    queue,
		bridges:  bridges,
		registry: registry,
		tel:      tel,
		fanout:   fanout,
		secrets:  stores.secrets,
	})
}

// serveRuntime bundles what serveAndDrain runs the background loops and the
// scheduler daemon over, so the helper takes one argument instead of a long
// parameter list.
type serveRuntime struct {
	server   *api.Server
	bridges  *bridgeRuntime
	manager  *api.Manager
	queue    scheduler.WorkQueue
	registry *runner.Registry
	tel      *telemetry.Telemetry
	fanout   *notify.Fanout
	secrets  secrets.Source
}

// serveAndDrain starts the retention sweep, hive subscribe, and chat bridge
// background loops, prints the ready banner, serves until ctx cancels, then
// drains every loop and flushes telemetry, returning the exit code. Split out of
// runServe so that composition root stays under the maintainability ceiling.
func (a *App) serveAndDrain(ctx context.Context, cfg *config.Config, stores *serveStores, logger *slog.Logger, deps *serveRuntime) int {
	bridgeDone := a.startBridges(ctx, logger, deps.bridges)
	fanoutDone := StartFanout(ctx, deps.fanout)

	a.printOutf("serving; interrupt to stop\n")

	sweepDone := startSweep(ctx, cfg, stores.db, stores.keyStore, logger)
	hiveDone := startHiveSubscribe(ctx, cfg, deps.secrets, stores.db, stores.scheds, logger)

	a.announceHiveSubscribe(hiveDone)

	code := a.serveUntilCanceled(ctx, deps.server, &serveSchedulerDeps{
		scheds:  stores.scheds,
		manager: deps.manager,
		pauses:  stores.db,
		queue:   deps.queue,
		sources: buildPollSources(deps.registry),
		cfg:     &cfg.Scheduler,
		metrics: &queueMetricsAdapter{rec: deps.tel.Recorder()},
		fanout:  deps.fanout,
	})

	<-sweepDone

	if hiveDone != nil {
		<-hiveDone
	}

	<-bridgeDone
	<-fanoutDone

	a.closeTelemetry(ctx, deps.tel)

	return code
}

// serveAPIDeps bundles what newServeAPIConfig reads outside the store set, so
// the helper takes one argument instead of a long parameter list.
type serveAPIDeps struct {
	registry     *runner.Registry
	breakers     api.BreakerAdmin
	manager      *api.Manager
	logger       *slog.Logger
	workflowsDir string
}

// newServeAPIConfig assembles the API server's config from the stores, the
// engine, and the run manager serve already built. It is a straight literal
// lifted out of runServe, which keeps that composition root under the
// maintainability ceiling.
func newServeAPIConfig(cfg *config.Config, stores *serveStores, deps *serveAPIDeps) *api.Config {
	return &api.Config{
		GRPCAddr:     cfg.API.GRPC,
		HTTPAddr:     cfg.API.HTTP,
		WorkflowsDir: deps.workflowsDir,
		Store:        stores.db,
		Memory:       stores.mem,
		Vault:        stores.vault,
		Rules:        stores.rules,
		Registry:     deps.registry,
		Breakers:     deps.breakers,
		Manager:      deps.manager,
		Logger:       deps.logger,
		BindPolicy: api.BindPolicy{
			AuthEnabled:               cfg.Auth.Enabled,
			TLSTerminated:             cfg.Auth.TLSTerminated,
			ServeWithoutAuthOnNetwork: cfg.Auth.ServeWithoutAuthOnNetwork,
		},
		CORSOrigins:     cfg.API.CORSOrigins,
		MaxBodyBytes:    cfg.API.MaxBodyBytes,
		MaxRecvMsgBytes: cfg.API.MaxRecvMsgBytes,
	}
}

// applyServeAuth stamps the authenticator and its login guards onto the API
// config. It assigns only when authn is non-nil, because a typed-nil
// *auth.Authenticator inside the interface field would read as "auth on" to the
// server.
func applyServeAuth(apiCfg *api.Config, cfg *config.Config, authn *auth.Authenticator) {
	if authn == nil {
		return
	}

	apiCfg.Auth = authn
	apiCfg.LoginLimiter = auth.NewLoginLimiter(auth.LoginLimitConfig{
		MaxConcurrent: cfg.Auth.MaxConcurrentLogins,
		RatePerMinute: cfg.Auth.LoginRatePerMinute,
	})
	apiCfg.StreamRecheck = cfg.Auth.StreamRecheck
}

// openServeSecretSourceAt builds the serve process's shared secret source, the
// openStoreAt shape, so cmdServe opens it alongside the other stores and hands
// runServe one bundle. Reporting a key or store failure on stderr here keeps the
// failure ahead of any listener binding. The returned closer releases the store
// handle at shutdown; the default env backend opens nothing.
func (a *App) openServeSecretSourceAt(ctx context.Context, cfg *config.Config, dir string) (secrets.Source, func() error, int, bool) {
	source, closeSource, err := a.buildSecretSource(ctx, dir, cfg)
	if err != nil {
		a.printErrf("serve: %v\n", err)

		return nil, nil, ExitFailure, false
	}

	return source, closeSource, ExitOK, true
}

// serveEngineDeps bundles what buildServeEngine wires onto the serve-mode
// engine, so the helper takes one argument instead of a long parameter list.
type serveEngineDeps struct {
	db           store.Store
	mem          memory.Store
	secrets      secrets.Source
	manager      *api.Manager
	tel          *telemetry.Telemetry
	registry     *runner.Registry
	fanout       *notify.Fanout
	logger       *slog.Logger
	workflowsDir string
}

// buildServeEngine assembles and wires the serve-mode engine: the file-config
// tuning plus the manager hooks, telemetry, agent loop, governance, and the
// directory-backed workflow resolver a workflow-call action resolves its child
// through. It binds the engine to the manager and wires breaker telemetry,
// returning an error so runServe can drain and exit on a wiring failure.
func buildServeEngine(ctx context.Context, cfg *config.Config, deps *serveEngineDeps) (*engine.Engine, error) {
	engCfg := engineConfig(cfg, deps.secrets)
	engCfg.NewID = deps.manager.NewID
	engCfg.OnEvent = WrapRunFailedFanout(ctx, deps.manager.Dispatch, deps.fanout)
	engCfg.Instrumentation = deps.tel.Recorder()
	engCfg.AgentExecutor = agentloop.New(deps.registry, deps.mem, deps.db, deps.tel.Recorder())
	engCfg.Workflows = api.NewWorkflowResolver(deps.workflowsDir, deps.logger)

	if notifier := NewBudgetFanoutNotifier(deps.fanout, deps.logger); notifier != nil {
		engCfg.Notifier = notifier
	}

	engCfg.Budget = deps.db
	engCfg.Pause = deps.db

	eng := engine.New(engCfg, deps.registry, deps.db)
	deps.manager.Bind(eng)

	if err := wireBreakerTelemetry(ctx, eng, deps.tel.Recorder()); err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	return eng, nil
}

// releaseHivePrimary drops the primary lock on shutdown so another node takes
// over promptly. The release rides a detached context, since ctx is already
// canceled by the time this runs.
func releaseHivePrimary(ctx context.Context, closeCoord func(context.Context)) {
	closeCoord(context.WithoutCancel(ctx))
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
// [hive] is enabled, landing tips in the suggestion queue so they flow through
// the same human review as local suggestions (.agents/spec/09-a2a-and-hive.md).
// It returns nil and starts nothing when the hive is disabled. Subscribing is
// receive-only and never publishes, so the loop is safe regardless of the
// publish gate.
func startHiveSubscribe(ctx context.Context, cfg *config.Config, source secrets.Source, db store.Store, scheds scheduleStore, logger *slog.Logger) <-chan struct{} {
	if !cfg.Hive.Enabled {
		return nil
	}

	if cfg.HiveNeedsPostgres() {
		logger.Warn("hive subscribe loop not started: the hive requires the postgres store backend",
			"store_backend", cfg.Store.Backend)

		return nil
	}

	coord, closeCoord := newHiveCoordinator(scheds)
	if coord == nil {
		logger.Warn("hive subscribe loop not started: no postgres coordinator for the hive primary")

		return nil
	}

	client, err := buildHiveClient(ctx, source, cfg)
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
		defer releaseHivePrimary(ctx, closeCoord)

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
// expired denylist rows. The denylist is assigned only when non-nil, so sweep's
// own nil check stays meaningful.
func startSweep(ctx context.Context, cfg *config.Config, db store.Store, keyStore authStore, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})

	sweepCfg := &sweep.Config{
		Store:    db,
		Interval: cfg.Retention.Sweep,
		Runs:     cfg.Retention.Runs,
		Outputs:  cfg.Retention.Outputs,
		Logger:   logger,
	}

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

// The daemon runs in a goroutine because a scheduled fire blocks inside the
// daemon loop until its run finishes. Shutdown cancels active runs through the
// manager, which is what unblocks a daemon stuck mid-fire.

// serveUntilCanceled runs the scheduler daemon alongside the API and drains both
// on cancellation. The daemon enqueues each due fire and the worker drains the
// queue through the manager fire runner. The coordinator is the advisory-lock PostgresCoordinator on a
// Postgres backend, so only one serving node fires schedules, and the
// SoloCoordinator on sqlite (.agents/spec/14-open-decisions.md, S2).
func (a *App) serveUntilCanceled(ctx context.Context, server *api.Server, sched *serveSchedulerDeps) int {
	coord, closeCoord := newCoordinator(sched.scheds)
	if closeCoord != nil {
		defer closeCoord(context.WithoutCancel(ctx))
	}

	monitor := newReachMonitor(sched.fanout, a.logger)

	enqueue := &scheduleEnqueueRunner{queue: sched.queue, logger: a.logger}
	pollRunner := newPollAwareRunner(enqueue, sched.queue, sched.sources, sched.pauses, monitor, a.logger)

	daemon := scheduler.New(scheduler.Config{
		Store:       sched.scheds,
		Runner:      pollRunner,
		Coordinator: coord,
		Logger:      a.logger,
	})

	fire := api.NewFireRunner(sched.manager, sched.pauses, a.logger)
	worker := newQueueWorker(sched.queue, &QueueRunnerAdapter{fire: fire, reach: monitor}, sched.cfg, sched.metrics, sched.fanout, monitor, a.logger)

	daemonDone := make(chan error, 1)
	workerDone := make(chan error, 1)

	go monitor.Run(ctx)
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
	sources poll.Sources
	cfg     *config.Scheduler
	metrics scheduler.QueueMetrics
	fanout  *notify.Fanout
}
