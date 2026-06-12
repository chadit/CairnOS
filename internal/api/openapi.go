package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
)

// openapiJSON is the OpenAPI v2 document protoc-gen-openapiv2 generates from
// the same protos that build the gRPC server and the gateway, so the HTTP
// surface it describes cannot drift from the running services.
//
//go:embed gen/openapi/cairnos.swagger.json
var openapiJSON []byte

// routeEntry is one line of the /routes listing.
type routeEntry struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	RPC    string `json:"rpc"`
}

// routesDoc is the /routes response body.
type routesDoc struct {
	Routes []routeEntry `json:"routes"`
}

// swaggerPaths is the slice of the OpenAPI document /routes reads: path ->
// HTTP method -> operation. Operation IDs come out of protoc-gen-openapiv2
// as Service_Method, which names the backing RPC.
type swaggerPaths struct {
	//nolint:tagliatelle // operationId is the OpenAPI spec's own field name
	Paths map[string]map[string]struct {
		OperationID string `json:"operationId"`
	} `json:"paths"`
}

// buildRoutes derives the route table from the embedded OpenAPI document, so
// /routes stays a thin view over generated artifacts rather than a second
// source of truth (.agents/spec/07-api-protocols.md, "Discovery").
func buildRoutes() ([]routeEntry, error) {
	var doc swaggerPaths

	if err := json.Unmarshal(openapiJSON, &doc); err != nil {
		return nil, fmt.Errorf("parse embedded openapi document: %w", err)
	}

	var routes []routeEntry

	for path, operations := range doc.Paths {
		for method, operation := range operations {
			routes = append(routes, routeEntry{
				Method: strings.ToUpper(method),
				Path:   path,
				RPC:    operation.OperationID,
			})
		}
	}

	slices.SortFunc(routes, func(left, right routeEntry) int {
		if cmp := strings.Compare(left.Path, right.Path); cmp != 0 {
			return cmp
		}

		return strings.Compare(left.Method, right.Method)
	})

	return routes, nil
}

// openapiHandler serves the embedded OpenAPI document at /openapi.json.
func openapiHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if _, err := w.Write(openapiJSON); err != nil {
			slog.Warn("write openapi document", "error", err)
		}
	}
}

// routesHandler serves the generated route table at /routes.
func routesHandler(routes []routeEntry) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(routesDoc{Routes: routes}); err != nil {
			slog.Warn("write routes listing", "error", err)
		}
	}
}
