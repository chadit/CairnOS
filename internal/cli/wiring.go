package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/credentials"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/runner/plugin"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/secrets"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/postgres"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/internal/tenancy"
	"github.com/chadit/CairnOS/internal/vault"
	"github.com/chadit/CairnOS/internal/webhook"
	"github.com/chadit/CairnOS/pkg/runner"
)

// backendPostgres is the [store] backend value that selects the Postgres
// stores and the advisory-lock scheduler coordinator
// (.agents/spec/11-deployment-and-config.md, "[store] backend"). Anything
// else (the "sqlite" default) takes the sqlite path. The memory and auth
// stores share the run store's database when postgres is selected: different
// tables in one Postgres database, the same arrangement the sqlite backends
// use within one file.
const backendPostgres = "postgres"

// Scaffold layout names (.agents/spec/11-deployment-and-config.md,
// "cairnos init").
const (
	configFileName   = "config.toml"
	dataDirName      = "data"
	workflowsDirName = "workflows"
	examplesDirName  = "examples"
	skillsDirName    = "skills"
	contextDirName   = "context"
	helloFileName    = "hello.toml"
)

// Filesystem permissions for scaffolded content: group-readable directories
// and owner-only files, within gosec's G301/G306 bounds.
const (
	dirPerm  = 0o750
	filePerm = 0o600
)

// Flag names and help text shared by every subcommand.
const (
	flagDir      = "dir"
	flagDirUsage = "CairnOS home directory (default ~/.cairnos)"
)

// triggerCLI marks runs started by this binary in run history.
const triggerCLI = "cli"

// Subcommand names shared by several command families (runs, schedule,
// suggestions, memory, keys).
const (
	subcmdList   = "list"
	subcmdAdd    = "add"
	subcmdGet    = "get"
	subcmdImport = "import"
)

// defaultListLimit caps tabular output when no --limit is given.
const defaultListLimit = 20

// Circuit breaker defaults from the spec's runner circuit_breaker example;
// a per-runner config surface arrives with the Phase 1a runner blocks.
const (
	breakerFailureThreshold = 0.5
	breakerWindow           = time.Minute
	breakerMinRequests      = 10
	breakerCooldown         = 30 * time.Second
)

// resolveDir picks the CairnOS home: the --dir flag when set, ~/.cairnos
// otherwise.
func resolveDir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(home, ".cairnos"), nil
}

// storePath turns the configured sqlite DSN into a filesystem path. Relative
// paths resolve against the CairnOS home, not the process working directory,
// so commands behave the same from anywhere. It is meaningless for the
// postgres backend, whose DSN is a connection URL, not a file.
func storePath(dir, dsn string) string {
	path := strings.TrimPrefix(dsn, "file:")
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}

	return path
}

// ensureDataDir creates the sqlite data directory so a fresh home works
// without init. It is a no-op concept for postgres, so callers only invoke
// it on the sqlite path.
func ensureDataDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	return nil
}

// openStore loads the config under dir and opens the run store its DSN
// points at, branching on the configured backend
// (.agents/spec/11-deployment-and-config.md, "[store] backend"). The sqlite
// path creates the data directory first; the postgres path connects to the
// DSN (credentials from CAIRNOS_STORE_DSN). ctx bounds the postgres connect;
// sqlite opens a file and ignores it. The returned store.Store keeps callers
// backend-agnostic.
func openStore(ctx context.Context, dir string) (store.Store, config.Config, error) {
	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		return nil, cfg, fmt.Errorf("load config: %w", err)
	}

	if cfg.Store.Backend == backendPostgres {
		db, openErr := postgres.Open(ctx, cfg.Store.DSN)
		if openErr != nil {
			return nil, cfg, fmt.Errorf("open store: %w", openErr)
		}

		return db, cfg, nil
	}

	path := storePath(dir, cfg.Store.DSN)
	if err := ensureDataDir(path); err != nil {
		return nil, cfg, err
	}

	//nolint:contextcheck // sqlite.Open carries no ctx by contract: schema setup is process-lifetime initialization, not request work
	db, err := sqlite.Open(path)
	if err != nil {
		return nil, cfg, fmt.Errorf("open store: %w", err)
	}

	return db, cfg, nil
}

// memoryStore is a memory.Store that also closes, so the memory subcommands
// hold one value for both the store surface and cleanup regardless of
// backend.
type memoryStore interface {
	memory.Store
	Close() error
}

