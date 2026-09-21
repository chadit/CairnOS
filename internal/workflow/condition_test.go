package workflow_test

import (
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/workflow"
)

// staticResolver answers references from a fixed table, standing in for the
// engine's runtime resolution.
func staticResolver(values map[string]any) workflow.Resolver {
	return func(ref workflow.Ref) (any, error) {
		value, ok := values[ref.Raw]
		if !ok {
			return nil, errors.New("no value for " + ref.Raw)
		}

		return value, nil
	}
}

func TestConditionEvaluatesComparisonsAndBooleans(t *testing.T) {
	t.Parallel()

	resolver := staticResolver(map[string]any{
		"${{ score.output.count }}": int64(3),
		"${{ score.output.name }}":  "go",
		"${{ score.output.ok }}":    true,
	})

	cases := []struct {
		name string
		expr string
		want bool
	}{
		{name: "number greater than", expr: "${{ score.output.count }} > 0", want: true},
		{name: "number less than fails", expr: "${{ score.output.count }} < 3", want: false},
		{name: "number equality with float literal", expr: "${{ score.output.count }} == 3.0", want: true},
		{name: "string equality single quotes", expr: "${{ score.output.name }} == 'go'", want: true},
		{name: "string equality double quotes", expr: `${{ score.output.name }} == "rust"`, want: false},
		{name: "bool literal comparison", expr: "${{ score.output.ok }} == true", want: true},
		{name: "and combines", expr: "${{ score.output.count }} > 0 && ${{ score.output.ok }}", want: true},
		{name: "or rescues", expr: "${{ score.output.count }} > 99 || ${{ score.output.ok }}", want: true},
		{name: "not inverts", expr: "!${{ score.output.ok }}", want: false},
		{name: "parens group", expr: "(${{ score.output.count }} > 99 || ${{ score.output.count }} > 0) && true", want: true},
		{name: "string ordering", expr: "'apple' < 'banana'", want: true},
		{name: "bare true literal", expr: "true", want: true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			expr, err := workflow.ParseCondition(tt.expr)
			if err != nil {
				t.Fatalf("parse %q: %v", tt.expr, err)
			}

			got, err := workflow.EvalBool(expr, resolver)
			if err != nil {
				t.Fatalf("eval %q: %v", tt.expr, err)
			}

			if got != tt.want {
				t.Fatalf("eval %q = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestConditionShortCircuitSkipsRightSide(t *testing.T) {
	t.Parallel()

	// The resolver has no values at all; short-circuiting means it is never
	// consulted when the left side already decides.
	resolver := staticResolver(map[string]any{})

	expr, err := workflow.ParseCondition("false && ${{ x.output }} == 1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got, err := workflow.EvalBool(expr, resolver)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}

	if got {
		t.Fatal("false && ... evaluated to true")
	}
}

func TestConditionRejectsBadSyntax(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		expr string
	}{
		{name: "trailing operator", expr: "1 =="},
		{name: "unknown word", expr: "maybe"},
		{name: "unterminated string", expr: "'open"},
		{name: "unterminated reference", expr: "${{ a.output"},
		{name: "missing close paren", expr: "(true"},
		{name: "garbage character", expr: "true @ false"},
		{name: "empty", expr: ""},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := workflow.ParseCondition(tt.expr); !errors.Is(err, workflow.ErrBadExpression) {
				t.Fatalf("parse %q error = %v, want ErrBadExpression", tt.expr, err)
			}
		})
	}
}

func TestConditionEvalRejectsTypeMismatches(t *testing.T) {
	t.Parallel()

	resolver := staticResolver(map[string]any{
		"${{ a.output }}": "text",
	})

	cases := []struct {
		name string
		expr string
	}{
		{name: "string compared to number", expr: "${{ a.output }} > 3"},
		{name: "not on a string", expr: "!${{ a.output }}"},
		{name: "and on a string", expr: "${{ a.output }} && true"},
		{name: "non-bool result", expr: "'just a string'"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			expr, err := workflow.ParseCondition(tt.expr)
			if err != nil {
				t.Fatalf("parse %q: %v", tt.expr, err)
			}

			if _, err := workflow.EvalBool(expr, resolver); !errors.Is(err, workflow.ErrEvalType) {
				t.Fatalf("eval %q error = %v, want ErrEvalType", tt.expr, err)
			}
		})
	}
}

func TestExtractRefsClassifiesNamespaces(t *testing.T) {
	t.Parallel()

	text := "a=${{ score.output.count }} b=${{ inputs.region }} c=${{ env.HOME }} d=${{ secrets.api_key }}"

	refs, errs := workflow.ExtractRefs(text)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	if len(refs) != 4 {
		t.Fatalf("got %d refs, want 4", len(refs))
	}

	wantKinds := []workflow.RefKind{workflow.RefAction, workflow.RefInput, workflow.RefEnv, workflow.RefSecret}
	wantTargets := []string{"score", "region", "HOME", "api_key"}

	for idx, ref := range refs {
		if ref.Kind != wantKinds[idx] {
			t.Errorf("ref %d kind = %v, want %v", idx, ref.Kind, wantKinds[idx])
		}

		if ref.Target != wantTargets[idx] {
			t.Errorf("ref %d target = %q, want %q", idx, ref.Target, wantTargets[idx])
		}
	}

	if len(refs[0].Path) != 1 || refs[0].Path[0] != "count" {
		t.Errorf("action ref path = %v, want [count]", refs[0].Path)
	}
}

func TestExtractRefsReportsMalformedBodies(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		text string
	}{
		{name: "missing output segment", text: "${{ score }}"},
		{name: "wrong namespace arity", text: "${{ inputs.a.b }}"},
		{name: "empty body", text: "${{ }}"},
		{name: "unterminated", text: "${{ score.output"},
		{name: "action without output keyword", text: "${{ score.result }}"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, errs := workflow.ExtractRefs(tt.text)
			if len(errs) == 0 {
				t.Fatalf("ExtractRefs(%q) reported no errors", tt.text)
			}

			if !errors.Is(errs[0], workflow.ErrBadReference) {
				t.Fatalf("error = %v, want ErrBadReference", errs[0])
			}
		})
	}
}

