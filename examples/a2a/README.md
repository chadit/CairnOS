# A2A client example

A small reference consumer that drives a CairnOS node through the whole
Agent-to-Agent loop over plain HTTP: it fetches the signed agent card and
verifies it, logs in for a bearer token, creates a task, and streams the task
updates to completion.

It talks to the node through the HTTP gateway only, no gRPC stack. That's the
fallback path from spec 09: an agent that speaks just HTTP gets the same
surface as a native gRPC client, because the gateway bridges every A2A method.

## What it does, step by step

1. `GET /.well-known/agent.json` and verify the card's signature. The card is
   signed with the node's ed25519 key, so a tampered card fails verification.
2. `POST /v1/login` with an access key, getting back a short-lived bearer token.
3. `POST /v1/a2a/tasks` naming a skill (a workflow), getting back a task id.
4. `GET /v1/a2a/tasks/{id}/updates`, reading the newline-delimited update stream
   until the task finishes.

## Run it against a local node

Start a node with A2A turned on. In your config:

```toml
[a2a]
enabled = true
```

Mint an access key that holds the `a2a:task` scope, then serve:

```bash
$ cairnos keys add --label demo --scopes a2a:task
created key 01... (demo)

  ck_01..._...      # copy this, it's shown once

$ cairnos serve
gRPC listening on 127.0.0.1:7777
HTTP listening on 127.0.0.1:8080
a2a card published at http://127.0.0.1:8080/.well-known/agent.json
serving; interrupt to stop
```

Then point the client at it:

```bash
$ go run ./examples/a2a \
    -base  http://127.0.0.1:8080 \
    -key   ck_01..._... \
    -skill hello
card: cairnos 0.0.1 (a2a/1), endpoint grpc://127.0.0.1:7777
card signature verified against its embedded EdDSA key
logged in; got a bearer token
created task 01... for skill "hello"
update: {"task_id":"01...","type":"TASK_UPDATE_TYPE_STARTED"}
update: {"task_id":"01...","type":"TASK_UPDATE_TYPE_ACTION","step":{"name":"greet","status":"running"}}
...
update: {"task_id":"01...","type":"TASK_UPDATE_TYPE_FINISHED","task":{"state":"TASK_STATE_SUCCEEDED",...}}
task stream closed
```

The `hello` skill is the two-action sample workflow. Name any workflow in the
node's workflows directory with `-skill`.

## The same loop with curl

If you'd rather see the raw calls:

```bash
BASE=http://127.0.0.1:8080
KEY=ck_01..._...

# 1. Fetch the card (public, no token). Inspect signing.public_key and
#    signing.signature; a real consumer pins that key and verifies later fetches.
curl -s "$BASE/.well-known/agent.json"

# 2. Log in for a token.
TOKEN=$(curl -s -X POST "$BASE/v1/login" \
  -H 'Content-Type: application/json' \
  -d "{\"key\":\"$KEY\"}" | jq -r .token)

# 3. Create a task.
TASK=$(curl -s -X POST "$BASE/v1/a2a/tasks" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"skill":"hello"}' | jq -r .task.task_id)

# 4. Stream its updates until the task finishes.
curl -s -N "$BASE/v1/a2a/tasks/$TASK/updates" \
  -H "Authorization: Bearer $TOKEN"
```

## Vocabulary

A2A and CairnOS are the same machine under two names:

| A2A term | CairnOS term |
| --- | --- |
| task | run |
| skill | workflow name |
| params | workflow inputs |
| task state | run status |
| task update | run event |

So `CreateTask` starts a run, `GetTask` reads one, `ListTasks` lists them,
`CancelTask` stops one, and the update stream is the run's event stream.
