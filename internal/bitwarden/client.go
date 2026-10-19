package bitwarden

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/execretry"
)

// Canonical codes, one per step, so a failure says which round trip broke
// without ever carrying a value. Response bodies never reach an error message:
// only status codes, field names, and the item name do.
const (
	// CodeCredentials marks a credentials file that is missing, unreadable, or
	// too widely readable.
	// #nosec G101 -- an error code, not a credential.
	CodeCredentials = "bitwarden.credentials"
	CodeStatus      = "bitwarden.status"
	CodeLogin       = "bitwarden.login"
	CodeServe       = "bitwarden.serve"
	CodeReady       = "bitwarden.ready"
	CodeUnlock      = "bitwarden.unlock"
	CodeList        = "bitwarden.list"
	CodeItem        = "bitwarden.item"
	CodeFields      = "bitwarden.fields"
)

// Timing. The readiness budget matches the helper this protocol was taken from;
// the poll interval is short because the daemon usually answers in well under a
// second and a deploy is waiting on it.
const (
	// DefaultTimeout bounds one HTTP call and each bw subcommand.
	DefaultTimeout = 60 * time.Second

	readyTimeout  = 15 * time.Second
	readyInterval = 250 * time.Millisecond

	// waitDelay bounds the gap between killing the daemon and giving up on the
	// pipes it left behind, so a child holding them cannot block a deploy.
	waitDelay = 5 * time.Second

	// lockTimeout bounds the two cleanup calls, which run on the way out of a
	// failure and must not extend it.
	lockTimeout = 10 * time.Second
)

// statusUnauthenticated is the one bw status value that needs a login. Any other
// value means credentials are already on disk from an earlier run.
const statusUnauthenticated = "unauthenticated"

// noInteraction keeps bw from prompting. Without it a missing credential turns a
// deploy into a process waiting on a terminal nobody is watching.
const noInteraction = "BW_NOINTERACTION=true"

// appDataDirEnv is bw's own override for where it keeps its stored login. It is
// forwarded when set so an operator who moved that directory keeps working.
const appDataDirEnv = "BITWARDENCLI_APPDATA_DIR"

// probeTimeout bounds one readiness probe. It is short because the failure it
// has to survive is a loopback SYN the host drops on a recycled port: the dial
// never answers, and only a new one from a fresh source port does.
const probeTimeout = 750 * time.Millisecond

// serveAttempts bounds how many ports one start will try. The port is released
// before the child binds it, so a busy host can lose it in between, and nothing
// but another port helps.
const serveAttempts = 3

// stderrTail bounds how much of a dead child's stderr an error carries: a bind
// refusal is one line, and a chatty daemon must not push a log into a message.
const stderrTail = 512

// Client drives one `bw serve` daemon for the life of a single Fields call. It
// holds no state between calls: the vault session lives only in the daemon's
// memory and dies with it, which is the whole reason this talks to serve rather
// than caching a session token.
type Client struct {
	// Logger receives step-level progress. Nil disables logging; nothing logged
	// here carries a value.
	Logger *slog.Logger

	// Env answers the few variables a bw child is given. Nil reads the real
	// process environment.
	Env environ.Lookup

	// Clock measures the readiness budget. Nil reads the machine's clock. A
	// caller that drives its own clock decides when the budget runs out, so a
	// loaded host cannot expire a wait the daemon met.
	Clock Clock

	// Binary is the bw executable, resolved by the caller through LookPath.
	Binary string

	// Timeout bounds one HTTP call and each bw subcommand. Zero means
	// DefaultTimeout.
	Timeout time.Duration
}

// Clock is how the readiness wait measures its budget, so waiting is an
// observable event rather than a bet on how fast this host starts a process.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// item is the part of a vault item this package reads. Everything else the
// search returns is ignored rather than modeled.
type item struct {
	Name   string      `json:"name"`
	Fields []itemField `json:"fields"`
}

// itemField is one custom field on an item.
type itemField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// listResponse is the double-nested envelope the serve list endpoint returns.
type listResponse struct {
	Data struct {
		Data []item `json:"data"`
	} `json:"data"`
}

// Item is one vault item's custom fields, keyed by field name, with the item's
// own name for error messages.
type Item struct {
	Fields map[string]string
	Name   string
}

// Fields opens the vault, reads the named custom fields off the item, and closes
// it again. It is Read plus Select, kept together for the explicit-mapping path.
func (c *Client) Fields(
	ctx context.Context, creds Credentials, itemName string, names []string,
) (map[string]string, error) {
	item, err := c.Read(ctx, creds, itemName)
	if err != nil {
		return nil, err
	}

	return Select(item, names)
}