// openMemory opens the memory store on the configured backend. On sqlite it
// shares the run store's database file (one file stays the whole local
// footprint, disjoint tables); on postgres it shares the run store's
// database (different tables in the same Postgres database, reached through
// the same CAIRNOS_STORE_DSN).
func openMemory(ctx context.Context, dir string) (memoryStore, error) {
	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if cfg.Store.Backend == backendPostgres {
		mem, openErr := memory.OpenPostgres(ctx, cfg.Store.DSN)
		if openErr != nil {
			return nil, fmt.Errorf("open memory store: %w", openErr)
		}

		return mem, nil
	}

	path := storePath(dir, cfg.Store.DSN)
	if err := ensureDataDir(path); err != nil {
		return nil, err
	}

	//nolint:contextcheck // memory.OpenSQLite carries no ctx by contract: schema setup is process-lifetime initialization, not request work
	mem, err := memory.OpenSQLite(path)
	if err != nil {
		return nil, fmt.Errorf("open memory store: %w", err)
	}

	return mem, nil
}

// vaultStore is a vault.Store that also closes, so the vault subcommands hold
// one value for both the store surface and cleanup regardless of backend.
type vaultStore interface {
	vault.Store
	Close() error
}

// openVault opens the vault store on the configured backend, sharing the run
// store's database the same way openMemory does: disjoint document and link
// tables in the one sqlite file, or in the one Postgres database.
func openVault(ctx context.Context, dir string) (vaultStore, error) {
	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if cfg.Store.Backend == backendPostgres {
		vaultDB, openErr := vault.OpenPostgres(ctx, cfg.Store.DSN)
		if openErr != nil {
			return nil, fmt.Errorf("open vault store: %w", openErr)
		}

		return vaultDB, nil
	}

	path := storePath(dir, cfg.Store.DSN)
	if err := ensureDataDir(path); err != nil {
		return nil, err
	}

	//nolint:contextcheck // vault.OpenSQLite carries no ctx by contract: schema setup is process-lifetime initialization, not request work
	vaultDB, err := vault.OpenSQLite(path)
	if err != nil {
		return nil, fmt.Errorf("open vault store: %w", err)
	}

	return vaultDB, nil
}

// openSecretsStore opens the encrypted secret store on the configured [store]
// backend, the openVault shape: it shares the run store's DSN (a disjoint
// secrets table in the one sqlite file, or in the one Postgres database) and
// wraps it in the sealer built from the environment key material. The master
// key is required for the store backend, so a missing or malformed key
// (secrets.LoadKeys) fails here rather than at first resolve; those errors name
// env vars only, never key bytes.
func openSecretsStore(ctx context.Context, dir string, cfg *config.Config) (secrets.Store, error) {
	primary, previous, err := secrets.LoadKeys(os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("load secrets keys: %w", err)
	}

	sealer, err := secrets.NewSealer(primary, previous...)
	if err != nil {
		return nil, fmt.Errorf("build secrets sealer: %w", err)
	}

	if cfg.Store.Backend == backendPostgres {
		secretStore, openErr := secrets.OpenPostgres(ctx, cfg.Store.DSN, sealer)
		if openErr != nil {
			return nil, fmt.Errorf("open secrets store: %w", openErr)
		}

		return secretStore, nil
	}

	path := storePath(dir, cfg.Store.DSN)
	if err := ensureDataDir(path); err != nil {
		return nil, err
	}

	//nolint:contextcheck // secrets.OpenSQLite carries no ctx by contract: schema setup is process-lifetime initialization, not request work
	secretStore, err := secrets.OpenSQLite(path, sealer)
	if err != nil {
		return nil, fmt.Errorf("open secrets store: %w", err)
	}

	return secretStore, nil
}

// buildSecretSource resolves the engine's secret source for the configured
// [secrets] backend. The default "env" backend returns the env source alone
// with a no-op closer, byte-for-byte today's behavior and no key material
// needed. The "store" backend opens the encrypted store and composes it as
// Chain(env, store) so env still wins, warns about any store secret an env var
// shadows (env wins, so a stale variable can silently mask a rotated store
// value), and returns the store's closer.
func (a *App) buildSecretSource(ctx context.Context, dir string, cfg *config.Config) (secrets.Source, func() error, error) {
	if cfg.Secrets.Backend != config.SecretsBackendStore {
		return secrets.NewEnv(), func() error { return nil }, nil
	}

	secretStore, err := openSecretsStore(ctx, dir, cfg)
	if err != nil {
		return nil, nil, err
	}

	a.warnSecretsShadowedByEnv(ctx, secretStore)

	return secrets.Chain(secrets.NewEnv(), secretStore), secretStore.Close, nil
}

