// Package cli implements the cairnos subcommands over injected streams
// (.agents/spec/11-deployment-and-config.md, "CLI surface"). The cmd/cairnos
// binary is a thin shim over App so tests drive the full command surface
// without a subprocess.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/environ"
)

// Process exit codes: success, a failed or canceled run (or an unhealthy
// store), and usage or validation problems.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2

	// ExitBehind reports a host whose service runs a build other than the one
	// an update would install. Only `cairnos update --check` returns it, and a
	// scheduled check elsewhere keys on this exact number.
	ExitBehind = 3
)

// usageText is the top-level help.
const usageText = `CairnOS workflow engine.

Usage: cairnos <command> [flags]

Commands:
  init      Scaffold the config file, data directory, and example workflows
  run       Run a workflow file once, locally
  validate  Parse and validate a workflow file without running it
  runs      Inspect persisted runs: list, show <run-id>
  memory    Manage memory entries: add, get <id>, rm <id>, search
  vault     Manage vault documents: put, get <id>, query
  rules     Language rules: set <lang> [<project>] <key> <value>, get <lang> [<project>], import <dir>
  secrets   Encrypted secret store: set <name>, get <name>, rm <name>, ls, rotate, keygen
  schedule  Manage scheduled workflows: add <file>, list, remove <id-or-name>, run
  webhook   Manage webhook triggers: add <file>, list, rotate <workflow>, remove <workflow>
  serve     Serve the gRPC and HTTP API with the scheduler daemon embedded
  chat      Talk to CairnOS: an interactive conversation over a running serve
  msg       Send chat messages as a configured identity: send, list
  keys      Manage API access keys: add, list, enable <id>, disable <id>
  workspace Manage workspaces and members: create <name>, list, add-member, members
  auth      Token revocation: revoke --jti X | --key K
  a2a       Agent-to-Agent protocol: card (print this node's signed agent card)
  mcp       MCP server: serve (run as a stdio MCP server for an external client)
  hive      Hive network (Phase 6): subscribe (pull tips), publish <id> (gated), status
  cost      Aggregate per-workflow usage and cost from run history
  suggestions  Tuning suggestions: scan, list, approve <id>, reject <id>, apply <id>, revert <workflow>
  pause     Pause a workflow: block new runs until resumed
  resume    Resume a paused workflow
  budget    Show a workflow's budget spend, limit, and pause state
  audit     List governance audit actions (pause, resume, terminate)
  prune     Delete runs and outputs past the retention window
  health    Exit 0 when the config loads and the store answers
  usage     Report what each model CLI login has spent of its rate-limit windows
  service   Run serve under the host supervisor: setup, status, uninstall
  model     Managed local LLM: setup, status, check, upgrade, uninstall
  update    Update this host: install the latest release or rebuild the
            checkout, pull secrets, restart on change; --check reports and
            exits 3 when behind

Use "cairnos <command> -h" for the command's flags.
`

