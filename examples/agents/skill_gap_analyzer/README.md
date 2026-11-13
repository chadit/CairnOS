# Skill Gap Analyzer

Takes a JSON array of job postings (a saved Career Scout digest works), has
a model extract the skills they ask for, then compares that list against
your profile with an in-process Go function. The result lands in your
inbox.

## Runners

- `extract_skills` uses `anthropic`: pulls a deduplicated skill list out of
  the postings. The system prompt demands a bare JSON array.
- `compare` uses `local_fn`: calls the registered `skill_gap_compare`
  function.
- `email_summary` uses `email`: sends the gap report.

## The compare function

The stock `cairnos` binary registers no `local_fn` functions, so this example
needs a modified build. Code outside the CairnOS module cannot import
`internal/runner/builtins`, so the function goes in a fork or checkout of this
repository: add it to the empty map that `fixedBuiltins` passes to
`builtins.LocalFnFactory` in `internal/cli/wiring.go`, then rebuild with
`go build ./cmd/cairnos`.

The contract: read `required` (the model's JSON array, as text) and
`profile` (comma-separated skills) from the task params, return a
human-readable gap report string. A reference shape:

```go
// internal/cli/wiring.go, in fixedBuiltins
{name: builtins.LocalFnName, factory: builtins.LocalFnFactory(map[string]builtins.Func{
    "skill_gap_compare": func(
        _ context.Context, task *runner.Task,
    ) (any, error) {
        required, _ := task.Params["required"].(string)
        profile, _ := task.Params["profile"].(string)
        // parse required as JSON, diff against profile, format the report
        return report(required, profile), nil
    },
})},
```

Until you register it, validation passes but a run fails at the `compare`
action with `local_fn.unknown_fn`.

## Config

```toml
[runner.anthropic]
model = "claude-sonnet-5"

[runner.email]
host = "smtp.example.com"
port = "587"
from = "cairnos@example.com"
user = "cairnos@example.com"
```

Environment:

- `CAIRNOS_RUNNER_ANTHROPIC_API_KEY` for the extraction step.
- `CAIRNOS_RUNNER_EMAIL_PASSWORD` for SMTP auth.

## Run it

```sh
cairnos validate examples/agents/skill_gap_analyzer/skill_gap_analyzer.toml

cairnos run examples/agents/skill_gap_analyzer/skill_gap_analyzer.toml \
  --input jobs_json='[{"title": "Platform Engineer"}]' \
  --input profile="Go, PostgreSQL, gRPC" \
  --input notify_to=me@example.com
```

Trigger-only: no schedule, run it when you have fresh postings to analyze.
