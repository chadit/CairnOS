# CairnOS Builder (web)

The visual builder for CairnOS. It is a static single-page app that talks to
the CairnOS HTTP gateway. It builds and edits workflows, starts runs and
watches them stream, and reviews tuning suggestions.

It is a pure API client. It holds no private path into the engine. Everything
it does goes through the same public gateway the CLI and agents use, so if the
UI needs something, that something is a public API method. See
`.agents/spec/12-ui-and-governance.md`, "the UI is a client, not a second
engine."

## Develop

The gateway must be running. Start it with `cairnos serve` (the default HTTP
bind is `:8080`).

```bash
npm ci
npm run dev
```

The dev server runs on `http://localhost:5173` and proxies every `/v1` call to
`http://localhost:8080`. Point it elsewhere with `CAIRNOS_GATEWAY`:

```bash
CAIRNOS_GATEWAY=http://localhost:9090 npm run dev
```

## Scripts

| Command          | What it does                                              |
| ---------------- | --------------------------------------------------------- |
| `npm run dev`    | Start the dev server with the gateway proxy.              |
| `npm run build`  | Type-check, then build the static bundle into `dist/`.    |
| `npm test`       | Run the Vitest suite once. No real network; MSW mocks it. |
| `npm run lint`   | Run ESLint over the whole tree.                           |
| `npm run format` | Format with Prettier.                                     |

There is no Makefile here on purpose; the root Makefile is untouched. Drive the
web build through these npm scripts. `npm run build`, `npm test`, and
`npm run lint` are the three a CI job needs.

## Auth

Sign in with an access key (the plaintext key `cairnos keys add` prints once).
The app exchanges it for a token through `POST /v1/login` and keeps the token
in memory and `localStorage`, attaching it as `Authorization: Bearer <token>`.

Auth is optional in CairnOS. With auth off, `Health` and the read calls answer
without a token, so the login screen offers a "Continue without signing in"
path. If a guarded call returns 401, the app drops the token and returns to the
login screen.

### Token storage and the CSP (M2)

The token lives in `localStorage`, not an httpOnly cookie. That was a deliberate
call (decision M2 in `.agents/spec/14-open-decisions.md`): the access key stays
the one credential to manage, the token rides a header so there is no CSRF
surface, and it works on any topology with no server change. The trade is that a
script running on the page could read the token, so two things cap the risk:

- The token is short-lived (1h) and revocable through the `jti`/`sub` denylist,
  so a stolen token's window is an hour on a credential an admin can kill.
- The production build ships a strict Content-Security-Policy. `vite.config.ts`
  injects it into `dist/index.html` at build time only (the dev server needs
  inline scripts and a websocket for HMR, so it stays unconstrained):
  `default-src 'self'; script-src 'self'; connect-src 'self'; object-src 'none'`
  and friends. With nothing but the app's own bundle allowed to run and only
  same-origin connections allowed, the surface for token theft is small.

Two operator notes follow from `connect-src 'self'`:

- **Serve the SPA same-origin with the API** (the gateway serving `dist/`, or one
  reverse proxy in front of both). The dev proxy already does this for `/v1`.
- A **cross-origin** deployment (SPA on a CDN, API elsewhere) must widen
  `connect-src` to name the API origin, and should add a
  `Content-Security-Policy: frame-ancestors 'none'` response header at the static
  host, since a meta-tag CSP cannot set `frame-ancestors`.

Closing the read-the-token gap entirely means moving to an httpOnly cookie, which
adds CSRF handling and a gateway `Set-Cookie` hook on `/v1/login`; M2 keeps that
documented as the upgrade if it is ever needed.

## The TOML round-trip

A workflow you build on the canvas exports to TOML the CLI runs, and re-imports
with a **semantic** round-trip: same actions, same `depends_on` edges, same
params, same agent settings, same behavior. Comments and exact formatting are
**not** preserved. This matches the Phase 5a acceptance criterion in
`.agents/spec/12-ui-and-governance.md`.

What this means in practice:

- Export rewrites the file in a canonical form: tables sorted, stamped defaults
  (`version = "1"`, `on_error = "halt"`, `kind = "task"`) omitted because the
  parser re-stamps them. Two equivalent workflows produce byte-identical TOML.
- Your hand-written comments and spacing do not survive a round-trip through the
  builder. The behavior does.

The mapper lives in `src/workflow/`:

