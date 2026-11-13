package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/credentials"
	"github.com/chadit/CairnOS/internal/discover"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/hostenv"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/runner/plugin"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/secrets"
	sourcesync "github.com/chadit/CairnOS/internal/source"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/postgres"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/internal/tenancy"
	"github.com/chadit/CairnOS/internal/vault"
	"github.com/chadit/CairnOS/internal/webhook"
	"github.com/chadit/CairnOS/pkg/runner"
)

// backendPostgres is the [store] backend value that selects the Postgres stores
// and the advisory-lock scheduler coordinator. Anything else (the "sqlite"
// default) takes the sqlite path. The memory and auth stores share the run
// store's database when postgres is selected: different tables in one Postgres
// database, the same arrangement the sqlite backends use within one file.
const backendPostgres = "postgres"

// Scaffold layout names.
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

// Command words that more than one dispatch switch matches. A top-level
// command can recur as another family's subcommand (schedule run, mcp serve),
// so each word is spelled once.
const (
	subcmdList      = "list"
	subcmdAdd       = "add"
	subcmdGet       = "get"
	subcmdImport    = "import"
	subcmdStatus    = "status"
	subcmdRun       = "run"
	subcmdServe     = "serve"
	subcmdAuth      = "auth"
	subcmdPublish   = "publish"
	subcmdRemove    = "remove"
	subcmdRm        = "rm"
	subcmdSet       = "set"
	subcmdSetup     = "setup"
	subcmdUninstall = "uninstall"
	subcmdApprove   = "approve"
	subcmdRotate    = "rotate"
)

// Words that ask a dispatcher for its usage text instead of naming a
// subcommand.
const (
	helpWord      = "help"
	helpShortFlag = "-h"
	helpLongFlag  = "--help"
)

// runtime.GOOS values the package branches on. Each branch says why its
// platform behaves differently.
const (
	goosLinux   = "linux"
	goosDarwin  = "darwin"
	goosWindows = "windows"
)

// defaultListLimit caps tabular output when no --limit is given.
const defaultListLimit = 20

// Circuit breaker defaults every runner shares.
const (
	breakerFailureThreshold = 0.5
	breakerWindow           = time.Minute
	breakerMinRequests      = 10
	breakerCooldown         = 30 * time.Second
)

// resolveDir picks the CairnOS home: the --dir flag when set, ~/.cairnos
// otherwise. The home comes from the injected environment, so a case can point
// one command at a staged directory without moving the whole process.
func resolveDir(flagValue string, lookup environ.Lookup) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}

	home, err := homeDir(lookup)
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(home, ".cairnos"), nil
}

// homeEnvKey is the variable os.UserHomeDir reads on this platform. The
// injected environment has to be asked the same question the process
// environment would be.
func homeEnvKey() string {
	switch runtime.GOOS {
	case goosWindows:
		return "USERPROFILE"
	case "plan9":
		return "home"
	default:
		return "HOME"
	}
}

