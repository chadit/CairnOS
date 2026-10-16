//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	pgImage = "postgres:18-alpine"

	// pgIdentity is the disposable database name, user, and password for the
	// throwaway container, a local-only credential rather than a real secret.
	pgIdentity = "cairnos"

	// pgReadyOccurrences waits for the second "ready" line: Postgres logs it once
	// while initdb runs and again on the real start, and only the second means
	// the server will accept connections.
	pgReadyOccurrences = 2
	pgStartupTimeout   = 2 * time.Minute

	// pgMaxConnections sets the server connection ceiling. Each parallel subtest
	// opens its own pool against this one container; with pgPoolMaxConns capping
	// each pool, the realistic peak stays well under this.
	pgMaxConnections = "100"

	// pgPoolMaxConns caps every per-test pgxpool. pgxpool reads pool_max_conns
	// from the DSN, so baking it into the shared DSN bounds the parallel fan-out
	// from one place instead of raising the server ceiling to hide it. A
	// conformance subtest queries sequentially, so a few connections is plenty.
	pgPoolMaxConns = "4"
)

// pgShared holds the one Postgres container the whole integration package runs
// against. Tests start it lazily through DSN; TestMain reaps it through Stop.
//
//nolint:gochecknoglobals // single shared-container cell for the integration suite.
var pgShared sharedPostgres

type sharedPostgres struct {
	err       error
	container *tcpostgres.PostgresContainer
	dsn       string
	once      sync.Once
}

// DSN returns the shared container's connection string, starting it on first
// call. A start failure fails the calling test instead of panicking a package
// init, so the error surfaces on the test that needed the database.
func DSN(t *testing.T) string {
	t.Helper()

	pgShared.once.Do(startPostgres)

	if pgShared.err != nil {
		t.Fatalf("start postgres container: %v", pgShared.err)
	}

	return pgShared.dsn
}

// Stop terminates the shared container if it started. TestMain calls it after
// the suite, so the container is reaped even where the testcontainers Ryuk
// reaper cannot run (rootless podman cannot bind-mount its machine socket, so
// the suite runs with the reaper disabled and cleans up here instead).
func Stop() {
	if pgShared.container == nil {
		return
	}

	if err := pgShared.container.Terminate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "integration: terminate postgres container:", err)
	}
}

// startPostgres launches the throwaway Postgres container and records its DSN.
// It owns its own context so callers in _test files never reach for
// context.Background, and tunes for a test workload (fsync off, capped per-pool
// connections) since the data does not outlive the run.
func startPostgres() {
	ctx := context.Background()

	container, err := tcpostgres.Run(
		ctx, pgImage,
		tcpostgres.WithDatabase(pgIdentity),
		tcpostgres.WithUsername(pgIdentity),
		tcpostgres.WithPassword(pgIdentity),
		testcontainers.WithCmd("postgres", "-c", "max_connections="+pgMaxConnections, "-c", "fsync=off"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(pgReadyOccurrences).
				WithStartupTimeout(pgStartupTimeout),
		),
	)
	if err != nil {
		pgShared.err = fmt.Errorf("run postgres container: %w", err)

		return
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable", "pool_max_conns="+pgPoolMaxConns)
	if err != nil {
		pgShared.err = errors.Join(
			fmt.Errorf("postgres connection string: %w", err),
			container.Terminate(ctx),
		)

		return
	}

	pgShared.container = container
	pgShared.dsn = dsn
}
