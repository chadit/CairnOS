# Contributing to CairnOS

Thanks for helping. This file covers the tools you need, the make targets, and
what a pull request needs before it can merge. Report security problems through
[SECURITY.md](SECURITY.md), not an issue. Everyone taking part follows the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Prerequisites

- **Go 1.27.2.** `go.mod` sets the version. With the default `GOTOOLCHAIN=auto`,
  any Go from 1.21 on downloads 1.27.2 the first time it builds the module.
- **buf**, with nothing to install. `go.mod` declares buf and the `protoc` plugins
  as tool directives, so `go tool buf` runs the pinned version.
- **Node and npm** for the web UI in `web/`. `web/package.json` accepts Node
  `^22.22.2`, `^24.15.0`, or `>=26.0.0`, and CI uses the newest LTS.
- **Docker or Podman** for the integration suite. `scripts/integration.sh` finds
  a rootless Podman socket on Linux or a Podman machine on macOS when the Docker
  socket is absent.
- **jq** for the shell script tests.

## Make targets

`make` with no target runs `make help`, which lists these.

| Target | What it does |
| --- | --- |
| `help` | Lists every target with its description. |
| `build` | Runs `proto`, then builds `bin/cairnos` from `./cmd/cairnos`. |
| `deploy` | Runs `build`, then `scripts/deploy.sh`, which lets `cairnos update --no-fetch` pull secrets and restart the service when the build changed. It is for a host that runs CairnOS. Contributors do not need it. |
| `test` | Runs `proto`, `test-unit`, `test-integration`, and `test-scripts` in that order. |
| `test-unit` | Runs `proto`, then `go test -race -count=1 ./...`. No container. |
| `test-integration` | Runs `proto`, then `scripts/integration.sh ./tests/integration/...`, which runs `go test -tags integration -race` against a Postgres container that testcontainers starts. |
| `test-scripts` | Runs `scripts/update-deps_test.sh` and `scripts/model-update-issues_test.sh` against stub tools. Needs jq, touches no network. |
| `lint` | Runs `gosec` and `golangci-lint`, then `golangci-lint run` with `.golangci.yml`. |
| `gosec` | Installs the newest `gosec` into `bin/tools` and runs it over the tree, skipping the generated code in `internal/api/gen`. |
| `golangci-lint` | Installs the newest `golangci-lint` into `bin/tools`. It does not run it; `lint` does. |
| `proto` | Runs `go tool buf generate`, which regenerates the gRPC, gateway, and OpenAPI code in `internal/api/gen` from `proto/`. |
| `tidy` | Runs `go mod tidy` to sync `go.mod` and `go.sum` with the source. |
| `update-deps` | Runs `scripts/update-deps.sh`, which upgrades every dependency and pins each to its newest release, except the holds that script explains. Send the result as its own pull request, not mixed into a feature. |

## Running tests

`make test-unit` is the fast loop. The integration tests carry the
`//go:build integration` tag, so a plain `go test` skips them and needs no
container:

```sh
make test-unit                      # whole tree, race detector, regenerates proto first
go test -race ./internal/config/... # one package tree, no regeneration
```

`make test-integration` needs Docker or Podman running. `make test` runs every
suite, the same set CI runs on Linux.

For the web UI, from `web/`:

```sh
npm ci
npm run lint
npm run test
npm run build
```

## Changing the protobuf API

The `.proto` files live in `proto/`. After you change one:

1. Run `make proto` (or `go tool buf generate`).
2. Run `go tool buf lint`, which checks the rules in `buf.yaml`.
3. Commit the regenerated files in `internal/api/gen` in the same pull request.
   The release build compiles the committed code and does not regenerate it.

## Lint gate

`make lint` runs `gosec` and `golangci-lint` against the whole tree. It must pass
before a pull request merges, and CI runs both. Neither linter is pinned: the
Makefile and CI both install the newest release, so a new upstream check can
flag code that passed before. Fix what it reports instead of adding exclusions.

## Commits and pull requests

- Keep each pull request small and about one change. A refactor and a feature go
  in separate pull requests.
- A behavior change needs a test that covers it. A bug fix needs a test that
  fails without the fix.
- CI must pass. It builds and vets the tree, cross-compiles for macOS and
  Windows, runs the unit, script, and integration suites, runs the plugin tests
  on Windows, lints, tests, and builds the web UI, and runs the lint gate. A
  maintainer approves CI runs on a pull request from a fork before they start.
- Commit messages follow the Conventional Commits style the history uses, such
  as `feat:`, `fix:`, `refactor:`, `chore:`, and `ci:`.
- Update the README or the examples when a change alters behavior, config, or a
  command. The pull request template has a checklist for this.
- Never commit secrets, a real config file, or a `.env` file.

## Examples

`examples/` holds working samples, and changes to them follow the same rules as
the code:

- `examples/agents/` holds workflow TOML files. Start with
  `examples/agents/quickstart`. Check one with
  `./bin/cairnos validate examples/agents/<name>/<name>.toml`.
- `examples/a2a/` is a Go client that drives a CairnOS node through the
  Agent-to-Agent loop over the HTTP gateway.
- `examples/plugins/echo/` is a minimal plugin built on `pkg/plugin`.
- `examples/dashboards/` holds Grafana dashboards and Prometheus alert rules for
  the `cairnos_*` metrics.

Build example programs into `bin/` (`go build -o bin/ ./examples/a2a`) so the
binaries stay out of the tree.
