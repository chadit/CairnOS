package builtins_test

import (
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Vault query param names; tagsKey is shared with the memory tool tests.
const (
	tagsKey       = "tags"
	hopsKey       = "hops"
	linkedFromKey = "linked_from"
)

// TestVaultQueryValidateChecksFieldTypes proves an empty query is valid, a
// load-time reference passes, and a resolved field of the wrong type fails
// instead of being dropped from the query. Validate never reads the store, so
// the tool is built over none.
func TestVaultQueryValidateChecksFieldTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		params   map[string]any
		name     string
		wantCode string
		resolved bool
	}{
		{name: "empty query lists the vault", params: map[string]any{}, resolved: true},
		{
			name: "every field well formed",
			params: map[string]any{
				textKey: "breaker", projKey: "cairnos", linkedFromKey: "doc-1",
				tagsKey: []any{tagOps}, hopsKey: 2.0, limitKey: int64(5),
			},
			resolved: true,
		},
		{
			name: "null fields read as absent",
			params: map[string]any{
				textKey: nil, projKey: nil, linkedFromKey: nil,
				tagsKey: nil, hopsKey: nil, limitKey: nil,
			},
			resolved: true,
		},
		{name: "load-time reference in hops", params: map[string]any{hopsKey: "${inputs.hops}"}},
		{name: "load-time reference in tags", params: map[string]any{tagsKey: "${inputs.tags}"}},
		{
			name:     "text is not a string",
			params:   map[string]any{textKey: 7},
			resolved: true,
			wantCode: builtins.VaultQueryName + ".bad_text",
		},
		{
			name:     "linked_from is not a string",
			params:   map[string]any{linkedFromKey: []any{"doc-2"}},
			wantCode: builtins.VaultQueryName + ".bad_linked_from",
		},
		{
			name:     "resolved limit is not a number",
			params:   map[string]any{limitKey: "eleven"},
			resolved: true,
			wantCode: builtins.VaultQueryName + ".bad_limit",
		},
		{
			name:     "resolved tags is not a list",
			params:   map[string]any{tagsKey: "ops"},
			resolved: true,
			wantCode: builtins.VaultQueryName + ".bad_tags",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := builtins.NewVaultQueryTool(nil).Validate(&runner.Task{Params: tt.params, Resolved: tt.resolved})
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("Validate: %v, want nil", err)
				}

				return
			}

			assertCodeError(t, err, tt.wantCode)
		})
	}
}
