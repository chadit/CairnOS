# Sample agents

Start with [quickstart](./quickstart/README.md): one prompt through a
logged-in CLI tool, the fastest proof that a fresh install works.

Eight more workflows ship with Phase 1. They aren't finished products;
each one exists to show how actions compose, how data passes between them,
and which runner fits which job. Every directory holds one workflow TOML
and a README with its config block, env keys, and run commands.

Sourcing stance (S5): Career Scout and Housing Monitor fetch only from
sources you own or hold API keys for; no sample scrapes LinkedIn, Indeed,
or any listing site.

## What each sample shows

| Agent | Schema features | Runners |
| --- | --- | --- |
| quickstart | smallest possible workflow | codex (or any CLI runner) |
| career_scout | schedule, retry, condition | webhook, openai, email |
| skill_gap_analyzer | inputs, data passing | anthropic, local_fn, email |
| learning_coach | parallel fan-in, agent action | github, anthropic |
| portfolio_builder | chained prompts | anthropic, codex, webhook |
| housing_monitor | cron schedule, field condition | webhook, local_fn, email |
| language_rules_assistant | single-action workflow | language_rules_service |
| release_monitor | parallel fan-in, sentinel condition | github, anthropic |
| browser_snapshot | agent action, MCP tool | browser_nav, anthropic |
| usage_report | cron schedule, thread_key, bool condition | usage, message |

## What each sample needs

Env keys below are all prefixed `CAIRNOS_RUNNER_`; runner config blocks
(`[runner.openai]` and friends) are spelled out in each agent's README.

| Agent | Env keys (CAIRNOS_RUNNER_*) |
| --- | --- |
| career_scout | OPENAI_API_KEY, EMAIL_PASSWORD |
| skill_gap_analyzer | ANTHROPIC_API_KEY, EMAIL_PASSWORD |
| learning_coach | ANTHROPIC_API_KEY, GITHUB_TOKEN (optional) |
| portfolio_builder | ANTHROPIC_API_KEY, plus the codex CLI's own auth |
| housing_monitor | EMAIL_PASSWORD |
| language_rules_assistant | none; set dir in its config block |
| release_monitor | GITHUB_TOKEN, ANTHROPIC_API_KEY |
| browser_snapshot | ANTHROPIC_API_KEY |
| usage_report | none; the posts need CAIRNOS_BRIDGE_MATTERMOST_TOKEN under serve |

## Notes

- `skill_gap_analyzer` and `housing_monitor` call Go functions through
  `local_fn`. Code can't live in config, so the operator registers those
  functions at build time; each README documents the contract.
- `learning_coach` ends with a `kind = "agent"` action. It validates today
  and starts executing when the agent loop lands in Phase 1c.
- `browser_snapshot` drives a containerized Playwright MCP server through a
  named `mcp_client` runner. Its `[runner.browser_nav]` block goes in
  `config.toml` (see that sample's README) and needs the `mcp_client`
  named-instance path.
- Every file here passes `cairnos validate`. Start with
  `examples/hello.toml` (created by `cairnos init`) if you want the bare
  minimum first.
