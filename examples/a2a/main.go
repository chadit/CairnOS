// Command a2a-client is the reference Agent-to-Agent consumer: it drives a
// CairnOS node through the whole A2A loop over plain HTTP (the gateway
// fallback, so an agent that speaks only HTTP needs no gRPC stack). It fetches
// the signed agent card and verifies it, logs in for a bearer token, creates a
// task, and streams the task updates to completion.
//
// Run a local node first, with [a2a].enabled = true and an access key holding
// the a2a:task scope:
//
//	$ cairnos keys add --label demo --scopes a2a:task
//	$ cairnos serve
//
// Then point this client at it:
//
//	$ go run ./examples/a2a \
//	    -base http://127.0.0.1:8080 \
//	    -key  ck_... \
//	    -skill hello
//
// The skill names a workflow in the node's workflows directory; "hello" is the
// two-action sample the test fixtures use.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/a2a"
)

// httpTimeout bounds the non-streaming calls.
const httpTimeout = 30 * time.Second

// streamTimeout caps the updates stream generously: long enough for a real
// task to finish, short enough that a hung server does not block the demo
// forever. ctx (interrupt-aware) ends it sooner on Ctrl-C.
const streamTimeout = 10 * time.Minute

// statusClassDivisor maps an HTTP status to its leading digit: status/100
// equals 2 for the whole 2xx success class.
const statusClassDivisor = 100

// successClass is the leading digit shared by every 2xx response.
const successClass = 2

func main() {
	base := flag.String("base", "http://127.0.0.1:8080", "node HTTP base URL (the gateway address)")
	key := flag.String("key", "", "access key (ck_...) to exchange for a token")
	skill := flag.String("skill", "hello", "skill (workflow name) to run")

	flag.Parse()

	os.Exit(realMain(*base, *key, *skill))
}

// realMain owns the signal context and the deferred cleanup, returning an exit
// code so main is one os.Exit call. Keeping the defer here means it actually
// runs, which it would not under a log.Fatal in main.
func realMain(base, key, skill string) int {
	if key == "" {
		log.Println("a2a-client: -key is required (run `cairnos keys add --scopes a2a:task`)")

		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, base, key, skill); err != nil {
		log.Printf("a2a-client: %v", err)

		return 1
	}

	return 0
}

// run walks the full A2A loop and returns the first error that breaks it.
func run(ctx context.Context, base, key, skill string) error {
	card, err := fetchCard(ctx, base)
	if err != nil {
		return err
	}

	fmt.Printf("card: %s %s (%s), endpoint %s\n", card.Name, card.Version, card.Protocol, card.Endpoint)
	fmt.Printf("card signature verified against its embedded %s key\n", card.Signing.Alg)

	token, err := login(ctx, base, key)
	if err != nil {
		return err
	}

	fmt.Println("logged in; got a bearer token")

	taskID, err := createTask(ctx, base, token, skill)
	if err != nil {
		return err
	}

	fmt.Printf("created task %s for skill %q\n", taskID, skill)

	return streamUpdates(ctx, base, token, taskID)
}

// fetchCard fetches the well-known card and verifies its signature against its
// own embedded public key. A real consumer pins that key on first contact and
// passes it to Verify on every later fetch; this demo verifies self-consistency,
// which still rejects a card tampered with in transit.
func fetchCard(ctx context.Context, base string) (*a2a.Card, error) {
	body, err := get(ctx, base+a2a.WellKnownPath, "")
	if err != nil {
		return nil, fmt.Errorf("fetch card: %w", err)
	}

	var card a2a.Card
	if err := json.Unmarshal(body, &card); err != nil {
		return nil, fmt.Errorf("decode card: %w", err)
	}

	if err := a2a.Verify(&card, nil); err != nil {
		return nil, fmt.Errorf("verify card: %w", err)
	}

	return &card, nil
}

// login exchanges the access key for a bearer token at the card's login route.
func login(ctx context.Context, base, key string) (string, error) {
	payload, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		return "", fmt.Errorf("marshal login: %w", err)
	}

	body, err := post(ctx, base+"/v1/login", "", payload)
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}

	var resp struct {
		Token string `json:"token"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decode login: %w", err)
	}

	if resp.Token == "" {
		return "", errNoToken
	}

	return resp.Token, nil
}

// createTask starts a run by naming the skill, returning the new task id.
func createTask(ctx context.Context, base, token, skill string) (string, error) {
	payload, err := json.Marshal(map[string]any{"skill": skill})
	if err != nil {
		return "", fmt.Errorf("marshal create: %w", err)
	}

	body, err := post(ctx, base+"/v1/a2a/tasks", token, payload)
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}

	var resp struct {
		Task struct {
			TaskID string `json:"task_id"`
		} `json:"task"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decode create: %w", err)
	}

	if resp.Task.TaskID == "" {
		return "", errNoTaskID
	}

	return resp.Task.TaskID, nil
}

// streamUpdates reads the newline-delimited update stream the gateway emits for
// the task and prints each one until the stream closes.
func streamUpdates(ctx context.Context, base, token, taskID string) error {
	url := base + "/v1/a2a/tasks/" + taskID + "/updates"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("build updates request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	// A generous timeout, not the short one the other calls use: the stream
	// stays open for the task's whole lifetime, and ctx (interrupt-aware) ends
	// it sooner on Ctrl-C.
	resp, err := (&http.Client{Timeout: streamTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("open updates stream: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		fmt.Printf("update: %s\n", line)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read updates stream: %w", err)
	}

	fmt.Println("task stream closed")

	return nil
}

// get issues a GET with an optional bearer token and returns the body, failing
// on any non-2xx response so the caller sees the node's error envelope.
func get(ctx context.Context, url, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	return do(req, token)
}

// post issues a POST with a JSON body and an optional bearer token.
func post(ctx context.Context, url, token string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	return do(req, token)
}

// do sends one request under a bounded client and returns the body, turning a
// non-2xx status into an error carrying the node's response.
func do(req *http.Request, token string) ([]byte, error) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", req.URL, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode/statusClassDivisor != successClass {
		return nil, fmt.Errorf("%w: %s returned %d: %s", errStatus, req.URL, resp.StatusCode, body)
	}

	return body, nil
}
