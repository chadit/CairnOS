package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/chadit/CairnOS/internal/bridge/mattermost"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/messaging"
)

// serveFailedStateFile holds the last serve_failed notice, under the data
// directory. The process exits between attempts, so the repeat window cannot
// live in memory the way the source_dirty one does.
const serveFailedStateFile = "serve-failed.json"

// serveFailedPostTimeout bounds the one synchronous post a failed startup makes.
// The supervisor is about to restart the process, so a destination that hangs
// must not hold the exit open.
const serveFailedPostTimeout = 10 * time.Second

// serveFailedStatePerm keeps the state file readable only by the account serve
// runs as, matching the rest of the data directory.
const serveFailedStatePerm = 0o600

// serveFailedState is what one notice leaves behind: when it went out and a
// digest of the failure it described, so a restart loop reporting the same
// problem stays quiet while a new problem gets through at once.
type serveFailedState struct {
	Sent  time.Time `json:"sent"`
	Error string    `json:"error"`
}

// serveFailNotifier announces a serve startup that never finished. The bridges
// connect near the end of startup, so a failure before that point has no sender
// in the messaging registry and this builds its own.
type serveFailNotifier struct {
	cfg    *config.Config
	logger *slog.Logger
	env    environ.Lookup
	dir    string
	host   string
}

// newServeFailNotifier wires the notifier runServe hands its failures to,
// before the first stage that can fail, so a startup that never reaches the
// bridges still reports.
func newServeFailNotifier(
	cfg *config.Config, dir string, lookup environ.Lookup, logger *slog.Logger,
) *serveFailNotifier {
	return &serveFailNotifier{cfg: cfg, dir: dir, env: lookup, logger: logger, host: hostname()}
}

// announce posts one serve_failed line unless an earlier notice already covered
// this same failure inside the repeat window. Delivery is synchronous, since a
// queued line dies with the exiting process, and it drops the caller's
// cancellation, which the interrupt tearing serve down may already have fired.
func (n *serveFailNotifier) announce(ctx context.Context, stage string, cause error) {
	digest := serveFailedDigest(stage, cause)
	if n.silenced(digest) {
		n.logger.Warn("serve failed the same way again; the notice already went out",
			"stage", stage, "repeat_window", notifyRepeat)

		return
	}

	fanout := BuildFanout(n.cfg, n.oneShotRegistry(), n.env, n.logger, nil)
	if !fanout.Selects(config.NotifyEventServeFailed) {
		return
	}

	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveFailedPostTimeout)
	defer cancel()

	posted, err := fanout.Deliver(postCtx, config.NotifyEventServeFailed, serveFailedLine(n.host, stage, cause))
	if err != nil {
		n.logger.Error("serve failure notice did not reach every destination", "error", err)
	}

	if posted > 0 {
		n.record(digest)
	}
}

// clear forgets the last notice, so the next failure after a healthy start is
// reported at once rather than being mistaken for the tail of an old loop.
func (n *serveFailNotifier) clear() {
	if err := os.Remove(n.statePath()); err != nil && !os.IsNotExist(err) {
		n.logger.Warn("could not clear the serve failure state", "path", n.statePath(), "error", err)
	}
}

// silenced reports whether the same failure already went out inside the window.
// A missing or unreadable file counts as never sent: the notice matters more
// than the throttle, and a corrupt file is not evidence anyone was told.
func (n *serveFailNotifier) silenced(digest string) bool {
	raw, err := os.ReadFile(n.statePath()) // #nosec G304 -- the path is built from the serve data directory
	if err != nil {
		return false
	}

	var state serveFailedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return false
	}

	return state.Error == digest && time.Since(state.Sent) < notifyRepeat
}