// homeDir resolves the operator's home from the injected environment, so a case
// can point one command at a staged home without moving the whole process.
func homeDir(lookup environ.Lookup) (string, error) {
	key := homeEnvKey()

	if home := lookup.Get(key); home != "" {
		return home, nil
	}

	return "", fmt.Errorf("%w: %s", errNoHomeDir, key)
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

// storeBackends holds one store family's postgres and sqlite constructors, so
// open picks between them by the configured backend. Each constructor wraps its
// own error: wrapcheck wants the wrap where the other package's error enters
// cli, and that is also where the family's name is known.
type storeBackends[S any] struct {
	postgres func(ctx context.Context, dsn string) (S, error)
	sqlite   func(path string) (S, error)

	// require, when set, refuses an open the family must not attempt, before
	// open creates the data directory or connects.
	require func(cfg *config.Config) error
}

// open picks the backend the config names. Every family shares the run store's
// DSN, and sqlite's data directory comes first so a fresh home works without
// init. Only postgres takes ctx, since sqlite schema setup is process-lifetime
// work; the secret store's sqlite closure is the exception and carries its
// caller's ctx.
func (b storeBackends[S]) open(ctx context.Context, dir string, cfg *config.Config) (S, error) {
	var zero S

	if b.require != nil {
		if err := b.require(cfg); err != nil {
			return zero, err
		}
	}

	if cfg.Store.Backend == backendPostgres {
		return b.postgres(ctx, cfg.Store.DSN)
	}

	path := storePath(dir, cfg.Store.DSN)
	if err := ensureDataDir(path); err != nil {
		return zero, err
	}

	return b.sqlite(path)
}

// openBackend loads the config under dir and opens one store family on its
// backend. The config comes back for callers that read settings beside the
// store, such as auth's revocation windows.
func openBackend[S any](ctx context.Context, dir string, lookup environ.Lookup, backends storeBackends[S]) (S, config.Config, error) {
	cfg, err := config.LoadWith(filepath.Join(dir, configFileName), lookup)
	if err != nil {
		var zero S

		return zero, cfg, fmt.Errorf("load config: %w", err)
	}

	opened, err := backends.open(ctx, dir, &cfg)

	return opened, cfg, err
}

// openAt resolves the home from a --dir value and opens one store family,
// reporting failures on stderr under label: the shared spine of the
// store-backed subcommands. The ctx check keeps an already-canceled invocation
// from opening a database it will never use. Methods cannot take type
// parameters, so the App is an argument.
func openAt[S any](ctx context.Context, app *App, label, dirFlagValue string, backends storeBackends[S]) (S, config.Config, int, bool) {
	var zero S

	if err := ctx.Err(); err != nil {
		app.printErrf("%s: %v\n", label, err)

		return zero, config.Config{}, ExitFailure, false
	}

	dir, err := resolveDir(dirFlagValue, app.envLookup())
	if err != nil {
		app.printErrf("%s: %v\n", label, err)

		return zero, config.Config{}, ExitFailure, false
	}

	opened, cfg, err := openBackend(ctx, dir, app.envLookup(), backends)
	if err != nil {
		app.printErrf("%s: %v\n", label, err)

		return zero, cfg, ExitFailure, false
	}

	return opened, cfg, ExitOK, true
}

// runStoreBackends opens the run store: runs, steps, and the governance tables
// every other family sits beside.
func runStoreBackends() storeBackends[store.Store] {
	return storeBackends[store.Store]{
		postgres: func(ctx context.Context, dsn string) (store.Store, error) {
			db, err := postgres.Open(ctx, dsn)
			if err != nil {
				return nil, fmt.Errorf("open store: %w", err)
			}

			return db, nil
		},
		sqlite: func(path string) (store.Store, error) {
			db, err := sqlite.Open(path)
			if err != nil {
				return nil, fmt.Errorf("open store: %w", err)
			}

			return db, nil
		},
	}
}

// memoryStore is a memory.Store that also closes, so the memory subcommands
// hold one value for both the store surface and cleanup regardless of
// backend.
type memoryStore interface {
	memory.Store
	Close() error
}

func memoryBackends() storeBackends[memoryStore] {
	return storeBackends[memoryStore]{
		postgres: func(ctx context.Context, dsn string) (memoryStore, error) {
			mem, err := memory.OpenPostgres(ctx, dsn)
			if err != nil {
				return nil, fmt.Errorf("open memory store: %w", err)
			}

			return mem, nil
		},
		sqlite: func(path string) (memoryStore, error) {
			mem, err := memory.OpenSQLite(path)
			if err != nil {
				return nil, fmt.Errorf("open memory store: %w", err)
			}

			return mem, nil
		},
	}
}

// vaultStore is a vault.Store that also closes, so the vault subcommands hold
// one value for both the store surface and cleanup regardless of backend.
type vaultStore interface {
	vault.Store
	Close() error
}

func vaultBackends() storeBackends[vaultStore] {
	return storeBackends[vaultStore]{
		postgres: func(ctx context.Context, dsn string) (vaultStore, error) {
			vaultDB, err := vault.OpenPostgres(ctx, dsn)
			if err != nil {
				return nil, fmt.Errorf("open vault store: %w", err)
			}

			return vaultDB, nil
		},
		sqlite: func(path string) (vaultStore, error) {
			vaultDB, err := vault.OpenSQLite(path)
			if err != nil {
				return nil, fmt.Errorf("open vault store: %w", err)
			}

			return vaultDB, nil
		},
	}
}

// rulesStore is a rules.Service that also closes, so the rules subcommands hold
// one value for both the service surface and cleanup regardless of backend.
type rulesStore interface {
	rules.Service
	Close() error
}

func rulesBackends() storeBackends[rulesStore] {
	return storeBackends[rulesStore]{
		postgres: func(ctx context.Context, dsn string) (rulesStore, error) {
			svc, err := rules.OpenPostgres(ctx, dsn)
			if err != nil {
				return nil, fmt.Errorf("open rules store: %w", err)
			}

			return svc, nil
		},
		sqlite: func(path string) (rulesStore, error) {
			svc, err := rules.OpenSQLite(path)
			if err != nil {
				return nil, fmt.Errorf("open rules store: %w", err)
			}

			return svc, nil
		},
	}
}

// scheduleStore is a Schedules that also closes, so the schedule subcommands
// hold one value for both the daemon surface and cleanup regardless of
// backend.
type scheduleStore interface {
	scheduler.Schedules
	Close() error
}

func scheduleBackends() storeBackends[scheduleStore] {
	return storeBackends[scheduleStore]{
		postgres: func(ctx context.Context, dsn string) (scheduleStore, error) {
			scheds, err := scheduler.OpenPostgres(ctx, dsn)
			if err != nil {
				return nil, fmt.Errorf("open schedule store: %w", err)
			}

			return scheds, nil
		},
		sqlite: func(path string) (scheduleStore, error) {
			scheds, err := scheduler.OpenSQLite(path)
			if err != nil {
				return nil, fmt.Errorf("open schedule store: %w", err)
			}

			return scheds, nil
		},
	}
}

// authStore is an auth.Store that also closes, so the keys and auth
// subcommands hold one value for both the authenticator surface and cleanup
// regardless of backend.
type authStore interface {
	auth.Store
	Close() error
}

// authBackends opens the auth store: keys and the revocation denylist.
func authBackends() storeBackends[authStore] {
	return storeBackends[authStore]{
		postgres: func(ctx context.Context, dsn string) (authStore, error) {
			keyStore, err := auth.OpenPostgres(ctx, dsn)
			if err != nil {
				return nil, fmt.Errorf("open auth store: %w", err)
			}

			return keyStore, nil
		},
		sqlite: func(path string) (authStore, error) {
			keyStore, err := auth.OpenSQLite(path)
			if err != nil {
				return nil, fmt.Errorf("open auth store: %w", err)
			}

			return keyStore, nil
		},
	}
}

// tenancyStore is a tenancy.Store that also closes, so the workspace
// subcommands hold one value for both the store surface and cleanup
// regardless of backend.
type tenancyStore interface {
	tenancy.Store
	Close() error
}

// tenancyBackends opens the tenancy store, workspaces and members. Opening
// seeds the default workspace.
func tenancyBackends() storeBackends[tenancyStore] {
	return storeBackends[tenancyStore]{
		postgres: func(ctx context.Context, dsn string) (tenancyStore, error) {
			tenants, err := tenancy.OpenPostgres(ctx, dsn)
			if err != nil {
				return nil, fmt.Errorf("open tenancy store: %w", err)
			}

			return tenants, nil
		},
		sqlite: func(path string) (tenancyStore, error) {
			tenants, err := tenancy.OpenSQLite(path)
			if err != nil {
				return nil, fmt.Errorf("open tenancy store: %w", err)
			}

			return tenants, nil
		},
	}
}

// secretBackends opens the encrypted secret store, sealed with the key material
// in lookup. The store opens only when [secrets] backend selects it, so nobody
// writes to a store the engine is not reading. Keys load at open, not first
// resolve, and their errors name env vars only, never key bytes.
func secretBackends(ctx context.Context, lookup environ.Lookup) storeBackends[secrets.Store] {
	return storeBackends[secrets.Store]{
		require: func(cfg *config.Config) error {
			if err := requireSecretStore(cfg); err != nil {
				return err
			}

			// Building the sealer here as well as in the constructor checks the
			// keys before open creates the data directory, so a missing master
			// key leaves a fresh home as it was.
			_, err := newSealer(lookup)

			return err
		},
		postgres: func(ctx context.Context, dsn string) (secrets.Store, error) {
			sealer, err := newSealer(lookup)
			if err != nil {
				return nil, err
			}

			secretStore, err := secrets.OpenPostgres(ctx, dsn, sealer)
			if err != nil {
				return nil, fmt.Errorf("open secrets store: %w", err)
			}

			return secretStore, nil
		},
		sqlite: func(path string) (secrets.Store, error) {
			sealer, err := newSealer(lookup)
			if err != nil {
				return nil, err
			}

			secretStore, err := secrets.OpenSQLite(ctx, path, sealer)
			if err != nil {
				return nil, fmt.Errorf("open secrets store: %w", err)
			}

			return secretStore, nil
		},
	}
}

func requireSecretStore(cfg *config.Config) error {
	if cfg.Secrets.Backend == config.SecretsBackendStore {
		return nil
	}

	return fmt.Errorf("%w; set [secrets] backend = %q (currently %q)",
		errSecretStoreNotConfigured, config.SecretsBackendStore, cfg.Secrets.Backend)
}

// newSealer builds the secret store's sealer from the master and previous keys
// in the environment.
func newSealer(lookup environ.Lookup) (*secrets.Sealer, error) {
	primary, previous, err := secrets.LoadKeys(lookup.Get)
	if err != nil {
		return nil, fmt.Errorf("load secrets keys: %w", err)
	}

	sealer, err := secrets.NewSealer(primary, previous...)
	if err != nil {
		return nil, fmt.Errorf("build secrets sealer: %w", err)
	}

	return sealer, nil
}

// schedulePool returns the pool a postgres schedule store holds, for the stores
// that borrow it so a node keeps one pool. The borrower's own Close leaves the
// pool to whoever closes scheds.
func schedulePool(scheds scheduleStore) (*pgxpool.Pool, error) {
	pgScheds, ok := scheds.(*scheduler.Postgres)
	if !ok {
		return nil, errScheduleStoreNotPostgres
	}

	return pgScheds.Pool(), nil
}

// webhookBackends opens the webhook registration store beside scheds. Postgres
// borrows scheds' pool and has nothing of its own to close; sqlite opens the
// shared database file, and ownedCloser releases that handle.
func webhookBackends(scheds scheduleStore) storeBackends[webhook.Store] {
	return storeBackends[webhook.Store]{
		postgres: func(ctx context.Context, _ string) (webhook.Store, error) {
			pool, err := schedulePool(scheds)
			if err != nil {
				return nil, fmt.Errorf("open webhook store: %w", err)
			}

			hooks, err := webhook.NewPostgresWebhooks(ctx, pool)
			if err != nil {
				return nil, fmt.Errorf("open webhook store: %w", err)
			}

			return hooks, nil
		},
		sqlite: func(path string) (webhook.Store, error) {
			hooks, err := webhook.OpenSQLiteWebhooks(path)
			if err != nil {
				return nil, fmt.Errorf("open webhook store: %w", err)
			}

			return hooks, nil
		},
	}
}

// newCoordinator builds the advisory-lock coordinator that lets exactly one node
// own scheduling across a shared postgres database. It is nil off
// postgres: leave scheduler.Config.Coordinator unset then, and the daemon
// defaults to the SoloCoordinator, rather than storing a typed nil there.
func newCoordinator(scheds scheduleStore) *scheduler.PostgresCoordinator {
	pgScheds, ok := scheds.(*scheduler.Postgres)
	if !ok {
		return nil
	}

	return scheduler.NewPostgresCoordinator(pgScheds.Pool())
}

// hiveAdvisoryLockKey is the advisory-lock key the hive primary elects on,
// distinct from the scheduler leader's key so the two roles elect independently
// over the same database: a node can be the scheduler leader without being the
// hive primary. Same "CRNS" high bytes as the scheduler key, +2 in the low
// bytes so the two never collide in pg_locks.
const hiveAdvisoryLockKey int64 = 0x43524e53_00000002

// newHiveCoordinator builds the coordinator that elects the one node running the
// hive loop, on the hive key over the schedule store's pool. It is nil off
// postgres, where serve already refuses the hive.
func newHiveCoordinator(scheds scheduleStore) *scheduler.PostgresCoordinator {
	pgScheds, ok := scheds.(*scheduler.Postgres)
	if !ok {
		return nil
	}

	return scheduler.NewPostgresCoordinatorWithKey(pgScheds.Pool(), hiveAdvisoryLockKey)
}

// ownedCloser returns the Close of a store that owns its handle, and a no-op for
// one that borrows the schedule store's pool, so callers release either the
// same way without knowing the backend.
func ownedCloser(opened any) func() error {
	if closer, ok := opened.(io.Closer); ok {
		return closer.Close
	}

	return func() error { return nil }
}

// closeCoordinator releases a coordinator's advisory lock on shutdown so another
// node takes over promptly, logging instead of failing because the process is
// already on its way out. ctx is detached, since the caller's is canceled by now.
func closeCoordinator(ctx context.Context, coord *scheduler.PostgresCoordinator, logger *slog.Logger) {
	if err := coord.Close(context.WithoutCancel(ctx)); err != nil {
		logger.Warn("release advisory lock", "error", err)
	}
}

// buildSecretSource resolves the engine's secret source for the configured
// [secrets] backend. The default "env" backend chains the env source alone and
// needs no key material. The "store" backend composes Chain(env, store), so env
// still wins, and warns about any store secret an env var shadows, since a
// stale variable can mask a rotated value.
func (a *App) buildSecretSource(ctx context.Context, dir string, cfg *config.Config) (*secrets.ChainSource, func() error, error) {
	if cfg.Secrets.Backend != config.SecretsBackendStore {
		return secrets.Chain(secrets.NewEnvWith(a.envLookup())), func() error { return nil }, nil
	}

	secretStore, err := secretBackends(ctx, a.envLookup()).open(ctx, dir, cfg)
	if err != nil {
		return nil, nil, err
	}

	a.warnSecretsShadowedByEnv(ctx, secretStore)

	return secrets.Chain(secrets.NewEnvWith(a.envLookup()), secretStore), secretStore.Close, nil
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

	if shadowed := secrets.ShadowedByEnv(names, a.envLookup()); len(shadowed) > 0 {
		a.logger.Warn("store secrets shadowed by environment variables; env wins, so a stale variable can mask a rotated store value",
			"secrets", shadowed)
	}
}

// tokenClientTimeout bounds one OAuth2 token mint or refresh through the guarded
// client. x/oauth2 TokenSources take no per-call context, so this client Timeout
// is the only bound on a refresh (credentials context capture).
const tokenClientTimeout = 30 * time.Second

// buildCredentialManager builds the OAuth2 credential manager from the
// [credential.<name>] blocks, resolving each secret ref and minting tokens
// over the egress-guarded client. Construction does no network, so a missing
// ref fails serve at startup, not mid-run. With none configured it returns an
// empty, no-op manager. The http and mcp_client runners look credentials up by
// name (slice 5).
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

// loadConfigAt resolves the home from a --dir value and loads the config,
// reporting failures on stderr. It is the config-only spine of subcommands
// that read settings without opening a store (hive status).
func (a *App) loadConfigAt(dirFlagValue, label string) (config.Config, int, bool) {
	dir, err := resolveDir(dirFlagValue, a.envLookup())
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return config.Config{}, ExitFailure, false
	}

	cfg, err := config.LoadWith(filepath.Join(dir, configFileName), a.envLookup())
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return config.Config{}, ExitFailure, false
	}

	return cfg, ExitOK, true
}

