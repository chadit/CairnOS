// Package shellquote renders a value as one word for a POSIX shell. It exists
// because more than one caller builds a command another machine's shell runs,
// and a value that loses its quoting there is a second command rather than an
// argument.
package shellquote

import "strings"

// Word wraps value so a POSIX shell reads it as a single literal word. A
// single-quoted string ends at the next quote, so an embedded quote is spliced
// back in rather than escaped.
func Word(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
