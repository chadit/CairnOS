package a2a_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/chadit/CairnOS/internal/a2a"
	a2av1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/a2a/v1"
	"github.com/chadit/CairnOS/internal/auth"
)

// jsonContentType labels JSON request and response bodies.
const jsonContentType = "application/json"

// httpTimeout bounds every gateway request in these tests.
const httpTimeout = 30 * time.Second

// authedCtx attaches a bearer token the way a native gRPC client would.
func authedCtx(t *testing.T, token string) context.Context {
	t.Helper()

	return metadata.AppendToOutgoingContext(t.Context(), "authorization", bearerPrefix+token)
}

func TestA2AScopeEnforced(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true})

	// A token without a2a:task cannot create a task.
	readOnly := fix.token(t, auth.ScopeReadRun)

	_, err := fix.a2a.CreateTask(authedCtx(t, readOnly), &a2av1.CreateTaskRequest{Skill: helloSkill})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("CreateTask without a2a:task = %v, want PermissionDenied", status.Code(err))
	}

	// A token with a2a:task can.
	scoped := fix.token(t, auth.ScopeA2ATask)

	resp, err := fix.a2a.CreateTask(authedCtx(t, scoped), &a2av1.CreateTaskRequest{Skill: helloSkill})
	if err != nil {
		t.Fatalf("CreateTask with a2a:task: %v", err)
	}

	if resp.GetTask().GetTaskId() == "" {
		t.Fatal("CreateTask with a2a:task returned no task id")
	}
}

func TestA2ATokenlessRejected(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true})

	_, err := fix.a2a.ListTasks(t.Context(), &a2av1.ListTasksRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("tokenless ListTasks = %v, want Unauthenticated", status.Code(err))
	}
}

func TestWellKnownCardServedUnauthenticated(t *testing.T) {
	t.Parallel()

	// Auth is on, yet the card must still serve without a token: discovery is
	// public, and the signature is the trust mechanism.
	fix := startFixture(t, fixtureOptions{authEnabled: true})

	code, body := httpGet(t, fix.httpBase+a2a.WellKnownPath)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", a2a.WellKnownPath, code)
	}

	var card a2a.Card
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("decode served card: %v", err)
	}

	if card.Protocol != a2a.ProtocolVersion {
		t.Fatalf("card protocol = %q, want %q", card.Protocol, a2a.ProtocolVersion)
	}

	// The served card verifies against the key the fixture signed it with.
	if err := a2a.Verify(&card, fix.cardKey); err != nil {
		t.Fatalf("served card does not verify against the fixture key: %v", err)
	}
}

func TestGatewayCreateTaskRoundTrip(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})

	code, body := httpPost(t, fix.httpBase+"/v1/a2a/tasks", `{"skill":"hello"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /v1/a2a/tasks = %d (%s), want 200", code, body)
	}

	var resp struct {
		Task struct {
			TaskID string `json:"task_id"`
			Skill  string `json:"skill"`
		} `json:"task"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode create-task response: %v", err)
	}

	if resp.Task.TaskID == "" {
		t.Fatal("HTTP CreateTask returned no task id")
	}

	if resp.Task.Skill != helloSkill {
		t.Fatalf("HTTP task skill = %q, want %q", resp.Task.Skill, helloSkill)
	}

	// Stream the updates route to completion so the run record is persisted
	// before the GET below; CreateTask returns the moment the run id is minted,
	// a hair ahead of the store write, so an instant read would race it.
	drainHTTPUpdates(t, fix.httpBase+"/v1/a2a/tasks/"+resp.Task.TaskID+"/updates")

	// The same task reads back over the HTTP GET route.
	getStatus, getBody := httpGet(t, fix.httpBase+"/v1/a2a/tasks/"+resp.Task.TaskID)
	if getStatus != http.StatusOK {
		t.Fatalf("GET /v1/a2a/tasks/{id} = %d (%s), want 200", getStatus, getBody)
	}
}

// drainHTTPUpdates reads the gateway's newline-delimited TaskUpdates stream to
// EOF, so the caller knows the run finished and its record is durable.
func drainHTTPUpdates(t *testing.T, url string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build updates request: %v", err)
	}

	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain updates stream: %v", err)
	}
}

// TestEndToEndCardLoginCreateStream is the spec's acceptance loop: fetch and
// verify the card, log in, CreateTask, stream TaskUpdates to completion.
func TestEndToEndCardLoginCreateStream(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true})

	// 1. Fetch and verify the card.
	code, body := httpGet(t, fix.httpBase+a2a.WellKnownPath)
	if code != http.StatusOK {
		t.Fatalf("fetch card = %d, want 200", code)
	}

	var card a2a.Card
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}

	if err := a2a.Verify(&card, fix.cardKey); err != nil {
		t.Fatalf("verify card: %v", err)
	}

	// 2. Log in for a token, then 3. CreateTask, then 4. stream to completion.
	token := fix.token(t, auth.ScopeA2ATask)

	created, err := fix.a2a.CreateTask(authedCtx(t, token), &a2av1.CreateTaskRequest{Skill: helloSkill})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	stream, err := fix.a2a.TaskUpdates(authedCtx(t, token), &a2av1.TaskUpdatesRequest{TaskId: created.GetTask().GetTaskId()})
	if err != nil {
		t.Fatalf("TaskUpdates: %v", err)
	}

	updates := collectUpdates(t, stream)
	last := updates[len(updates)-1]

	if last.GetTask().GetState() != a2av1.TaskState_TASK_STATE_SUCCEEDED {
		t.Fatalf("end-to-end final state = %v, want SUCCEEDED", last.GetTask().GetState())
	}
}

// httpGet fetches one route and returns status plus body.
func httpGet(t *testing.T, url string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return resp.StatusCode, payload
}

// httpPost posts a JSON body and returns status plus body.
func httpPost(t *testing.T, url, body string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set("Content-Type", jsonContentType)

	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return resp.StatusCode, payload
}
