package api_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/store/sqlite"
)

// jsonContentType is the gateway's response content type.
const jsonContentType = "application/json"

// httpTimeout bounds every gateway request in these tests.
const httpTimeout = 30 * time.Second

// newHTTPClient builds a client with an explicit timeout, long enough for
// the streaming route to finish.
func newHTTPClient() *http.Client {
	return &http.Client{Timeout: httpTimeout}
}

// httpGet fetches one gateway route and returns status code and body.
func httpGet(t *testing.T, url string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return resp.StatusCode, body
}

// httpPost posts a JSON body to one gateway route.
func httpPost(t *testing.T, url, body string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set("Content-Type", jsonContentType)

	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}

	return resp
}

func TestGatewayTypedRouteSuccess(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	code, body := httpGet(t, fix.httpBase+"/v1/health")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/health = %d (%s), want 200", code, body)
	}

	// Success is the typed message directly, no {status, data} wrapper.
	var health struct {
		Status string `json:"status"`
	}

	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("parse health body %q: %v", body, err)
	}

	if health.Status != statusOK {
		t.Fatalf("health body = %s, want status ok", body)
	}
}

func TestGatewayErrorEnvelope(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	// An undeclared input rejects the run with workflow.invalid before any
	// record exists; the gateway must answer 400 with the canonical envelope.
	resp := httpPost(t, fix.httpBase+"/v1/workflows/hello/runs", `{"inputs":{"bogus":1}}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for workflow.invalid", resp.StatusCode)
	}

	var envelope struct {
		Status string `json:"status"`
		Error  struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("parse error envelope: %v", err)
	}

	if envelope.Status != statusError {
		t.Fatalf("envelope status = %q, want error", envelope.Status)
	}

	if envelope.Error.Code != "workflow.invalid" {
		t.Fatalf("envelope code = %q, want workflow.invalid", envelope.Error.Code)
	}

	if envelope.Error.Message == "" {
		t.Fatal("envelope carried no message")
	}
}

func TestGatewayErrorEnvelopeNotFound(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	code, body := httpGet(t, fix.httpBase+"/v1/runs/no-such-run")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d (%s), want 404", code, body)
	}

	var envelope struct {
		Status string `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("parse error envelope %q: %v", body, err)
	}

	if envelope.Status != statusError || envelope.Error.Code != "api.run_not_found" {
		t.Fatalf("envelope = %s, want status error with code api.run_not_found", body)
	}
}

func TestGatewayStreamsParseableJSONLines(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	resp := httpPost(t, fix.httpBase+"/v1/workflows/hello/runs", `{}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)

	var lines [][]byte

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		lines = append(lines, append([]byte(nil), line...))
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}

	if len(lines) < 3 {
		t.Fatalf("stream produced %d lines, want at least run_started, transitions, run_finished", len(lines))
	}

	var sawFinished bool

	for _, line := range lines {
		// grpc-gateway wraps each streamed message as {"result": {...}};
		// every line must parse alone, which is the NDJSON contract.
		var chunk struct {
			Result struct {
				Type string `json:"type"`
				Run  struct {
					Status string `json:"status"`
				} `json:"run"`
			} `json:"result"`
		}

		if err := json.Unmarshal(line, &chunk); err != nil {
			t.Fatalf("stream line %q is not standalone JSON: %v", line, err)
		}

		if chunk.Result.Type == "RUN_EVENT_TYPE_RUN_FINISHED" {
			sawFinished = true

			if chunk.Result.Run.Status != statusSucceeded {
				t.Fatalf("final streamed run status = %q, want %q", chunk.Result.Run.Status, statusSucceeded)
			}
		}
	}

	if !sawFinished {
		t.Fatal("stream never delivered the run_finished event")
	}
}

func TestGatewayServesOpenAPIAndRoutes(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	code, body := httpGet(t, fix.httpBase+"/openapi.json")
	if code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d, want 200", code)
	}

	var openapi struct {
		Paths map[string]any `json:"paths"`
	}

	if err := json.Unmarshal(body, &openapi); err != nil {
		t.Fatalf("parse openapi document: %v", err)
	}

	if _, exists := openapi.Paths["/v1/health"]; !exists {
		t.Fatalf("openapi paths %v missing /v1/health", openapi.Paths)
	}

	code, body = httpGet(t, fix.httpBase+"/routes")
	if code != http.StatusOK {
		t.Fatalf("GET /routes = %d, want 200", code)
	}

	var routes struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
			RPC    string `json:"rpc"`
		} `json:"routes"`
	}

	if err := json.Unmarshal(body, &routes); err != nil {
		t.Fatalf("parse routes listing: %v", err)
	}

	var foundHealth bool

	for _, route := range routes.Routes {
		if route.Path == "/v1/health" && route.Method == http.MethodGet {
			foundHealth = true

			if !strings.Contains(route.RPC, "Health") {
				t.Fatalf("health route rpc = %q, want the Health RPC named", route.RPC)
			}
		}
	}

	if !foundHealth {
		t.Fatalf("routes %v missing GET /v1/health", routes.Routes)
	}
}

func TestServerRefusesNetworkBinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		grpcAddr string
		httpAddr string
	}{
		{name: "grpc on all interfaces", grpcAddr: "0.0.0.0:0", httpAddr: loopbackEphemeral},
		{name: "http on a lan address", grpcAddr: loopbackEphemeral, httpAddr: "192.168.1.10:0"},
		{name: "hostname that is not localhost", grpcAddr: "example.com:0", httpAddr: loopbackEphemeral},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := newGuardServer(t, tt.grpcAddr, tt.httpAddr, api.BindPolicy{}, nil)

			// With the zero policy (auth off, no explicit acceptance), every
			// non-loopback bind keeps failing with ErrNetworkBind.
			err := server.Start(t.Context())
			if !errors.Is(err, api.ErrNetworkBind) {
				t.Fatalf("Start error = %v, want ErrNetworkBind (the network guard)", err)
			}
		})
	}
}

// newGuardServer builds a server with real stores but arbitrary bind
// addresses and policy, for guard tests on the bind matrix.
func newGuardServer(t *testing.T, grpcAddr, httpAddr string, policy api.BindPolicy, logger *slog.Logger) *api.Server {
	t.Helper()

	dir := t.TempDir()

	db, err := sqlite.Open(dir + "/cairnos.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	mem, err := memory.OpenSQLite(dir + "/cairnos.db")
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}

	t.Cleanup(func() { _ = mem.Close() })

	manager := api.NewManager(nil)
	manager.Bind(engine.New(testEngineConfig(manager), testRegistry(t), db))

	return api.NewServer(&api.Config{
		GRPCAddr:     grpcAddr,
		HTTPAddr:     httpAddr,
		WorkflowsDir: dir,
		Store:        db,
		Memory:       mem,
		Registry:     testRegistry(t),
		Manager:      manager,
		BindPolicy:   policy,
		Logger:       logger,
	})
}