// closeStore releases a database, logging instead of failing: by the time
// Close runs the command's outcome is already decided.
func (a *App) closeStore(closer io.Closer) {
	if err := closer.Close(); err != nil {
		a.logger.Warn("close store", "error", err)
	}
}

// runCloser runs a closer handed back by an opener, logging instead of failing
// like closeStore, so a defer or a build-failure cleanup path releases it as a
// plain statement. what names the resource in the log line.
func (a *App) runCloser(what string, closeFn func() error) {
	if err := closeFn(); err != nil {
		a.logger.Warn("close "+what, "error", err)
	}
}

// sourceSpecs flattens the configured [source.<name>] blocks into the slice the
// source package and its runner work from, in name order so the boot log and the
// registry see the same sequence every start. Every entry point passes its own
// config's blocks, so a local run agrees with serve about which names exist.
func sourceSpecs(sources map[string]config.Source) []sourcesync.Spec {
	specs := make([]sourcesync.Spec, 0, len(sources))

	for _, name := range slices.Sorted(maps.Keys(sources)) {
		block := sources[name]
		specs = append(specs, sourcesync.Spec{
			Name:       name,
			Repo:       block.Repo,
			Dest:       block.Dest,
			Ref:        block.Ref,
			DirtyReset: block.DirtyReset,
		})
	}

	return specs
}

