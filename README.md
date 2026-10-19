# CairnOS

A self-hosted agent runtime in one static Go binary. Workflows are TOML files
describing a DAG of actions; the engine runs them with retries and circuit
breakers, under budget caps. On top of that sit a bounded agent loop for
self-planning tasks, a durable schedule queue, memory and a document vault,
chat with a confirmation gate, an MCP server and client, and a React web UI
for building workflows and watching runs. gRPC is the API core, with a
JSON/HTTP gateway generated from the same protos.

## Install

From a clone (also what `make build` wraps):

```sh
go install ./cmd/cairnos    # installs cairnos into $GOBIN (default ~/go/bin)
```

Or straight from the module path, no clone needed:

```sh
go install github.com/chadit/CairnOS/cmd/cairnos@latest
```

Either way you get the full binary: engine, CLI, serve, chat, MCP, web UI.
Check it landed with `cairnos help`.

## Initialize and run your first prompt

```sh
cairnos init      # writes a starter config under ~/.cairnos
cairnos validate examples/agents/quickstart/quickstart.toml
cairnos run examples/agents/quickstart/quickstart.toml
```

The quickstart workflow sends one prompt through a CLI tool you have already
logged into on this machine (it ships pointing at `codex`; swap the runner
line for `claude`, `omp`, or any other logged-in tool, see
[examples/agents/quickstart](./examples/agents/quickstart/README.md)). If the
run prints a reply, your install, runner wiring, and tool auth all work.
`cairnos init --dir <path>` picks a different home.

## Deploy it as a service, not an installed CLI

Don't `go install` the binary onto your PATH and call that a deployment. The
long-lived process is `cairnos serve`: it owns the scheduler and its work
queue, the chat backend, the MCP server, the web UI, and the metrics endpoint.
A bare CLI install gives you one-shot runs and nothing durable.

The same binary is also the client. Commands like `cairnos chat`,
`cairnos schedule`, and `cairnos runs` talk to a running serve, so keep the
binary where the service runs and use it from there (or over SSH).

### One command

```sh
./bin/cairnos service setup                  # user service (systemd or launchd)
sudo ./bin/cairnos service setup --system    # machine-wide (Linux, for now)
```

`setup` detects the supervisor, writes the unit, seeds a `0600` env file for
secrets, and starts the service. `--dry-run` prints exactly what it would do
first. `cairnos service status` asks the supervisor how the service is doing,
and `cairnos service uninstall` removes the unit while keeping config, data,
and the env file.

User mode is the default and covers everything an agent normally does. Reach
for `--system` only when workflows must control the machine itself (packages,
other services, other users' files); it creates a dedicated `cairnos` account
and keeps root as the installer, never the runtime. On Linux, user services
keep running after logout once linger is on; setup tries to enable it and
prints the one sudo command when it can't. Supported today: systemd (user or
system) and launchd (user). runit and Windows still need the manual path.

Prefer to write the unit yourself? The equivalents:

### systemd

```ini
[Unit]
Description=CairnOS runtime
After=network-online.target
Wants=network-online.target

[Service]
User=cairnos
ExecStart=/opt/cairnos/cairnos serve --dir /var/lib/cairnos
Restart=on-failure
# Secrets (API keys, webhook URLs) come from the environment, never the
# config file.
EnvironmentFile=/etc/cairnos/env

[Install]
WantedBy=multi-user.target
```

### Container

```sh
docker build -t cairnos .
docker run -d --name cairnos \
  -v cairnos-data:/data \
  -p 7777:7777 -p 8080:8080 -p 9090:9090 \
  cairnos
```

The image's default command is `serve --dir /data`. Ports: gRPC on 7777, the
JSON/HTTP gateway on 8080, Prometheus metrics on 9090. Keep the ports on
localhost or a private network (Tailscale works well) and turn auth on
(`[auth] enabled = true`) before anything else can reach them.

## Keeping a host current

A host that runs CairnOS as a service and does no development has two moving
parts, and they move for different reasons.

**Workflows move on their own.** A `[source.<name>]` checkout is fetched and
reset by whatever schedule you gave the `source` runner, so a workflow file or a
script living in that repository reaches the host without anyone logging in.
Push it, wait for the next sync, and it is there.

