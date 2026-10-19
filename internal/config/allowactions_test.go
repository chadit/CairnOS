package config_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/config"
)

// addedName is the action every add case appends.
const addedName = "report"

func TestAddExecAllowActionEditsOnlyTheArray(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "one-line array keeps the comments around it",
			src:  "# host config\n[runner.exec]\n# who may run\nallow_actions = [\"reconcile\"]\ntimeout = 540\n",
			want: "# host config\n[runner.exec]\n# who may run\nallow_actions = [\"reconcile\", \"report\"]\ntimeout = 540\n",
		},
		{
			name: "empty one-line array",
			src:  "[runner.exec]\nallow_actions = []\ntimeout = 5\n",
			want: "[runner.exec]\nallow_actions = [\"report\"]\ntimeout = 5\n",
		},
		{
			name: "one-line array with a trailing comma and comment",
			src:  "[runner.exec]\nallow_actions = [ \"a\", ] # reviewed\ntimeout = 5\n",
			want: "[runner.exec]\nallow_actions = [ \"a\", \"report\" ] # reviewed\ntimeout = 5\n",
		},
		{
			name: "existing entry holding a bracket and a hash",
			src:  "[runner.exec]\nallow_actions = [\"x]#y\"]\ntimeout = 5\n",
			want: "[runner.exec]\nallow_actions = [\"x]#y\", \"report\"]\ntimeout = 5\n",
		},
		{
			name: "multi-line array with trailing commas and comments",
			src:  "[runner.exec]\nallow_actions = [\n    \"a\", # first\n    \"b\",\n]\ntimeout = 5\n",
			want: "[runner.exec]\nallow_actions = [\n    \"a\", # first\n    \"b\",\n    \"report\",\n]\ntimeout = 5\n",
		},
		{
			name: "multi-line array without a trailing comma",
			src:  "[runner.exec]\nallow_actions = [\n  \"a\",\n  \"b\" # last\n]\ntimeout = 5\n",
			want: "[runner.exec]\nallow_actions = [\n  \"a\",\n  \"b\", # last\n  \"report\"\n]\ntimeout = 5\n",
		},
		{
			name: "empty multi-line array",
			src:  "[runner.exec]\nallow_actions = [\n]\ntimeout = 5\n",
			want: "[runner.exec]\nallow_actions = [\n  \"report\",\n]\ntimeout = 5\n",
		},
		{
			name: "absent key goes under the header",
			src:  "[runner.exec] # host commands\ntimeout = 5\n\n[runner.exec.env]\nA = \"b\"\n",
			want: "[runner.exec] # host commands\nallow_actions = [\"report\"]\ntimeout = 5\n\n[runner.exec.env]\nA = \"b\"\n",
		},
		{
			name: "another runner's allow_actions is left alone",
			src:  "[runner.sandbox]\nallow_actions = [\"z\"]\n\n[ runner . exec ]\nallow_actions = [\"a\"]\ntimeout = 5\n",
			want: "[runner.sandbox]\nallow_actions = [\"z\"]\n\n[ runner . exec ]\nallow_actions = [\"a\", \"report\"]\ntimeout = 5\n",
		},
		{
			name: "header at the end of a file with no newline",
			src:  "timeout_note = 1\n[runner.exec]",
			want: "timeout_note = 1\n[runner.exec]\nallow_actions = [\"report\"]\n",
		},
		{
			name: "CRLF file with a plain header gains a CRLF key line",
			src:  "[runner.exec]\r\ntimeout = 5\r\n",
			want: "[runner.exec]\r\nallow_actions = [\"report\"]\r\ntimeout = 5\r\n",
		},
		{
			name: "CRLF header at the end of the file",
			src:  "a = 1\r\n[runner.exec]",
			want: "a = 1\r\n[runner.exec]\r\nallow_actions = [\"report\"]\r\n",
		},
		{
			name: "CRLF multi-line array gains a CRLF element line",
			src:  "[runner.exec]\r\nallow_actions = [\r\n  \"a\",\r\n]\r\ntimeout = 5\r\n",
			want: "[runner.exec]\r\nallow_actions = [\r\n  \"a\",\r\n  \"report\",\r\n]\r\ntimeout = 5\r\n",
		},
		{
			name: "CRLF multi-line array without a trailing comma",
			src:  "[runner.exec]\r\nallow_actions = [\r\n  \"a\"\r\n]\r\ntimeout = 5\r\n",
			want: "[runner.exec]\r\nallow_actions = [\r\n  \"a\",\r\n  \"report\"\r\n]\r\ntimeout = 5\r\n",
		},
		{
			name: "CRLF empty multi-line array",
			src:  "[runner.exec]\r\nallow_actions = [\r\n]\r\ntimeout = 5\r\n",
			want: "[runner.exec]\r\nallow_actions = [\r\n  \"report\",\r\n]\r\ntimeout = 5\r\n",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, added, err := config.AddExecAllowAction([]byte(testCase.src), addedName)
			if err != nil {
				t.Fatalf("add: %v", err)
			}

			if !added {
				t.Fatal("added = false, want true")
			}

			if string(got) != testCase.want {
				t.Fatalf("result:\n%s\nwant:\n%s", got, testCase.want)
			}
		})
	}
}