// App carries the writers every subcommand prints through, so tests capture
// output instead of scraping the process streams. User-facing output goes to
// stdout; operational logging goes through logger to stderr. stdin is read only
// by the interactive chat REPL; it defaults to os.Stdin and tests set it
// through WithStdin.
type App struct {
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader
	logger *slog.Logger

	// interrupts feeds the chat REPL's interrupt watcher; nil means cmdChat
	// registers a real signal.Notify channel. Tests inject one through
	// WithInterrupts so no test ever sends a real SIGINT.
	interrupts <-chan os.Signal

	// bwPin resolves the pinned Bitwarden release for a platform. New sets it
	// to the generated pin; tests replace it through WithBitwardenPin, since
	// the fake bw a suite stages can never hash to a published release.
	bwPin func(goos string) (bwAssetPin, error)

	// unsupervised answers why nothing would start serve again after the update
	// loop ends it, empty when something would. Nil is the real check, which
	// asks systemctl; a suite hands over an answer so no case reads the host's
	// own service.
	unsupervised func(ctx context.Context) string

	// bwClock measures the wait for `bw serve` to answer. Nil is the machine's
	// clock, which is what a deploy waits on; a suite hands over one it drives
	// so a busy host cannot expire a budget the daemon met.
	bwClock bitwarden.Clock

	// serveFail announces a serve startup that never finished. runServe builds
	// it once it knows the config and the data directory; every other command
	// leaves it nil, so failures elsewhere stay on stderr.
	serveFail *serveFailNotifier

	// env answers the environment variables the commands read. New sets it to
	// the real process environment; tests hand over a fixed set through WithEnv
	// so two cases can disagree about a variable at the same time. A zero-valued
	// App still reads the process environment, which envLookup supplies.
	env environ.Lookup

	// bridgeTransport carries the chat bridges' HTTP calls. Nil is the default
	// transport every real serve uses. A serve test hands over one that answers
	// in the process, so no case resolves a bridge host, reaches the public
	// Telegram API, or leaves a dial running after serve returns.
	bridgeTransport http.RoundTripper

	// echoNoticed keeps the visible-input warning to one line per run. The
	// prompts ask three times and the answer is the same every time.
	echoNoticed bool
}

// New wires an App over the given streams, reading interactive input from
// os.Stdin until WithStdin replaces it.
func New(stdout, stderr io.Writer) *App {
	return &App{
		stdout: stdout,
		stderr: stderr,
		stdin:  os.Stdin,
		logger: slog.New(slog.NewTextHandler(stderr, nil)),
		bwPin:  bwAsset,
		env:    environ.OS(),
	}
}

// WithEnv sets the environment the commands read, so a test can stage variables
// without rewriting the process environment and can therefore run in parallel.
// It returns the App for chaining.
func (a *App) WithEnv(env environ.Lookup) *App {
	a.env = env

	return a
}

// WithBitwardenClock sets the clock the wait for `bw serve` measures its budget
// on, and returns the App for chaining. Only a test build calls it: production
// waits on the machine's clock, and a suite drives its own so a loaded host
// cannot fail a wait the daemon answered.
func (a *App) WithBitwardenClock(clock bitwarden.Clock) *App {
	a.bwClock = clock

	return a
}

// WithBridgeTransport sets the round tripper the Telegram and Mattermost
// bridges send through, and returns the App for chaining. Only a test build
// calls it: the hosts stay the configured ones, and the tripper decides where
// their requests actually go.
func (a *App) WithBridgeTransport(transport http.RoundTripper) *App {
	a.bridgeTransport = transport

	return a
}

// WithStdin sets the reader the interactive chat REPL reads lines from, so a
// caller can drive the conversation without the process stdin. It returns the
// app for chaining.
func (a *App) WithStdin(stdin io.Reader) *App {
	a.stdin = stdin

	return a
}

// WithInterrupts sets the channel the chat REPL takes interrupt signals
// from, so tests can fire interrupts without sending a real SIGINT. It
// returns the App for chaining.
func (a *App) WithInterrupts(interrupts <-chan os.Signal) *App {
	a.interrupts = interrupts

	return a
}

