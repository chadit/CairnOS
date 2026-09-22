# Usage Report

Every hour, asks each model CLI login on a build machine how much of its
rate-limit windows it has spent, then posts the table into one chat thread. When
a login fails to report, a second post in the same thread pings whoever is on
call. Nothing calls a model, so a run costs no tokens.

## Runners

- `windows` uses `usage`: probes every launcher in `[runner.usage]` on
  `build_box` over ssh. A login that fails is recorded in the report and the
  others still answer.
- `post` uses `message`: posts `windows.output.summary`, a Markdown table in a
  code fence, to `#usage` as the `ops` identity.
- `alert` uses `message`: gated on `windows.output.has_errors`, so it's skipped,
  not failed, when every login answered. It waits on `post` so the first run
  opens the thread once instead of both actions racing to open it.

Both posts share `thread_key = "usage:${{ system.hostname }}"`. The first run
opens a thread and CairnOS stores its root post id. Every later run replies
there, and a root someone deleted is replaced on the next post. The hostname in
the key belongs to the machine running serve, so two serves running this file
keep separate threads.

## Why it talks to the CLIs

The runner speaks each CLI's own stdio protocol, the one Claude Code and Codex
expose to their SDKs, through your launcher. It never reads a credential file or
calls a vendor endpoint itself, because Anthropic reserves the Claude Code OAuth
login for Claude Code, and letting the CLI answer keeps every request inside the
tool that owns the login.

## Config

The host the probe runs on. `wrapper` sets up PATH on the far side, since a
non-login ssh shell rarely has the CLIs on it:

```toml
[host.build_box]
type    = "worker"
ssh     = "build-box"
wrapper = "/usr/local/bin/agent-env"
```

The launchers to probe, grouped by the protocol they speak:

```toml
[runner.usage]
claude = ["~/.local/bin/claude-agent", "~/.local/bin/claude-work"]
codex  = ["~/.local/bin/codex-agent"]
```

`claude_gap` defaults to 65 seconds. Claude Code caches its usage answer for
about a minute and doesn't key that cache by login, so two Claude logins sharing
one config directory are probed a gap apart. With the two Claude launchers above,
a run spends one gap waiting and finishes in a little over a minute; Codex never
waits. Set `claude_gap = 0` if each launcher has a config directory of its own.

The chat identity the posts go out as, from `[bridge.mattermost]` in the main
README. The workflow names it `ops`:

```toml
[bridge.mattermost]
url  = "https://mm.example.com"
team = "main"

[bridge.mattermost.identity.ops]
username = "ops-agent"
```

Environment:

- `CAIRNOS_BRIDGE_MATTERMOST_TOKEN` for the bot account.

## Run it

```sh
cairnos validate examples/agents/usage_report/usage_report.toml

# the table, right now, without posting anything
cairnos usage --host build_box

# a one-off run of the whole workflow
cairnos run examples/agents/usage_report/usage_report.toml --input mention=@me

# the hourly schedule, posted by serve
cairnos schedule add examples/agents/usage_report/usage_report.toml
```

Only serve holds the chat identities, so a `cairnos run` from a shell probes the
logins and records the report, but its two message actions fail with
`message.unavailable`. The scheduled run under serve is the one that posts.