// warnSecretsShadowedByEnv logs one startup warning naming the store secrets an
// env var shadows, so an operator sees Chain(env, store) will serve the env
// value for those names. It lists names only, never values, and stays quiet
// when nothing is shadowed or the store cannot be listed (a resolve-time error
// surfaces the real problem later).
func (a *App) warnSecretsShadowedByEnv(ctx context.Context, secretStore secrets.Store) {
	metas, err := secretStore.List(ctx)
	if err != nil {
		return
	}

	names := make([]string, len(metas))
	for idx := range metas {
		names[idx] = metas[idx].Name
	}

	if shadowed := secrets.ShadowedByEnv(names, os.LookupEnv); len(shadowed) > 0 {
		a.logger.Warn("store secrets shadowed by environment variables; env wins, so a stale variable can mask a rotated store value",
			"secrets", shadowed)
	}
}

// tokenClientTimeout bounds one OAuth2 token mint or refresh through the guarded
// client. x/oauth2 TokenSources take no per-call context, so this client Timeout
// is the only bound on a refresh (credentials context capture).
const tokenClientTimeout = 30 * time.Second

// buildCredentialManager constructs the outbound OAuth2 credential manager from
// the [credential.<name>] blocks, resolving each secret ref through the shared
// secret source and minting tokens over the egress-guarded client. Construction
// does no network: it resolves refs and builds token sources, so a missing ref or
// unreachable-config credential fails serve at startup rather than mid-run. With
// no credentials configured it returns an empty manager, which is a no-op. The
// http and mcp_client runners look credentials up by name (slice 5).
func buildCredentialManager(ctx context.Context, cfg *config.Config, source secrets.Source) (*credentials.Manager, error) {
	newClient := func(allowPrivate bool) *http.Client {
		return builtins.GuardedHTTPClient(tokenClientTimeout, allowPrivate)
	}

	manager, err := credentials.New(ctx, cfg.Credentials, source, newClient)
	if err != nil {
		return nil, fmt.Errorf("build credential manager: %w", err)
	}

	return manager, nil
}

// buildServeCredentials builds the credential manager at serve startup, its
// fail-fast side effect, and logs the configured credential names (never
// values). It returns the manager so the http and mcp_client runners mint
// bearer tokens through it. A false return with a printed error means a
// credential is unresolvable, so serve exits before binding a listener.
func (a *App) buildServeCredentials(ctx context.Context, cfg *config.Config, source secrets.Source) (*credentials.Manager, bool) {
	manager, err := buildCredentialManager(ctx, cfg, source)
	if err != nil {
		a.printErrf("serve: credentials: %v\n", err)

		return nil, false
	}

	if names := manager.Names(); len(names) > 0 {
		a.logger.Info("outbound credentials configured", "names", names)
	}

	return manager, true
}

// openVaultAt is openStoreAt for the vault subcommands.
func (a *App) openVaultAt(ctx context.Context, label, dirFlagValue string) (vaultStore, int, bool) {
	if err := ctx.Err(); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	dir, err := resolveDir(dirFlagValue)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	vaultDB, err := openVault(ctx, dir)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	return vaultDB, ExitOK, true
}

// rulesStore is a rules.Service that also closes, so the rules subcommands hold
// one value for both the service surface and cleanup regardless of backend.
type rulesStore interface {
	rules.Service
	Close() error
}

// openRules opens the rules store on the configured backend, sharing the run
// store's database the same way openMemory does: a disjoint language_rules
// table in the one sqlite file, or in the one Postgres database.
func openRules(ctx context.Context, dir string) (rulesStore, error) {
	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if cfg.Store.Backend == backendPostgres {
		svc, openErr := rules.OpenPostgres(ctx, cfg.Store.DSN)
		if openErr != nil {
			return nil, fmt.Errorf("open rules store: %w", openErr)
		}

		return svc, nil
	}

	path := storePath(dir, cfg.Store.DSN)
	if err := ensureDataDir(path); err != nil {
		return nil, err
	}

	//nolint:contextcheck // rules.OpenSQLite carries no ctx by contract: schema setup is process-lifetime initialization, not request work
	svc, err := rules.OpenSQLite(path)
	if err != nil {
		return nil, fmt.Errorf("open rules store: %w", err)
	}

	return svc, nil
}

