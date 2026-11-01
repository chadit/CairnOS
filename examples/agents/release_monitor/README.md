# Release Monitor

A CairnOS workflow built on top of the engine. Once a day it pulls
release feeds from a few upstream repos, asks an LLM whether any release lines up
with the CairnOS roadmap, and files an issue into your issues repo when one does.
The issue is a draft for a human to triage, not an automatic decision.

On a quiet day nothing gets filed: the compare step returns a `NONE` sentinel
and the issue step is skipped, not failed. Same pattern career_scout uses with
`NO_MATCHES`.

## What it does

1. `releases_otel` and `releases_mcp` each pull one repo's recent releases through
   the `github` runner. They share no dependency, so the engine runs them in
   parallel.
2. `format_releases` renders both release lists into one text block with a
   short `code` script, because a reference embedded in prompt text has to
   resolve to a single value, not a list.
3. `compare` feeds that text plus a roadmap summary to `anthropic` and gets back
   a verdict. The first line is one word, `MATCH` or `NONE`. When it says
   `MATCH`, the rest of the reply is a short issue body.
4. `file_issue` runs only when the verdict is not `NONE`. It opens an issue in
   `issues_repo` with the model's reasoning as the body.

## Runners

- `github` (op `releases`): GET a repo's release feed, returns `tag`, `name`,
  `published_at`, and `body` per release.
- `code`: runs the inline Starlark script that turns the release lists into
  text. Its `releases` param maps each repo name to that repo's list.
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
positives. The prompt even tells the model to lean toward `MATCH` when unsure.
So treat every filed issue as a draft a human reviews and closes if it is
noise, not as truth. That is the whole point of filing into an issues repo
instead of acting on the verdict directly.

## Fan-out: one action per repo

Each `github` op=releases action pulls one repo, so this file watches two repos
through two parallel actions and fans them into one formatting step.

To watch another repo:

1. Copy `releases_otel`, rename it, and point its `repo` at the new entry.
2. Add the new action's name to `format_releases`'s `depends_on`.
3. Add a line to `format_releases`'s `releases` table that maps the repo name to
   the new action's output, such as
   `"owner/name" = "${{ releases_new.output }}"`.

The `repos` input documents the intended watched set. The actions name their
repos directly, so editing `repos` alone changes nothing they fetch. A
`[action.<name>.foreach]` table can fan one action over `repos` instead, with
`${{ item }}` as each repo.

## Where the issues go

`issues_repo` is required and has no default, so a run without it stops before
any step does work instead of failing on the first day a release matches. Pass
your own repo as `owner/name` with `--input`, in place of the
`your-org/your-issues-repo` placeholder this README uses. A scheduled run
supplies no inputs, and `cairnos schedule add` refuses a file whose required
input has no default, so before you schedule your copy, replace its
`[workflow.inputs.issues_repo]` table with one that has a `default`:

```toml
[workflow.inputs.issues_repo]
type        = "string"
required    = true
default     = "your-org/your-issues-repo"
description = "owner/name of the issues repo that receives triage drafts. The github token needs write access to it."
```

## Config

```toml
[runner.anthropic]
model = "claude-sonnet-5"

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
  --input issues_repo=your-org/your-issues-repo

# put it on the daily schedule, once issues_repo has a default in your copy
cairnos schedule add examples/agents/release_monitor/release_monitor.toml
```