**CairnOS does not.** The binary the service runs is the one someone built from
a clone on that host. It changes when someone runs `cairnos update` or deploys
the clone, or on its own once [`[update] auto`](#letting-serve-update-itself) is
on.

### Update CairnOS before the workflow that needs it

That split comes with one rule. When a change in a synced source needs a newer
CairnOS, update the host first and let the source change land after.

Say a workflow starts using the six-field cron with a seconds field:

```toml
[workflow]
name     = "heartbeat"
schedule = "*/30 * * * * *"
```

A CairnOS built before that field existed reads the file and rejects it. The
stored schedule keeps firing on the cadence it was added with, the daemon loads
the file again at every fire, and every fire fails at the load:

```text
level=ERROR msg="scheduled run failed" schedule=sch_01J... workflow=heartbeat
  error="load /srv/team-workflows/workflows/heartbeat.toml: 1 error(s): workflow.invalid:
  schedule \"*/30 * * * * *\" must be a 5-field cron expression or an @-shorthand"
```

Nothing in the repository looks wrong, because nothing in it is wrong. The file
is fine and the binary reading it is old, and it stays that way until the binary
changes. The recovery is the deploy that should have come first:

```sh
cairnos update                # sync the clone, rebuild, pull secrets, restart on change
journalctl --user -u cairnos -n 50
```

The next fire loads the same file against the new binary and runs it. No run was
queued while it was failing, so there is no backlog to drain and nothing to
replay by hand.

`cairnos update` does the whole sequence in the order that matters, and it has
two ways to get a new binary. Which one it takes depends on where the running
binary lives.

**A binary built from a clone follows that clone.** The binary lives at
`<clone>/bin/cairnos`, so the clone is one directory up. The update fetches the
clone's remote, the one its branch tracks or `origin`, and puts the clone on
that remote's default branch as `git clone` recorded it in
`refs/remotes/<remote>/HEAD`. Then it rebuilds. A clean clone already on that
branch is fast-forwarded, which is the normal case.

Anything else is reset onto the default branch. Uncommitted changes and
untracked files git doesn't ignore are thrown away, and so are local commits. A
detached HEAD or another checked-out branch goes back to the default branch,
whose local copy is recreated at the fetched tip. One WARN line names the reason,
every discarded path as a `git status --short` line, and every dropped commit by
short sha and subject, so the journal shows what went. Ignored files stay, `bin/`
and the running binary included. Treat the clone as the host's copy of upstream
rather than somewhere to work: `scripts/deploy.sh` and `make deploy` pass
`--no-fetch`, which leaves the clone alone. A remote with no recorded default
branch falls back to the branch's own upstream, and a clone that tracks nothing
is built as it is.

A published release is never consulted for a clone, so a host tracking main is
never moved back onto an older tag.

**A binary with no clone around it installs the latest GitHub release.** The
latest release is asked for the asset matching this host's `GOOS` and `GOARCH`,
that file and the release's `checksums.txt` are downloaded to a temporary
directory, and the sha256 is checked before anything else touches the file.
Then the binary is written over the running one and the rest of the sequence
carries on from there. A release has to carry `cairnos_<goos>_<goarch>` (with
`.exe` on Windows) for your platform plus `checksums.txt` in `sha256sum`
format. No binary for your platform means there is nothing to install, and the
update says so. A binary with no `checksums.txt` beside it is a broken release,
and the update stops there.

Two things have to agree before a release binary is installed. The tag is
resolved to the commit it points at, following an annotated tag through its own
object, and the asset's embedded `vcs.revision` has to be exactly that commit.
An asset with no stamp, a `+dirty` stamp, or a different commit is refused, so a
release built from a scratch tree or a working copy with edits in it never lands
on a host. When the commit the service already runs is the release's commit,
nothing is downloaded and the secrets pull still runs, because a vault value can
move without the code moving.

Set `GITHUB_TOKEN` or `GH_TOKEN` in the environment to authenticate the lookup,
which lifts GitHub's unauthenticated API rate limit of 60 requests an hour per
IP address. `--source <dir>` names a clone to build instead.

Either way, the new build runs its own `secrets pull` and only then does the
service restart. Secrets before the restart is the part worth keeping straight:
the service reads both environment files once, at startup, so a pull after the
restart would leave the new process on the old values until the next one.

**The restart lets running jobs finish.** Instead of stopping the service under
whatever it was doing, the update asks it to drain: the scheduler stops firing,
the runs already going carry on to their end, and only then does the process
exit and systemd start it on the new binary. A webhook that lands mid-drain
still runs, and the update waits for it. The wait is bounded at 15 minutes, so
one stuck run cannot hold a deploy open; past that the process exits with the
run still going, the same as a plain restart would. `--restart-now` skips the
drain and restarts immediately, which is what you want when the running build
is the thing that is broken. An older service that predates the drain endpoint
is restarted the old way, and the update says so.

Three flags steer that choice. `--source <dir>` builds a named clone, which is
how a binary with no clone around it builds one. `--no-fetch` builds what is
already checked out without fetching or asking for a release. `--no-release`
skips the release lookup a binary outside a clone would make, which leaves
`--source` to name what to build. `scripts/deploy.sh` and `make deploy` pass
`--no-fetch`, so deploying a tree you are working in builds exactly that tree.

On the way out it points `~/.local/bin/cairnos` at the build it just made, so a
login shell runs the same binary the service does. It replaces a symlink aimed
somewhere else, refuses to touch a regular file sitting in that spot, and warns
when `~/.local/bin` is not on your PATH. `--no-link` skips the step.

### Letting serve update itself

`cairnos update` runs when someone runs it. To have the service keep itself
current instead, turn on the `[update]` block in `config.toml`:

```toml
[update]
auto     = true    # default false
interval = "15m"   # default 15m, how often serve checks
```

Every interval, serve works out the wanted revision the same way
`cairnos update` does. For a binary built from a clone that's the commit the
remote's default branch sits on, read after a `git fetch` that moves only
remote-tracking refs. For a binary with no clone around it, it's the
commit the latest release's tag points at. When it matches the revision stamped into the running
binary, nothing happens. When it doesn't, serve logs one line naming both
revisions and runs the sequence the command runs: install the release, or
fast-forward or reset the clone and `make build`, then `secrets pull` and the link. It
checks the new binary carries the wanted stamp, then drains itself the way
`cairnos update` drains it. In-flight runs finish, bounded at 15 minutes, and
the process exits for systemd to start the new binary.

A few things differ from running the command by hand:

- The loop only starts when serve is the main process of the systemd service
  `cairnos service setup` installs, whose `Restart=always` brings a cleanly
  exited serve back. The launchd agent restarts a crash only, and a serve run by
  hand has nothing behind it, so serve logs one warning at startup and leaves
  the loop off.
- A clone with uncommitted changes or local commits is reset onto the upstream
  before the build, the same way `cairnos update` resets it, and the WARN line
  lands in the journal. The loop only acts when the wanted revision differs from
  the running one, so a clone dirtied under a current build waits for the next
  upstream commit. A clone that tracks no upstream wants its own HEAD, as it
  does for `cairnos update`. With uncommitted changes it isn't built, because
  there's nothing to reset it onto and a `+dirty` build would look stale again
  right after the restart.
- The command and the loop take the same lock, `update.lock` in the CairnOS
  home, around an install. A `cairnos update` started while the loop is
  installing stops at once with "another update is already running on this
  host", and the loop waits for the next interval while the command holds it.
- A failed step (a dirty clone with no upstream, a failed reset, a build
  failure, a bad checksum, a failed secrets pull) is logged at ERROR with the
  step and the wanted revision, and the running build keeps serving. That revision isn't tried
  again until the wanted revision moves, so a broken build costs one attempt
  rather than one per interval.
- The revision is written to `auto-update-attempt` in the CairnOS home before
  each attempt starts and cleared once it succeeds, so a serve that dies
  mid-build (the host running out of memory, say) doesn't build it again once
  systemd brings it back. To retry sooner, fix what broke and run
  `cairnos update`, or delete that file and restart the service.
- The source follows the same rule as `cairnos update`: a binary in a clone
  follows that clone's upstream, and only a binary with no clone around it
  follows releases.
- The first check waits one interval after serve starts. `CAIRNOS_UPDATE_AUTO`
  and `CAIRNOS_UPDATE_INTERVAL` override the two keys from the environment.

### Telling whether a host is behind

`cairnos update --check` compares the build the service is running against what
an update would put there: what the clone would build once the update
fast-forwards or resets it, and the commit the latest release's tag points at for a
binary with no clone around it. It reads the revision Go stamps into every binary,
through `/proc`, so a file replaced on disk is not mistaken for the process in
memory:

```sh
cairnos update --check
running: 7929e3e8a1...
wanted:  625fb2e4c9...
```

It exits 3 when the two differ and 0 when they match. A clone the update would
reset also exits 3, even when the revisions match, and a third line says why:

```sh
running: 625fb2e4c9...
wanted:  625fb2e4c9...
reset:   2 changed or untracked path(s); 1 commit(s) not on origin/main
```

On the clone path it
fetches the tracked remote first, which moves only remote-tracking refs, so a
clone nobody has fetched in days still reports the commits waiting upstream. `--no-fetch`
skips the fetch and compares against the local HEAD. Either way the check
changes nothing on the host: the clone stays where it is and there is no build
or restart, so it is safe on a host you only want to look at. A full
`cairnos update` prints the same two revisions before it acts on them. There is
no `cairnos version` subcommand: that stamped revision is how a running build is
identified.

The check reads a systemd user service. Under launchd, or for a `--system`
unit, read the stamp out of the deployed file yourself and compare it to the
clone:

```sh
go version -m /opt/cairnos/cairnos | grep vcs.revision
git -C ~/src/cairnos rev-parse HEAD
```

### After you change a workflow's schedule

`cairnos schedule add` copies the `schedule` key into the store, so editing the
file afterwards leaves the stored expression where it was. Replace the entry:

```sh
cairnos schedule list
cairnos schedule remove heartbeat     # the id from the ID column, or the workflow name
cairnos schedule add /srv/team-workflows/workflows/heartbeat.toml
cairnos schedule list                 # the CRON column now reads the new expression
```

The `schedule` key is the only one this applies to. Everything else in the file
is read fresh at each fire.

### What a restart picks up

`config.toml` and the environment files are read once, at startup, so a change to
either needs a restart before the service sees it. Workflow files are read at
every fire, so an edited action, runner or timeout is live on the next one. A new
binary needs the restart that `cairnos update` already does.

## Secrets from Bitwarden

Every bot token and API key a host needs is otherwise hand-typed into its env
file. If you already keep them as custom fields on one Bitwarden item, CairnOS
can render that item into a second environment file at deployment time.

Name a custom field after the variable it fills and there is nothing to
configure. Every field matching `CAIRNOS_[A-Z0-9_]+` is written under its own
name, so an empty block is a working one:

```toml
[secrets.bitwarden]
```

Fields with names of your own need a line each. A mapping wins when both it and
an inferred field would set the same variable:

```toml
[secrets.bitwarden]
item = "chezmoi"

[secrets.bitwarden.fields]
CAIRNOS_BRIDGE_MATTERMOST_TOKEN = "mattermost bot token"
CAIRNOS_RUNNER_ZAI_API_KEY      = "z.ai key"
```

A field named after a Mattermost bot's username is written as that identity's
token, under the `CAIRNOS_BRIDGE_MATTERMOST_<ID>_TOKEN` name the identity reads
first. One vault item can then hold every bot's token under the bot's own name,
with no mapping per bot. A `CAIRNOS_`-named field or a `[secrets.bitwarden.fields]`
mapping for the same variable still wins, and a username field left empty counts
as absent.

`item` is optional here because the credentials file carries it as `BW_ITEM`;
set it in the config when you want one host reading a different item, and the
config value wins. `credentials_file` and `env_file` default to `bitwarden.env`
and `secrets.env` under the CairnOS home, and both take an absolute path
instead.

Create the credentials file with:

```sh
cairnos secrets auth
```

It asks for the item name and the vault's API key and master password, and
writes them at 0600. If your workstation runs chezmoi against the same vault,
its `[data.secret]` values are offered as the defaults, so four Enters are the
whole answer. Anything already in the process environment is taken from there
without asking, which is what a host that injects one of these elsewhere should
do. The three secrets are read with the terminal's echo turned off, and a
terminal that will not allow that gets one line saying so before the first
prompt. Run it again later and it checks instead of writing: it opens the
vault, reports the item and how many fields a pull would take off it, and locks up
again. A host running the system-mode unit keeps its config under
`/var/lib/cairnos` and both environment files under `/etc/cairnos`, so both
commands need to be told:

```sh
sudo cairnos secrets auth --system
sudo cairnos secrets pull --system
```

`--system` reads the system install's config on its own, so there is no `--dir`
to remember.

Writing the file by hand does the same job:

```sh
install -m 600 /dev/null ~/.cairnos/bitwarden.env
cat >> ~/.cairnos/bitwarden.env <<'EOF'
BW_ITEM=<the item name>
BW_CLIENTID=<client id from the vault API key>
BW_CLIENTSECRET=<client secret>
BW_PASSWORD=<your master password>
EOF
```

CairnOS refuses a credentials file that anyone but its owner can read. Nothing
you put there ever reaches a command line: the API key goes to the login child's
environment and the master password travels in a request body to a loopback
daemon, because arguments are readable from `/proc`.

You need the standalone `bw` CLI, not the npm package, since the service user has
no node toolchain. Every `cairnos secrets auth` and `cairnos secrets pull` looks
for one in this order:

1. The `bw` on the host's PATH, when `bw --version` reports a version that is
   not behind the release CairnOS pins. That binary is yours. CairnOS runs it
   and never moves, replaces or rewrites it.
2. The copy CairnOS keeps for itself, when there is no `bw` on PATH, when the
   host's is older than the pin, when it prints a version nothing can compare
   against (a suffix like `2026.8.0-beta`, or no answer at all), or when a call
   through it fails in a way that means the binary cannot do the job.

That copy lives at `~/.cairnos/tools/bw/<version>/bw`, in a `0700` directory
CairnOS owns. It never goes on PATH, nothing links to it from a PATH directory,
and it is called by its full path. CairnOS downloads it the first time it needs
it, checks the archive and the binary inside it against two digests compiled
into the build, and reuses that file afterwards. A file there that does not hash
to the pin is downloaded again rather than run.

Versions compare number by number across the dots, so `2026.9.0` is newer than
`2026.8.0`, and `2025.12.1` is older than both. A version carrying anything but
digits and dots counts as unreadable, which sends the call to the pinned copy.

Every run says which binary it used and why:

```text
level=INFO msg="using the Bitwarden CLI" path=/usr/bin/bw version=2026.9.1 reason="the host CLI is at or past the pinned version"
level=INFO msg="using the Bitwarden CLI" path=/home/ops/.cairnos/tools/bw/2026.8.0/bw version=2026.8.0 reason="the host CLI is older than the pinned version"
```

`cairnos secrets pull --dry-run` resolves the same way and prints the same line,
so you can see which binary a deploy will use before it writes anything.

A failure tells the two cases apart. A CLI that cannot start, cannot reach the
server, or never answers on its own loopback port is the binary's problem, so
the call is tried again through the pinned copy. A rejected API key or master
password is the vault's answer and would read the same from any build, so it
fails once and stops.

If you want a `bw` of your own on PATH, CairnOS installs the pinned release
there for you:

```sh
cairnos secrets install-bw
```

It downloads the pinned release, checks its sha256 against a digest compiled
into CairnOS, unpacks the one file the archive holds, checks that file against a
second compiled-in digest, and installs it at `~/.local/bin/bw`. A download that
does not match is discarded and nothing is written. `--bin-dir` installs
somewhere else, and `--force` replaces a file already sitting at that path.

Running it again is cheap: it hashes the installed file and skips the download
when that hash is the pinned one. It never asks the binary what version it is to
decide, because a file that prints the right version is no evidence of what it
does with your master password. It records the digest it wrote beside the
binary, which is how a later pin bump tells its own previous install from a `bw`
that arrived some other way.

Nothing requires this command. The vault commands find their own `bw` either
way, and one you install here is a host binary like any other, subject to the
order above.

Then pull:

```sh
cairnos secrets pull --dry-run   # resolve everything, report, write nothing
cairnos secrets pull             # write the file
```

Both print the variable names and the path, never a value, so the output is safe
in a deploy log. A mapped field that is missing fails the whole run and writes
nothing, so the service never starts against a half-filled file. An item with no
`CAIRNOS_` field and no mapping table fails the same way, naming the item it
read. With no `[secrets.bitwarden]` block the command says so and exits 0, which
is why `cairnos update` can run it on every host, on every run.

The pull also warns about the credentials your config implies that the item does
not carry: `CAIRNOS_BRIDGE_MATTERMOST_TOKEN` when an identity has no token by any
of the names it reads, and `CAIRNOS_RUNNER_<NAME>_API_KEY` for each openai or
anthropic runner whose `base_url` points off your own network. It is a warning
and not a failure, because the value may be in the hand-written env file beside
the generated one.

`cairnos service setup` points the unit at both files:

```ini
EnvironmentFile=-/home/you/.cairnos/env
EnvironmentFile=-/home/you/.cairnos/secrets.env
```

The first stays hand-written and yours. The second is generated whole and
replaced on every pull, so never edit it. The generated file comes second, so it
wins when both set the same variable. On macOS the launchd preamble sources both
in the same order. `cairnos update` pulls on every run, before it decides
whether to restart, and stops there if the pull fails. It restarts when the
build moved or when the pull changed `secrets.env`, so a rotated or renamed
vault field reaches the service without waiting for a code change. When neither
moved it leaves the running service alone.

Warning: this host now holds Bitwarden API credentials and your master password
on disk at 0600, which is the same trust you already place in a workstation
running chezmoi. Anyone who can read files as the service user can open the
vault. Use an API key scoped to this purpose, and treat losing the host as losing
the vault.

## Working from a clone

```sh
make build           # builds bin/cairnos without touching $GOBIN
./bin/cairnos validate examples/agents/release_monitor/release_monitor.toml
./bin/cairnos run <workflow.toml>   # one-shot run, no service needed
```

One-shot `cairnos run` is for trying a workflow out; anything scheduled or
conversational needs serve.

`make build` regenerates the gRPC and gateway code under `internal/api/gen`
first. `buf` and its plugins are Go tool directives in `go.mod`, so a clone
needs nothing beyond the Go toolchain; the first run downloads them.

### Upgrading dependencies

```sh
make update-deps
```

It moves the Go modules, the `go.mod` tool directives, the Go toolchain line,
the buf registry dependency, the npm packages under `web/`, the major of every
action the GitHub workflows use, the postgres image the integration tests
start, and the Bitwarden CLI pin, then regenerates the protobuf code. It leaves
`gosec` alone: `make lint` installs it at `@latest` on every run, the same as
`golangci-lint`, so neither linter has a version to move. It writes files and
stops there: no commit, no
push, no tests. At the end it prints `git status --short`. The builder `FROM`
line in the Dockerfile and the Node version CI installs float on purpose, so
they need no bump. The runtime `FROM` floats only within its Debian major: the
`static-debian12` tag pins that major, so moving to the next Debian release
means editing the tag by hand.

Run `make test` and `make lint` before committing any of it. `make lint`
installs both linters into `bin/tools` instead of using whatever is on PATH.
Neither is pinned: every run installs `golangci-lint` and `gosec` at `@latest`,
and CI does the same, so there is no linter version to keep current.

The Bitwarden CLI pin is the slow part of a bump, and it runs on its own too:

```sh
go run ./tools/bwpin
```

It lists the `bitwarden/clients` releases, takes the highest `cli-v` tag, and
says so and changes nothing when that is already what is pinned. Otherwise it
downloads both platform archives, hashes each archive and the single `bw`
inside it, and rewrites `internal/cli/bwpin_gen.go`. Upstream publishes no
checksum file for these assets, so the download is the only place those four
digests can come from, which is around 90 MB a bump.

## Model setup

There are two kinds of model runner. Pick per action.

**API runners** call a hosted API directly with a key:

```toml
[runner.anthropic]
model = "claude-sonnet-5"

[runner.openai]
model = "gpt-4.1"

[runner.zai]
type     = "openai"
model    = "glm-4.6"
base_url = "https://api.z.ai/api/paas/v4"
```

A block with a name of its own reads `CAIRNOS_RUNNER_<NAME>_API_KEY`, so the
block above reads `CAIRNOS_RUNNER_ZAI_API_KEY` and each vendor keeps its own
key. `CAIRNOS_RUNNER_OPENAI_API_KEY` and `CAIRNOS_RUNNER_ANTHROPIC_API_KEY`
stay as the fallback for a block that has no key of its own. Keys ride the
environment, never the config file, and a run that finds neither name set stops
and reports both.

`base_url` on a `type = "openai"` block points it at any endpoint that speaks
the OpenAI chat-completions API, which is how a hosted subscription with a
compatible API is reached.

**CLI runners** wrap a tool that carries its own subscription login, so no API
key touches CairnOS at all. Built-ins: `claude`, `codex`, `qwen`, `omp`,
`antigravity`. The first four launch their tools' headless one-shot modes out of
the box (`codex exec`, `claude -p`, `qwen -p`, `omp -p`), so a workflow action
never opens an interactive UI. `antigravity` has no default template and takes
the prompt as a bare positional argument, so give it `args` (shown below) when
your build needs a flag for one-shot mode. Say you have a Codex subscription:
log in once on the host that runs serve (`codex login`), then use it from a
workflow:

```toml
[action.plan]
runner = "codex"
params = { prompt = "Draft the migration plan for ..." }
```

Each CLI runner finds its binary on PATH by default; override the path or argv
when yours differs:

```toml
[runner.codex]
binary = "/usr/local/bin/codex"
# args = ["exec", "{prompt}"]   # the default; override to add flags
```

A wrapper script around one of these tools, such as one that pins a model or
picks a login before it runs the real binary, becomes a runner of its own from
config alone. Set `type` to the built-in whose argv template the script
accepts and point `binary` at the script. The block's name is the runner name a
workflow uses:

```toml
[runner.claude_sonnet]
type   = "claude"                        # start from claude's -p {prompt} template
binary = "/usr/local/bin/claude-sonnet"  # your script: adds --model sonnet, then runs claude
```

For a tool unrelated to the built-ins, pick any of them as the `type` and
replace the template with `args` or `stdin = true`:

```toml
[runner.summarize]
# Any built-in works as the type; args replaces its -p {prompt} template.
type                = "omp"
binary              = "/usr/local/bin/summarize"
# {prompt} becomes the action's prompt param.
args                = ["--format", "markdown", "{prompt}"]
# The default: refuse a prompt that starts with a dash.
allow_dashed_prompt = false
```

A prompt that starts with a dash would reach the binary as a flag, so it is
refused unless `"{prompt}"` follows a `"--"` in `args`, or the block sets
`allow_dashed_prompt = true` for a binary that reads that argument as text.
With `stdin = true` the prompt goes to the binary's standard input instead of
its argv, the dash check no longer applies, and a block with no `args` starts
the binary with no arguments.

A declared runner gets what the built-ins get: its child inherits serve's whole
environment with the `CAIRNOS_HOSTS` and `CAIRNOS_HOST_*` variables added, a
rate-limit refusal retries, and an expired login holds the job. The hold resumes
by itself only for `claude`, `codex`, and `qwen`, so a job held on any other
runner waits for you to resume it.

Warning: that environment carries every secret serve runs with, such as the
runner API keys and, under the installed service, everything in `secrets.env`,
so point `binary` only at a script you trust.

A block can't take a different built-in's name: `[runner.codex]` with
`type = "omp"` stops at startup. A block with no `type` under a name no built-in
owns stops startup too, with an error that names the block and the keys to add:
`type = "<built-in>"`, plus `binary` when the block sets none. A stale or
misspelled block is caught the first time CairnOS starts, not the first time a
workflow uses it.

When a CLI tool's login expires mid-schedule, the job is held rather than
failed. You get a webhook alert on Discord (`CAIRNOS_NOTIFY_DISCORD_WEBHOOK`),
and once you re-authenticate the tool on the host, the queue probes the fresh
credential and resumes the job on its own for the recognized tools.

Chain models with fallback so one expired or rate-limited provider doesn't
stop a workflow:

```toml
[engine.fallback]
codex = ["qwen", "anthropic"]
```

### Local model

`cairnos model setup` gives a node its own model. It downloads a pinned
llama.cpp `llama-server` build and a pinned Gemma 4 E4B GGUF, then puts the
server under the same supervisor that runs `cairnos serve`. It also writes the
config block that points a runner at it.

```sh
cairnos model setup            # about 5 GB of download, then a supervised server
cairnos model status           # installed versus pinned, plus a liveness probe
cairnos model check --json     # have the pins fallen behind upstream?
cairnos model upgrade          # re-download, restart, roll back if it won't start
cairnos model uninstall        # stop the service; --purge deletes the download too
```

Defaults: loopback port 8089, a 32K context, and two fewer threads than the
machine has cores so the model server doesn't starve the engine beside it. Each
is a flag (`--port`, `--ctx-size`, `--threads`, `--model`, `--dir`). `--dry-run`
prints the plan and writes nothing. `--no-service` installs the files without
registering a service, and `--offline` refuses to touch the network.

CPU is fine here. There's no GPU or driver stack to set up. Budget about 5 GB of
disk for the weights and 7-9 GB of memory while the server holds a 32K context.
On a laptop expect a few tokens per second, which suits background workflows
better than an interactive chat.

Setup appends an ordinary openai runner block pointed at loopback:

```toml
[runner.local]
type = "openai"
model = "gemma-4-e4b"
base_url = "http://127.0.0.1:8089/v1"
```

Reach it from a workflow with `runner = "local"`. No API key is involved: the
openai runner skips its key check when `base_url` names loopback, a private
network, or a tailnet address, since a server on your own side of the network
is the one case where demanding a provider credential buys nothing. An edited
block survives a re-run, so change the port or the model and setup will leave
your version alone.

One server can feed more than CairnOS. Anything that speaks chat-completions
takes the same endpoint, so another local tool can point at
`http://127.0.0.1:8089/v1` and share the loaded weights instead of starting a
second copy of a 5 GB model.

The supervised server needs systemd (user) on Linux or launchd on macOS.
Everywhere else, run `cairnos model setup --no-service`: it installs the
runtime and the weights and prints the llama-server command to start by hand.

### Host commands

The `exec` runner launches a plain host process on a schedule. It is for the
script that has to run where its logins already live, such as a reconciler
that shells out to a model CLI or to `gh`. There is no container and no
credential handling: the command inherits the environment of the user running
serve.

```toml
[runner.exec]
allow_actions = ["reconcile"]
timeout       = 540
# env = { REPORT_PROJECT = "billing-api" }
```

```toml
[action.reconcile]
runner = "exec"
params = { command = "/path/to/reconciler.py", workdir = "/path/to/scripts" }
```

`stdin` hands a string to the command's standard input, which is how a webhook
receiver gets its payload without it showing up in a process listing:

```toml
[action.approval]
runner = "exec"
params = { command = "/path/to/receiver.sh", stdin = "${{ inputs.payload }}" }
```

A long prompt reads better as its own file. `stdin_file` names one, and the
loader swaps it for `stdin` holding the file's text, so `${{ }}` references
inside the file resolve the same way they do in an inline string:

```toml
[action.summarize]
runner     = "exec"
depends_on = ["fetch"]
params     = { command = "/path/to/llm.sh", stdin_file = "prompt.md" }
```

```markdown
Summarize the report below in five bullet points.

## INPUT

${{ fetch.output }}
```

The path is relative to the workflow file's directory and has to stay inside
it: absolute paths, `..`, and symlinks that lead out are refused. A symlink
with an absolute target is refused even when it points inside the directory,
so link with a relative target. The file must be UTF-8 text of at most 256 KiB,
and it is read whenever the workflow loads, so a missing file fails
`cairnos validate` with the workflow and action named. Set `stdin` or
`stdin_file`, not both. Only the `exec` runner reads stdin, so `stdin_file` on
any other runner, `sandbox_exec` included, is refused rather than dropped.

The file carries the same trust as the workflow file, so keep it under the
same review. Editing it changes the workflow's hash, which `cairnos runs replay`
checks, the same as editing the workflow does. `cairnos suggestions revert`
restores only the workflow's TOML, not an included file, so after a prompt edit
it warns that the restored workflow hashes differently from the saved version.

A definition sent through the API resolves the path against the workflows
directory. Validating one needs only `read:workflow`, and its errors say
whether a file there exists, its kind and size, and whether it is UTF-8, and
can quote `${{ }}` fragments from it. Keep only workflows and their prompts in
the workflows directory for that reason.

`allow_actions` denies by default. An action name absent from the list is
refused at validate time, and a block that is missing or lists nothing refuses
every action.

Add a name with `cairnos config allow-action add`, and see the list with
`cairnos config allow-action list`:

```sh
cairnos config allow-action add summarize   # --dir D for a home other than ~/.cairnos
cairnos config allow-action list
```

The command edits only the array's text, so comments and layout in
`config.toml` stay as they were, and it checks the edit by decoding the file
before and after. A name already listed is reported and nothing changes. It
refuses, leaving the file alone, when the file does not parse, the name is not
a valid action name, the array is written in a shape it does not edit (such as a
closing bracket on the same line as a value), or there is no `[runner.exec]`
block. That last case is left to you because a new block also needs a
`timeout`. serve reads `config.toml` only at startup, so restart it after an
add.

Warning: a command listed here runs on the host as the serve user, with that
user's full environment and no isolation.

### Rate-limit windows

The `usage` runner reports how much of each rate-limit window a model CLI login
has spent: the 5-hour and weekly windows for Claude Code, and whatever windows
the Codex plan carries. Point it at the launchers you already use, here or on
another box, and schedule it like anything else.

```toml
[runner.usage]
timeout    = 60
claude_gap = 65
claude     = ["claude-agent"]
codex      = ["codex-agent"]
```

Both lists default to the bare tool name (`claude`, `codex`), so a host that
never wrote the block still reports whatever is on its `PATH`. An entry is
either a bare name, an absolute path, or a `~/` path. Anything a shell would act
on, a space included, is refused at config load, because a remote entry is
placed into a command the far side runs. `timeout` is seconds per launcher and
defaults to 60.

`claude_gap` is the wait between two Claude logins on the same machine, in
seconds, and defaults to 65. Claude Code caches its usage answer in the shared
config directory for about a minute and does not key that cache by login, so
launchers that swap only the token and share `~/.claude` hand back the previous
login's figures when probed inside that window. The gap outlasts the cache.
Probing three Claude logins therefore takes two gaps of wall clock, a little
over two minutes; probing one takes none, and Codex never waits. If your
launchers give each login a config directory of its own, set it to `0`.

```toml
[action.windows]
runner = "usage"
params = { host = "build_box", launchers = ["claude-agent"] }
```

`host` names a `[host.<name>]` block and needs its `ssh` alias; leave it out to
probe this machine. `launchers` narrows the run to some of the configured ones,
and a name the block never listed fails at workflow load. When the remote box's
non-login `PATH` has no CLI on it, set `wrapper` on the host block and the
launcher runs through that command.

Output is a report per login, with the plan, every window's used percentage, and
its reset time. A launcher that fails records its reason and the rest still
report; only a run where nothing answered fails the action. Two more fields save
a downstream action from walking the list. `summary` is the table below as
Markdown, wrapped in a code fence so a chat client keeps the columns lined up.
`has_errors` is `true` when any login failed, which is what a condition reads:

```toml
[action.alert]
runner     = "message"
depends_on = ["windows"]
condition  = "${{ windows.output.has_errors }} == true"
params     = { identity = "ops", to = "#usage", text = "${{ windows.output.summary }}" }
```

`cairnos usage` runs the same probe by hand and prints that table, with reset
times in UTC. `--host` and `--launchers a,b` mean what the params do, `--json`
prints the whole report instead, and `--dir` picks the CairnOS home:

```text
$ cairnos usage --host build_box
LOGIN         PLAN     WINDOW  USED  RESETS
claude-agent  max      5h      2%    2026-09-22 18:50 UTC
                       7d      23%   2026-09-24 12:00 UTC
codex-agent   prolite  7d      24%   2026-09-26 10:01 UTC
```

A login that failed prints its error across the window columns. When the Claude
gap applies, the command says how long it will wait before it starts. It exits
non-zero only when no login reported at all.

This runner speaks each CLI's own stdio protocol, the one those tools expose to
their SDKs, through your launcher. It never opens a credential file, never sets
`CLAUDE_CONFIG_DIR` or `CODEX_HOME`, and never calls the vendors' HTTP endpoints
itself. Whichever login your launcher picks is the login it reports on.

#### Reading a report file instead

A host that already tracks its logins with a script of its own can have that
script write a report file, and the runner reads the file instead of probing
each login. Set `script`, `output`, and `interval` in place of the launcher
lists:

```toml
[runner.usage]
script   = ["/usr/local/bin/usage-report", "--out", "{output}"]
output   = "~/.cache/cairnos/usage.json"
interval = 900
```

`serve` runs `script` when it starts and again every `interval` seconds, and the
script writes `output`. An element that is exactly `{output}` stands for the
`output` path, so the path is written once. `timeout` bounds each run and
defaults to 300 seconds here, since the script reads every account in turn
before it writes anything. A request reads the file and never runs the script,
so `cairnos usage` and the chat tool answer at once, even while serve is down.

Set `host` to a `[host.<name>]` block to run the script there over ssh, through
that host's `wrapper` the way a launcher runs. The file stays on that host, and a
request reads it over the same hop. A request's own `host` param then has to name
that block or be left out.

Each account carries the time it was read, so a script that keeps the entries a
run didn't read can hold fresh codex numbers beside old claude ones in one
file. An account read longer ago than two intervals plus `timeout` still
comes back, with a `notice` line above the table naming it and how old its
reading is, because the refresh is failing or serve isn't running. `notice` is
empty otherwise, so a condition can test `${{ windows.output.notice }} != ''`. A
missing file fails the request with a message naming the path. The file has to
carry `"schema": "cairnos.usage.v1"`, and any other schema is refused
rather than guessed at. When some account has a score or an unexpired banked
reset, the table gains `SCORE` for the script's score and `CREDITS` for how many
of those resets are left.

The file is CairnOS's own `cairnos.usage.v1` format, and any script can write
it. Every time in it (`written_at`, `read_at`, `resets_at`, and each
`reset_credit_expiries` entry) is epoch seconds, fractions allowed. Each account
names the `tool` behind it, the `wrapper` a caller launches (what `launchers`
matches and the `LOGIN` column shows), and its `windows`. `tool` and `wrapper`
are required and must be non-empty, because no caller could select an account
without them, so a file holding such an account is refused. `plan`, `error`,
`score`, `read_at`, and `reset_credit_expiries` are optional, and an account
without `read_at` counts as read when the file was written:

```json
{
  "schema": "cairnos.usage.v1",
  "written_at": 1790000000,
  "accounts": [
    {
      "tool": "codex",
      "wrapper": "codex-agent",
      "plan": "prolite",
      "read_at": 1790000000,
      "windows": [{"name": "7d", "used_percent": 24, "resets_at": 1790416865}],
      "reset_credit_expiries": [1790500000],
      "score": 14.3
    },
    {"tool": "claude", "wrapper": "claude-agent", "error": "HTTP 401: token expired", "windows": []}
  ]
}
```

Every time in the sample is a placeholder that has already passed. Copied
as-is, the file comes back with a `notice` naming stale accounts and `CREDITS`
at 0, so put in current epoch seconds before testing with it.

The two modes don't mix. `script`, `output`, `interval`, and `host` can't sit
beside `claude`, `codex`, or `claude_gap`, and config load names the keys that
collide. `launchers` narrows the report to wrappers named in the file.

### Schedule syntax

A workflow's `schedule` key takes `@hourly`, `@daily`, `@weekly`, `@monthly`, a
5-field cron expression, or a 6-field one whose leading field is seconds:

```toml
[workflow]
name     = "heartbeat"
schedule = "*/30 * * * * *"   # sec min hour dom month dow: fires at :00 and :30
```

Five fields mean what they have always meant, so `*/5 * * * *` still fires at
second zero of every fifth minute and nothing you already scheduled changes. The
seconds field only opens the sub-minute range; a schedule that would fire more
often than every 10 seconds is refused by `cairnos schedule add` with the rate it
computed, because a run costs far more than a fire does.

Two behaviors matter once fires get close together:

- **Overlap.** While a schedule's previous run is still queued or running, the
  next fire is skipped rather than stacked behind it. A 30-second schedule on a
  job that sometimes takes a minute runs at half cadence instead of building a
  backlog it can never drain.
- **Downtime.** A schedule that fell behind while the process was down fires
  once, not once per missed occurrence. An hour of downtime on a 30-second
  schedule enqueues a single run and the next fire is computed from now.

`cairnos schedule list` prints `LAST FIRED` and `NEXT FIRE` to the second.

### Required hosts

A scheduled workflow that talks to a host behind a VPN can say so, and serve
will probe that host once a minute instead of letting every fire discover the
outage on its own:

```toml
[workflow.requires]
urls = ["https://git.example.com/team/app"]
vpn  = "tun0"
```

`urls` is required and each entry is probed with one TCP connect to its host,
on the port the URL names or else 443 for https and 80 for http. `vpn` is
optional and names a network interface; when it is down, the message says the
VPN is offline rather than repeating the dial error it causes. While
anything is unreachable the schedule skips its fires, a run already in flight
is held rather than retried and dead-lettered, and one line goes out under the
`connectivity` notify event. It repeats hourly while the outage lasts and a
final line reports the recovery and how long the host was gone. Add
`"connectivity"` to `[notify.mattermost]` or `[notify.discord]` `events` to
receive them.

### Sources

A host can get its workflows and scripts from a git repository instead of a
deploy step. Name the repository and where it goes, and serve clones it at
startup when the path is missing:

```toml
[source.team]
repo = "https://github.com/example/team-workflows.git"
dest = "/srv/team-workflows"
ref  = "main"

# Let workflows reach the checkout by name instead of by path.
[env]
allowlist = ["TEAM_WORKFLOWS"]
```

Give the serve process the same path in its environment file, the one
`EnvironmentFile=` points at:

```sh
TEAM_WORKFLOWS=/srv/team-workflows
```

Workflows then name the variable rather than the path:

```toml
[action.reconcile]
runner = "exec"
params = { command = "${{ env.TEAM_WORKFLOWS }}/scripts/reconciler.py" }
```

The workflow file lives in the repository and every host that clones it reads the
same text, so only `dest` and the environment file should name the path, and a box
where `$HOME` moves needs no edit to the workflow. `allowlist` is the gate: a
variable missing from it fails the run instead of being read.

`${{ system.hostname }}` needs no allowlist entry. It resolves when the action
launches to the name of the machine running it, so one workflow deployed to
several hosts can tell their posts or files apart. Any other `system.` name fails
at workflow load.

`dest` is an absolute path outside the CairnOS directory, since CairnOS prunes
and backs up its own home on its own schedule. CairnOS passes no git credentials
of its own, so a private repository works only when the serve user's git setup
already reaches it.

Two settings keep a credential question from stalling a boot or a scheduled run.
`GIT_TERMINAL_PROMPT=0` turns off git's own username and password prompt. For an
`ssh://` or `git@host:` remote the question comes from ssh instead, so CairnOS
also sets `GIT_SSH_COMMAND` to `ssh -o BatchMode=yes` unless you have set that
variable yourself. A host key that is not in `known_hosts`, or a key with a
passphrase, then fails the clone with a message rather than waiting for an answer
nobody is there to give. Put the deploy key and the host key on the box before
pointing a source at an ssh remote.

`ref` is resolved against the remote rather than declared. A remote branch is
followed to its tip. An exact tag pins the tree until you move the pin. A `ref`
containing `*`, `?` or `[` is a tag pattern whose newest match by version sort
wins, so `v*` follows releases. Write it as `v[0-9]*.[0-9]*.[0-9]*` when the
pattern should skip pre-release tags like `v2.0.0-rc1`. A name that is both a
branch and a tag fails instead of picking one.

The sync is an ordinary runner action, so you decide when it runs:

```toml
[action.sync]
runner = "source"
params = { source = "team" }
```

Its output is empty when nothing moved and `<old>..<new>` when HEAD did, which is
what the announcement below keys on. A scheduled workflow is the usual way to
poll:

```toml
[workflow]
name     = "source_sync"
schedule = "*/5 * * * *"
timeout  = "9m"

[workflow.requires]
urls = ["https://github.com"]

[action.sync]
runner = "source"
params = { source = "team" }

[action.announce]
runner     = "message"
depends_on = ["sync"]
condition  = "${{ sync.output }} != ''"
params     = { identity = "ops", to = "#ops", text = "team-workflows moved ${{ sync.output }} on agent-box" }
```

That file can live inside the tree it syncs. Point `cairnos schedule add` at its
path there once the first boot has cloned it.

A sync that can fast-forward does. When it cannot, because the tree carries
uncommitted changes to tracked files, sits on another branch, has a detached
HEAD, or holds commits the remote cannot fast-forward past, `dirty_reset`
decides what happens next:

```toml
[source.team]
repo = "https://github.com/example/team-workflows.git"
dest = "/srv/team-workflows"
ref  = "main"
# Default. false makes a blocked sync report the problem instead of fixing it.
dirty_reset = true
```

Left at its default, the sync fetches and then forces the tree back onto `ref`:
a forced checkout that recreates the local branch at the remote tip, or a forced
detached checkout for a tag. Tracked edits and local commits go with it. Set it
to `false` and the sync leaves the tree exactly as it found it, succeeds with
empty output, and reports the reason instead. Either way the run does not fail,
so a box no longer files a `run_failed` line every five minutes until someone
logs in.

Untracked files survive both paths, with one exception. Nothing in CairnOS runs
`clean`, `stash`, or `reset`, so no untracked file is swept. A forced checkout
does replace an untracked file when the incoming ref tracks a file at the same
path, since that is what `-f` means: the ref's copy wins. A scratch file at a
path the repository does not track is still there afterwards.

A skipped sync is quiet in the run history, so it gets its own notification. One
line goes out under the `source_dirty` event naming the host, the source, the
path, and what is in the way, at most once every 12 hours per source. A sync of
that source that is not skipped clears the timer, so a tree you fix and dirty
again is reported again rather than staying silent. Add `"source_dirty"` to
`[notify.mattermost]` or `[notify.discord]` `events` to receive them. Every reset
and every skip is also logged at warn level each time it happens.

A serve that cannot finish starting is the other quiet failure. The supervisor
restarts it every few seconds, so the only record is the journal on a box nobody
is logged into. When startup dies before the chat bridges connect, serve posts
one line under the `serve_failed` event naming the host, the stage that failed,
and the error, through a client it builds for that one post. The same failure
stays quiet for 12 hours, a different failure is reported at once, and a start
that reaches the bridges clears the window. That window lives in
`serve-failed.json` under the data directory, since the process exits between
attempts.

At startup serve checks that an existing tree is the repository the block names
and puts it on the configured ref when it is somewhere else, which is how moving
a pin works: edit `ref`, restart, and the next boot checks the new tag out. Boot
never fetches and never forces anything: a tree carrying local changes is logged
and left where it is, and the first sync repairs or reports it. If the new ref is
not on disk yet, serve logs a line saying so and starts anyway, and the first
sync fetches it.

Two syncs for one source never overlap inside one serve process. That lock is
per process, though, so running `cairnos schedule run source_sync` by hand while
serve's own schedule fires puts two processes in one tree. Git's index lock turns
that into a failed run rather than a damaged checkout, but the run still fails.

Warning: whoever can push to `ref` runs code on this host as the serve user
within one polling interval. Pin `ref` to a tag on hosts where a change should be
reviewed before it runs.

### Hosts and harnesses

A job that drives a model CLI on another machine has to know where that binary
lives. A non-login ssh shell usually starts without `~/.local/bin` on its `PATH`,
and the same tool can sit under a different name on each box. Describe the
machines once and every job gets the answer:

```toml
[host.build_box]
type    = "worker"
ssh     = "build-box"
wrapper = "~/.local/bin/agent-env"

[host.build_box.harness.claude]
name = "claude-agent"

[host.build_box.harness.codex]
name = "codex-agent"

[host.build_box.harness.copilot]

[host.build_box.harness.cursor-agent]
provider = "cursor"
```

`type` is `worker` for a machine that runs the CLIs or `agent` for one that
drives them, and `ssh` is the alias a job reaches it by, held to the same
grammar as the block name since that is what it is. `wrapper` is optional: a
command on that host the launcher is run through, as
`<wrapper> <launcher> <args...>`. It sets up the environment (PATH, nvm, and so
on) and must exec its arguments. Use it when the box's non-login `PATH` lacks
the CLI. It is absolute or `~/`-prefixed, and it may not carry a character a
shell would act on, because it goes into a command that host's login shell
runs. Each tool block takes
`path`, `name`, and `provider`, and all three are optional. `path` defaults to
`~/.local/bin` and must not end in a slash, `name` defaults to the block's own
key, so `copilot` above needs no keys at all. `provider` is a label for whose
model sits behind the tool.

Serve, `cairnos run`, `cairnos schedule run`, `cairnos suggestions`, and the MCP
server all render the same table into the environment of every child they launch.
The block above becomes exactly this:

```sh
CAIRNOS_HOSTS=build_box
CAIRNOS_HOST_BUILD_BOX_TYPE=worker
CAIRNOS_HOST_BUILD_BOX_SSH=build-box
CAIRNOS_HOST_BUILD_BOX_WRAPPER=~/.local/bin/agent-env
CAIRNOS_HOST_BUILD_BOX_HARNESSES=claude codex copilot cursor-agent
CAIRNOS_HOST_BUILD_BOX_HARNESS_CLAUDE=~/.local/bin/claude-agent
CAIRNOS_HOST_BUILD_BOX_HARNESS_CODEX=~/.local/bin/codex-agent
CAIRNOS_HOST_BUILD_BOX_HARNESS_COPILOT=~/.local/bin/copilot
CAIRNOS_HOST_BUILD_BOX_HARNESS_CURSOR_AGENT=~/.local/bin/cursor-agent
CAIRNOS_HOST_BUILD_BOX_HARNESS_CURSOR_AGENT_PROVIDER=cursor
```

The name after `CAIRNOS_HOST_` is the block name upper-cased with every dot and
dash turned into an underscore, and the same rule applies to the tool key. That
mapping loses information, so two names that would land on one variable, such as
`build_box` and `build-box`, fail config load instead of one quietly
winning.

A variable is absent rather than empty when the thing it describes is not
configured. `_SSH`, `_WRAPPER`, and `_PROVIDER` appear only when the block sets them, and a
host with no tool blocks gets no `_HARNESSES` line at all, so a job can tell
"nothing configured" from "configured as nothing".

A leading `~` is passed through exactly as written. CairnOS never expands it,
because the machine that runs the binary is the only one that knows its own home
directory. A job sending the path to another box should rewrite `~/` to `"$HOME"/`
in the remote command so that box expands it.

Two things do not receive the table. `[runner.exec] env` refuses any key starting
with `CAIRNOS_HOST_`, so the blocks are the one place these values come from.
And `sandbox_exec` passes no environment into its container at all, which is its
isolation contract; a sandboxed action that needs a path should take it as a
param.

Which model and how hard it thinks are not host settings. They belong to the job
that runs, next to the rest of its own config, because the same binary serves
several jobs with different budgets.

## Chat in Mattermost

Run your agents as bot users in a self-hosted Mattermost. Each identity is one
bot account: it answers DMs, answers `@mentions` in channels, holds a
conversation per thread, and can gate a workflow run behind a named approver.

Create the bot account in Mattermost yourself, then declare it. CairnOS never
creates or edits a bot account.

```toml
[bridge.mattermost]
url          = "https://mm.example.com"
team         = "main"    # needed when you name channels by #name
ack_reaction = "eyes"    # emoji put on a post the bot picked up; "" turns it off

[bridge.mattermost.identity.ops]
username      = "ops-agent"      # must match the account the token belongs to
display_name  = "Ops Agent"
workspace     = "default"
personality   = "ops"
approvers     = ["alice"]
allow_bots_in = ["#agents"]
```

The token is read only from the environment. One Mattermost server is the
ordinary case and every identity on it can post as the same bot, so there is one
variable:

```sh
export CAIRNOS_BRIDGE_MATTERMOST_TOKEN=...
```

Two identities that must talk as different bots need different tokens. Name the
variable on the identity:

```toml
[bridge.mattermost.identity.review]
username  = "review-agent"
workspace = "default"
token_env = "CAIRNOS_BRIDGE_MATTERMOST_REVIEW_TOKEN"
```

`CAIRNOS_BRIDGE_MATTERMOST_<ID>_TOKEN` also works without a config line: an
identity reads it when it is set, ahead of the shared one, which keeps a host
that already exports it working untouched. An identity that sets `token_env`
reads only that name, so a typo there stops the identity rather than quietly
posting as another bot.

No token means no connection for that identity, and serve logs one line naming
every variable it looked for. No identity at all means the whole bridge stays
off.

A DM needs no mention. In a channel the bot answers only when you `@mention` it
in ordinary text, and it replies in that post's thread, so each thread is its
own conversation with its own history.

When the bot takes a post on, it reacts to that post with `ack_reaction` so you
know it was seen and is being worked on. The default is `eyes`. Use the emoji's
short name without colons, such as `white_check_mark`, or set it to `""` to
skip the reaction. Posts the bot ignores or declines get no reaction, and a
reaction that fails never holds up the reply.

`approvers` lists the usernames that may answer `approve` or `deny` when the
agent proposes a workflow run. Leave it empty and every proposal expires
unanswered, which is the safe default: being able to chat with an agent is not
the same as being able to start its runs.

`allow_bots_in` lists the channels where a post from another bot or a webhook
still earns a reply. Everywhere else those posts are dropped, which is what
keeps two agents from talking to each other forever. A sender that wants a reply
outside those channels can set the post prop `activate_ai = "true"`. Inside an
allowed channel a sliding window still applies, so a runaway conversation goes
quiet for a cooldown instead of running up your bill.

Mention the bot with `usage`, or ask how much of the weekly limit is left, and
it answers in the thread with the same table `cairnos usage` prints. There is
no approval step, because the probe changes nothing and spends no quota. Each
Claude login after the first waits out the CLI's usage cache (`claude_gap`,
65 seconds by default), so three Claude logins take a little over two minutes
before the reply lands.

