package builtins_test

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
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
		keyItems: []any{
			map[string]any{"n": int64(2)},
			map[string]any{"n": int64(3)},
		},
	}

	cases := []struct {
		in   any
		want any
		name string
	}{
		{name: "none", in: nil, want: nil},
		{name: "bool", in: true, want: true},
		{name: "int64", in: int64(42), want: int64(42)},
		{name: "int normalizes to int64", in: 7, want: int64(7)},
		{name: "float64", in: 3.5, want: 3.5},
		{name: "string", in: testBody, want: testBody},
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

// TestCodeRejectsUnmappableParam covers a channel, which has no JSON shape and
// no Starlark equivalent, at the top of params and nested inside a list and a
// dict: building the predeclared params dict fails before the script runs.
func TestCodeRejectsUnmappableParam(t *testing.T) {
	t.Parallel()

	cases := []struct {
		value any
		name  string
	}{
		{name: "top level", value: make(chan int)},
		{name: "inside a list", value: []any{make(chan int)}},
		{name: "inside a dict", value: map[string]any{"c": make(chan int)}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			task := codeTask(scriptNoop, map[string]any{"v": tt.value}, nil)

			_, err := builtins.NewCode().Execute(t.Context(), task)
			assertCodeError(t, err, wantBadParams)
		})
	}
}

// TestCodeDictKeysIterateInSortedOrder pins the order a script sees a table's
// keys in. Twenty keys spread over several map groups, so an insert in Go map
// order would almost never come out sorted by chance.
func TestCodeDictKeysIterateInSortedOrder(t *testing.T) {
	t.Parallel()

	const keyCount = 20

	table := make(map[string]any, keyCount)
	for index := range keyCount {
		table[fmt.Sprintf("key%02d", index)] = int64(index)
	}

	want := make([]any, 0, len(table))
	for _, key := range slices.Sorted(maps.Keys(table)) {
		want = append(want, key)
	}

	cases := []struct {
		params map[string]any
		inputs map[string]any
		name   string
		script string
	}{
		{name: "inputs", script: `output = list(inputs.keys())`, inputs: table},
		{name: "nested param", script: `output = list(params["v"].keys())`, params: map[string]any{"v": table}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := builtins.NewCode().Execute(t.Context(), codeTask(tt.script, tt.params, tt.inputs))
			if err != nil {
				t.Fatalf("execute: %v", err)
			}

			if !reflect.DeepEqual(result.Output, want) {
				t.Fatalf("keys = %v, want %v", result.Output, want)
			}
		})
	}
}
