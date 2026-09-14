package hive_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/hive"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/tuning"
)

// Secret fixtures the no-leak proof scans for. Each is a distinct string that
// would never appear in a content-free tip, so finding any of them in the
// outbound tip's JSON is proof of a leak. The api-key fixture is a synthetic
// placeholder shaped to look like a credential without being a real one.
// goconst applies to tests here.
const (
	rawPromptText   = "summarize the resume of jane doe ssn 123-45-6789"
	rawAPIKey       = "secret-live-deadbeefcafef00d-token"
	rawFilePath     = "/home/operator/secrets/profile.json"
	rawEndpoint     = "https://internal.example.com/v1/private"
	rawWorkflowName = "jane_private_jobsearch"
	rawOutputText   = "jane is a strong match for the staff engineer role at acme"

	openaiRunner = "openai"
)

// realisticSuggestion builds a suggestion whose evidence and proposed map are
// stuffed with raw content the way a real param-tuning suggestion would carry
// it: the prompt, the output, a credential, a file path, an endpoint, and the
// workflow name. The transform must reduce this to a tip that carries none of
// it.
func realisticSuggestion() store.Suggestion {
	return store.Suggestion{
		ID:        "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Kind:      store.SuggestionParamTuning,
		Target:    rawWorkflowName + ".score_candidate",
		Rationale: "the action " + rawPromptText + " ran with max_tokens far above its output",
		Evidence: map[string]any{
			tuning.EvidenceRunner:    openaiRunner,
			tuning.EvidenceWorkflow:  rawWorkflowName,
			tuning.EvidenceAction:    "score_candidate",
			"raw_prompt":             rawPromptText,
			"recorded_output":        rawOutputText,
			"api_key":                rawAPIKey,
			"endpoint":               rawEndpoint,
			"workflow_file":          rawFilePath,
			tuning.EvidenceMaxTokens: 4096,
		},
		Proposed: map[string]any{
			tuning.ProposedMaxTokens:   512,
			tuning.ProposedConfigBlock: "runner." + openaiRunner,
			"prompt":                   rawPromptText,
		},
		EstSavings: store.Savings{
			CostUSDPerRun: 0.0234,
			Confidence:    store.ConfidenceLow,
		},
	}
}

// TestTransformLeaksNoRawContent is the spec's "find raw input in an outbound
// tip and fail" test (.agents/spec/09-a2a-and-hive.md, acceptance). It builds a
// suggestion carrying raw prompt text, a credential, a path, an endpoint, the
// workflow name, and the output, marshals the tip to the bytes that would
// leave the node, and fails if any raw string survives.
func TestTransformLeaksNoRawContent(t *testing.T) {
	t.Parallel()

	transformer, err := hive.NewTransformer(hive.CategoryJobSearch)
	if err != nil {
		t.Fatalf("new transformer: %v", err)
	}

	tip, ok := transformer.Transform(realisticSuggestion())
	if !ok {
		t.Fatal("transform refused a shareable param-tuning suggestion")
	}

	outbound, err := json.Marshal(tip)
	if err != nil {
		t.Fatalf("marshal outbound tip: %v", err)
	}

	wire := string(outbound)

	forbidden := []string{
		rawPromptText,
		rawAPIKey,
		rawFilePath,
		rawEndpoint,
		rawWorkflowName,
		rawOutputText,
		"score_candidate", // the action name is content-tied too
		"4096",            // the raw max_tokens value
		"512",             // the proposed value: only the direction may leak
		"0.0234",          // the exact saving: only the bucket may leak
	}

	for _, secret := range forbidden {
		if strings.Contains(wire, secret) {
			t.Errorf("outbound tip leaked raw content %q\nwire: %s", secret, wire)
		}
	}
}

// TestTransformEmitsOnlyAllowlistedFields proves the tip carries the expected
// content-free fields and nothing structurally unexpected: enum kind, fixed
// category, coarse runner type, bucketed savings, and a param shape with only a
// direction.
func TestTransformEmitsOnlyAllowlistedFields(t *testing.T) {
	t.Parallel()

	transformer, err := hive.NewTransformer(hive.CategoryJobSearch)
	if err != nil {
		t.Fatalf("new transformer: %v", err)
	}

	tip, ok := transformer.Transform(realisticSuggestion())
	if !ok {
		t.Fatal("transform refused a shareable suggestion")
	}

	if tip.Kind != store.SuggestionParamTuning {
		t.Errorf("tip kind = %q, want param_tuning", tip.Kind)
	}

	if tip.Category != hive.CategoryJobSearch {
		t.Errorf("tip category = %q, want job-search", tip.Category)
	}

	if tip.RunnerType != hive.RunnerTypeOpenAI {
		t.Errorf("tip runner type = %q, want openai", tip.RunnerType)
	}

	if tip.ParamShape == nil {
		t.Fatal("param-tuning tip carries no param shape")
	}

	if tip.ParamShape.Param != tuning.ProposedMaxTokens || tip.ParamShape.Direction != hive.DirectionLowered {
		t.Errorf("param shape = %+v, want lowered max_tokens", tip.ParamShape)
	}

	if !strings.HasSuffix(tip.CostBucket, "%") {
		t.Errorf("cost bucket = %q, want a percentage band", tip.CostBucket)
	}
}

