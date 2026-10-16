package api_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/store"
)

// The principal cases assert what the auth interceptor stamps onto a handler's
// context: the caller's tenant and the subject naming them inside it. No
// shipped method hands its own request context back, so they drive a real
// server through a service registered here as an api.Extension. The public
// half is pinned by TestHealthNeedsNoTokenWithAuthOn, and the stamped scopes
// by TestChatScopeEnforced.
const (
	principalService      = "cairnos.test.v1.PrincipalService"
	principalUnaryMethod  = "/" + principalService + "/Report"
	principalStreamName   = "ReportStream"
	principalStreamMethod = "/" + principalService + "/" + principalStreamName
)

// Field names in the report a principal handler sends back.
const (
	fieldSubject    = "subject"
	fieldHasSubject = "has_subject"
	fieldWorkspace  = "workspace"
)

// The report carries no scope probe. ScopeForMethod has no entry for a service
// that exists only here, so the method falls through to admin, and
// auth.HasScope treats admin as satisfying everything while an unstamped
// context answers true as well. The scopes are pinned by TestChatScopeEnforced
// instead, which confirms a trigger with a chat:session token and no admin.

// principalReport renders what the interceptor stamped onto ctx. A Struct on
// both sides of the wire is what keeps this test-only service free of a
// generated message type.
func principalReport(ctx context.Context) *structpb.Struct {
	subject, stamped := api.ContextSubject(ctx)

	return &structpb.Struct{Fields: map[string]*structpb.Value{
		fieldSubject:    structpb.NewStringValue(subject),
		fieldHasSubject: structpb.NewBoolValue(stamped),
		fieldWorkspace:  structpb.NewStringValue(store.WorkspaceFromContext(ctx)),
	}}
}

// reportStream is the server-streaming half of the report service. It sends one
// report and returns, which is enough to prove the stream's context carries the
// same claims the unary path does.
func reportStream(_ any, stream grpc.ServerStream) error {
	return stream.SendMsg(principalReport(stream.Context()))
}

// principalExtension registers the report service on a test server. It is a
// function rather than a package variable so no case can mutate the descriptor
// another case registers. The unary handler is a literal because grpc-go's
// MethodHandler takes its context in second position, which a declared function
// may not.
func principalExtension() *api.Extension {
	unary := func(
		_ any,
		ctx context.Context,
		dec func(any) error,
		interceptor grpc.UnaryServerInterceptor,
	) (any, error) {
		request := new(structpb.Struct)
		if err := dec(request); err != nil {
			return nil, err
		}

		handler := func(ctx context.Context, _ any) (any, error) {
			return principalReport(ctx), nil
		}

		if interceptor == nil {
			return handler(ctx, request)
		}

		return interceptor(ctx, request, &grpc.UnaryServerInfo{FullMethod: principalUnaryMethod}, handler)
	}

	desc := &grpc.ServiceDesc{
		ServiceName: principalService,
		HandlerType: (*any)(nil),
		Methods:     []grpc.MethodDesc{{MethodName: "Report", Handler: unary}},
		Streams: []grpc.StreamDesc{
			{StreamName: principalStreamName, Handler: reportStream, ServerStreams: true},
		},
		Metadata: "principal_test.go",
	}

	return &api.Extension{
		Name: principalService,
		// A nil service implementation is what lets the descriptor carry plain
		// functions: grpc-go skips its HandlerType check when there is nothing
		// to check it against.
		RegisterGRPC: func(server *grpc.Server) { server.RegisterService(desc, nil) },
	}
}

// callReport invokes the unary report with the given bearer token, empty for no
// token at all.
func callReport(t *testing.T, conn *grpc.ClientConn, token string) map[string]*structpb.Value {
	t.Helper()

	var report structpb.Struct

	if err := conn.Invoke(bearerContext(t, token), principalUnaryMethod, &structpb.Struct{}, &report); err != nil {
		t.Fatalf("Report: %v", err)
	}

	return report.GetFields()
}

// callReportStream opens the streaming report and reads its single message.
func callReportStream(t *testing.T, conn *grpc.ClientConn, token string) map[string]*structpb.Value {
	t.Helper()

	desc := &grpc.StreamDesc{StreamName: principalStreamName, ServerStreams: true}

	stream, err := conn.NewStream(bearerContext(t, token), desc, principalStreamMethod)
	if err != nil {
		t.Fatalf("open ReportStream: %v", err)
	}

	if err := stream.SendMsg(&structpb.Struct{}); err != nil {
		t.Fatalf("ReportStream send: %v", err)
	}

	if err := stream.CloseSend(); err != nil {
		t.Fatalf("ReportStream close send: %v", err)
	}

	var report structpb.Struct

	if err := stream.RecvMsg(&report); err != nil {
		t.Fatalf("ReportStream recv: %v", err)
	}

	return report.GetFields()
}

// bearerContext attaches a token to an outgoing call, or leaves it bare when
// token is empty.
func bearerContext(t *testing.T, token string) context.Context {
	t.Helper()

	if token == "" {
		return t.Context()
	}

	return metadata.AppendToOutgoingContext(t.Context(), "authorization", "Bearer "+token)
}

// assertStamped checks one report against the principal the token described.
func assertStamped(t *testing.T, report map[string]*structpb.Value, subject string) {
	t.Helper()

	if !report[fieldHasSubject].GetBoolValue() {
		t.Fatalf("handler context carried no subject: %v", report)
	}

	if got := report[fieldSubject].GetStringValue(); got != subject {
		t.Errorf("handler subject = %q, want the token's key id %q", got, subject)
	}

	if got := report[fieldWorkspace].GetStringValue(); got != store.DefaultWorkspace {
		t.Errorf("handler workspace = %q, want %q", got, store.DefaultWorkspace)
	}
}

// TestUnaryStampsTheVerifiedPrincipal proves a unary handler runs under the
// token's tenant and subject. The subject is what lets a handler tell
// two keys in one workspace apart, which the A2A conversation path needs to
// keep one peer's session away from another's.
func TestUnaryStampsTheVerifiedPrincipal(t *testing.T) {
	t.Parallel()

	fix := startAuthServerWith(t, principalExtension())

	token, subject := fix.tokenAndSubject(t, auth.ScopeAdmin)

	assertStamped(t, callReport(t, fix.conn, token), subject)
}

// TestStreamStampsTheVerifiedPrincipal proves a streaming handler reads the same
// claims a unary one does, so a stream cannot quietly lose the subject a unary
// call carries.
func TestStreamStampsTheVerifiedPrincipal(t *testing.T) {
	t.Parallel()

	fix := startAuthServerWith(t, principalExtension())

	token, subject := fix.tokenAndSubject(t, auth.ScopeAdmin)

	assertStamped(t, callReportStream(t, fix.conn, token), subject)
}

// TestUnverifiedCallStampsNoSubject proves a call carrying no verified token
// leaves the subject unset rather than stamping an empty one. A reader can then
// tell "no token rode this call" from "a token whose subject is empty", which is
// what the A2A fallback to the workspace keys off. Auth is off on this server,
// the shape an unauthenticated node takes.
func TestUnverifiedCallStampsNoSubject(t *testing.T) {
	t.Parallel()

	fix := startServerWith(t, principalExtension())

	report := callReport(t, fix.conn, "")

	if report[fieldHasSubject].GetBoolValue() {
		t.Errorf("an unverified call stamped subject %q, want none",
			report[fieldSubject].GetStringValue())
	}
}