// sourceWiring is what the registry needs from the [source.<name>] blocks: the
// specs the source runner syncs, the trust the exec gate extends, and the two
// hooks only serve fills.
type sourceWiring struct {
	observer  builtins.SourceObserver
	reconcile builtins.SourceReconciler
	trust     builtins.ExecTrust
	specs     []sourcesync.Spec
}

// sourcesFor builds the wiring every entry point shares, so a local run and
// serve agree on which names exist and which checkouts exec trusts.
func sourcesFor(sources map[string]config.Source) *sourceWiring {
	return &sourceWiring{specs: sourceSpecs(sources), trust: execTrust(sources)}
}

// execTrust collects the specs of the sources marked trusted, in name order.
func execTrust(sources map[string]config.Source) builtins.ExecTrust {
	var trusted []sourcesync.Spec

	for _, spec := range sourceSpecs(sources) {
		if sources[spec.Name].Trusted {
			trusted = append(trusted, spec)
		}
	}

	return builtins.ExecTrust{Git: sourcesync.GitCommand, Sources: trusted}
}

// sourceReconciler is serve's workflow discovery for one source by name. A
// source with no workflows key is a no-op, and a failed pass is logged rather
// than failing the sync, since the next sync retries it.
func sourceReconciler(
	sources map[string]config.Source, scheds scheduler.Schedules, logger *slog.Logger,
) builtins.SourceReconciler {
	return func(ctx context.Context, name string) {
		block := sources[name]
		if len(block.Workflows) == 0 {
			return
		}

		tree := discover.Tree{Name: name, Dest: block.Dest, Ref: block.Ref, Globs: block.Workflows}
		if err := discover.Reconcile(ctx, scheds, sourcesync.GitCommand, tree, logger); err != nil {
			logger.Warn("workflow discovery failed", "source", name, "error", err)
		}
	}
}

