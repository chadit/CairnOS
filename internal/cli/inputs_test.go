package cli_test

import (
	"reflect"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/workflow"
)

// Input names reused across declarations and expectations.
const (
	keyCount = "count"
	keyLabel = "label"
	keyRatio = "ratio"
	keyDeep  = "deep"
	keyTags  = "tags"
)

func TestCoerceInputs(t *testing.T) {
	t.Parallel()

	decls := map[string]workflow.InputDecl{
		keyCount: {Type: workflow.TypeInt},
		keyRatio: {Type: workflow.TypeFloat},
		keyDeep:  {Type: workflow.TypeBool},
		keyTags:  {Type: workflow.TypeStringSlice},
		keyLabel: {Type: workflow.TypeString},
	}

	tests := []struct {
		want    map[string]any
		name    string
		pairs   []string
		wantErr bool
	}{
		{
			name:  "int and string slice",
			pairs: []string{"count=5", "tags=a,b"},
			want:  map[string]any{keyCount: 5, keyTags: []string{"a", "b"}},
		},
		{
			name:  "float bool and string",
			pairs: []string{"ratio=0.5", "deep=true", "label=summit"},
			want:  map[string]any{keyRatio: 0.5, keyDeep: true, keyLabel: "summit"},
		},
		{
			name:  "undeclared key passes through as text",
			pairs: []string{"other=x"},
			want:  map[string]any{"other": "x"},
		},
		{
			name:  "value containing an equals sign splits once",
			pairs: []string{"label=a=b"},
			want:  map[string]any{keyLabel: "a=b"},
		},
		{
			name:    "uncoercible int",
			pairs:   []string{"count=five"},
			wantErr: true,
		},
		{
			name:    "missing equals",
			pairs:   []string{keyCount},
			wantErr: true,
		},
		{
			name:    "empty key",
			pairs:   []string{"=5"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := cli.CoerceInputs(decls, tt.pairs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("coerce %v: want an error", tt.pairs)
				}

				return
			}

			if err != nil {
				t.Fatalf("coerce %v: %v", tt.pairs, err)
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("coerce %v = %#v, want %#v", tt.pairs, got, tt.want)
			}
		})
	}
}
