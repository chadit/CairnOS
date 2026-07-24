# Quickstart

The first workflow to run on a fresh install. One action, one prompt, through
a CLI tool you have already logged into on this machine, so no API key enters
CairnOS at all. If this run prints a reply, your install, runner config, and
tool auth all work.

## Run it

```sh
cairnos validate examples/agents/quickstart/quickstart.toml
cairnos run examples/agents/quickstart/quickstart.toml
```

The run streams the action's status, prints the model's reply as the action
output, and records the run in history (`cairnos runs list`).

## Pick your runner

The file ships pointing at `codex`. Every built-in CLI runner already launches
its tool's headless one-shot mode (`codex exec`, `claude -p`, `omp-glm -p`),
so the only edit is the runner name:

- `runner = "claude"` for a Claude Code login
- `runner = "omp_glm"` for the omp GLM wrapper
- `runner = "qwen"` / `"omp"` / `"omp_codex"` / `"omp_qwen"` likewise
- `runner = "anthropic"` or `"openai"` to use an API key instead
  (`CAIRNOS_RUNNER_ANTHROPIC_API_KEY` / `CAIRNOS_RUNNER_OPENAI_API_KEY`)

## Where to next

- `cairnos chat` starts the conversational agent against a running serve.
- The sibling directories under `examples/agents/` show real multi-action
  DAGs: parallel fetches, judged comparisons, conditional follow-ups.
- `cairnos schedule add <workflow.toml>` puts a workflow on the durable
  schedule once serve is running.