// openRulesAt is openStoreAt for the rules subcommands.
func (a *App) openRulesAt(ctx context.Context, label, dirFlagValue string) (rulesStore, int, bool) {
	if err := ctx.Err(); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	dir, err := resolveDir(dirFlagValue)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	svc, err := openRules(ctx, dir)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	return svc, ExitOK, true
}

// scheduleStore is a Schedules that also closes, so the schedule subcommands
// hold one value for both the daemon surface and cleanup regardless of
// backend.
type scheduleStore interface {
	scheduler.Schedules
	Close() error
}

// openSchedules opens the schedule store on the configured backend, sharing
// the run store's database the same way openMemory does: a disjoint
// schedules table in the one sqlite file, or in the one Postgres database.
func openSchedules(ctx context.Context, dir string) (scheduleStore, error) {
	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if cfg.Store.Backend == backendPostgres {
		scheds, openErr := scheduler.OpenPostgres(ctx, cfg.Store.DSN)
		if openErr != nil {
			return nil, fmt.Errorf("open schedule store: %w", openErr)
		}

		return scheds, nil
	}

	path := storePath(dir, cfg.Store.DSN)
	if err := ensureDataDir(path); err != nil {
		return nil, err
	}

	//nolint:contextcheck // scheduler.OpenSQLite carries no ctx by contract: schema setup is process-lifetime initialization, not request work
	scheds, err := scheduler.OpenSQLite(path)
	if err != nil {
		return nil, fmt.Errorf("open schedule store: %w", err)
	}

	return scheds, nil
}

// openWebhooks opens the webhook registration store on the configured backend,
// the schedule-store opener shape. On sqlite it shares the run store's database
// file through a disjoint webhook_registrations table, and the returned closer
// releases that handle. On postgres the table lives in the run store's database,
// reached through a schedule store opened only for its pool: PostgresWebhooks
// borrows that pool (its own Close is a no-op), so the closer closes the
// schedule store to release the pool.
func openWebhooks(ctx context.Context, dir string) (webhook.Store, func() error, error) {
	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}

	if cfg.Store.Backend == backendPostgres {
		scheds, openErr := scheduler.OpenPostgres(ctx, cfg.Store.DSN)
		if openErr != nil {
			return nil, nil, fmt.Errorf("open webhook store: %w", openErr)
		}

		hooks, hookErr := webhook.NewPostgresWebhooks(ctx, scheds.Pool())
		if hookErr != nil {
			return nil, nil, fmt.Errorf("open webhook store: %w", errors.Join(hookErr, scheds.Close()))
		}

		return hooks, scheds.Close, nil
	}

	//nolint:contextcheck // openSQLiteWebhooks does schema setup, process-lifetime initialization with no request ctx to inherit
	return openSQLiteWebhooks(dir, &cfg)
}

// openSQLiteWebhooks opens the sqlite registration store sharing the run store's
// database file through a disjoint table, the branch both webhook-store openers
// share. The returned closer releases the file handle. It carries no ctx: schema
// setup is process-lifetime initialization, not request work.
func openSQLiteWebhooks(dir string, cfg *config.Config) (webhook.Store, func() error, error) {
	path := storePath(dir, cfg.Store.DSN)
	if err := ensureDataDir(path); err != nil {
		return nil, nil, err
	}

	hooks, err := webhook.OpenSQLiteWebhooks(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open webhook store: %w", err)
	}

	return hooks, hooks.Close, nil
}

// openWebhooksAt is openStoreAt for the webhook subcommands. It hands back the
// registration store and the closer that releases the backend's handle (the
// pool on postgres, the file on sqlite), so the caller defers one close.
func (a *App) openWebhooksAt(ctx context.Context, label, dirFlagValue string) (webhook.Store, func() error, int, bool) {
	if err := ctx.Err(); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	dir, err := resolveDir(dirFlagValue)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	hooks, closeHooks, err := openWebhooks(ctx, dir)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	return hooks, closeHooks, ExitOK, true
}

