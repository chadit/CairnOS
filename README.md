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

Or straight from the module path, no clone needed. The repo is private, so
tell the Go toolchain to fetch it over your git auth instead of the public
proxy:

```sh
GOPRIVATE=github.com/chadit go install github.com/chadit/CairnOS/cmd/cairnos@latest
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
line for `claude`, `omp_glm`, or any other logged-in tool, see
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
a clone on that host. It changes when you update that clone and deploy it, and
at no other time.

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
  error="load /srv/automation/workflows/heartbeat.toml: 1 error(s): workflow.invalid:
  schedule \"*/30 * * * * *\" must be a 5-field cron expression or an @-shorthand"
```

Nothing in the repository looks wrong, because nothing in it is wrong. The file
is fine and the binary reading it is old, and it stays that way until the binary
changes. The recovery is the deploy that should have come first:

```sh
cd ~/src/cairnos              # the clone this host builds from
git fetch && git merge --ff-only
make deploy                   # rebuilds when the running build is behind, then restarts
journalctl --user -u cairnos -n 50
```

The next fire loads the same file against the new binary and runs it. No run was
queued while it was failing, so there is no backlog to drain and nothing to
replay by hand.

### Telling whether a host is behind

`scripts/deploy.sh --check` compares the build the service is running against
the clone's HEAD. It reads the revision Go stamps into every binary, through
`/proc`, so a file replaced on disk is not mistaken for the process in memory:

```sh
./scripts/deploy.sh --check
I: running: 7929e3e8a1...
I: wanted:  625fb2e4c9...
```

It exits 3 when the two differ, 0 when they match, and it pulls no secrets and
restarts nothing, so it is safe on a host you only want to look at. `make deploy`
prints the same two lines before it acts on them. There is no `cairnos version`
subcommand: that stamped revision is how a running build is identified.

The script drives a systemd user service. Under launchd, or for a `--system`
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
cairnos schedule add /srv/automation/workflows/heartbeat.toml
cairnos schedule list                 # the CRON column now reads the new expression
```

The `schedule` key is the only one this applies to. Everything else in the file
is read fresh at each fire.

### What a restart picks up

`config.toml` and the environment files are read once, at startup, so a change to
either needs a restart before the service sees it. Workflow files are read at
every fire, so an edited action, runner or timeout is live on the next one. A new
binary needs the restart that `make deploy` already does.

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
is why `make deploy` can run it on every host, on every run.

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
in the same order. `make deploy` pulls on every run, before it decides whether
to restart, and stops there if the pull fails. It restarts when the binary is
behind HEAD or when the pull changed `secrets.env`, so a rotated or renamed
vault field reaches the service without waiting for a code change. When neither
moved it says `secrets unchanged` and leaves the running service alone.

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
make update-dep
```

It moves the Go modules, the `go.mod` tool directives, the Go toolchain line,
the buf registry dependency, the npm packages under `web/`, and the two linter
versions the Makefile pins, then regenerates the protobuf code. It writes files
and stops there: no commit, no push, no tests. At the end it prints
`git status --short` along with what it left alone on purpose, which is the two
`FROM` lines in the Dockerfile (both carry float markers), the postgres image
the integration tests start, and the action versions in the GitHub workflows,
which you bump by hand.

Run `make test` and `make lint` before committing any of it. `make lint`
installs `golangci-lint` and `gosec` into `bin/tools` at the versions the
Makefile names, so a developer machine and CI run the same linters instead of
whatever each happens to have on PATH.

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
model = "claude-sonnet-4-5"

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
`omp_codex`, `omp_qwen`, `omp_glm`, `antigravity`. Each launches its tool's
headless one-shot mode out of the box (`codex exec`, `claude -p`, `omp-glm
-p`, and so on), so a workflow action never opens an interactive UI. Say you
have a Codex subscription: log in once on the host that runs serve
(`codex login`), then use it from a workflow:

```toml
[action.plan]
runner = "codex"
params = { prompt = "Draft the migration plan for ..." }
```

Or route through an OMP wrapper. Each CLI runner finds its binary on PATH by
default (`omp_glm` launches `omp-glm`); override the path or argv when yours
differs:

