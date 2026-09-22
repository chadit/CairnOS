package shellquote_test

import (
	"testing"

	"github.com/chadit/CairnOS/internal/shellquote"
)

func TestWord(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "an empty value still occupies one word", value: "", want: "''"},
		{name: "a plain value", value: "/usr/bin/claude", want: "'/usr/bin/claude'"},
		{name: "a value a shell would act on", value: "a b;id", want: "'a b;id'"},
		{name: "an embedded quote is spliced, not escaped", value: "it's", want: `'it'\''s'`},
		{name: "a value that is only a quote", value: "'", want: `''\'''`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := shellquote.Word(testCase.value); got != testCase.want {
				t.Fatalf("Word(%q) = %q, want %q", testCase.value, got, testCase.want)
			}
		})
	}
}