// newServeRegistry builds serve's runner registry along with the source
// notifier wired into it. The notifier comes back rather than staying inside
// because serve only has a fan-out to give it later, once BuildFanout has run.
func newServeRegistry(
	ctx context.Context,
	cfg *config.Config,
	reconcile builtins.SourceReconciler,
	env environ.Lookup,
	mem memory.Store,
	rulesSvc rules.Service,
	credLookup builtins.TokenLookup,
	msgSender builtins.MessageSender,
	msgThreads builtins.MessageThreads,
	logger *slog.Logger,
	runnerTransport http.RoundTripper,
) (*runner.Registry, *SourceDirtyNotifier, error) {
	notifier := NewSourceDirtyNotifier(nil)

	sources := sourcesFor(cfg.Sources)
	sources.observer = notifier
	sources.reconcile = reconcile

	registry, err := newRegistry(ctx, cfg.Runners, sources, cfg.Hosts, env, mem,
		rulesSvc, credLookup, msgSender, msgThreads, logger, runnerTransport)
	if err != nil {
		return nil, nil, err
	}

	return registry, notifier, nil
}

// newRegistry wires every built-in runner, plus one runner under its own name
// for each [runner.<name>] block whose type selects one (see
// registerConfiguredRunners). BuildAll hands each factory its block and spawns
// plugin child processes, so ctx bounds construction.
//
// msgSender, msgThreads, and the source hooks are seams only serve fills; with
// nil the message runner refuses and the source runner only logs.
func newRegistry(
	ctx context.Context,
	runnerCfgs map[string]map[string]any,
	sources *sourceWiring,
	hosts map[string]config.Host,
	env environ.Lookup,
	mem memory.Store,
	rulesSvc rules.Service,
	credLookup builtins.TokenLookup,
	msgSender builtins.MessageSender,
	msgThreads builtins.MessageThreads,
	logger *slog.Logger,
	runnerTransport http.RoundTripper,
) (*runner.Registry, error) {
	registry := runner.NewRegistry()
	hostEnv := hostenv.Render(hosts)

	for name, tool := range builtins.CLIDefaults() {
		if err := registry.Register(name, builtins.CLIFactory(name, tool, hostEnv, os.Environ())); err != nil {
			return nil, fmt.Errorf("register %s runner: %w", name, err)
		}
	}

	fixed := fixedBuiltins(sources, hosts, hostEnv, env, rulesSvc, credLookup,
		msgSender, msgThreads, logger, runnerTransport)

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

	deps := configuredRunnerDeps{hostEnv: hostEnv, env: env, credLookup: credLookup, runnerTransport: runnerTransport}
	if err := registerConfiguredRunners(ctx, registry, runnerCfgs, deps); err != nil {
		return nil, err
	}

	if err := registry.BuildAll(runnerCfgs); err != nil {
		return nil, fmt.Errorf("build runners: %w", err)
	}

	return registry, nil
}