// newCoordinator picks the scheduler coordinator for the configured backend:
// the single-node SoloCoordinator for sqlite, and the advisory-lock
// PostgresCoordinator for postgres so exactly one node owns scheduling
// across a shared database (.agents/spec/14-open-decisions.md, S2). The
// postgres coordinator borrows the schedule store's pool, so the lock lives
// on the same database; closer releases the lock on shutdown (nil for
// sqlite). A schedule store that is not a *scheduler.Postgres (the sqlite
// case) yields the solo coordinator.
func newCoordinator(scheds scheduleStore) (scheduler.Coordinator, func(context.Context)) {
	pgScheds, ok := scheds.(*scheduler.Postgres)
	if !ok {
		return scheduler.SoloCoordinator{}, nil
	}

	coord := scheduler.NewPostgresCoordinator(pgScheds.Pool())

	return coord, func(ctx context.Context) {
		_ = coord.Close(ctx)
	}
}

// hiveAdvisoryLockKey is the advisory-lock key the hive primary elects on,
// distinct from the scheduler leader's key so the two roles elect independently
// over the same database: a node can be the scheduler leader without being the
// hive primary. Same "CRNS" high bytes as the scheduler key, +2 in the low
// bytes so the two never collide in pg_locks.
const hiveAdvisoryLockKey int64 = 0x43524e53_00000002

// newHiveCoordinator picks the hive primary-election coordinator: the advisory-
// lock PostgresCoordinator on the hive key for postgres, so exactly one node in
// the cluster runs the hive loop (R3 in .agents/spec/14-open-decisions.md). It
// borrows the schedule store's pool, the same shared database the scheduler
// leader elects on. A non-postgres schedule store yields no coordinator; serve
// already refuses the hive off postgres, so that path stays unused.
func newHiveCoordinator(scheds scheduleStore) (scheduler.Coordinator, func(context.Context)) {
	pgScheds, ok := scheds.(*scheduler.Postgres)
	if !ok {
		return nil, nil
	}

	coord := scheduler.NewPostgresCoordinatorWithKey(pgScheds.Pool(), hiveAdvisoryLockKey)

	return coord, func(ctx context.Context) {
		_ = coord.Close(ctx)
	}
}

// openStoreAt resolves the home from a --dir value and opens the run store,
// reporting failures on stderr; the shared spine of the store-backed
// subcommands. The ctx check keeps an already-canceled invocation from
// opening a database it will never use, and bounds the postgres connect.
func (a *App) openStoreAt(ctx context.Context, label, dirFlagValue string) (store.Store, config.Config, int, bool) {
	if err := ctx.Err(); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, config.Config{}, ExitFailure, false
	}

	dir, err := resolveDir(dirFlagValue)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, config.Config{}, ExitFailure, false
	}

	db, cfg, err := openStore(ctx, dir)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, cfg, ExitFailure, false
	}

	return db, cfg, ExitOK, true
}

// loadConfigAt resolves the home from a --dir value and loads the config,
// reporting failures on stderr. It is the config-only spine of subcommands
// that read settings without opening a store (hive status).
func (a *App) loadConfigAt(dirFlagValue, label string) (config.Config, int, bool) {
	dir, err := resolveDir(dirFlagValue)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return config.Config{}, ExitFailure, false
	}

	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return config.Config{}, ExitFailure, false
	}

	return cfg, ExitOK, true
}

// openMemoryAt is openStoreAt for the memory subcommands.
func (a *App) openMemoryAt(ctx context.Context, label, dirFlagValue string) (memoryStore, int, bool) {
	if err := ctx.Err(); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	dir, err := resolveDir(dirFlagValue)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	mem, err := openMemory(ctx, dir)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	return mem, ExitOK, true
}

// authStore is an auth.Store that also closes, so the keys and auth
// subcommands hold one value for both the authenticator surface and cleanup
// regardless of backend.
type authStore interface {
	auth.Store
	Close() error
}

// openAuth opens the auth store (keys and the revocation denylist) on the
// configured backend, sharing the run store's database the same way
// openMemory does: disjoint auth tables in the one sqlite file, or in the one
// Postgres database.
func openAuth(ctx context.Context, dir string) (authStore, config.Config, error) {
	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		return nil, cfg, fmt.Errorf("load config: %w", err)
	}

	if cfg.Store.Backend == backendPostgres {
		keyStore, openErr := auth.OpenPostgres(ctx, cfg.Store.DSN)
		if openErr != nil {
			return nil, cfg, fmt.Errorf("open auth store: %w", openErr)
		}

		return keyStore, cfg, nil
	}

	path := storePath(dir, cfg.Store.DSN)
	if err := ensureDataDir(path); err != nil {
		return nil, cfg, err
	}

	//nolint:contextcheck // auth.OpenSQLite carries no ctx by contract: schema setup is process-lifetime initialization, not request work
	keyStore, err := auth.OpenSQLite(path)
	if err != nil {
		return nil, cfg, fmt.Errorf("open auth store: %w", err)
	}

	return keyStore, cfg, nil
}

