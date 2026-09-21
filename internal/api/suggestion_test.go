package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
)

// Suggestion fixture literals.
const (
	suggestionTarget   = "repeat-wf.fetch"
	suggestionRunner   = "anthropic"
	statusOpenLiteral  = "open"
	statusApprovedWire = "approved"
)

// seedSuggestion stages one open suggestion row under the fixture's server.
func seedSuggestion(t *testing.T, fix *fixture) string {
	t.Helper()

	id := sqlite.NewID()

	err := fix.db.SaveSuggestion(t.Context(), &store.Suggestion{
		ID:        id,
		Kind:      store.SuggestionCaching,
		Target:    suggestionTarget,
		Rationale: "the action repeated itself across runs",
		Evidence: map[string]any{
			"runner":       suggestionRunner,
			"repeat_count": float64(4),
		},
		Proposed: map[string]any{
			"cache":     true,
			"cache_ttl": "1h",
		},
		EstSavings: store.Savings{
			CostUSDPerRun: 0.075,
			LatencyDelta:  time.Second,
			Confidence:    store.ConfidenceHigh,
		},
		Status: store.SuggestionOpen,
	})
	if err != nil {
		t.Fatalf("seed suggestion: %v", err)
	}

	return id
}

func TestSuggestionListAndApproveOverGRPC(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	id := seedSuggestion(t, fix)

	listed, err := fix.suggestion.ListSuggestions(t.Context(), &cairnosv1.ListSuggestionsRequest{Status: statusOpenLiteral})
	if err != nil {
		t.Fatalf("list suggestions: %v", err)
	}

	if len(listed.GetSuggestions()) != 1 {
		t.Fatalf("listed %d suggestions, want 1", len(listed.GetSuggestions()))
	}

	wire := listed.GetSuggestions()[0]

	if wire.GetId() != id || wire.GetKind() != string(store.SuggestionCaching) || wire.GetTarget() != suggestionTarget {
		t.Fatalf("wire suggestion = %+v, want the seeded row", wire)
	}

	if wire.GetEstSavings().GetCostUsdPerRun() != 0.075 || wire.GetEstSavings().GetConfidence() != store.ConfidenceHigh {
		t.Fatalf("wire savings = %+v, want 0.075 at high confidence", wire.GetEstSavings())
	}

	if wire.GetEvidence().AsMap()["runner"] != suggestionRunner {
		t.Fatalf("wire evidence = %v, want the runner preserved", wire.GetEvidence().AsMap())
	}

	approved, err := fix.suggestion.ApproveSuggestion(t.Context(), &cairnosv1.ApproveSuggestionRequest{Id: id})
	if err != nil {
		t.Fatalf("approve suggestion: %v", err)
	}

	if approved.GetSuggestion().GetStatus() != statusApprovedWire {
		t.Fatalf("status after approve = %s, want approved", approved.GetSuggestion().GetStatus())
	}

	// Approval returns the annotation for manual application; the server
	// writes no workflow or config files.
	if !strings.Contains(approved.GetAnnotation(), "apply this by hand") {
		t.Fatalf("annotation missing the manual-application contract: %q", approved.GetAnnotation())
	}

	remaining, err := fix.suggestion.ListSuggestions(t.Context(), &cairnosv1.ListSuggestionsRequest{Status: statusOpenLiteral})
	if err != nil {
		t.Fatalf("list after approve: %v", err)
	}

	if len(remaining.GetSuggestions()) != 0 {
		t.Fatalf("open queue still holds %d suggestions after approval", len(remaining.GetSuggestions()))
	}
}

func TestSuggestionRejectOverGRPC(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	id := seedSuggestion(t, fix)

	rejected, err := fix.suggestion.RejectSuggestion(t.Context(), &cairnosv1.RejectSuggestionRequest{Id: id})
	if err != nil {
		t.Fatalf("reject suggestion: %v", err)
	}

	if rejected.GetSuggestion().GetStatus() != string(store.SuggestionRejected) {
		t.Fatalf("status after reject = %s, want rejected", rejected.GetSuggestion().GetStatus())
	}
}

func TestSuggestionGatewayRoutes(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	id := seedSuggestion(t, fix)

	code, body := httpGet(t, fix.transport, fix.httpBase+"/v1/suggestions?status="+statusOpenLiteral)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/suggestions = %d: %s", code, body)
	}

	var listed struct {
		Suggestions []struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
		} `json:"suggestions"`
	}

	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("parse gateway listing: %v", err)
	}

	if len(listed.Suggestions) != 1 || listed.Suggestions[0].ID != id {
		t.Fatalf("gateway listed %+v, want the seeded suggestion", listed.Suggestions)
	}

	resp := httpPost(t, fix.transport, fix.httpBase+"/v1/suggestions/"+id+"/approve", "{}")
	defer func() { _ = resp.Body.Close() }()

	approveBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read approve body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST approve = %d: %s", resp.StatusCode, approveBody)
	}

	var approved struct {
		Suggestion struct {
			Status string `json:"status"`
		} `json:"suggestion"`
		Annotation string `json:"annotation"`
	}

	if err := json.Unmarshal(approveBody, &approved); err != nil {
		t.Fatalf("parse approve body: %v", err)
	}

	if approved.Suggestion.Status != statusApprovedWire || !strings.Contains(approved.Annotation, "apply this by hand") {
		t.Fatalf("gateway approve = %+v, want approved status and the annotation", approved)
	}
}

func TestSuggestionNotFoundCode(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	_, err := fix.suggestion.ApproveSuggestion(t.Context(), &cairnosv1.ApproveSuggestionRequest{Id: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("approve of a missing id = %v, want NotFound", err)
	}
}

func TestSuggestionInvalidStatusFilter(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	_, err := fix.suggestion.ListSuggestions(t.Context(), &cairnosv1.ListSuggestionsRequest{Status: "bogus"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid status filter = %v, want InvalidArgument", err)
	}
}