// Read opens the vault, returns every custom field on the item, and closes it
// again. The vault is locked on every exit path, including a failure part way
// through.
//
// It returns the whole item because a caller may be inferring which fields it
// wants from their names, and the alternative is paying the daemon's whole
// lifecycle twice for one item.
func (c *Client) Read(
	ctx context.Context, creds Credentials, itemName string,
) (Item, error) {
	if err := c.ensureAuthenticated(ctx, creds); err != nil {
		return Item{}, err
	}

	daemon, err := c.serveReady(ctx)
	if err != nil {
		return Item{}, err
	}

	// close runs with its own background context on purpose: the vault has to be
	// locked even when ctx is already canceled, which is exactly the path where
	// leaving it open would matter most.
	defer daemon.close(c.Logger) //nolint:contextcheck // cleanup must outlive a canceled ctx

	if unlockErr := daemon.unlock(ctx, creds, c.timeout()); unlockErr != nil {
		return Item{}, unlockErr
	}

	daemon.sync(ctx, c.timeout(), c.Logger)

	found, err := daemon.find(ctx, itemName, c.timeout())
	if err != nil {
		return Item{}, err
	}

	byName := make(map[string]string, len(found.Fields))
	for _, field := range found.Fields {
		byName[field.Name] = field.Value
	}

	return Item{Name: found.Name, Fields: byName}, nil
}

// timeout resolves the per-call bound.
func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}

	return DefaultTimeout
}

// ensureAuthenticated logs in only when bw says no credentials are stored. The
// API key goes to the child's environment, never argv, because arguments are
// readable from /proc by any process the same user owns.
func (c *Client) ensureAuthenticated(ctx context.Context, creds Credentials) error {
	state, err := c.status(ctx)
	if err != nil {
		return err
	}

	if state != statusUnauthenticated {
		return nil
	}

	logStep(c.Logger, "bitwarden login")

	// The API key is the one thing added to the child environment, and only for
	// the login call: the serve daemon never needs it.
	runCtx, build, cancel := c.command(ctx, []string{
		EnvClientID + "=" + creds.ClientID,
		EnvClientSecret + "=" + creds.ClientSecret,
	}, "login", "--apikey", "--raw")
	defer cancel()

	if _, runErr := run(runCtx, build); runErr != nil {
		return cairnerr.Wrapf(runErr, CodeLogin,
			"bw login failed; check %s and %s", EnvClientID, EnvClientSecret)
	}

	return nil
}

// status asks bw whether the account is authenticated. A non-JSON answer is
// reported rather than guessed at, since guessing would either skip a needed
// login or repeat one that already happened.
func (c *Client) status(ctx context.Context) (string, error) {
	runCtx, build, cancel := c.command(ctx, nil, "status")
	defer cancel()

	out, err := run(runCtx, build)
	if err != nil {
		return "", cairnerr.Wrapf(err, CodeStatus, "bw status failed: %v", err)
	}

	var parsed struct {
		Status string `json:"status"`
	}

	if jsonErr := json.Unmarshal([]byte(out), &parsed); jsonErr != nil {
		return "", cairnerr.Wrapf(jsonErr, CodeStatus, "bw status did not answer with JSON")
	}

	return parsed.Status, nil
}

// command builds bw invocations with prompting disabled, no stdin, and a
// deadline, so network work cannot hang a deploy. It returns a builder so a
// caller can retry a bw the installer just wrote; extra lands on top of the
// child environment.
//
// That environment is built, not inherited: an inherited copy would leak the
// master password through /proc.
func (c *Client) command(ctx context.Context, extra []string, argv ...string,
) (context.Context, execretry.Build, context.CancelFunc) {
	runCtx, cancel := context.WithTimeout(ctx, c.timeout())

	return runCtx, func() (*exec.Cmd, error) {
		// #nosec G204 -- argv is this package's own fixed subcommands; the operator's
		// values travel in the environment and in request bodies, never here.
		cmd := exec.CommandContext(runCtx, c.Binary, argv...)

		cmd.Env = append(childEnv(c.Env), extra...)
		cmd.Stdin = nil
		cmd.WaitDelay = waitDelay

		return cmd, nil
	}, cancel
}

