# Learning Coach

Watches release feeds from a few upstream projects once a week, then asks a
model what you should study next given your goals. The three release fetches
run in parallel because nothing orders them; the suggestion step fans them
back in through `depends_on`.

## Runners

- `releases_go`, `releases_grpc`, `releases_mcp` use `github`: each pulls
  the last five releases from one repo (`op = "releases"`). Edit the `repo`
  params to follow the projects you actually care about.
- `suggest` uses `anthropic`: turns the three feeds plus your `goals` input
  into two or three concrete study suggestions.
- `log_progress` is a `kind = "agent"` action with the `memory` tool. It is
  schema-valid today but does nothing yet: agent actions execute once the
  agent loop lands in Phase 1c. Until then the run ends at `suggest`, and
  you read the result from `cairnos runs`.

Progress logging is the point of that last action. Once Phase 1c ships, the
agent stores each week's suggestion in memory so later runs stop repeating
material. Today the workflow has no memory of past weeks.

## Config

```toml
[runner.anthropic]
model = "claude-sonnet-5"
```

Environment:

- `CAIRNOS_RUNNER_ANTHROPIC_API_KEY` for the suggestion step.
- `CAIRNOS_RUNNER_GITHUB_TOKEN` is optional: public release feeds work
  anonymously at a lower rate limit.

## Run it

```sh
cairnos validate examples/agents/learning_coach/learning_coach.toml

cairnos run examples/agents/learning_coach/learning_coach.toml \
  --input goals="learn eBPF and kernel tracing"

# put it on the weekly schedule
cairnos schedule add examples/agents/learning_coach/learning_coach.toml
```