```toml
[runner.omp_glm]
binary = "/usr/local/bin/omp-glm"
# args = ["-p", "{prompt}"]   # the default; override to add flags
```

When a CLI tool's login expires mid-schedule, the job is held rather than
failed. You get a webhook alert on Discord (`CAIRNOS_NOTIFY_DISCORD_WEBHOOK`),
and once you re-authenticate the tool on the host, the queue probes the fresh
credential and resumes the job on its own for the recognized tools.

Chain models with fallback so one expired or rate-limited provider doesn't
stop a workflow:

```toml
[engine.fallback]
codex = ["omp_qwen", "anthropic"]
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

`allow_actions` denies by default. An action name absent from the list is
refused at validate time, and a block that is missing or lists nothing refuses
every action.

Warning: a command listed here runs on the host as the serve user, with that
user's full environment and no isolation.

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
[source.automation]
repo = "https://github.com/example/automation.git"
dest = "/srv/automation"
ref  = "main"

# Let workflows reach the checkout by name instead of by path.
[env]
allowlist = ["AUTOMATION_ROOT"]
```

Give the serve process the same path in its environment file, the one
`EnvironmentFile=` points at:

```sh
AUTOMATION_ROOT=/srv/automation
```

Workflows then name the variable rather than the path:

```toml
[action.reconcile]
runner = "exec"
params = { command = "${{ env.AUTOMATION_ROOT }}/engines/pr-worker/scripts/reconcile.py" }
```

The workflow file lives in the repository and every host that clones it reads the
same text, so only `dest` and the environment file should name the path, and a box
where `$HOME` moves needs no edit to the workflow. `allowlist` is the gate: a
variable missing from it fails the run instead of being read.

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
params = { source = "automation" }
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
params = { source = "automation" }

[action.announce]
runner     = "message"
depends_on = ["sync"]
condition  = "${{ sync.output }} != ''"
params     = { identity = "ops", to = "#ops", text = "automation moved ${{ sync.output }} on agent-box" }
```

That file can live inside the tree it syncs. Point `cairnos schedule add` at its
path there once the first boot has cloned it.

A sync that can fast-forward does. When it cannot, because the tree carries
uncommitted changes to tracked files, sits on another branch, has a detached
HEAD, or holds commits the remote cannot fast-forward past, `dirty_reset`
decides what happens next:

```toml
[source.automation]
repo = "https://github.com/example/automation.git"
dest = "/srv/automation"
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
type = "worker"
ssh  = "build-box"

[host.build_box.harness.claude]
name = "claude-agent"

[host.build_box.harness.codex]
name = "codex-agent"

[host.build_box.harness.copilot]

[host.build_box.harness.omp-glm]
provider = "zai"
```

`type` is `worker` for a machine that runs the CLIs or `agent` for one that
drives them, and `ssh` is the alias a job reaches it by, held to the same
grammar as the block name since that is what it is. Each tool block takes
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
CAIRNOS_HOST_BUILD_BOX_HARNESSES=claude codex copilot omp-glm
CAIRNOS_HOST_BUILD_BOX_HARNESS_CLAUDE=~/.local/bin/claude-agent
CAIRNOS_HOST_BUILD_BOX_HARNESS_CODEX=~/.local/bin/codex-agent
CAIRNOS_HOST_BUILD_BOX_HARNESS_COPILOT=~/.local/bin/copilot
CAIRNOS_HOST_BUILD_BOX_HARNESS_OMP_GLM=~/.local/bin/omp-glm
CAIRNOS_HOST_BUILD_BOX_HARNESS_OMP_GLM_PROVIDER=zai
```

The name after `CAIRNOS_HOST_` is the block name upper-cased with every dot and
dash turned into an underscore, and the same rule applies to the tool key. That
mapping loses information, so two names that would land on one variable, such as
`build_box` and `build-box`, fail config load instead of one quietly
winning.

A variable is absent rather than empty when the thing it describes is not
configured. `_SSH` and `_PROVIDER` appear only when the block sets them, and a
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
url  = "https://mm.example.com"
team = "main"            # needed when you name channels by #name

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
