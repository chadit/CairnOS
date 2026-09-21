package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/chadit/CairnOS/internal/api"
	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/service"
	"github.com/chadit/CairnOS/internal/store"
)

// updateSubject names the update in the token it mints for itself, so a drain
// request is told apart from an operator's key in the issued-token record.
const updateSubject = "cairnos-update"

// restartHooks are the two outside-world halves of a restart: asking the
// running service to drain, and telling the supervisor to restart it now. They
// are function values so a test pins which one a run chose without a
// supervisor or a running service.
type restartHooks struct {
	// Drain asks the running service to finish its runs and exit, answering
	// with the runs still going. Any failure (no endpoint on an older build, a
	// server that will not answer) is the signal to fall back.
	Drain func(ctx context.Context) (int, error)

	// Restart stops and starts the unit through the supervisor, cutting short
	// whatever was running.
	Restart func(ctx context.Context) error
}

// driveRestart picks how the service turns over and reports whether the drain
// was accepted. The drain is preferred because a service that exits on its own
// finishes its runs first, where a supervisor restart cancels them inside the
// ten-second shutdown window.
func (a *App) driveRestart(ctx context.Context, hooks restartHooks, restartNow bool) (bool, error) {
	if restartNow {
		a.logger.Info("update: --restart-now, restarting without a drain")

		return false, hooks.Restart(ctx)
	}

	inFlight, err := hooks.Drain(ctx)
	if err != nil {
		a.logger.Info("update: the running service took no drain request, restarting instead",
			"reason", err)

		return false, hooks.Restart(ctx)
	}

	a.logger.Info("update: draining; the service exits once its runs finish", "in_flight", inFlight)

	return true, nil
}

// restartHooksFor wires the real pair for one discovered install.
func (a *App) restartHooksFor(install service.Install, home string) restartHooks {
	return restartHooks{
		Drain: func(ctx context.Context) (int, error) { return a.requestDrain(ctx, home) },
		Restart: func(ctx context.Context) error {
			a.logger.Info("update: restarting", "service", install.Label)

			if _, err := service.ExecCommand(ctx, install.RestartArgv()); err != nil {
				return fmt.Errorf("restart %s: %w", install.Label, err)
			}

			return nil
		},
	}
}

// requestDrain asks the service serving this home to finish its in-flight runs
// and exit, and reports how many were still going. The ceiling is left to the
// server so the two sides cannot disagree about when to give up.
func (a *App) requestDrain(ctx context.Context, home string) (int, error) {
	cfg, err := config.Load(filepath.Join(home, configFileName))
	if err != nil {
		return 0, fmt.Errorf("load the config in %s: %w", home, err)
	}

	token, err := a.drainToken(ctx, home, &cfg)
	if err != nil {
		return 0, err
	}

	conn, err := dialServe(cfg.API.GRPC)
	if err != nil {
		return 0, err
	}
	defer a.closeConn(conn)

	resp, err := cairnosv1.NewAdminServiceClient(conn).
		RequestRestart(bearerContext(ctx, token), &cairnosv1.RequestRestartRequest{})
	if err != nil {
		return 0, fmt.Errorf("ask %s for a restart: %w", cfg.API.GRPC, err)
	}

	return int(resp.GetInFlight()), nil
}

// drainToken mints the admin token the drain request carries. Auth off needs
// none; auth on mints one from the same signing material the serve process
// verifies with, so an update needs no operator-held token on the host. A host
// whose signing secret is not in this process's environment fails here, and the
// caller falls back to the supervisor restart.
func (a *App) drainToken(ctx context.Context, home string, cfg *config.Config) (string, error) {
	if !cfg.Auth.Enabled {
		return "", nil
	}

	keyStore, _, err := openAuth(ctx, home)
	if err != nil {
		return "", fmt.Errorf("open the auth store in %s: %w", home, err)
	}
	defer a.closeStore(keyStore)

	authn, err := buildAuthenticator(cfg, keyStore)
	if err != nil {
		return "", err
	}

	token, _, err := authn.MintForSubject(ctx, updateSubject, store.DefaultWorkspace, []string{auth.ScopeAdmin})
	if err != nil {
		return "", fmt.Errorf("mint an admin token for the drain request: %w", err)
	}

	return token, nil
}

// restartWindow is how long to wait for the new main process. A drained service
// only exits once its runs finish, so that wait carries the whole drain ceiling
// on top of the ordinary start allowance.
func restartWindow(drained bool) time.Duration {
	if drained {
		return api.DefaultDrainTimeout + restartTimeout
	}

	return restartTimeout
}