### Sending messages

Every identity is also a send target, from three places.

From the command line, against a running serve:

```sh
cairnos msg list
cairnos msg send --as ops --to '#alerts' "deploy finished"
cairnos msg send --as ops --to '@alice' --thread <post-id> -   # text from stdin
```

From a workflow, with the `message` runner. It needs no runner block:

```toml
[action.announce]
runner = "message"
params = { identity = "ops", to = "#alerts", text = "run ${run.id} finished" }
```

The action outputs the created post id, so a later action can answer in the
same thread with `thread = "${action.announce.output}"`.

A scheduled workflow that should keep posting into one thread sets `thread_key`
instead. The first post under a key opens a new thread and CairnOS stores its
root post id. Every later post with the same identity, target, and key replies
there, across runs and restarts:

```toml
[action.report]
runner = "message"
params = { identity = "ops", to = "#usage", thread_key = "usage:${{ system.hostname }}", text = "${{ windows.output.summary }}" }
```

If someone deletes the root post, the next send opens a fresh thread and
remembers that one. `thread` and `thread_key` together fail at workflow load.

And from the notification fan-out, which posts held jobs and failed runs into a
channel:

```toml
[notify.mattermost]
identity = "ops"
channel  = "#alerts"
events   = ["held", "run_failed", "source_dirty", "serve_failed"]
```

`events` accepts `held`, `run_failed`, `dead_lettered`, `budget_denied`,
`connectivity`, `source_dirty`, and `serve_failed`; any other name fails the
config load. The Discord webhook and this block both deliver when both are set.
Targets are written as `#channel`, `@user`, `channel:<id>`, or `user:<id>`.

## Development

```sh
make test-unit          # race tests, no container
make test-integration   # live-Postgres suite via testcontainers
make lint               # golangci-lint + gosec
```

SQLite is the default store. Postgres backs multi-node setups; both are
configured in the file `cairnos init` writes.
