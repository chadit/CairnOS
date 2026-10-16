package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Registry contract paths, relative to the configured base URL. CairnOS owns
// this small JSON contract (POST /publish, GET /servers); pointing baseURL at
// the public MCP registry is the path forward once its schema settles
// (.agents/spec/10-mcp-integration.md, "MCP Registry").
const (
	registryPublishPath = "/publish"
	registryServersPath = "/servers"
)

// registryTimeout bounds a single registry HTTP call when the caller passes no
// client of its own.
const registryTimeout = 30 * time.Second

// maxRegistryError caps how much of a non-2xx response body an error carries,
// so a misbehaving registry cannot flood the CLI with an unbounded message.
const maxRegistryError = 4 << 10 // 4 KiB

// transportHTTP labels the transport in a published record. CairnOS publishes
// its http endpoint, the only transport another node can import and reach (a
// stdio server is spawned locally, not addressed over a URL).
const transportHTTP = "http"

// runnerTypeMCPClient is the type value every imported block carries. It is
// also a reserved runner name: the CLI wiring treats a block literally named
// mcp_client as configuration for the built-in instance, so import must never
// emit that name (see ImportConfig).
const runnerTypeMCPClient = "mcp_client"

// slugPattern collapses every run of non-slug characters to one underscore when
// turning a server and tool name into a runner key.
var slugPattern = regexp.MustCompile(`[^a-z0-9]+`)

// ToolEntry is one tool in a registry record: the name and description the MCP
// server declares for it. The input schema is intentionally absent. An imported
// tool becomes an mcp_client runner that fetches its live schema via Describe on
// first use, so the record carries only what discovery and import need.
type ToolEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ServerEntry is one MCP server's registry record: its identity, the endpoint
// an importer points an mcp_client runner at, and the tools it exposes.
type ServerEntry struct {
	Name        string      `json:"name"`
	Version     string      `json:"version,omitempty"`
	Description string      `json:"description,omitempty"`
	Endpoint    string      `json:"endpoint"`
	Transport   string      `json:"transport,omitempty"`
	Tools       []ToolEntry `json:"tools"`
}

// serversResponse is the GET /servers envelope.
type serversResponse struct {
	Servers []ServerEntry `json:"servers"`
}

// RegistryClient talks to an MCP registry over HTTP: publish this node's record,
// list community records to import. The token is a bearer secret read from the
// environment by the caller, never persisted in config.
type RegistryClient struct {
	http    *http.Client
	baseURL string
	token   string
}

// NewRegistryClient builds a client against a base URL. An empty URL is a
// configuration error the caller gates on before construction. A nil http
// client falls back to a bounded default.
func NewRegistryClient(baseURL, token string, client *http.Client) (*RegistryClient, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, ErrNoRegistryURL
	}

	if client == nil {
		client = &http.Client{Timeout: registryTimeout}
	}

	return &RegistryClient{baseURL: trimmed, token: token, http: client}, nil
}

// Publish registers one server record. A non-2xx response surfaces with the
// status and any body the registry returned, so an auth rejection or a malformed
// record is legible at the CLI.
func (c *RegistryClient) Publish(ctx context.Context, entry *ServerEntry) error {
	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode server entry: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+registryPublishPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build publish request: %w", err)
	}

	req.Header.Set("Content-Type", mimeJSON)
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("publish to registry: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return registryStatusError("publish", resp)
	}

	return nil
}

// List fetches the registry's server records.
func (c *RegistryClient) List(ctx context.Context) ([]ServerEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+registryServersPath, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build list request: %w", err)
	}

	req.Header.Set("Accept", mimeJSON)
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list registry servers: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, registryStatusError("list", resp)
	}

	var decoded serversResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode registry servers: %w", err)
	}

	return decoded.Servers, nil
}

// authorize attaches the bearer token when one is configured.
func (c *RegistryClient) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// RegistryError is a non-2xx response from the registry. It carries the status
// and a bounded slice of the body, so a caller can branch on the code and a CLI
// can show the registry's own message rather than a generic failure.
type RegistryError struct {
	// Op is the request that failed ("publish" or "list").
	Op string

	// Status is the full HTTP status line.
	Status string

	// Body is the trimmed, bounded response body, empty when the registry sent
	// none.
	Body string

	// StatusCode is the HTTP status the registry returned.
	StatusCode int
}

// Error renders the operation, status, and any body the registry returned.
func (e *RegistryError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("registry %s: %s", e.Op, e.Status)
	}

	return fmt.Sprintf("registry %s: %s: %s", e.Op, e.Status, e.Body)
}