func TestWholeRefDetectsExactSingleReference(t *testing.T) {
	t.Parallel()

	ref, ok := workflow.WholeRef("  ${{ score.output }}  ")
	if !ok {
		t.Fatal("WholeRef rejected a lone reference")
	}

	if ref.Target != "score" {
		t.Fatalf("target = %q, want score", ref.Target)
	}

	if _, embedded := workflow.WholeRef("prefix ${{ score.output }}"); embedded {
		t.Fatal("WholeRef accepted embedded reference")
	}

	if _, double := workflow.WholeRef("${{ a.output }}${{ b.output }}"); double {
		t.Fatal("WholeRef accepted two references")
	}
}

func TestParseDurationSupportsDays(t *testing.T) {
	t.Parallel()

	got, err := workflow.ParseDuration("90d")
	if err != nil {
		t.Fatalf("parse 90d: %v", err)
	}

	const hoursPerDay = 24
	if got.Hours() != 90*hoursPerDay {
		t.Fatalf("90d = %v hours, want %v", got.Hours(), 90*hoursPerDay)
	}

	if _, err := workflow.ParseDuration("1h30m"); err != nil {
		t.Fatalf("parse 1h30m: %v", err)
	}

	if _, err := workflow.ParseDuration("soon"); !errors.Is(err, workflow.ErrBadDuration) {
		t.Fatalf("parse soon error = %v, want ErrBadDuration", err)
	}

	if _, err := workflow.ParseDuration(""); !errors.Is(err, workflow.ErrEmptyDuration) {
		t.Fatalf("empty duration error = %v, want ErrEmptyDuration", err)
	}
}
