# Language Rules Assistant

Asks the rules service for the coding rules of one language, with optional
per-project overrides. A single root action and no schedule: the smallest
real workflow in this directory, and the second sample from the original
vision doc.

## Runners

- `get_rules` uses `language_rules_service`: looks up `language` (and the
  optional `project` overlay) and returns the merged rule set.

As of Phase 7b this runner is backed by the real Language Rules Service when
you run it under `cairnos serve` or `cairnos mcp serve`, which stores rules in
the node's database. Set rules with `cairnos rules set`, or seed the old
static-file dir in with `cairnos rules import <dir>`. The workflow did not
change: the runner queries the service first and falls back to the static
files for a language the store does not know, so the file-based setup below
still works for a local `cairnos run`.

## Config

The runner needs one config key, the rules directory:

```toml
[runner.language_rules_service]
dir = "rules"
```

Rule files are flat `key = "value"` TOML, one file per language. The base
set lives in `<language>.toml` and an optional `<language>.<project>.toml`
overrides it key by key. A minimal `rules/go.toml`:

```toml
formatting = "gofmt is not optional; CI rejects unformatted code"
errors     = "wrap errors with context: fmt.Errorf(\"doing x: %w\", err)"
testing    = "table-driven tests; t.Parallel() unless state forbids it"
```

Add `rules/go.finance-app.toml` next to it to override keys for one
project:

```toml
errors = "use the team's apperr package, not bare fmt.Errorf"
```

No environment keys: the runner reads local files only.

## Run it

```sh
cairnos validate \
  examples/agents/language_rules_assistant/language_rules_assistant.toml

cairnos run \
  examples/agents/language_rules_assistant/language_rules_assistant.toml

cairnos run \
  examples/agents/language_rules_assistant/language_rules_assistant.toml \
  --input language=go --input project=finance-app
```

Asking for a language with no rules (no store rows and no file) fails the
action with `rules.unknown_language`. A missing project overlay is fine; you
just get the language defaults.

## Real rules under serve

To use the stored service instead of static files, set rules on the running
node:

```sh
cairnos rules set go max_line 120
cairnos rules set go finance-app max_line 100
cairnos rules get go finance-app          # max_line shows 100, the override

# migrate an existing static dir into the store
cairnos rules import rules/
```

The same data is reachable over the API (`GET /v1/rules/go`,
`POST /v1/rules`) and as the `get_language_rules` MCP tool, all under the
`read:vault` / `write:vault` scopes.
