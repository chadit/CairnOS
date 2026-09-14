package mcp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/internal/auth"
)

// bearerRoundTripper attaches a static bearer token to every request, the
// stand-in for an MCP client that holds a CairnOS token. The streamable client
// transport has no header field of its own, so the token rides on the HTTP
// client instead.
type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

// RoundTrip stamps the Authorization header and delegates to the base
// transport.
func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)

	return b.base.RoundTrip(clone) //nolint:wrapcheck // a test round tripper passes the transport error through unchanged
}

func TestHTTPTransportEnforcesScopeFromHeader(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true})

	handler := fix.service.Extension("/mcp")
	mux := http.NewServeMux()

	for pattern, route := range handler.HTTPRoutes {
		mux.HandleFunc(pattern, route)
	}

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	tests := []struct {
		name    string
		scopes  []string
		wantErr bool
	}{
		{name: "write_workflow_runs", scopes: []string{auth.ScopeWriteWorkflow}, wantErr: false},
		{name: "read_memory_forbidden", scopes: []string{auth.ScopeReadMemory}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			token := fix.token(t, tt.scopes...)
			session := connectHTTP(t, server.URL+"/mcp", token)

			result, err := session.CallTool(t.Context(), &sdk.CallToolParams{
				Name:      toolRunWorkflow,
				Arguments: map[string]any{argWorkflow: helloWorkflow},
			})
			if err != nil {
				t.Fatalf("call run_workflow over http: %v", err)
			}

			if result.IsError != tt.wantErr {
				t.Fatalf("IsError = %v, want %v (body: %s)", result.IsError, tt.wantErr, resultText(result.Content))
			}

			if tt.wantErr && !strings.Contains(resultText(result.Content), codeForbidden) {
				t.Errorf("error body %q missing %q", resultText(result.Content), codeForbidden)
			}
		})
	}
}

// connectHTTP dials the streamable MCP endpoint with a token-injecting client.
func connectHTTP(t *testing.T, endpoint, token string) *sdk.ClientSession {
	t.Helper()

	httpClient := &http.Client{
		Transport: &bearerRoundTripper{token: token, base: http.DefaultTransport},
	}

	client := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: clientVersion}, nil)

	session, err := client.Connect(t.Context(), &sdk.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: httpClient,
	}, nil)
	if err != nil {
		t.Fatalf("connect http client: %v", err)
	}

	t.Cleanup(func() { _ = session.Close() })

	return session
}
