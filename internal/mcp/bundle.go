package mcp

import (
	"context"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/internal/api"
)

// DefaultHTTPPath is the mux pattern the streamable MCP handler mounts on when
// the config sets no path. It is a plain prefix so the SDK handler owns every
// method under it (POST for calls, GET for the SSE stream).
const DefaultHTTPPath = "/mcp"

// bearerScheme is the accepted Authorization scheme, compared case-insensitively
// per RFC 9110, the same parse the gRPC interceptor does.
const bearerScheme = "bearer "

// HTTPHandler builds the streamable-HTTP http.Handler for the MCP server. Each
// request gets a fresh *mcp.Server bound to that request's bearer token, so the
// tool and resource handlers enforce scopes against the caller who made the
// call. With auth enabled and no token present, the handlers themselves answer
// auth.unauthenticated, so the token check sits in one place rather than split
// between the transport and the handlers.
//
// The SDK's getServer callback is the per-request hook: it receives the
// *http.Request, so the Authorization header is read here and threaded into the
// server the SDK then drives for that session.
func (s *Service) HTTPHandler() http.Handler {
	getServer := func(req *http.Request) *mcp.Server {
		return s.build(bearerFromHeader(req.Header))
	}

	return mcp.NewStreamableHTTPHandler(getServer, nil)
}

// Extension returns the api.Extension that mounts the MCP HTTP handler on the
// shared API server's root mux at path. The handler sits outside the gRPC
// gateway and outside the gRPC auth interceptor (the MCP handlers enforce auth
// themselves), the same way the A2A well-known route and the OAuth login route
// mount as root HTTP routes.
//
// The path is registered as a prefix ("/mcp/") plus the bare path ("/mcp") so a
// client can POST to either, matching how the streamable transport addresses a
// session.
func (s *Service) Extension(path string) *api.Extension {
	if path == "" {
		path = DefaultHTTPPath
	}

	handler := s.HTTPHandler()
	bare := strings.TrimSuffix(path, "/")

	routes := map[string]http.HandlerFunc{
		bare:       handler.ServeHTTP,
		bare + "/": handler.ServeHTTP,
	}

	return &api.Extension{
		Name:       "mcp",
		HTTPRoutes: routes,
	}
}

// ServeStdio runs the MCP server over the stdio transport until ctx cancels or
// the peer closes the connection. This is the `cairnos mcp serve` path: an MCP
// client spawns CairnOS as a subprocess and speaks MCP over its stdin and
// stdout. Auth is the process boundary here, not a token: whoever can spawn the
// process already holds the machine, so the server is built with an empty token
// and (when an authenticator is configured) the handlers authorize on the
// default workspace. The stdio transport carries no HTTP header to lift a token
// from, which is why the boundary, not a scope check, is the control.
func (s *Service) ServeStdio(ctx context.Context) error {
	server := s.build("")

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return wrapRun(err)
	}

	return nil
}

// Connect connects one MCP server bound to token over the given transport,
// returning the live session. It is the embedding entry point an in-process
// client connects to (an in-memory transport pair, say), the same build the
// stdio and http transports drive. The caller owns the returned session and
// closes it when done.
func (s *Service) Connect(ctx context.Context, token string, transport mcp.Transport) (*mcp.ServerSession, error) {
	session, err := s.build(token).Connect(ctx, transport, nil)
	if err != nil {
		return nil, wrapRun(err)
	}

	return session, nil
}

// wrapRun adds context to a transport-level run failure through a typed error
// rather than fmt.Errorf, so the CLI can match it and the cause stays
// inspectable through Unwrap.
func wrapRun(err error) error {
	return &runError{cause: err}
}

// runError wraps a stdio transport failure so the CLI reports it with context
// without the mcp package importing fmt just for one Errorf.
type runError struct {
	cause error
}

// Error renders the wrapped cause.
func (e *runError) Error() string {
	return "mcp stdio server: " + e.cause.Error()
}

// Unwrap exposes the cause for errors.Is/As.
func (e *runError) Unwrap() error {
	return e.cause
}

// bearerFromHeader pulls the token out of an Authorization header, returning the
// empty string when the header is missing or not a bearer token. An empty
// result lets the handlers answer auth.unauthenticated uniformly rather than
// branching on "no header" versus "bad scheme".
func bearerFromHeader(header http.Header) string {
	value := header.Get("Authorization")
	if len(value) > len(bearerScheme) && strings.EqualFold(value[:len(bearerScheme)], bearerScheme) {
		return value[len(bearerScheme):]
	}

	return ""
}
