# Release Monitor

Phase 8, built as a CairnOS workflow on top of the engine. Once a day it pulls
release feeds from a few upstream repos, asks an LLM whether any release lines up
with the CairnOS roadmap, and files an issue into a private repo when one does.
The issue is a draft for a human to triage, not an automatic decision.

On a quiet day nothing gets filed: the compare step returns a `NONE` sentinel
and the issue step is skipped, not failed. Same pattern career_scout uses with
`NO_MATCHES`.

## What it does

1. `releases_otel` and `releases_mcp` each pull one repo's recent releases through
   the `github` runner. They share no dependency, so the engine runs them in
   parallel.
2. `compare` feeds both release lists plus a roadmap summary to `anthropic` and
   gets back a verdict. The first line is one word, `MATCH` or `NONE`. When it
   says `MATCH`, the rest of the reply is a short issue body.
3. `file_issue` runs only when the verdict is not `NONE`. It opens an issue in
   `issues_repo` with the model's reasoning as the body.

## Runners

- `github` (op `releases`): GET a repo's release feed, returns `tag`, `name`,
  `published_at`, and `body` per release.
- `anthropic`: judges the releases against the roadmap. Swap in `openai` if you
  prefer; the params (`system`, `prompt`) are the same.
- `github` (op `create_issue`): files the triage issue with `title`, `body`, and
  `labels`.

## LLM output is plain text, so gate on a sentinel

The LLM returns plain text, not structured JSON. There is no typed match
field to read. So the system prompt pins a one-word verdict on the first line
and the `file_issue` condition checks it:

```toml
condition = "${{ compare.output }} != 'NONE'"
```

On a `NONE` day the prompt tells the model to write nothing after that first
line, so the output is exactly `NONE` and the condition skips the issue step.

## Reality check: these issues are drafts

"Does this release fit our roadmap" is an LLM judgment, and it will throw false
positives (Phase 8 in `.agents/spec/13-roadmap.md` calls this out). The prompt
even tells the model to lean toward `MATCH` when unsure. So treat every filed
issue as a draft a human reviews and closes if it is noise, not as truth. That
is the whole point of filing into an issues repo instead of acting on the
verdict directly.

## Fan-out limit: no foreach yet

Each `github` op=releases action pulls one repo. The schema has no `foreach`
(it is reserved for a later phase in `.agents/spec/02-workflow-schema.md`), so
you cannot loop one action over the `repos` input. This file watches two repos
through two parallel actions and fans them into one compare step.

To watch another repo:

1. Copy a `release_*` action, rename it, and point its `repo` at the new entry.
2. Add the new action's name to `compare`'s `depends_on`.
3. Add a line to `compare`'s `prompt` that interpolates the new action's output.

The `repos` input documents the intended watched set. It is there so the file
reads as one watched group today and converts cleanly to a `foreach` once the
schema gains it. Until then the actions name their repos directly.

## S6 note

The issues repo is `CairnOS-Issues`, corrected from the vision's
`CairOS-Issues` (S6 in `.agents/spec/14-open-decisions.md`). The default
`issues_repo` input reflects the corrected name. Any -task migration is
tracked as a separate concern and is not wired into this workflow.

## Config

```toml
[runner.anthropic]
model = "claude-sonnet-4-5"

[runner.github]
# base_url defaults to https://api.github.com; set it for a GitHub
# Enterprise host.
```

Environment:

- `CAIRNOS_RUNNER_GITHUB_TOKEN` for the `github` runner. Public release reads
  work without it at a lower rate limit, but `create_issue` needs a token with
  write access to `issues_repo`.
- `CAIRNOS_RUNNER_ANTHROPIC_API_KEY` for the compare step (or
  `CAIRNOS_RUNNER_OPENAI_API_KEY` if you swap `compare` to `openai`).

## Run it

```sh
cairnos validate examples/agents/release_monitor/release_monitor.toml

cairnos run examples/agents/release_monitor/release_monitor.toml \
  --input issues_repo=chadit/CairnOS-Issues

# put it on the daily schedule
cairnos schedule add examples/agents/release_monitor/release_monitor.toml
```
