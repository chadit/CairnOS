package bitwarden

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
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

// probeTimeout bounds one readiness probe, short enough that several fit inside
// readyTimeout.
const probeTimeout = 2 * time.Second

// Client drives one `bw serve` daemon for the life of a single Fields call. It
// holds no state between calls: the vault session lives only in the daemon's
// memory and dies with it, which is the whole reason this talks to serve rather
// than caching a session token.
type Client struct {
	// Logger receives step-level progress. Nil disables logging; nothing logged
	// here carries a value.
	Logger *slog.Logger

	// Binary is the bw executable, resolved by the caller through LookPath.
	Binary string

	// Timeout bounds one HTTP call and each bw subcommand. Zero means
	// DefaultTimeout.
	Timeout time.Duration
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
// again. The daemon is started and killed inside this call, and the vault is
// locked on every exit path including a failure part way through.
//
// It returns the whole item because a caller may be inferring which fields it
// wants from their names, and paying the daemon's whole lifecycle twice to ask
// two questions about one item would be the only alternative.
func (c *Client) Read(
	ctx context.Context, creds Credentials, itemName string,
) (Item, error) {
	if err := c.ensureAuthenticated(ctx, creds); err != nil {
		return Item{}, err
	}

	daemon, err := c.startServe(ctx)
	if err != nil {
		return Item{}, err
	}

	// close runs with its own background context on purpose: the vault has to be
	// locked even when ctx is already canceled, which is exactly the path where
	// leaving it open would matter most.
	defer daemon.close(c.Logger) //nolint:contextcheck // cleanup must outlive a canceled ctx

	if readyErr := daemon.waitReady(ctx); readyErr != nil {
		return Item{}, readyErr
	}

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

	cmd, cancel := c.command(ctx, "login", "--apikey", "--raw")
	defer cancel()

	// The API key is the one thing added to the child environment, and only for
	// the login call: the serve daemon never needs it.
	cmd.Env = append(cmd.Env,
		EnvClientID+"="+creds.ClientID,
		EnvClientSecret+"="+creds.ClientSecret,
	)

	if _, runErr := run(cmd); runErr != nil {
		return cairnerr.Wrapf(runErr, CodeLogin,
			"bw login failed; check %s and %s", EnvClientID, EnvClientSecret)
	}

	return nil
}

// status asks bw whether the account is authenticated. A non-JSON answer is
// reported rather than guessed at, since guessing would either skip a needed
// login or repeat one that already happened.
func (c *Client) status(ctx context.Context) (string, error) {
	cmd, cancel := c.command(ctx, "status")
	defer cancel()

	out, err := run(cmd)
	if err != nil {
		return "", cairnerr.Wrapf(err, CodeStatus, "bw status failed: %v", err)
	}

	var parsed struct {
		Status string `json:"status"`
	}

	if jsonErr := json.Unmarshal([]byte(out), &parsed); jsonErr != nil {
		return "", cairnerr.New(CodeStatus, "bw status did not answer with JSON")
	}

	return parsed.Status, nil
}

// command builds one bw invocation with prompting disabled, no stdin, and a
// deadline, so a subcommand doing network work cannot hang a deploy between the
// build and the restart. The caller runs it and then calls the returned cancel.
//
// The child environment is built rather than inherited. LoadCredentials accepts
// the master password from this process's environment, and an inherited copy
// would sit in a long-lived daemon's /proc entry for the life of the pull.
func (c *Client) command(ctx context.Context, argv ...string) (*exec.Cmd, context.CancelFunc) {
	runCtx, cancel := context.WithTimeout(ctx, c.timeout())

	// #nosec G204 -- argv is this package's own fixed subcommands; the operator's
	// values travel in the environment and in request bodies, never here.
	cmd := exec.CommandContext(runCtx, c.Binary, argv...)

	cmd.Env = childEnv()
	cmd.Stdin = nil
	cmd.WaitDelay = waitDelay

	return cmd, cancel
}

// childEnv is the minimum bw needs: a PATH to find its own helpers, a HOME and
// the app-data override that decide where its stored login lives, and the
// no-prompt flag. Everything else this process holds stays here.
func childEnv() []string {
	env := []string{noInteraction}

	for _, name := range []string{"PATH", "HOME", appDataDirEnv} {
		if value, found := os.LookupEnv(name); found {
			env = append(env, name+"="+value)
		}
	}

	return env
}

// run collects stdout and returns stderr inside the error, which is safe: bw
// writes diagnostics there, and no value this package sends travels in argv.
func run(cmd *exec.Cmd) (string, error) {
	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	stdout, err := cmd.Output()
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

		return nil, cairnerr.Newf(CodeFields, "item %q has no value for %s",
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
		return 0, cairnerr.New(CodeServe, "loopback listener reported no TCP port")
	}

	return addr.Port, nil
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

	// #nosec G204 -- argv is this package's own fixed subcommand and a port it
	// chose itself.
	cmd := exec.CommandContext(serveCtx, c.Binary,
		"serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port))

	cmd.Env = childEnv()
	cmd.Stdin = nil
	cmd.WaitDelay = waitDelay

	if startErr := cmd.Start(); startErr != nil {
		cancel()

		return nil, cairnerr.Wrapf(startErr, CodeServe, "start bw serve: %v", startErr)
	}

	logStep(c.Logger, "bitwarden serve started", "port", port)

	return &daemon{
		base:    "http://127.0.0.1:" + strconv.Itoa(port),
		cmd:     cmd,
		cancel:  cancel,
		client:  &http.Client{Timeout: c.timeout()},
		timeout: c.timeout(),
	}, nil
}

// daemon is one running bw serve process and the HTTP client that talks to it.
type daemon struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	client  *http.Client
	base    string
	timeout time.Duration
}

// close locks the vault and stops the daemon. It runs on every exit path,
// success or failure, so an interrupted pull never leaves an unlocked vault
// listening on loopback.
func (d *daemon) close(logger *slog.Logger) {
	lockCtx, cancelLock := context.WithTimeout(context.WithoutCancel(context.Background()), lockTimeout)
	defer cancelLock()

	if _, err := d.post(lockCtx, "/lock", nil); err != nil {
		logStep(logger, "bitwarden lock failed; the daemon is being killed anyway")
	}

	d.cancel()

	_ = d.cmd.Wait()
}

// waitReady polls until the daemon answers, which is the only signal bw gives
// that its HTTP listener is up.
func (d *daemon) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(readyTimeout)

	for time.Now().Before(deadline) {
		// Each probe gets its own short bound. freePort hands back a port it has
		// already released, so something else can be listening and simply never
		// answer; without this the client timeout (a minute by default) would
		// outlast the whole readiness budget this loop exists to enforce.
		if d.probe(ctx) == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return cairnerr.Wrapf(ctx.Err(), CodeReady, "canceled while waiting for bw serve: %v", ctx.Err())
		case <-time.After(readyInterval):
		}
	}

	return cairnerr.Newf(CodeReady, "bw serve did not answer within %s", readyTimeout)
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
		return nil, cairnerr.New(CodeList, "the item list did not answer with JSON")
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
		return nil, cairnerr.Newf(CodeItem, "no vault item is named %q", name)
	}

	if len(matches) > 1 {
		return nil, cairnerr.Newf(CodeItem, "%d vault items are named %q; rename all but one",
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
		return nil, fmt.Errorf("%s answered %w", path, statusError(resp.StatusCode))
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read the %s response: %w", path, err)
	}

	return raw, nil
}

// statusError carries an HTTP status as an error with no body attached.
type statusError int

func (s statusError) Error() string {
	return "HTTP " + strconv.Itoa(int(s))
}
