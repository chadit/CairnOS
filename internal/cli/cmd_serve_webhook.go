package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/secrets"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/internal/webhook"
	"github.com/chadit/CairnOS/internal/workflow"
)

// webhookIngressPath is the production ingress route prefix, echoed in the serve
// banner so an operator sees where the extension mounted. The token completes it
// per registration, so the banner shows the prefix only, never a token.
const webhookIngressPath = "/v1/hooks/"

// webhookComponent names the webhook ingress in serve logs, both as the
// extension label and as the setup stage a failure reports.
const webhookComponent = "webhook"

// webhookDeps is what buildWebhookExtension wires the ingress handler over: the
// schedule store it borrows a pool from on postgres, the run manager it starts
// runs through, the pause store it gates on, and the logger.
type webhookDeps struct {
	scheds  scheduleStore
	manager *api.Manager
	pauses  api.SchedulePauseChecker
	secrets secrets.Source
	logger  *slog.Logger
}

// Kept out of runServe so that function stays under the complexity ceiling.

// attachWebhookServe mounts the webhook ingress extension when [webhook]
// enabled is true, appending it to apiCfg and returning the store closer for
// shutdown. A disabled config logs one line and returns a no-op closer. The
// bool is false only on a build failure, carrying the exit code with telemetry
// flushed; it stays true when disabled so serve keeps starting.
func (a *App) attachWebhookServe(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry, dir string, apiCfg *api.Config, deps *webhookDeps) (func() error, int, bool) {
	noop := func() error { return nil }

	if !cfg.Webhook.Enabled {
		deps.logger.Info("webhook ingress disabled")

		return noop, ExitOK, true
	}

	ext, closeHooks, err := buildWebhookExtension(ctx, cfg, dir, deps)
	if err != nil {
		return noop, a.failServe(ctx, tel, webhookComponent, err), false
	}

	apiCfg.Extensions = append(apiCfg.Extensions, ext)

	a.printOutf("webhook ingress available at %s%s\n", "http://"+cfg.API.HTTP, webhookIngressPath)

	return closeHooks, ExitOK, true
}

// The path token is a write:workflow-equivalent capability for exactly one
// workflow, and every webhook run executes under store.DefaultWorkspace, which
// the handler stamps. The interceptor's scope table is untouched, so no gRPC
// route loses its scoped bearer requirement.

// buildWebhookExtension opens the registration store, builds the ingress
// handler over the run manager and pause store, and returns an api.Extension
// mounting the two POST routes on the root mux outside the gRPC interceptor.
// The returned closer releases the store's handle.
func buildWebhookExtension(ctx context.Context, cfg *config.Config, dir string, deps *webhookDeps) (*api.Extension, func() error, error) {
	hooks, err := webhookBackends(deps.scheds).open(ctx, dir, cfg)
	if err != nil {
		return nil, nil, err
	}

	handler := webhook.NewHandler(&webhook.HandlerConfig{
		Store:  hooks,
		Runs:   &webhookRunStarter{manager: deps.manager},
		Pauses: &webhookPauseChecker{store: deps.pauses},
		// HMAC secrets resolve through the same chain the engine uses (env, then
		// the encrypted store when configured), so a store-managed HMAC secret is
		// not hidden from ingress (security council amendment 7).
		Secrets:       deps.secrets,
		MaxBodyBytes:  cfg.API.MaxBodyBytes,
		RatePerMinute: cfg.Webhook.RatePerMinute,
		TestWait:      cfg.Webhook.TestWait,
		SignatureSkew: cfg.Webhook.SignatureSkew,
		Logger:        deps.logger,
	})

	return &api.Extension{
		Name:       webhookComponent,
		HTTPRoutes: handler.Routes(),
	}, ownedCloser(hooks), nil
}

// webhookRunStarter adapts the run manager to webhook.RunStarter: it loads the
// workflow fresh per hit (FireRunner precedent) and starts an unwatched run,
// bridging the manager's terminal RunOutcome onto the handler's RunResult
// channel. The workspace stamped on the run context by the handler
// (store.DefaultWorkspace) is what the manager records the run under.
type webhookRunStarter struct {
	manager *api.Manager
}

// StartRun loads and starts one webhook-triggered run. A load failure is a
// generic error the handler maps to 500; a StartRun rejection (the engine's
// canonical workflow.invalid from validateInputs) is passed through wrapped so
// the handler's canonical extraction still finds it and answers 400.
func (s *webhookRunStarter) StartRun(ctx context.Context, workflowPath string, inputs map[string]any, trigger string) (webhook.StartedRun, error) {
	flow, err := workflow.Load(workflowPath)
	if err != nil {
		return webhook.StartedRun{}, fmt.Errorf("load webhook workflow: %w", err)
	}

	handle, err := s.manager.StartRun(ctx, flow, inputs, false, trigger)
	if err != nil {
		return webhook.StartedRun{}, fmt.Errorf("start webhook run: %w", err)
	}

	// The manager's buffered Outcome carries a *store.Run; the handler wants an
	// opaque record on a RunResult channel. One goroutine forwards the single
	// terminal value so the buffered manager channel never blocks.
	outcome := make(chan webhook.RunResult, 1)

	go func() {
		res := <-handle.Outcome
		outcome <- webhook.RunResult{Run: res.Run, Err: res.Err}
	}()

	return webhook.StartedRun{RunID: handle.ID, Outcome: outcome}, nil
}

// webhookPauseChecker adapts the store's GetWorkflowPause to webhook.PauseChecker,
// the same pause seam the scheduler's FireRunner reads, so a paused workflow
// answers 409 and starts no run.
type webhookPauseChecker struct {
	store api.SchedulePauseChecker
}

// IsPaused reports whether the workflow is paused, dropping the pause record the
// handler does not need.
func (c *webhookPauseChecker) IsPaused(ctx context.Context, workflowName string) (bool, error) {
	_, paused, err := c.store.GetWorkflowPause(ctx, workflowName)
	if err != nil {
		return false, fmt.Errorf("webhook pause check: %w", err)
	}

	return paused, nil
}
