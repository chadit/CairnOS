# Career Scout

Checks a job feed every morning, scores the postings against your resume,
and emails you a digest when something fits. On a quiet day the email step
is skipped, not failed: the scoring prompt returns a `NO_MATCHES` sentinel
and the condition gates on it.

## Where the jobs come from (read this first)

This workflow does not scrape anything, and you shouldn't point it at a
site that forbids it. Scraping LinkedIn, Indeed, or other listing sites
violates their terms of service (S5 in `.agents/spec/14-open-decisions.md`).
The `feed_url` input must be your own source: an official API you hold a
key for, a job board that permits API access, or an aggregator you run
yourself. The fetch step is a plain `webhook` POST so any endpoint you own
works; swap it for an `mcp_client` action if your source speaks MCP.

## Runners

- `fetch_jobs` uses `webhook`: POST to your feed, the response body becomes
  the action output. Marked idempotent with exponential-backoff retry.
- `score_jobs` uses `openai`: scores postings against `resume_summary`
  through a system prompt.
- `send_digest` uses `email`: sends the ranked list. Condition-gated, runs
  only when the scorer found matches.

## Config

```toml
[runner.openai]
model = "gpt-4o"

[runner.email]
host = "smtp.example.com"
port = "587"
from = "cairnos@example.com"
user = "cairnos@example.com"
```

Environment:

- `CAIRNOS_RUNNER_OPENAI_API_KEY` for the scoring step.
- `CAIRNOS_RUNNER_EMAIL_PASSWORD` for SMTP auth.

## Run it

```sh
cairnos validate examples/agents/career_scout/career_scout.toml

cairnos run examples/agents/career_scout/career_scout.toml \
  --input feed_url=https://feeds.example.com/jobs \
  --input resume_summary="Senior Go engineer, distributed systems" \
  --input notify_to=me@example.com

# put it on the daily schedule
cairnos schedule add examples/agents/career_scout/career_scout.toml
```
