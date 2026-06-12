package api_test

import (
	"errors"
	"io"
	"slices"
	"testing"

	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
)

// TestReflectionListsServices is the grpcurl-equivalent discovery check: the
// reflection service must list every cairnos.v1 service so standard tooling
// can call them without a hand-written client
// (.agents/spec/07-api-protocols.md, "Discovery").
func TestReflectionListsServices(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	stream, err := reflectionpb.NewServerReflectionClient(fix.conn).ServerReflectionInfo(t.Context())
	if err != nil {
		t.Fatalf("open reflection stream: %v", err)
	}

	req := &reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{ListServices: ""},
	}

	if err := stream.Send(req); err != nil {
		t.Fatalf("send list services: %v", err)
	}

	resp, err := stream.Recv()
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("recv list services: %v", err)
	}

	listed := resp.GetListServicesResponse().GetService()

	names := make([]string, 0, len(listed))
	for _, svc := range listed {
		names = append(names, svc.GetName())
	}

	want := []string{
		"cairnos.v1.AdminService",
		"cairnos.v1.LoginService",
		"cairnos.v1.MemoryService",
		"cairnos.v1.SuggestionService",
		"cairnos.v1.WorkflowService",
	}

	for _, name := range want {
		if !slices.Contains(names, name) {
			t.Fatalf("reflection listed %v, missing %s", names, name)
		}
	}
}