// languageRulesFactory picks the language_rules runner. With a Rules Service
// wired it queries the service and falls back to the static-file
// reader for an unknown language. With none, on the local `cairnos run` path, it
// is the static-file-only runner the examples have always used.
func languageRulesFactory(rulesSvc rules.Service) runner.Factory {
	if rulesSvc == nil {
		return builtins.LanguageRulesFactory()
	}

	return builtins.LanguageRulesServiceFactory(newRulesLookup(rulesSvc))
}

// builtinRunner pairs a built-in's registered name with the factory that builds
// it.
type builtinRunner struct {
	factory runner.Factory
	name    string
}

// fixedBuiltins lists every built-in registered under its own name. One table
// rather than an if per runner, because the list grows with every built-in and a
// per-runner branch pushed newRegistry past the complexity ceiling.
func fixedBuiltins(
	sources *sourceWiring,
	hosts map[string]config.Host,
	hostEnv []string,
	env environ.Lookup,
	rulesSvc rules.Service,
	credLookup builtins.TokenLookup,
	msgSender builtins.MessageSender,
	msgThreads builtins.MessageThreads,
	logger *slog.Logger,
	runnerTransport http.RoundTripper,
) []builtinRunner {
	return []builtinRunner{
		{name: builtins.MockName, factory: builtins.MockFactory()},
		{name: builtins.LocalFnName, factory: builtins.LocalFnFactory(map[string]builtins.Func{})},
		{name: builtins.EmailName, factory: builtins.EmailFactory(env)},
		{name: builtins.WebhookName, factory: builtins.WebhookFactory()},
		{name: builtins.HTTPName, factory: builtins.HTTPFactoryWithCredentials(credLookup)},
		{name: builtins.CodeName, factory: builtins.CodeFactory()},
		{name: builtins.LanguageRulesName, factory: languageRulesFactory(rulesSvc)},
		{name: builtins.OpenAIName, factory: builtins.OpenAIFactory(env, runnerTransport)},
		{name: builtins.AnthropicName, factory: builtins.AnthropicFactory(env, runnerTransport)},
		{name: builtins.GitHubName, factory: builtins.GitHubFactory(env)},
		{name: builtins.MCPClientName, factory: builtins.MCPClientFactoryWithCredentials(credLookup, runnerTransport)},
		{name: builtins.SandboxExecName, factory: builtins.SandboxExecFactory()},
		{name: builtins.ExecName, factory: builtins.ExecFactoryWithHostEnv(hostEnv, os.Environ(), sources.trust)},
		{name: builtins.MessageName, factory: builtins.MessageFactory(msgSender, msgThreads)},
		{name: builtins.SourceName, factory: builtins.SourceFactory(sources.specs, sourcesync.GitCommand, logger, sources.observer, sources.reconcile)},
		{name: builtins.UsageName, factory: builtins.UsageFactory(usageOptions(hosts, env, nil))},
	}
}