// TestTransformBucketsCoarsely proves savings round down to a wide band rather
// than carrying the exact figure: 0.23 of a dollar becomes the "20%" bucket.
func TestTransformBucketsCoarsely(t *testing.T) {
	t.Parallel()

	transformer, err := hive.NewTransformer(hive.CategoryGeneral)
	if err != nil {
		t.Fatalf("new transformer: %v", err)
	}

	suggestion := store.Suggestion{
		Kind:     store.SuggestionCaching,
		Evidence: map[string]any{tuning.EvidenceRunner: openaiRunner},
		EstSavings: store.Savings{
			CostUSDPerRun: 0.23,
			Confidence:    store.ConfidenceHigh,
		},
	}

	tip, ok := transformer.Transform(suggestion)
	if !ok {
		t.Fatal("transform refused a caching suggestion")
	}

	if tip.CostBucket != bucket20 {
		t.Errorf("cost bucket = %q, want 20%% (0.23 floors to the 20 band)", tip.CostBucket)
	}
}

// TestTransformRefusesPromptRefactor proves the allowlist excludes the one kind
// that cannot be made content-free: a prompt_refactor suggestion never becomes
// a tip.
func TestTransformRefusesPromptRefactor(t *testing.T) {
	t.Parallel()

	transformer, err := hive.NewTransformer(hive.CategoryCodeGen)
	if err != nil {
		t.Fatalf("new transformer: %v", err)
	}

	suggestion := store.Suggestion{
		Kind:     "prompt_refactor",
		Evidence: map[string]any{tuning.EvidenceRunner: openaiRunner},
		Proposed: map[string]any{"prompt": rawPromptText},
	}

	if _, ok := transformer.Transform(suggestion); ok {
		t.Error("transform accepted a prompt_refactor suggestion; the allowlist must refuse it")
	}
}

// TestTransformRefusesMissingRunner proves a suggestion with no runner name in
// its evidence cannot be shaped into a tip: a tip with no runner type is not
// useful, and guessing one would be wrong.
func TestTransformRefusesMissingRunner(t *testing.T) {
	t.Parallel()

	transformer, err := hive.NewTransformer(hive.CategoryResearch)
	if err != nil {
		t.Fatalf("new transformer: %v", err)
	}

	suggestion := store.Suggestion{
		Kind:       store.SuggestionCaching,
		Evidence:   map[string]any{},
		EstSavings: store.Savings{CostUSDPerRun: 0.5},
	}

	if _, ok := transformer.Transform(suggestion); ok {
		t.Error("transform accepted a suggestion with no runner name")
	}
}

// TestTransformMapsUnknownRunnerToOther proves a node-specific runner name maps
// to the coarse "other" type rather than riding on the tip verbatim.
func TestTransformMapsUnknownRunnerToOther(t *testing.T) {
	t.Parallel()

	transformer, err := hive.NewTransformer(hive.CategoryAutomation)
	if err != nil {
		t.Fatalf("new transformer: %v", err)
	}

	suggestion := store.Suggestion{
		Kind:       store.SuggestionBatching,
		Evidence:   map[string]any{tuning.EvidenceRunner: "my_private_runner_42"},
		EstSavings: store.Savings{CostUSDPerRun: 0.5},
	}

	tip, ok := transformer.Transform(suggestion)
	if !ok {
		t.Fatal("transform refused a batching suggestion")
	}

	if tip.RunnerType != hive.RunnerTypeOther {
		t.Errorf("runner type = %q, want other (the private name must not leak)", tip.RunnerType)
	}

	outbound, err := json.Marshal(tip)
	if err != nil {
		t.Fatalf("marshal tip: %v", err)
	}

	if strings.Contains(string(outbound), "my_private_runner_42") {
		t.Errorf("tip leaked the private runner name: %s", outbound)
	}
}

// TestNewTransformerRejectsInvalidCategory proves a category outside the fixed
// set fails at construction.
func TestNewTransformerRejectsInvalidCategory(t *testing.T) {
	t.Parallel()

	if _, err := hive.NewTransformer("not-a-real-category"); !errors.Is(err, hive.ErrInvalidCategory) {
		t.Errorf("NewTransformer error = %v, want ErrInvalidCategory", err)
	}
}
