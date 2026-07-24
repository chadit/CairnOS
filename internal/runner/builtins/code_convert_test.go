package builtins_test

import (
	"reflect"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
)

// roundTripScript echoes one param back as output, so a single script exercises
// both directions of the value mapping.
const roundTripScript = `output = params["v"]`

// valueTwo is shared list-element fixture data.
const valueTwo = "two"

func TestCodeMapsValuesBothDirections(t *testing.T) {
	t.Parallel()

	nested := map[string]any{
		"items": []any{
			map[string]any{"n": int64(2)},
			map[string]any{"n": int64(3)},
		},
	}

	cases := []struct {
		name string
		in   any
		want any
	}{
		{name: "none", in: nil, want: nil},
		{name: "bool", in: true, want: true},
		{name: "int64", in: int64(42), want: int64(42)},
		{name: "int normalizes to int64", in: 7, want: int64(7)},
		{name: "float64", in: 3.5, want: 3.5},
		{name: "string", in: "hi", want: "hi"},
		{name: "list", in: []any{int64(1), valueTwo, true}, want: []any{int64(1), valueTwo, true}},
		{name: "dict", in: map[string]any{"a": int64(1), "b": "x"}, want: map[string]any{"a": int64(1), "b": "x"}},
		{name: "nested", in: nested, want: nested},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := builtins.NewCode().Execute(t.Context(), codeTask(roundTripScript, map[string]any{"v": tt.in}, nil))
			if err != nil {
				t.Fatalf("execute: %v", err)
			}

			if !reflect.DeepEqual(result.Output, tt.want) {
				t.Fatalf("output = %#v, want %#v", result.Output, tt.want)
			}
		})
	}
}

func TestCodeTupleOutputBecomesList(t *testing.T) {
	t.Parallel()

	result, err := builtins.NewCode().Execute(t.Context(), codeTask(`output = (1, "two", True)`, nil, nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	want := []any{int64(1), valueTwo, true}
	if !reflect.DeepEqual(result.Output, want) {
		t.Fatalf("output = %#v, want %#v", result.Output, want)
	}
}

func TestCodeRejectsNonDataOutput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		script string
	}{
		// int64 max is 9223372036854775807, so +1 is a bigint that cannot fit.
		{name: "int outside int64", script: "output = 9223372036854775808"},
		{name: "function value", script: "output = lambda x: x"},
		{name: "non-string dict key", script: `output = {1: "a"}`},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := builtins.NewCode().Execute(t.Context(), codeTask(tt.script, nil, nil))
			assertCodeError(t, err, wantBadOutput)
		})
	}
}

func TestCodeRejectsUnmappableParam(t *testing.T) {
	t.Parallel()

	// A channel has no JSON shape and no Starlark equivalent, so building the
	// predeclared params dict fails before the script runs.
	task := codeTask(scriptNoop, map[string]any{"v": make(chan int)}, nil)

	_, err := builtins.NewCode().Execute(t.Context(), task)
	assertCodeError(t, err, wantBadParams)
}