// record writes the notice that just went out. A write failure costs the
// throttle, not the notification, so it logs and moves on.
func (n *serveFailNotifier) record(digest string) {
	raw, err := json.Marshal(serveFailedState{Sent: time.Now().UTC(), Error: digest})
	if err != nil {
		n.logger.Warn("could not encode the serve failure state", "error", err)

		return
	}

	if err := os.WriteFile(n.statePath(), raw, serveFailedStatePerm); err != nil {
		n.logger.Warn("could not record the serve failure state", "path", n.statePath(), "error", err)
	}
}

// statePath is where the repeat window lives between process lifetimes.
func (n *serveFailNotifier) statePath() string {
	return filepath.Join(n.dir, serveFailedStateFile)
}

// oneShotRegistry holds the single identity [notify.mattermost] names, built the
// way the bridge builds its own so the notice goes out over the same client and
// the same send path. A nil registry leaves BuildFanout with Discord alone,
// which needs no bridge.
func (n *serveFailNotifier) oneShotRegistry() *messaging.Registry {
	name := n.cfg.Notify.Mattermost.Identity

	bridge, built := n.oneShotBridge(name)
	if !built {
		return nil
	}

	sender, ok := bridge.Senders()[name]
	if !ok {
		return nil
	}

	registry := messaging.NewRegistry()
	registry.Add(name, sender)

	return registry
}

// oneShotBridge builds a bridge over one identity for its send surface alone.
// It is never started: it opens no socket and never calls Connect, so a
// #channel target resolves on one lookup while a @user target, which needs the
// bot's own id, does not.
func (n *serveFailNotifier) oneShotBridge(name string) (*mattermost.Bridge, bool) {
	settings := &n.cfg.Bridge.Mattermost

	declared, known := settings.Identity[name]
	if name == "" || !known {
		return nil, false
	}

	token, _ := config.ResolveMattermostToken(&declared, name, n.env.Get)
	if token == "" {
		n.logger.Info("serve failure notice has no mattermost token", "identity", name,
			"tried", config.MattermostTokenEnvs(&declared, name))

		return nil, false
	}

	client, err := mattermost.NewClient(mattermost.ClientConfig{
		Limiter: mattermost.NewBucket(mattermost.BucketConfig{
			RatePerSecond: float64(settings.RatePerSec),
			Burst:         float64(settings.RateBurst),
		}),
		Log:          n.logger,
		URL:          settings.URL,
		Token:        token,
		Timeout:      settings.SendTimeout,
		InsecureHTTP: settings.InsecureHTTP,
	})
	if err != nil {
		n.logger.Warn("serve failure notice has no mattermost client", "identity", name, "error", err)

		return nil, false
	}

	return mattermost.New(&mattermost.Config{
		Logger:     n.logger,
		Identities: []mattermost.Identity{{Client: client, Name: name, Config: declared}},
		Server:     *settings,
	}), true
}

// serveFailedLine names the host and the stage, and says what the box is doing
// about it, because the supervisor restarts serve every few seconds and an
// operator reading the channel cannot see that from the line alone.
func serveFailedLine(host, stage string, cause error) string {
	return fmt.Sprintf("CairnOS on %s: serve failed at %s: %v; systemd restarts it every few seconds until this is fixed",
		host, stage, cause)
}

// serveFailedDigest identifies one failure. It is a hash rather than the message
// so the state file cannot carry a path, a host, or a token fragment that landed
// in an error string.
func serveFailedDigest(stage string, cause error) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\n%v", stage, cause))

	return hex.EncodeToString(sum[:])
}

// notifyServeFailure is the hook failServe calls. Outside runServe no notifier
// was ever built, so every other command's failures stay on stderr.
func (a *App) notifyServeFailure(ctx context.Context, stage string, cause error) {
	if a.serveFail == nil {
		return
	}

	a.serveFail.announce(ctx, stage, cause)
}

// clearServeFailure is called once startup has gone far enough that the bridges
// are up, so the next failure after a healthy run is reported at once.
func (a *App) clearServeFailure() {
	if a.serveFail == nil {
		return
	}

	a.serveFail.clear()
}