// registryStatusError reads a bounded slice of the body and builds a typed
// RegistryError for a non-2xx response. A failed body read still yields the
// RegistryError, because the status is the failure the caller acts on; the
// read error rides in Body so it is not lost.
func registryStatusError(operation string, resp *http.Response) error {
	snippet, err := io.ReadAll(io.LimitReader(resp.Body, maxRegistryError))

	body := strings.TrimSpace(string(snippet))
	if err != nil {
		body = strings.TrimSpace(fmt.Sprintf("%s [reading the body failed: %v]", body, err))
	}

	return &RegistryError{
		Op:         operation,
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       body,
	}
}

// ServerRecord builds this node's registry record for an advertised endpoint.
// includeRules adds get_language_rules, matching the served surface: a node with
// a rules store wired exposes it, and every CairnOS node wires one. Identity,
// descriptions, and the tool gating come from the same constants the server
// registers, so the published record never drifts from what an MCP client sees.
func ServerRecord(endpoint string, includeRules bool) ServerEntry {
	tools := []ToolEntry{
		{Name: toolRunWorkflow, Description: descRunWorkflow},
		{Name: toolQueryMemory, Description: descQueryMemory},
	}

	if includeRules {
		tools = append(tools, ToolEntry{Name: toolGetLanguageRules, Description: descGetLanguageRules})
	}

	return ServerEntry{
		Name:        serverName,
		Version:     serverVersion,
		Description: serverDescription,
		Endpoint:    endpoint,
		Transport:   transportHTTP,
		Tools:       tools,
	}
}

// Each block points an mcp_client runner at the server's endpoint and tool; the
// CLI wiring registers a per-block instance of the built-in client factory under
// each block's name. The name mcp_client itself is never emitted: the wiring's
// skip rule makes a block with that literal name plain configuration for the
// built-in instance, so a generated block using it would silently reconfigure
// the built-in instead of registering a new runner.

// ImportConfig renders registry server records as mcp_client runner blocks, one
// per tool, in the TOML an operator pastes into their config
// (.agents/spec/10-mcp-integration.md, "MCP Registry"). A server whose endpoint
// matches selfEndpoint is skipped, so a node does not import its own published
// tools. Returns the rendered TOML and the runner names it defined.
func ImportConfig(servers []ServerEntry, selfEndpoint string) (string, []string) {
	self := strings.TrimRight(strings.TrimSpace(selfEndpoint), "/")

	var (
		builder strings.Builder
		names   []string
	)

	// Seeding the reserved name pushes a record slugging to mcp_client onto the
	// existing _2 suffix path instead of emitting the built-in's own name.
	seen := map[string]bool{runnerTypeMCPClient: true}

	builder.WriteString("# Imported from the MCP registry. Review before use.\n")
	builder.WriteString("# Authenticated MCP servers aren't supported yet: the mcp_client runner\n")
	builder.WriteString("# sends no Authorization header.\n")
	builder.WriteString("# Endpoints on private or loopback addresses need\n")
	builder.WriteString("# allow_private_targets = true on that runner block.\n")

	for idx := range servers {
		server := &servers[idx]
		if server.Endpoint == "" || strings.TrimRight(server.Endpoint, "/") == self {
			continue
		}

		for _, tool := range server.Tools {
			name := uniqueRunnerName(runnerName(server.Name, tool.Name), seen)
			names = append(names, name)

			fmt.Fprintf(&builder, "\n[runner.%s]\n", name)
			fmt.Fprintf(&builder, "type       = %s\n", tomlString(runnerTypeMCPClient))
			fmt.Fprintf(&builder, "server_url = %s\n", tomlString(server.Endpoint))
			fmt.Fprintf(&builder, "tool       = %s\n", tomlString(tool.Name))
		}
	}

	return builder.String(), names
}

// runnerName slugs a server and tool into one runner key. An empty result (a
// record with no usable identifier) falls back to "imported".
func runnerName(server, tool string) string {
	slug := strings.Trim(slugPattern.ReplaceAllString(strings.ToLower(server+"_"+tool), "_"), "_")
	if slug == "" {
		return "imported"
	}

	return slug
}

// uniqueRunnerName disambiguates a collision by suffixing the next free index,
// so two servers exposing the same tool name both land as distinct runners.
func uniqueRunnerName(base string, seen map[string]bool) string {
	if !seen[base] {
		seen[base] = true

		return base
	}

	for idx := 2; ; idx++ {
		candidate := fmt.Sprintf("%s_%d", base, idx)
		if !seen[candidate] {
			seen[candidate] = true

			return candidate
		}
	}
}

// tomlString renders a Go string as a TOML basic string, escaping the backslash
// and double-quote a server-supplied endpoint could carry.
func tomlString(in string) string {
	replaced := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(in)

	return `"` + replaced + `"`
}
