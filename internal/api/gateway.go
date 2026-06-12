package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
)

// envelopeStatus is the fixed status discriminator in HTTP error bodies. The
// HTTP status code, never this field, decides success versus failure; the
// field exists so error bodies are self-describing in logs.
const envelopeStatus = "error"

// errorEnvelope is the canonical HTTP error shape
// (.agents/spec/07-api-protocols.md, "HTTP response (error)").
type errorEnvelope struct {
	Status string        `json:"status"`
	Error  envelopeError `json:"error"`
}

// envelopeError mirrors cairnerr.Error on the wire.
type envelopeError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// newGatewayMux builds the HTTP mux that grpc-gateway routes through, with
// snake_case field names (matching the protos and the spec examples) and the
// canonical error envelope on both error paths: WithErrorHandler covers
// unary failures, and envelopeMarshaler rewrites the stream error chunk,
// since a streaming RPC that fails before its first message bypasses the
// unary error handler inside grpc-gateway.
func newGatewayMux() *runtime.ServeMux {
	return runtime.NewServeMux(
		runtime.WithErrorHandler(gatewayError),
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &envelopeMarshaler{
			JSONPb: &runtime.JSONPb{
				MarshalOptions:   protojson.MarshalOptions{UseProtoNames: true},
				UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
			},
		}),
	)
}

// envelopeMarshaler is the gateway JSON marshaler plus one rewrite: the
// {"error": <grpc status>} chunk grpc-gateway emits for stream errors
// becomes the canonical envelope, so a failed RunWorkflow over HTTP carries
// the same shape and code as every unary error.
type envelopeMarshaler struct {
	*runtime.JSONPb
}

// Marshal rewrites stream error chunks and delegates everything else.
func (m *envelopeMarshaler) Marshal(value any) ([]byte, error) {
	chunk, isChunk := value.(map[string]proto.Message)
	if isChunk {
		if statusProto, isStatus := chunk["error"].(*statuspb.Status); isStatus {
			body, err := json.Marshal(envelopeFor(status.FromProto(statusProto)))
			if err != nil {
				return nil, fmt.Errorf("marshal stream error envelope: %w", err)
			}

			return body, nil
		}
	}

	body, err := m.JSONPb.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal gateway response: %w", err)
	}

	return body, nil
}

// envelopeFor renders one gRPC status as the canonical HTTP error body. The
// canonical code comes from the cairnos.v1.Error riding in the status
// details; failures that never reached a handler (bad routes, unparseable
// bodies) synthesize an api.* code from the gRPC code so the shape never
// varies.
func envelopeFor(grpcStatus *status.Status) errorEnvelope {
	body := errorEnvelope{
		Status: envelopeStatus,
		Error: envelopeError{
			Code:    prefixAPI + snakeCase(grpcStatus.Code().String()),
			Message: grpcStatus.Message(),
			Details: map[string]any{},
		},
	}

	for _, detail := range grpcStatus.Details() {
		canonical, isCanonical := detail.(*cairnosv1.Error)
		if !isCanonical {
			continue
		}

		body.Error.Code = canonical.GetCode()
		body.Error.Message = canonical.GetMessage()
		body.Error.Details = canonical.GetDetails().AsMap()

		break
	}

	return body
}

// gatewayError renders every unary gateway failure as the canonical
// envelope: {"status":"error","error":{code,message,details}}.
func gatewayError(ctx context.Context, _ *runtime.ServeMux, _ runtime.Marshaler, w http.ResponseWriter, _ *http.Request, err error) {
	grpcStatus := status.Convert(err)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(runtime.HTTPStatusFromCode(grpcStatus.Code()))

	if encodeErr := json.NewEncoder(w).Encode(envelopeFor(grpcStatus)); encodeErr != nil {
		slog.WarnContext(ctx, "write gateway error body", "error", encodeErr)
	}
}

// snakeCase renders a gRPC code name ("NotFound") as its canonical dotted
// suffix ("not_found").
func snakeCase(name string) string {
	var out strings.Builder

	// Worst case adds one underscore per camel hump; a small fixed headroom
	// covers every gRPC code name without a second allocation.
	out.Grow(len(name) + 4) //nolint:mnd // allocation headroom, not a tunable

	for idx, char := range name {
		if char >= 'A' && char <= 'Z' {
			if idx > 0 {
				out.WriteByte('_')
			}

			out.WriteRune(char - 'A' + 'a')

			continue
		}

		out.WriteRune(char)
	}

	return out.String()
}