// childEnv is the minimum bw needs: a PATH to find its own helpers, a HOME and
// the app-data override that decide where its stored login lives, and the
// no-prompt flag. Everything else this process holds stays here.
func childEnv(lookup environ.Lookup) []string {
	if lookup == nil {
		lookup = environ.OS()
	}

	return append([]string{noInteraction}, environ.Environ(lookup, "PATH", "HOME", appDataDirEnv)...)
}

// run collects stdout and returns stderr inside the error, which is safe: bw
// writes diagnostics there, and no value this package sends travels in argv.
func run(ctx context.Context, build execretry.Build) (string, error) {
	var stderr bytes.Buffer

	stdout, err := execretry.Output(ctx, func() (*exec.Cmd, error) {
		cmd, buildErr := build()
		if buildErr != nil {
			return nil, buildErr
		}

		stderr.Reset()
		cmd.Stderr = &stderr

		return cmd, nil
	})
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(string(stdout)), nil
}

// logStep records progress without values.
func logStep(logger *slog.Logger, message string, args ...any) {
	if logger == nil {
		return
	}

	logger.Info(message, args...)
}

// Select maps the item's custom fields onto the requested names, naming every
// missing one at once so a single run tells the operator everything to fix. A
// field present but empty counts as missing: an empty value in the generated
// file would look configured and behave as if it were not.
func Select(found Item, names []string) (map[string]string, error) {
	values := make(map[string]string, len(names))

	var missing []string

	for _, name := range names {
		if value := found.Fields[name]; value != "" {
			values[name] = value

			continue
		}

		missing = append(missing, name)
	}

	if len(missing) > 0 {
		slices.Sort(missing)

		return nil, vaultError(CodeFields, "item %q has no value for %s",
			found.Name, strings.Join(missing, ", "))
	}

	return values, nil
}

// freePort asks the kernel for an unused loopback port and gives it straight
// back. A port could in principle be taken in between, which is the same race
// the helper this protocol came from accepts; the alternative is parsing the
// daemon's stdout, which bw does not promise.
func freePort(ctx context.Context) (int, error) {
	var config net.ListenConfig

	listener, err := config.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, cairnerr.Wrapf(err, CodeServe, "reserve a loopback port: %v", err)
	}

	defer func() { _ = listener.Close() }()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, vaultError(CodeServe, "loopback listener reported no TCP port")
	}

	return addr.Port, nil
}

// serveReady starts the daemon and waits for it to answer, moving to another
// port when the one it had turns out unusable. The port is released before the
// child binds it, so another process can take it, and a port the host stops
// carrying traffic for leaves the child up but unreachable.
func (c *Client) serveReady(ctx context.Context) (*daemon, error) {
	var (
		lastErr error
		causes  []string
	)

	for attempt := 1; attempt <= serveAttempts; attempt++ {
		started, startErr := c.startServe(ctx)
		if startErr != nil {
			return nil, startErr
		}

		readyErr := started.waitReady(ctx)
		if readyErr == nil {
			return started, nil
		}

		// The old child is stopped and reaped before the next one starts, so no
		// failed attempt leaves a process behind holding a port.
		started.close(c.Logger) //nolint:contextcheck // cleanup must outlive a canceled ctx

		cause, retryable := serveRetryCause(readyErr)
		if !retryable {
			return nil, readyErr
		}

		lastErr = readyErr
		causes = append(causes, readyErr.Error())

		logStep(c.Logger, "bitwarden serve was not reachable; trying another port",
			"port", started.port, "cause", cause, "attempt", attempt)
	}

	return nil, cairnerr.Wrapf(lastErr, CodeReady, "bw serve did not come up in %d starts: %s",
		serveAttempts, strings.Join(causes, "; "))
}

// serveRetryCause names why a start failed and reports whether another port can
// fix it. A canceled caller cannot, so it ends the read where it happened.
func serveRetryCause(err error) (string, bool) {
	switch {
	case errors.Is(err, errServeExited):
		return "exited", true
	case errors.Is(err, errServeSilent):
		return "silent", true
	default:
		return "", false
	}
}

