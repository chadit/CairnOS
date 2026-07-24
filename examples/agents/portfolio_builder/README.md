# Portfolio Builder

Plans a small portfolio project aimed at a target role, hands the plan to
the codex CLI to scaffold a skeleton on disk, then posts a completion ping
to your webhook. Trigger-only: run it when you want a new project, no
schedule.

## Runners

- `plan` uses `anthropic`: produces a scoped project plan (pitch, features,
  stack, file layout) for the `target_role` input.
- `scaffold` uses `codex`: the prompt is assembled from the plan output
  plus the `project_dir` input, so the CLI creates the skeleton the plan
  describes. The codex binary must be on PATH (or set `binary` in its
  config block).
- `notify` uses `webhook`: posts a short completion message with the
  scaffold summary to `notify_url`.

## Config

```toml
[runner.anthropic]
model = "claude-sonnet-4-5"

[runner.codex]
# binary = "codex"          # override if the CLI lives elsewhere; the runner
#                           # already launches `codex exec` by default
```

Environment:

- `CAIRNOS_RUNNER_ANTHROPIC_API_KEY` for the planning step.
- Whatever auth the codex CLI itself needs (its own login or key).

## Run it

```sh
cairnos validate examples/agents/portfolio_builder/portfolio_builder.toml

cairnos run examples/agents/portfolio_builder/portfolio_builder.toml \
  --input target_role="Staff Platform Engineer" \
  --input notify_url=https://hooks.example.com/portfolio
```

Heads up: the scaffold step writes files wherever the codex CLI runs, under
`project_dir` (default `./portfolio`). Point it somewhere disposable the
first time.
