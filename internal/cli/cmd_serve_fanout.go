package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/messaging"
	"github.com/chadit/CairnOS/internal/notify"
	"github.com/chadit/CairnOS/internal/store"
)

// BuildFanout builds the event fan-out over every configured destination: a
// Discord sink when the webhook env is set, a Mattermost sink when
// [notify.mattermost] names an identity, and both when both (FR34). Neither
// gives a nil fan-out, which disables every producer. A nil registry is the
// standalone `schedule run` worker, which holds no identities to send through.
// webhookTransport carries the Discord sink, the one client this function
// builds itself; nil means the default one. The Mattermost sink sends through
// a client the registry already holds.
func BuildFanout(
	cfg *config.Config,
	registry *messaging.Registry,
	lookup environ.Lookup,
	logger *slog.Logger,
	webhookTransport http.RoundTripper,
) *notify.Fanout {
	sinks := make([]notify.Sink, 0, 2)

	// The webhook URL is a credential, so it is read only from the environment
	// and never from a config key.
	if webhook := lookup.Get(NotifyDiscordWebhookEnv); webhook != "" {
		sinks = append(sinks, notify.Sink{
			Poster: notify.NewDiscordNotifierOver(webhook, webhookTransport),
			Events: cfg.Notify.Discord.Events,
		})
	}

	sinks = appendMattermostSink(sinks, cfg, registry, logger)

	if len(sinks) == 0 {
		return nil
	}

	return notify.NewFanout(logger, sinks...)
}

// appendMattermostSink adds the Mattermost fan-out sink, or says why it cannot.
// The standalone `schedule run` worker holds no identity registry, so a held job
// that posts to #alerts under serve posts to Discord only there, and #alerts
// staying quiet reads as "no jobs held".
func appendMattermostSink(
	sinks []notify.Sink,
	cfg *config.Config,
	registry *messaging.Registry,
	logger *slog.Logger,
) []notify.Sink {
	target := cfg.Notify.Mattermost
	if target.Identity == "" {
		return sinks
	}

	if registry == nil {
		logger.Info("notify.mattermost is configured but this worker has no messaging identities; skipping the sink",
			"identity", target.Identity, "channel", target.Channel)

		return sinks
	}

	return append(sinks, notify.Sink{
		Poster: notify.MattermostSink(registry, target.Identity, target.Channel),
		Events: target.Events,
	})
}

// StartFanout drains the event fan-out on its own goroutine, closing the
// returned channel when it exits so shutdown can wait for an in-flight post. A
// nil fan-out (no destination configured) makes Run return at once, so the
// goroutine closes the channel immediately, the same supervised shape as
// startSweep (decision 7).
func StartFanout(ctx context.Context, fanout *notify.Fanout) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		fanout.Run(ctx)
	}()

	return done
}

// WrapRunFailedFanout wraps the manager dispatch so a failed run also emits a
// run_failed line. Dispatch runs first and unchanged; the emit is additive and
// fire-and-forget, and a nil fan-out or a deselected event leaves the wrapper a
// straight pass-through. engine.OnEvent carries no context, so the emit rides
// the serve lifetime context and skips once shutdown cancels it.
func WrapRunFailedFanout(ctx context.Context, dispatch func(engine.Event), fanout *notify.Fanout) func(engine.Event) {
	if !fanout.Selects(config.NotifyEventRunFailed) {
		return dispatch
	}

	return func(evt engine.Event) {
		dispatch(evt)

		if evt.Type != engine.EventRunFinished || evt.Status != string(store.RunFailed) {
			return
		}

		fanout.Emit(ctx, config.NotifyEventRunFailed, runFailedLine(&evt))
	}
}

// runFailedLine renders a failed run as one fan-out line. engine.Event carries no
// workflow name, so the line names the run ID plus the error code and message
// rather than doing a cross-context store lookup off the request path (the
// accepted trade-off in the spec).
func runFailedLine(evt *engine.Event) string {
	if evt.Err != nil {
		return fmt.Sprintf("CairnOS: run %s failed: %s: %s", evt.RunID, evt.Err.Code, evt.Err.Message)
	}

	return fmt.Sprintf("CairnOS: run %s failed", evt.RunID)
}

// BudgetFanoutNotifier routes engine budget breaches to the fan-out. Its
// BudgetWarn reproduces the warning the engine's nil-notifier fallback logs, so
// warn-mode behavior is unchanged; BudgetDenied emits a budget_denied line on the
// pause path when that event is selected (.agents/spec/parity/messaging-bridge.md,
// decision 7).
type BudgetFanoutNotifier struct {
	fanout *notify.Fanout
	logger *slog.Logger
}

// NewBudgetFanoutNotifier builds the engine's budget notifier over the fan-out,
// or nil when the fan-out is off so the engine keeps its own slog warn fallback
// and the pause path notifies nothing beyond the refusal error.
func NewBudgetFanoutNotifier(fanout *notify.Fanout, logger *slog.Logger) *BudgetFanoutNotifier {
	if fanout == nil {
		return nil
	}

	return &BudgetFanoutNotifier{fanout: fanout, logger: logger}
}

// BudgetWarn logs the same warning the engine's nil-notifier fallback emits, so a
// warn-mode breach still surfaces even though no warn event is fanned out.
func (n *BudgetFanoutNotifier) BudgetWarn(ctx context.Context, workflowName string, spend, limitUSD float64) {
	n.logger.WarnContext(ctx, "workflow over its warn budget; run proceeding",
		"workflow", workflowName, "spend_usd", spend, "limit_usd", limitUSD)
}

// BudgetDenied emits a budget_denied line naming the workflow, its spend, and the
// limit it passed, so an operator sees the pause-mode refusal.
func (n *BudgetFanoutNotifier) BudgetDenied(ctx context.Context, workflowName string, spend, limitUSD float64) {
	n.fanout.Emit(ctx, config.NotifyEventBudgetDenied,
		fmt.Sprintf("CairnOS: workflow %q denied a run: spent $%.2f, at or over its $%.2f budget",
			workflowName, spend, limitUSD))
}
