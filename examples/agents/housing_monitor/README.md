# Housing Monitor

Pulls listings from a real-estate API every morning at 7:30 (a plain cron
schedule instead of an `@` shorthand), filters them by price with an
in-process Go function, and emails you when anything survives the filter.
No matches means the alert is skipped, not failed.

## Where the listings come from (read this first)

This workflow does not scrape listing sites, and you shouldn't make it.
Scraping Zillow, Redfin, or similar violates their terms of service (S5 in
`.agents/spec/14-open-decisions.md`). The `listings_url` input must be a
source you may query: an official API you hold a key for, an MLS feed you
have access to, or a service you run yourself. The fetch step is a plain
`webhook` POST; swap it for an `mcp_client` action if your source speaks
MCP.

## Runners

- `fetch_listings` uses `webhook`: POST your region and price bound to the
  API, the response body becomes the action output. Idempotent with
  exponential-backoff retry.
- `filter_listings` uses `local_fn`: calls the registered `listing_filter`
  function.
- `alert` uses `email`: gated on `filter_listings.output.count > 0`.

## The filter function

The operator registers `listing_filter` at build time (code can't live in
config). The contract: read `listings` (the raw API response) and
`max_price` from the task params, return a map with a `count` (number of
matches) and a `summary` (the email body). The condition reads `count` and
the alert body reads `summary`, so both keys are required:

```go
funcs := map[string]builtins.Func{
    "listing_filter": func(
        _ context.Context, task *runner.Task,
    ) (any, error) {
        listings, _ := task.Params["listings"].(string)
        // parse, drop anything over max_price, format a summary
        matches := filter(listings)
        return map[string]any{
            "count":   len(matches),
            "summary": format(matches),
        }, nil
    },
}
registry.Register(builtins.LocalFnName, builtins.LocalFnFactory(funcs))
```

## Config

```toml
[runner.email]
host = "smtp.example.com"
port = "587"
from = "cairnos@example.com"
user = "cairnos@example.com"
```

Environment:

- `CAIRNOS_RUNNER_EMAIL_PASSWORD` for SMTP auth.

## Run it

A scheduled run supplies no inputs, and `cairnos schedule add` refuses a file
whose required input has no default, so before you schedule your copy, give
`listings_url`, `region`, and `notify_to` each a `default`.

```sh
cairnos validate examples/agents/housing_monitor/housing_monitor.toml

cairnos run examples/agents/housing_monitor/housing_monitor.toml \
  --input listings_url=https://api.example.com/listings \
  --input region=austin-tx \
  --input notify_to=me@example.com

# put it on the 7:30am schedule, once each required input has a default
cairnos schedule add examples/agents/housing_monitor/housing_monitor.toml
```
