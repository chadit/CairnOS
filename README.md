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

## Development

```sh
make test-unit          # race tests, no container
make test-integration   # live-Postgres suite via testcontainers
make lint               # golangci-lint + gosec
```

SQLite is the default store. Postgres backs multi-node setups; both are
configured in the file `cairnos init` writes.