// tenancyStore is a tenancy.Store that also closes, so the workspace
// subcommands hold one value for both the store surface and cleanup
// regardless of backend.
type tenancyStore interface {
	tenancy.Store
	Close() error
}

// openTenancy opens the tenancy store (workspaces and members) on the
// configured backend, sharing the run store's database the same way openAuth
// does: disjoint tenancy tables in the one sqlite file, or in the one Postgres
// database. Opening seeds the default workspace.
func openTenancy(ctx context.Context, dir string) (tenancyStore, error) {
	cfg, err := config.Load(filepath.Join(dir, configFileName))
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if cfg.Store.Backend == backendPostgres {
		tenants, openErr := tenancy.OpenPostgres(ctx, cfg.Store.DSN)
		if openErr != nil {
			return nil, fmt.Errorf("open tenancy store: %w", openErr)
		}

		return tenants, nil
	}

	path := storePath(dir, cfg.Store.DSN)
	if err := ensureDataDir(path); err != nil {
		return nil, err
	}

	//nolint:contextcheck // tenancy.OpenSQLite carries no ctx by contract: schema setup is process-lifetime initialization, not request work
	tenants, err := tenancy.OpenSQLite(path)
	if err != nil {
		return nil, fmt.Errorf("open tenancy store: %w", err)
	}

	return tenants, nil
}

// openTenancyAt is openStoreAt for the workspace subcommands.
func (a *App) openTenancyAt(ctx context.Context, label, dirFlagValue string) (tenancyStore, int, bool) {
	if err := ctx.Err(); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	dir, err := resolveDir(dirFlagValue)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	tenants, err := openTenancy(ctx, dir)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	return tenants, ExitOK, true
}

// openAuthAt is openStoreAt for the keys and auth subcommands; the config
// rides along because revocation windows come from auth.access_token_ttl.
func (a *App) openAuthAt(ctx context.Context, label, dirFlagValue string) (authStore, config.Config, int, bool) {
	if err := ctx.Err(); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, config.Config{}, ExitFailure, false
	}

	dir, err := resolveDir(dirFlagValue)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, config.Config{}, ExitFailure, false
	}

	keyStore, cfg, err := openAuth(ctx, dir)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, cfg, ExitFailure, false
	}

	return keyStore, cfg, ExitOK, true
}

// openSchedulesAt is openStoreAt for the schedule subcommands.
func (a *App) openSchedulesAt(ctx context.Context, label, dirFlagValue string) (scheduleStore, int, bool) {
	if err := ctx.Err(); err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	dir, err := resolveDir(dirFlagValue)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	scheds, err := openSchedules(ctx, dir)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	return scheds, ExitOK, true
}

// closeStore releases a database, logging instead of failing: by the time
// Close runs the command's outcome is already decided.
func (a *App) closeStore(closer io.Closer) {
	if err := closer.Close(); err != nil {
		a.logger.Warn("close store", "error", err)
	}
}

// closeSecretSource runs a secret source's closer, logging instead of failing.
// It exists so a build-failure cleanup path can release the source as a plain
// statement alongside closeTelemetry and closeRegistry, rather than a bare
// error-discarding assignment.
func (a *App) closeSecretSource(closeSource func() error) {
	if err := closeSource(); err != nil {
		a.logger.Warn("close secret source", "error", err)
	}
}

