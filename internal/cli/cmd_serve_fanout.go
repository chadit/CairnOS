package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/notify"
	"github.com/chadit/CairnOS/internal/store"
)

// BuildDiscordFanout builds the Discord event fan-out when the webhook secret is
// set, selecting the configured event set. It returns nil when the webhook env is
// unset, which disables every producer (held, run_failed, dead_lettered,
// budget_denied) the same way an unset token disables the Telegram bridge
// (.agents/spec/parity/messaging-bridge.md, decision 7). The webhook URL is a
// credential, so it is read only from the environment, never a config key.
func BuildDiscordFanout(cfg *config.Config, logger *slog.Logger) *notify.Fanout {
	webhook := os.Getenv(NotifyDiscordWebhookEnv)
	if webhook == "" {
		return nil
	}

	return notify.NewFanout(notify.NewDiscordNotifier(webhook), cfg.Notify.Discord.Events, logger)
}

// StartFanout drains the Discord event fan-out on its own goroutine, closing the
// returned channel when it exits so shutdown can wait for an in-flight post. A
// nil fan-out (no webhook) makes Run return at once, so the goroutine closes the
// channel immediately, the same supervised shape as startSweep (decision 7).
func StartFanout(ctx context.Context, fanout *notify.Fanout) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		fanout.Run(ctx)
	}()

	return done
}

// WrapRunFailedFanout wraps the manager dispatch so a failed run also emits a
// run_failed line to the fan-out. Dispatch runs first and unchanged; the emit is
// additive and fire-and-forget. A nil fan-out or a deselected event makes the
// wrapper a straight pass-through, so the run stream is untouched when Discord is
// off (.agents/spec/parity/messaging-bridge.md, decision 7). engine.OnEvent
// carries no context, so the emit rides the serve lifetime context: once serve
// shuts down it is canceled and the emit skips, which is what draining wants.
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

// runFailedLine renders a failed run as one Discord line. engine.Event carries no
// workflow name, so the line names the run ID plus the error code and message
// rather than doing a cross-context store lookup off the request path (the
// accepted trade-off in the spec).
func runFailedLine(evt *engine.Event) string {
	if evt.Err != nil {
		return fmt.Sprintf("CairnOS: run %s failed: %s: %s", evt.RunID, evt.Err.Code, evt.Err.Message)
	}

	return fmt.Sprintf("CairnOS: run %s failed", evt.RunID)
}

// BudgetFanoutNotifier routes engine budget breaches to the Discord fan-out. Its
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