// startServe launches the daemon bound to loopback only, so nothing off the host
// can reach an unlocked vault.
func (c *Client) startServe(ctx context.Context) (*daemon, error) {
	port, err := freePort(ctx)
	if err != nil {
		return nil, err
	}

	// The daemon outlives one call's timeout on purpose: it has to stay up for
	// the unlock, sync and list that follow, each of which carries its own bound.
	serveCtx, cancel := context.WithCancel(ctx)

	var stderr bytes.Buffer

	cmd, startErr := execretry.Start(ctx, func() (*exec.Cmd, error) {
		// #nosec G204 -- argv is this package's own fixed subcommand and a port it
		// chose itself.
		built := exec.CommandContext(serveCtx, c.Binary,
			"serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port))

		built.Env = childEnv(c.Env)
		built.Stdin = nil
		built.WaitDelay = waitDelay

		stderr.Reset()
		built.Stderr = &stderr

		return built, nil
	})
	if startErr != nil {
		cancel()

		return nil, cairnerr.Wrapf(startErr, CodeServe, "start bw serve: %v", startErr)
	}

	logStep(c.Logger, "bitwarden serve started", "port", port)

	// The machine's clock is what a deploy waits on, so it backs a nil Clock.
	now, after := time.Now, time.After
	if c.Clock != nil {
		now, after = c.Clock.Now, c.Clock.After
	}

	started := &daemon{
		base:    "http://127.0.0.1:" + strconv.Itoa(port),
		cmd:     cmd,
		cancel:  cancel,
		exited:  make(chan struct{}),
		now:     now,
		after:   after,
		client:  newDaemonClient(c.timeout()),
		stderr:  &stderr,
		port:    port,
		timeout: c.timeout(),
	}

	// One reaper rather than a Wait on the way out: waitReady watches exited,
	// so a child that dies during startup ends the wait at once instead of
	// leaving the readiness budget to run out on a dead process.
	go func() {
		defer close(started.exited)

		started.waitErr = cmd.Wait()
	}()

	return started, nil
}

// newDaemonClient gives one daemon its own connection pool. The default
// transport is shared across this process and keyed by address alone, so a
// daemon on a recycled loopback port can be handed a connection the last one
// left there. Keep-alives stay off so every request dials again, and a bare
// transport keeps a proxy variable off a loopback call.
func newDaemonClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
}

// daemon is one running bw serve process and the HTTP client that talks to it.
type daemon struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	exited chan struct{}
	now    func() time.Time
	after  func(time.Duration) <-chan time.Time
	client *http.Client

	// stderr collects what the child wrote, so a start that failed says why.
	// Reading it is safe only once exited is closed: until Wait returns, the
	// copy into it is still running.
	stderr *bytes.Buffer

	// waitErr is how the child exited, written by the reaper before it closes
	// exited, so it is safe to read on the same terms as stderr.
	waitErr error

	base    string
	port    int
	timeout time.Duration
}

// close locks the vault and stops the daemon. It runs on every exit path,
// success or failure, so an interrupted pull never leaves an unlocked vault
// listening on loopback.
func (d *daemon) close(logger *slog.Logger) {
	d.lock(logger)
	d.cancel()

	<-d.exited
}

// lock closes the vault, skipping the call when the child is already gone: a
// dead daemon holds nothing open, and the failed POST would report a lock
// problem where there is none.
func (d *daemon) lock(logger *slog.Logger) {
	select {
	case <-d.exited:
		return
	default:
	}

	lockCtx, cancelLock := context.WithTimeout(context.WithoutCancel(context.Background()), lockTimeout)
	defer cancelLock()

	if _, err := d.post(lockCtx, "/lock", nil); err != nil {
		logStep(logger, "bitwarden lock failed; the daemon is being killed anyway")
	}
}

// stderrTail returns the end of what the child wrote, which is where a bind
// refusal lands. Call it only once exited is closed.
func (d *daemon) stderrTail() string {
	text := strings.TrimSpace(d.stderr.String())
	if len(text) <= stderrTail {
		return text
	}

	return text[len(text)-stderrTail:]
}

// waitReady blocks until the daemon answers, which is the only signal bw gives
// that its listener is up. The child exiting ends it too, because no length of
// budget rescues a daemon that is already gone. Its two failures carry sentinels
// because serveReady answers both by starting over on another port.
func (d *daemon) waitReady(ctx context.Context) error {
	deadline := d.now().Add(readyTimeout)

	for d.now().Before(deadline) {
		// Each probe gets its own short bound, so a dial the host drops costs one
		// attempt rather than the whole budget: the next one leaves from a fresh
		// source port and gets through.
		if d.probe(ctx) == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return cairnerr.Wrapf(ctx.Err(), CodeReady, "canceled while waiting for bw serve: %v", ctx.Err())
		case <-d.exited:
			return d.exitError()
		case <-d.after(readyInterval):
		}
	}

	return fmt.Errorf("%w on port %d within %s", errServeSilent, d.port, readyTimeout)
}

