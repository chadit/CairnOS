// Workflow fixtures for the round-trip table. hello.toml is copied verbatim
// from internal/cli/cmd_init.go's helloWorkflow so the UI mapper is tested
// against exactly what `cairnos init` writes. The agent fixture mirrors the
// kind="agent" action from examples/agents/learning_coach. Each is meant to
// survive parse -> emit -> parse with the same normalized form.

/** helloToml is the example `cairnos init` scaffolds, byte for byte. */
export const helloToml = `# hello.toml: the smallest useful CairnOS workflow.
#
# Run it:
#   cairnos run examples/hello.toml
# Override the input:
#   cairnos run examples/hello.toml --input name=cairn

[workflow]
name        = "hello"
description = "Greet someone, then echo the greeting through a second action."

[workflow.inputs.name]
type        = "string"
default     = "world"
description = "Who to greet."

[action.greet]
runner = "mock"
params = { output = "hello, \${{ inputs.name }}" }

[action.echo]
runner     = "mock"
depends_on = ["greet"]
params     = { output = "echo: \${{ greet.output }}" }
`;

/** agentToml carries a kind="agent" action plus a task action it depends on,
 * the shape from examples/agents/learning_coach.toml. */
export const agentToml = `[workflow]
name        = "study_helper"
version     = "1"
description = "Suggest study material, then record it."

[workflow.inputs.goals]
type    = "string"
default = "Go concurrency"

[action.suggest]
runner = "anthropic"
params = { prompt = "Suggest two things to study for: \${{ inputs.goals }}" }

[action.log_progress]
kind       = "agent"
depends_on = ["suggest"]
driver     = "anthropic"
goal       = "Store this week's study suggestion in memory: \${{ suggest.output }}"
tools      = ["memory"]
budget_usd = 0.25
`;

/** fanInToml exercises parallel roots fanning into one action, with a
 * retry sub-table, a condition, and notify, to widen mapper coverage. */
export const fanInToml = `[workflow]
name     = "fan_in"
schedule = "@daily"
on_error = "continue"
notify   = ["email:me@example.com", "webhook:https://hooks.example/x"]
timeout  = "15m"

[workflow.inputs.region]
type        = "string"
required    = true
description = "Target metro."

[action.fetch_a]
runner     = "webhook"
idempotent = true
params     = { url = "https://a.example", count = 5 }

[action.fetch_a.retry]
max_attempts = 3
backoff      = "exponential"
base_delay   = "2s"
max_delay    = "30s"

[action.fetch_b]
runner = "webhook"
params = { url = "https://b.example" }

[action.merge]
runner     = "mock"
depends_on = ["fetch_a", "fetch_b"]
condition  = "\${{ fetch_a.output }} != 'EMPTY'"
params     = { a = "\${{ fetch_a.output }}", b = "\${{ fetch_b.output }}", tags = ["x", "y"] }
`;

export const roundTripFixtures = [
  { name: 'hello', toml: helloToml },
  { name: 'agent', toml: agentToml },
  { name: 'fan-in', toml: fanInToml },
];