// usageOptions builds the usage runner's seams from config. The usage command and
// the registry share it so both probe the same host table. now is the report
// clock, nil for time.Now.
func usageOptions(hosts map[string]config.Host, env environ.Lookup, now func() time.Time) builtins.UsageOptions {
	return builtins.UsageOptions{
		Hosts:   usageHosts(hosts),
		Lookup:  env,
		Now:     now,
		Version: appVersion(),
	}
}

// usageHosts narrows the host table to the two keys the usage runner reads, which
// keeps the built-in off the config package's import path.
func usageHosts(hosts map[string]config.Host) map[string]builtins.UsageHost {
	targets := make(map[string]builtins.UsageHost, len(hosts))

	for name, host := range hosts {
		targets[name] = builtins.UsageHost{SSH: host.SSH, Wrapper: host.Wrapper}
	}

	return targets
}

// alreadyRegisteredAsBuiltin reports whether a block carries the built-in's own
// name. That one is registered with the same factory and BuildAll hands it this
// block, so a second Register would fail existing configs. Any other collision
// surfaces as the registry's duplicate error rather than shadowing silently.
func alreadyRegisteredAsBuiltin(name, kind string) bool {
	return name == kind
}

// configuredRunnerDeps carries what a config-declared runner shares with the
// built-ins: the rendered host table, the environment, the credential lookup,
// and the transport the openai, anthropic, and mcp_client runners send through.
type configuredRunnerDeps struct {
	env             environ.Lookup
	credLookup      builtins.TokenLookup
	runnerTransport http.RoundTripper
	hostEnv         []string
}

