package openaicompat

import (
	"net/http"

	"github.com/chadit/CairnOS/internal/api"
)

// extensionName labels the adapter in the api server's extension list and logs.
const extensionName = "openai-compat"

// Route patterns the adapter mounts on. Go 1.22+ method patterns on the root
// mux beat the gateway's "/" catch-all, so these win over the gRPC gateway
// without shadowing it (.agents/spec/parity/openai-compat.md, "Routing safety").
const (
	routeCompletions = "POST /v1/chat/completions"
	routeModels      = "GET /v1/models"
)

// Extension returns the api.Extension that mounts the adapter's HTTP routes on
// the shared API server's root mux, the same way internal/mcp/bundle.go mounts
// the MCP handler. The routes sit outside the gRPC gateway and its auth
// interceptor; the handlers enforce auth themselves.
func (s *Service) Extension() *api.Extension {
	routes := map[string]http.HandlerFunc{
		routeCompletions: s.handleCompletions,
		routeModels:      s.handleModels,
	}

	return &api.Extension{
		Name:       extensionName,
		HTTPRoutes: routes,
	}
}
