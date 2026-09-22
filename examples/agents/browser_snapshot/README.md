# Browser Snapshot

Open a web page through a containerized Playwright MCP server and summarize what
it says. No browser code runs on the host: the Playwright server lives in a
container, CairnOS talks to it over stdio through the `mcp_client` runner, and
the page snapshot comes back as text a normal LLM action reads.

The same `browser_nav` tool is shown two ways in one workflow. `snapshot` is a
plain task action that calls it directly. `investigate` is a `kind = "agent"`
action that lists it in `tools` and lets the loop decide when to browse. Both go
through the same runner, so both honor the same operator config.

## What it does

1. `snapshot` calls `browser_navigate` on the containerized Playwright server
   with the `url` input. One call returns the page's accessibility snapshot.
2. `summarize` feeds that snapshot to `anthropic` with the `question` input and
   gets back a plain-text answer.
3. `investigate` is the agent-driven equivalent. It runs after `summarize` and
   plans its own `browser_nav` call to answer the same question. It validates
   today and executes once the Phase 1c agent loop is wired.

## The browser_nav runner (paste into config.toml)

`browser_nav` is a named `mcp_client` instance. A workflow file only holds
`[workflow]` and `[action]` tables, so the runner block does not live in
`browser_snapshot.toml`. It goes in `~/.cairnos/config.toml`:

```toml
[runner.browser_nav]
type    = "mcp_client"
tool    = "browser_navigate"
command = [
  "podman", "run", "--rm", "-i",
  "mcr.microsoft.com/playwright/mcp:v0.0.77",
]
```

Docker is the same block with one word changed:

```toml
command = ["docker", "run", "--rm", "-i", "mcr.microsoft.com/playwright/mcp:v0.0.77"]
```

The tag is pinned on purpose. Never use `latest`: it drifts, and a browser
automation image that changes under you is a debugging trap. To bump it, read
the current release from
`https://mcr.microsoft.com/v2/playwright/mcp/tags/list`, pick the newest
`vX.Y.Z` tag, and change it in this README and your config together.

This block needs the `mcp_client` named-instance registration delivered by slice
1 of the `parity/mcp-import-fix` spec.
Without it, a `[runner.browser_nav]` block registers no runner and the run fails
to resolve `browser_nav`.

## One session per call, so this is single-shot

`mcp_client` opens a session, makes the call, and closes it, every `Execute`
(the runner's own doc comment spells this out). Browser state does not survive
from one action to the next. `browser_navigate` returns the snapshot in the same
call it navigates, which is why this example works as one shot. A click-through
flow that needs the page to stay open across calls waits on `mcp_client` session
pooling, already tracked as future work in the runner.

## Network posture: this container is online by design

The Playwright container has network access because that is its whole job: it
fetches pages. That is the opposite of the `sandbox_exec` runner, whose default
is `--network=none` precisely so an agent-callable shell can't reach the network
(see the `parity/sandbox-exec` spec). Keep
that difference in mind before you put `browser_nav` in an agent action's
`tools`: you are handing the loop a tool that reaches the public web. The same
agent posture the sandbox docs recommend applies here. Grant it deliberately,
scope the goal tightly, and read the transcript.

This example uses the stdio `command` transport, so the boundary is the spawned
container process, not an HTTP call, and the shared egress guard does not sit in
front of it. If you ever swap `command` for a `server_url` transport pointed at
a remote MCP endpoint, that endpoint rides the runner egress guard: a private or
loopback target is refused with `mcp_client.blocked_target` unless the block
sets `allow_private_targets = true`.

## Two consent gates

`investigate` shows both gates that have to line up before an agent loop can
touch the browser:

- The operator wires `[runner.browser_nav]` in config. Nothing a workflow author
  or a driver writes can register a tool that the operator did not.
- The agent action lists `browser_nav` in `tools`. There is no "all tools"
  value, so an agent only gets what the action names.

Same shape the `sandbox_exec` runner uses, where a tool call also needs the
agent action's name in the runner's `allow_actions`.

## Config

```toml
[runner.anthropic]
model = "claude-sonnet-5"

# plus the [runner.browser_nav] block shown above
```

Environment:

- `CAIRNOS_RUNNER_ANTHROPIC_API_KEY` for the `summarize` step and the
  `investigate` driver (or `CAIRNOS_RUNNER_OPENAI_API_KEY` if you swap them to
  `openai`).
- No env key for `browser_nav`: a stdio `command` runner has no request headers,
  so it carries no bearer token.

Pre-pull the image once so the first run does not pay the pull inside its
timeout:

```sh
podman pull mcr.microsoft.com/playwright/mcp:v0.0.77
# docker pull mcr.microsoft.com/playwright/mcp:v0.0.77
```

## Run it

```sh
cairnos validate examples/agents/browser_snapshot/browser_snapshot.toml

cairnos run examples/agents/browser_snapshot/browser_snapshot.toml \
  --input url=https://example.com \
  --input question="What is this page about?"
```
