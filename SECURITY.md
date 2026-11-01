# Security Policy

## Supported versions

| Version | Supported |
| --- | --- |
| `main` | Yes |
| Latest release | Yes |
| Older releases | No |

Fixes land on `main` and ship in the next release. Older releases do not get
backports, so `cairnos update` to the latest release is how a host picks up a fix.

## Reporting a vulnerability

Do not open a public issue, pull request, or discussion for a security problem.
Report it privately through GitHub private vulnerability reporting:

<https://github.com/chadit/CairnOS/security/advisories/new>

Include what you can of the following:

- The release tag or commit you tested. `go version -m <path-to-cairnos> | grep vcs.revision` prints the commit a binary was built from.
- The config and workflow TOML needed to reproduce it, with secrets removed.
- The steps you took and what an attacker gains.

## What to expect

- Acknowledgement within 3 business days.
- A first assessment within 10 business days: whether the report is confirmed, its severity, and the planned fix.
- A fix within 30 days for a high-severity issue and within 90 days for anything else. If a fix needs longer, you hear why and the new date.
- Once the fix ships, a published GitHub security advisory that credits you, unless you ask not to be named.

## Scope

These parts of CairnOS carry most of the risk, and reports about them are in scope:

- **The exec runner.** `[runner.exec]` starts host processes as the user running `cairnos serve`, limited to the actions its `allow_actions` list names. A way to run an action the list does not name, or to get untrusted input such as a webhook payload or chat message into a command line, its arguments, or its working directory, is in scope.
- **Secrets.** API keys, webhook URLs, and Bitwarden credentials come from the environment or the Bitwarden CLI. Any path that writes a secret into logs, run output, API responses, metrics, the store, or a process listing is in scope.
- **The API.** gRPC (port 7777) and the JSON/HTTP gateway (port 8080) are the control surface. An authentication bypass, a token that reaches data or actions it should not, or a request that gets past `[auth] enabled = true` is in scope.

Out of scope:

- A deployment that exposes its ports to an untrusted network with auth turned off. The README says to keep the ports on localhost or a private network and to turn auth on first.
- What an allowed exec action does once it runs. It runs with the serve user's permissions and environment by design, with no container around it.
- An attack that needs write access to the config file or the account that runs serve.
- A vulnerability in a dependency or an external CLI that CairnOS does not reach. Report those upstream.