// registerConfiguredRunners registers a runner for every [runner.<name>] block
// whose type selects one: "plugin", "mcp_client", "openai", "anthropic", or a
// builtins.CLIDefaults name, which launches another command-line tool, such as
// a wrapper script, from that runner's defaults. Any other block configures the
// same-named built-in, or stops startup when none exists. That check needs
// every built-in registered first.
func registerConfiguredRunners(
	ctx context.Context,
	registry *runner.Registry,
	runnerCfgs map[string]map[string]any,
	deps configuredRunnerDeps,
) error {
	for _, name := range slices.Sorted(maps.Keys(runnerCfgs)) {
		block := runnerCfgs[name]
		kind, _ := block["type"].(string)

		factory := configuredRunnerFactory(ctx, kind, name, deps)
		if factory == nil && !registry.Registered(name) {
			return unclaimedBlockError(name, kind, block)
		}

		if factory == nil {
			continue
		}

		if err := registry.Register(name, factory); err != nil {
			return fmt.Errorf("register %s runner %q: %w", kind, name, err)
		}
	}

	return nil
}

// unclaimedBlockError explains a [runner.<name>] block that neither a built-in's
// name nor its type claims. It stops startup, so the message names the keys
// that fix the block instead of sending the operator to the docs.
func unclaimedBlockError(name, kind string, block map[string]any) error {
	types := strings.Join(configuredRunnerTypes(), ", ")

	if kind != "" {
		return fmt.Errorf("%w: [runner.%s] sets type = %q, which is not a runner type, and no built-in runner has that name; set type to one of %s",
			runner.ErrUnknownRunner, name, kind, types)
	}

	var binaryFix string
	if _, hasBinary := block["binary"]; !hasBinary {
		binaryFix = `, and binary = "<path>" for the program to launch`
	}

	return fmt.Errorf(`%w: [runner.%s] sets no type and no built-in runner has that name; add type = "<built-in>" for the tool it wraps%s (type is one of %s)`,
		runner.ErrUnknownRunner, name, binaryFix, types)
}

// pluginRunnerType is the type value that loads an out-of-process runner.
const pluginRunnerType = "plugin"

// configuredRunnerTypes lists, sorted, every type value configuredRunnerFactory
// turns into a runner, for the startup error to offer. A type added there
// belongs here too, or the error stops offering it.
func configuredRunnerTypes() []string {
	types := append(slices.Collect(maps.Keys(builtins.CLIDefaults())),
		pluginRunnerType, builtins.MCPClientName, builtins.OpenAIName, builtins.AnthropicName)
	slices.Sort(types)

	return types
}

// configuredRunnerFactory returns the factory a block's type selects, or nil
// for a block named after its own non-plugin type, an empty type, or an
// unknown value; such a block configures the same-named built-in. A CLI type
// gets the host table and environment the built-in CLI runners get, so a
// wrapper in config sees what its parent tool would.
func configuredRunnerFactory(ctx context.Context, kind, name string, deps configuredRunnerDeps) runner.Factory {
	if kind == pluginRunnerType {
		return plugin.Factory(ctx, name, deps.env)
	}

	if alreadyRegisteredAsBuiltin(name, kind) {
		return nil
	}

	switch kind {
	case builtins.MCPClientName:
		return builtins.MCPClientFactoryForWithCredentials(name, deps.credLookup, deps.runnerTransport)
	case builtins.OpenAIName, builtins.AnthropicName:
		return namedLLMFactory(kind, name, deps.env, deps.runnerTransport)
	}

	if tool, isCLI := builtins.CLIDefaults()[kind]; isCLI {
		return builtins.CLIFactory(name, tool, deps.hostEnv, os.Environ())
	}

	return nil
}

// namedLLMFactory builds the registration for one chat-completions block under
// a name of its own. The name is threaded into the factory because it decides
// which API key the runner reads. A local server reached this way is an openai
// block with a base_url.
func namedLLMFactory(kind, name string, env environ.Lookup, transport http.RoundTripper) runner.Factory {
	if kind == builtins.AnthropicName {
		return builtins.AnthropicFactoryFor(name, env, transport)
	}

	return builtins.OpenAIFactoryFor(name, env, transport)
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
	breakers := eng.Breakers()
	breakers.OnTransition(func(name string, to reliability.State) {
		rec.BreakerTransition(ctx, name, to)
	})

	if err := rec.ObserveBreakers(breakers.States); err != nil {
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
// caller should print its usage line, which is also how extra positionals come
// back, so that line explains the expected shape.
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