- `model.ts` mirrors `internal/workflow/workflow.go`: the same fields and the
  same defaults.
- `parse.ts` maps TOML to the model, mirroring `internal/workflow/parse.go`
  (default stamping, reserved-key rejection). It does not re-run the engine's
  deep validation (cycles, reference-vs-`depends_on`); that is the server's job
  through `ValidateWorkflow`.
- `emit.ts` writes the model back to canonical TOML.
- `graph.ts` maps the model to and from reactflow nodes and edges. A node is an
  action; an edge is a `depends_on` relationship. `graphToWorkflow` rebuilds
  `depends_on` purely from edges, so the canvas is the single source of truth
  for the DAG.
- `normalize.ts` produces the comparison form that proves the round-trip: it
  collapses stamped defaults and the order of `depends_on`, `tools`, and table
  keys, leaving only behavior.

The round-trip is proven mechanically in `src/workflow/roundtrip.test.ts`: for a
table of fixtures (including the real `hello.toml` from `cairnos init` and an
agent action), `parse -> emit -> parse` and the full `parse -> graph -> emit ->
parse` both normalize to the same workflow.

## Chat

The Chat page is a client over the gateway's `ChatService` (the D9
conversational surface). The sidebar lists sessions and starts a new one with a
personality picker; the main column shows the transcript and a composer.

Two of the chat calls stream. `SendMessage` and `ConfirmAction` return the same
newline-delimited JSON the run stream uses, one `{result|error}` `ChatEvent` per
line, so `src/api/chat.ts` reuses the stream-consumer loop from `stream.ts`. The
unary calls (`CreateSession`, `ListSessions`, `ListMessages`) go through the
shared `request`, so they get the bearer-token attach, the error-envelope
unwrap, and the 401-to-login behavior for free.

`useChatStream` folds each `ChatEvent` into the transcript:

- `assistant_chunk` appends to the current assistant bubble, so a streamed reply
  grows in one bubble rather than spawning one per chunk.
- `tool_call` adds a quiet "used `<tool>`" note.
- `run_started` and `run_finished` add and update a run-status row.

### The confirmation gate

When the stream sends a `confirmation_required` event, the agent wants to run a
workflow. The fold does not run it. It parks the `WorkflowProposal` in state and
the page renders a `ConfirmationCard` with the workflow name, its inputs, and the
estimated budget, plus Approve and Deny buttons. Nothing runs until the user
clicks. Approve calls `ConfirmAction(approve=true)` and folds the follow-on run
events; Deny calls `ConfirmAction(approve=false)` and no workflow runs. There is
no code path from a parked proposal to a `ConfirmAction` call except the two
buttons, so a stray proposal cannot auto-run. `ChatPage.test.tsx` pins this: the
gate test asserts `ConfirmAction` is never called and no run row appears until a
button is clicked.

The chat feature lives in `src/chat/` (the stream hook, the transcript, the
sidebar, the composer, the confirmation card) with the page in
`src/pages/ChatPage.tsx`.

## Why the API types are hand-written

The types in `src/api/types.ts` are hand-written against
`internal/api/gen/openapi/cairnos.swagger.json` rather than generated. The
swagger is OpenAPI 2.0, and several fields are freeform `object` (run inputs,
action output, error details), which a generator renders as opaque `unknown`.
The streaming `RunWorkflow` response also has no first-class swagger shape. Hand
typing these (a recursive `JsonValue`, a `StreamEnvelope<RunEvent>`) keeps the
call sites honestly typed. The types track the swagger by hand; when a proto
field changes, update `types.ts` to match.

## Project layout

```text
src/
  api/        HTTP client, typed endpoints, the streaming consumer, hooks
  auth/       auth context and the bearer-token gate
  builder/    the canvas store, the reactflow node, inspector, TOML panel
  chat/       the chat-stream hook, transcript, sidebar, composer, confirm card
  components/ shared shell, error banner, spinner
  pages/      one component per route
  runs/       the run-stream hook and status badge
  workflow/   the model and the TOML <-> graph <-> TOML mapper
  test/       MSW server, handlers, fixtures, the render helper
```

## Phase 5b is not here

Budgets and pause/resume controls need engine enforcement that ships in Phase
5b. The Governance page labels them as previews and changes no server behavior.
The one real control today is `CancelRun`, on a run's detail page. The app does
not fake a spend cap.