// Run dispatches one invocation and returns the process exit code. Codes
// travel up because only main may call os.Exit. Serve-client and governance
// commands split into dispatchServeClients and dispatchGovernance so no switch
// crosses the complexity ceiling.
func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", usageText)

		return ExitUsage
	}

	name, rest := args[0], args[1:]

	switch name {
	case "init":
		return a.cmdInit(rest)
	case subcmdRun:
		return a.cmdRun(ctx, rest)
	case "validate":
		return a.cmdValidate(rest)
	case "runs":
		return a.cmdRuns(ctx, rest)
	case "memory":
		return a.cmdMemory(ctx, rest)
	case "schedule":
		return a.cmdSchedule(ctx, rest)
	case subcmdServe:
		return a.cmdServe(ctx, rest)
	case "keys":
		return a.cmdKeys(ctx, rest)
	case "workspace":
		return a.cmdWorkspace(ctx, rest)
	case subcmdAuth:
		return a.cmdAuth(ctx, rest)
	case "a2a":
		return a.cmdA2A(rest)
	case "mcp":
		return a.cmdMCP(ctx, rest)
	case "hive":
		return a.cmdHive(ctx, rest)
	case "cost":
		return a.cmdCost(ctx, rest)
	case "suggestions":
		return a.cmdSuggestions(ctx, rest)
	case "prune":
		return a.cmdPrune(ctx, rest)
	case "health":
		return a.cmdHealth(ctx, rest)
	case "usage":
		return a.cmdUsage(ctx, rest)
	default:
		return a.dispatchServeClients(ctx, name, rest)
	}
}

// dispatchServeClients handles the commands that talk to a running serve over
// gRPC rather than opening the local store, then hands anything else on. They
// live here rather than in Run's switch because Run is at the complexity
// ceiling and these two are the family with the least to do with local state.
func (a *App) dispatchServeClients(ctx context.Context, name string, rest []string) int {
	switch name {
	case "chat":
		return a.cmdChat(ctx, rest)
	case "msg":
		return a.cmdMsg(ctx, rest)
	default:
		return a.dispatchGovernance(ctx, name, rest)
	}
}

// dispatchGovernance handles the governance and host-service commands and
// the help and unknown fallbacks, keeping Run's primary switch under the
// complexity ceiling.
func (a *App) dispatchGovernance(ctx context.Context, name string, rest []string) int {
	switch name {
	case "service":
		return a.cmdService(ctx, rest)
	case "model":
		return a.cmdModel(ctx, rest)
	case "update":
		return a.cmdUpdate(ctx, rest)
	case "webhook":
		return a.cmdWebhook(ctx, rest)
	case "vault":
		return a.cmdVault(ctx, rest)
	case "rules":
		return a.cmdRules(ctx, rest)
	case "secrets":
		return a.cmdSecrets(ctx, rest)
	case "pause":
		return a.cmdPause(ctx, rest)
	case "resume":
		return a.cmdResume(ctx, rest)
	case "budget":
		return a.cmdBudget(ctx, rest)
	case "audit":
		return a.cmdAudit(ctx, rest)
	case helpWord, helpShortFlag, helpLongFlag:
		a.printOutf("%s", usageText)

		return ExitOK
	default:
		a.printErrf("unknown command %q\n\n", name)
		a.printErrf("%s", usageText)

		return ExitUsage
	}
}

// envLookup answers with the App's environment, falling back to the process one
// so an App built without New behaves the way a direct os.Getenv would.
func (a *App) envLookup() environ.Lookup {
	if a.env == nil {
		return environ.OS()
	}

	return a.env
}

// printOutf writes user-facing output, logging a broken stdout instead of
// failing the command; the outcome is the exit code, not the print.
func (a *App) printOutf(format string, args ...any) {
	if _, err := fmt.Fprintf(a.stdout, format, args...); err != nil {
		a.logger.Warn("write stdout", "error", err)
	}
}

// printErrf writes diagnostics to stderr with the same tolerance as printOutf.
func (a *App) printErrf(format string, args ...any) {
	if _, err := fmt.Fprintf(a.stderr, format, args...); err != nil {
		a.logger.Warn("write stderr", "error", err)
	}
}

// printWorkflowErrors writes one line per canonical validation error so a
// file's problems are fixed in a single pass; other errors print as-is.
func (a *App) printWorkflowErrors(err error) {
	list, ok := errors.AsType[cairnerr.Errors](err)
	if !ok {
		a.printErrf("%v\n", err)

		return
	}

	for _, item := range list {
		a.printErrf("%s: %s\n", item.Code, item.Message)
	}
}
