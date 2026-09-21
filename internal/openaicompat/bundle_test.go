package openaicompat_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/openaicompat"
)

// gatewayMarker is the body the stand-in gateway "/" handler writes, so a test
// tells an adapter hit apart from a gateway hit.
const gatewayMarker = "gateway-stub"

// TestExtensionRouteBeatsGatewayCatchAll mounts the adapter's Extension routes
// alongside a "/" catch-all standing in for the gRPC gateway, the same way the
// api server's root mux does, and proves the explicit method pattern wins on its
// own path while every other path still reaches the gateway. This locks the mux
// specificity the design leans on (.agents/spec/parity/openai-compat.md,
// "Routing safety").
func TestExtensionRouteBeatsGatewayCatchAll(t *testing.T) {
	t.Parallel()

	service, err := openaicompat.NewService(&openaicompat.Config{
		Agent:         answerAgent(),
		Store:         &fakeStore{},
		Personalities: chat.NewPersonalities(nil, ""),
	})
	if err != nil {
		t.Fatalf("build service: %v", err)
	}

	root := http.NewServeMux()
	root.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, gatewayMarker)
	})

	for pattern, handler := range service.Extension().HTTPRoutes {
		root.HandleFunc(pattern, handler)
	}

	server := nettest.New(root)

	// The completions POST reaches the adapter, which answers a full completion,
	// not the gateway marker.
	fix := &fixture{server: server}

	status, data := fix.post(t, "", simpleUserBody)
	if status != http.StatusOK {
		t.Fatalf("completions status = %d, want 200 (body: %s)", status, data)
	}

	if decodeCompletion(t, data).Object != objectCompletion {
		t.Errorf("completions did not reach the adapter; body: %s", data)
	}

	// Every other chat path falls through to the gateway stub: the adapter did
	// not shadow the gateway.
	if got := getBody(t, server, "/v1/chat/sessions"); got != gatewayMarker {
		t.Errorf("sessions route body = %q, want the gateway marker", got)
	}
}