func TestAddExecAllowActionIsIdempotent(t *testing.T) {
	t.Parallel()

	src := []byte("[runner.exec]\n# keep\nallow_actions = [\"report\", \"a\"]\ntimeout = 5\n")

	got, added, err := config.AddExecAllowAction(src, addedName)
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	if added {
		t.Fatal("added = true for a name already listed")
	}

	if !bytes.Equal(got, src) {
		t.Fatalf("result changed:\n%s", got)
	}
}

func TestAddExecAllowActionRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		want error
		name string
		src  string
	}{
		{name: "not TOML", src: "[runner.exec\nallow_actions = [", want: config.ErrInvalidConfig},
		{name: "no exec table", src: "[runner.usage]\ntimeout = 5\n", want: config.ErrNoExecTable},
		{name: "empty file", src: "", want: config.ErrNoExecTable},
		{name: "allow_actions is not a list", src: "[runner.exec]\nallow_actions = \"a\"\n", want: config.ErrInvalidConfig},
		{name: "allow_actions holds a number", src: "[runner.exec]\nallow_actions = [1]\n", want: config.ErrInvalidConfig},
		{
			name: "closing bracket shares a line with a value",
			src:  "[runner.exec]\nallow_actions = [\n  \"a\"]\ntimeout = 5\n",
			want: config.ErrAllowActionsShape,
		},
		{
			name: "multi-line string element",
			src:  "[runner.exec]\nallow_actions = ['''a''']\ntimeout = 5\n",
			want: config.ErrAllowActionsShape,
		},
		{
			name: "exec written as an inline table",
			src:  "[runner]\nexec = { timeout = 5, allow_actions = [\"a\"] }\n",
			want: config.ErrAllowActionsShape,
		},
		{
			name: "quoted allow_actions key",
			src:  "[runner.exec]\n\"allow_actions\" = [\"a\"]\ntimeout = 5\n",
			want: config.ErrAllowActionsShape,
		},
		{
			// The key pattern finds the line inside the note first. The decode
			// check sees the note change and refuses instead of writing it.
			name: "look-alike line inside a multi-line string",
			src:  "[runner.exec]\nnote = \"\"\"\nallow_actions = [\"fake\"]\n\"\"\"\nallow_actions = [\"a\"]\ntimeout = 5\n",
			want: config.ErrAllowActionsVerify,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, added, err := config.AddExecAllowAction([]byte(testCase.src), addedName)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}

			if added || got != nil {
				t.Fatalf("added = %v, result = %q; a refusal returns nothing to write", added, got)
			}
		})
	}
}

func TestExecAllowActions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
		want []string
	}{
		{name: "listed names in order", src: "[runner.exec]\nallow_actions = [\"b\", \"a\"]\n", want: []string{"b", "a"}},
		{name: "no exec table to read", src: "[server]\nport = 1\n", want: nil},
		{name: "no allow_actions key", src: "[runner.exec]\ntimeout = 5\n", want: nil},
		{name: "empty list", src: "[runner.exec]\nallow_actions = []\n", want: []string{}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := config.ExecAllowActions([]byte(testCase.src))
			if err != nil {
				t.Fatalf("list: %v", err)
			}

			if !slices.Equal(got, testCase.want) {
				t.Fatalf("names = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestExecAllowActionsRejectsBadFile(t *testing.T) {
	t.Parallel()

	if _, err := config.ExecAllowActions([]byte("[runner.exec")); !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("error = %v, want %v", err, config.ErrInvalidConfig)
	}
}