// newRegistry wires every built-in runner, plus a plugin-backed runner per
// type = "plugin" block and a per-name mcp_client per type = "mcp_client"
// block (the shape `cairnos mcp import` emits). Each factory gets its
// [runner.<name>] config block through BuildAll, where plugin factories spawn
// their child process, so ctx bounds construction.
func newRegistry(ctx context.Context, runnerCfgs map[string]map[string]any, mem memory.Store, rulesSvc rules.Service, credLookup builtins.TokenLookup) (*runner.Registry, error) {
	registry := runner.NewRegistry()

	for name, tool := range builtins.CLIDefaults() {
		if err := registry.Register(name, builtins.CLIFactory(name, tool)); err != nil {
			return nil, fmt.Errorf("register %s runner: %w", name, err)
		}
	}

	// The language_rules_service runner queries the real Rules Service when one
	// is wired (Phase 7b), with the static-file reader as the fallback for an
	// unknown language. With no service (the local `cairnos run` path), it is
	// the static-file-only runner the examples have always used.
	rulesFactory := builtins.LanguageRulesFactory
	if rulesSvc != nil {
		rulesFactory = builtins.LanguageRulesServiceFactory(newRulesLookup(rulesSvc))
	}

	// One table rather than one if per runner: the list grows with every
	// built-in, and a per-runner branch pushed this function past the
	// complexity ceiling. credLookup reaches the http and mcp_client factories
	// here; a nil lookup stays fine until a block names a credential, which
	// then fails BuildAll.
	fixed := []struct {
		factory runner.Factory
		name    string
	}{
		{name: builtins.MockName, factory: builtins.MockFactory},
		{name: builtins.LocalFnName, factory: builtins.LocalFnFactory(map[string]builtins.Func{})},
		{name: builtins.EmailName, factory: builtins.EmailFactory},
		{name: builtins.WebhookName, factory: builtins.WebhookFactory},
		{name: builtins.HTTPName, factory: builtins.HTTPFactoryWithCredentials(credLookup)},
		{name: builtins.CodeName, factory: builtins.CodeFactory},
		{name: builtins.LanguageRulesName, factory: rulesFactory},
		{name: builtins.OpenAIName, factory: builtins.OpenAIFactory},
		{name: builtins.AnthropicName, factory: builtins.AnthropicFactory},
		{name: builtins.GitHubName, factory: builtins.GitHubFactory},
		{name: builtins.MCPClientName, factory: builtins.MCPClientFactoryWithCredentials(credLookup)},
		{name: builtins.SandboxExecName, factory: builtins.SandboxExecFactory},
		{name: builtins.ExecName, factory: builtins.ExecFactory},
	}

	for _, builtin := range fixed {
		if err := registry.Register(builtin.name, builtin.factory); err != nil {
			return nil, fmt.Errorf("register %s runner: %w", builtin.name, err)
		}
	}

	if mem != nil {
		if err := registry.Register(builtins.MemoryName, builtins.MemoryToolFactory(mem)); err != nil {
			return nil, fmt.Errorf("register memory tool: %w", err)
		}
	}

	if err := registerConfiguredRunners(ctx, registry, runnerCfgs, credLookup); err != nil {
		return nil, err
	}

	if err := registry.BuildAll(runnerCfgs); err != nil {
		return nil, fmt.Errorf("build runners: %w", err)
	}

	return registry, nil
}

// registerConfiguredRunners adds a runner for every [runner.<name>] block
// whose type string selects a registration path, additional to the built-in
// names: "plugin" wires an out-of-process runner (Phase 1b), "mcp_client"
// wires a per-name instance of the built-in MCP client factory (the shape
// `cairnos mcp import` emits). Blocks without a type key, or with another
// value, stay plain config for the built-in of that name. A configured name
// colliding with an already-registered one surfaces as the registry's
// duplicate-registration error rather than silently shadowing it. The one
// exception is a block literally named mcp_client: the built-in is already
// registered under that name with the same factory and BuildAll hands it
// that block, so a second Register would spuriously fail existing configs.
func registerConfiguredRunners(ctx context.Context, registry *runner.Registry, runnerCfgs map[string]map[string]any, credLookup builtins.TokenLookup) error {
	for _, name := range slices.Sorted(maps.Keys(runnerCfgs)) {
		kind, _ := runnerCfgs[name]["type"].(string)

		switch kind {
		case "plugin":
			if err := registry.Register(name, plugin.Factory(ctx, name)); err != nil {
				return fmt.Errorf("register plugin runner %q: %w", name, err)
			}
		case builtins.MCPClientName:
			if name == builtins.MCPClientName {
				continue
			}

			if err := registry.Register(name, builtins.MCPClientFactoryForWithCredentials(name, credLookup)); err != nil {
				return fmt.Errorf("register mcp_client runner %q: %w", name, err)
			}
		}
	}

	return nil
}

// closeRegistry shuts every built runner down, logging instead of failing:
// plugin-backed runners hold child processes that must not outlive the
// command, but by Close time the command's outcome is already decided.
func (a *App) closeRegistry(registry *runner.Registry) {
	if err := registry.Close(); err != nil {
		a.logger.Warn("close runner registry", "error", err)
	}
}

