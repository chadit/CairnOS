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

## Working from a clone

```sh
make build           # builds bin/cairnos without touching $GOBIN
./bin/cairnos validate examples/agents/release_monitor/release_monitor.toml
./bin/cairnos run <workflow.toml>   # one-shot run, no service needed
```

One-shot `cairnos run` is for trying a workflow out; anything scheduled or
conversational needs serve.

## Model setup

There are two kinds of model runner. Pick per action.

**API runners** call a hosted API directly with a key:

```toml
[runner.anthropic]
model = "claude-sonnet-4-5"

[runner.openai]
model = "gpt-4.1"
# base_url points this runner at any OpenAI-compatible endpoint, so a hosted
# subscription with a compatible API works through it too.
# base_url = "https://api.example.com/v1"
```

Keys ride the environment (`CAIRNOS_RUNNER_ANTHROPIC_API_KEY`,
`CAIRNOS_RUNNER_OPENAI_API_KEY`), never the config file.

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

### Required hosts

A scheduled workflow that talks to a host behind a VPN can say so, and serve
will probe that host once a minute instead of letting every fire discover the
outage on its own:

```toml
[workflow.requires]
urls = ["https://git.example.com/operator/Imagetool-Work"]
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

The token is read only from the environment, one variable per identity, named
after the identity:

```sh
export CAIRNOS_BRIDGE_MATTERMOST_OPS_TOKEN=...
```

No token means no connection for that identity, and serve logs one line naming
the variable it looked for. No identity at all means the whole bridge stays off.

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
events   = ["held", "run_failed"]
```

The Discord webhook and this block both deliver when both are set. Targets are
written as `#channel`, `@user`, `channel:<id>`, or `user:<id>`.

## Development

```sh
make test-unit          # race tests, no container
make test-integration   # live-Postgres suite via testcontainers
make lint               # golangci-lint + gosec
```

SQLite is the default store. Postgres backs multi-node setups; both are
configured in the file `cairnos init` writes.