// exitError reports a child that died before it answered, with how it exited
// and the tail of what it wrote. A child that exited cleanly has no wait error
// to wrap.
func (d *daemon) exitError() error {
	if d.waitErr == nil {
		return fmt.Errorf("%w on port %d: %s", errServeExited, d.port, d.stderrTail())
	}

	return fmt.Errorf("%w on port %d (%w): %s", errServeExited, d.port, d.waitErr, d.stderrTail())
}

// unlock opens the vault. The master key travels in the request body because
// loopback HTTP is the one path here that keeps it out of argv and out of the
// environment of anything but this process.
func (d *daemon) unlock(ctx context.Context, creds Credentials, timeout time.Duration) error {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body := map[string]string{"password": creds.MasterKey}

	if _, err := d.post(callCtx, "/unlock", body); err != nil {
		return cairnerr.Wrapf(err, CodeUnlock, "unlock the vault: %v", err)
	}

	return nil
}

// sync refreshes the local copy. A failure is logged and not returned, matching
// the helper: a stale-but-present vault still answers, and failing the deploy
// over a refresh nobody asked for would be worse than the staleness.
func (d *daemon) sync(ctx context.Context, timeout time.Duration, logger *slog.Logger) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := d.post(callCtx, "/sync", nil); err != nil {
		logStep(logger, "bitwarden sync failed; continuing with the vault as stored")
	}
}

// find returns the item whose name matches exactly. The endpoint is a substring
// search, so the first hit is the wrong answer whenever two item names share a
// prefix; two exact matches are refused rather than picked between.
func (d *daemon) find(ctx context.Context, name string, timeout time.Duration) (*item, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	raw, err := d.get(callCtx, "/list/object/items?search="+url.QueryEscape(name))
	if err != nil {
		return nil, cairnerr.Wrapf(err, CodeList, "list vault items: %v", err)
	}

	var parsed listResponse
	if jsonErr := json.Unmarshal(raw, &parsed); jsonErr != nil {
		return nil, cairnerr.Wrapf(jsonErr, CodeList, "the item list did not answer with JSON")
	}

	return exactMatch(parsed.Data.Data, name)
}

// exactMatch picks the one item named name.
func exactMatch(items []item, name string) (*item, error) {
	var matches []item

	for _, candidate := range items {
		if candidate.Name == name {
			matches = append(matches, candidate)
		}
	}

	if len(matches) == 0 {
		return nil, vaultError(CodeItem, "no vault item is named %q", name)
	}

	if len(matches) > 1 {
		return nil, vaultError(CodeItem, "%d vault items are named %q; rename all but one",
			len(matches), name)
	}

	return &matches[0], nil
}

// probe is one readiness check under its own short deadline.
func (d *daemon) probe(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	_, err := d.get(probeCtx, "/status")

	return err
}

// get performs one GET and returns the body, refusing a non-2xx by status code
// alone so a body carrying a value never reaches an error message.
func (d *daemon) get(ctx context.Context, path string) ([]byte, error) {
	return d.do(ctx, http.MethodGet, path, nil)
}

// post performs one POST with an optional JSON body.
func (d *daemon) post(ctx context.Context, path string, body map[string]string) ([]byte, error) {
	return d.do(ctx, http.MethodPost, path, body)
}

// do is the one place a request is built, so every call gets the same timeout,
// the same JSON content type, and the same body-free error handling.
func (d *daemon) do(ctx context.Context, method, path string, body map[string]string) ([]byte, error) {
	var payload io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode the %s body: %w", path, err)
		}

		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, d.base+path, payload)
	if err != nil {
		return nil, fmt.Errorf("build the %s request: %w", path, err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	return readResponse(resp, path)
}

// readResponse drains the body on success and reports only the status code on
// failure, because a bw error body can quote the request it was given.
func readResponse(resp *http.Response, path string) ([]byte, error) {
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%s answered %w", path, &statusError{code: resp.StatusCode})
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read the %s response: %w", path, err)
	}

	return raw, nil
}

// statusError carries an HTTP status as an error with no body attached. Only
// the pointer implements error, so the type has one unambiguous error form.
type statusError struct {
	code int
}

func (s *statusError) Error() string {
	return "HTTP " + strconv.Itoa(s.code)
}

// Is reports a statusError as errAnswered: any status at all is a reply.
func (*statusError) Is(target error) bool {
	return target == errAnswered
}