// engineConfig maps the file config onto the engine's tuning knobs. source is
// the resolved secret source (env alone, or Chain(env, store) when [secrets]
// backend = "store"), built by the caller through buildSecretSource so every
// entry point resolves ${{ secrets.* }} through the one configured chain.
func engineConfig(cfg *config.Config, source secrets.Source) engine.Config {
	return engine.Config{
		MaxConcurrentActions: cfg.Engine.MaxConcurrentActions,
		MaxCallDepth:         cfg.Engine.MaxCallDepth,
		DefaultTimeout:       cfg.Engine.DefaultTimeout,
		DefaultRetry: reliability.Policy{
			MaxAttempts: cfg.Engine.Retry.MaxAttempts,
			Backoff:     cfg.Engine.Retry.Backoff,
			BaseDelay:   cfg.Engine.Retry.BaseDelay,
			MaxDelay:    cfg.Engine.Retry.MaxDelay,
			Jitter:      cfg.Engine.Retry.Jitter,
		},
		Fallback: cfg.Engine.Fallback,
		Breaker: reliability.BreakerConfig{
			FailureThreshold: breakerFailureThreshold,
			Window:           breakerWindow,
			MinRequests:      breakerMinRequests,
			Cooldown:         breakerCooldown,
		},
		EnvAllowlist: cfg.Env.Allowlist,
		Secrets:      source,
		NewID:        store.NewID,
	}
}

// appVersion reads the module build version for telemetry's service
// metadata; "devel" when build info is unavailable or unstamped.
func appVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "devel"
	}

	return info.Main.Version
}

// wireBreakerTelemetry connects engine breaker transitions and the state
// gauge to the recorder. ctx rides into the transition callback because
// reliability's hook carries none of its own.
func wireBreakerTelemetry(ctx context.Context, eng *engine.Engine, rec *telemetry.Recorder) error {
	eng.OnBreakerTransition(func(name string, to reliability.State) {
		rec.BreakerTransition(ctx, name, to)
	})

	if err := rec.ObserveBreakers(eng.BreakerStates); err != nil {
		return fmt.Errorf("wire breaker gauge: %w", err)
	}

	return nil
}

// closeTelemetry flushes the telemetry stack, bounded by the serve drain
// budget and detached from cancellation so an interrupt still flushes. A
// failed flush warns instead of failing the command: observability must
// never decide a workload's exit code.
func (a *App) closeTelemetry(ctx context.Context, tel *telemetry.Telemetry) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveShutdownTimeout)
	defer cancel()

	if err := tel.Shutdown(shutdownCtx); err != nil {
		a.printErrf("telemetry shutdown: %v\n", err)
	}
}

// dirFlagOn registers the shared --dir flag on a subcommand's flag set.
func dirFlagOn(flags *flag.FlagSet) *string {
	return flags.String(flagDir, "", flagDirUsage)
}

// parseFlags parses subcommand flags. The returned code only matters when
// proceed is false: -h exits clean, anything else is a usage error (the
// flag package already printed the problem).
func parseFlags(flags *flag.FlagSet, args []string) (int, bool) {
	err := flags.Parse(args)
	if err == nil {
		return ExitOK, true
	}

	if errors.Is(err, flag.ErrHelp) {
		return ExitOK, false
	}

	return ExitUsage, false
}

// parsePositional handles commands with one positional argument that may
// carry flags on either side, since the stdlib flag package stops parsing
// at the first non-flag token. An empty positional in the result means the
// caller should print its usage line.
func parsePositional(flags *flag.FlagSet, args []string) (string, int, bool) {
	code, proceed := parseFlags(flags, args)
	if !proceed {
		return "", code, false
	}

	rest := flags.Args()
	if len(rest) == 0 {
		return "", ExitOK, true
	}

	code, proceed = parseFlags(flags, rest[1:])
	if !proceed {
		return "", code, false
	}

	if flags.NArg() > 0 {
		// Extra positionals surface as "no positional" so the caller's
		// usage line explains the expected shape.
		return "", ExitOK, true
	}

	return rest[0], ExitOK, true
}

// parseThreePositionals handles a command with several leading positional
// arguments followed by flags, since the stdlib flag package stops at the
// first non-flag token. It peels the leading non-flag tokens off, then parses
// what remains as flags. The caller validates the positional count.
func parseThreePositionals(flags *flag.FlagSet, args []string) ([]string, int, bool) {
	var positionals []string

	rest := args
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}

	if code, proceed := parseFlags(flags, rest); !proceed {
		return nil, code, false
	}

	return positionals, ExitOK, true
}
