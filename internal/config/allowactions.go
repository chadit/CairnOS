package config

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	keyRunner       = "runner"
	keyExec         = "exec"
	keyAllowActions = "allow_actions"

	// newElementIndent indents an element added to a multi-line array that has
	// no element of its own to copy the indentation from.
	newElementIndent = "  "
)

var (
	// tableHeaderLine matches a bare-key [table] or [[table]] header with an
	// optional trailing comment. Quoted keys are not matched, which only means
	// such a file is refused instead of edited.
	tableHeaderLine = regexp.MustCompile(`^[ \t]*\[\[?[ \t]*([A-Za-z0-9_-]+(?:[ \t]*\.[ \t]*[A-Za-z0-9_-]+)*)[ \t]*\]\]?[ \t]*(?:#.*)?$`)
	allowActionsKey = regexp.MustCompile(`(?m)^[ \t]*` + keyAllowActions + `[ \t]*=[ \t]*`)
	headerSpaces    = regexp.MustCompile(`[ \t]*\.[ \t]*`)
)

// ExecAllowActions decodes [runner.exec] allow_actions from config text. An
// absent table or key answers an empty list, which is what the exec runner
// reads as deny everything.
func ExecAllowActions(src []byte) ([]string, error) {
	decoded, err := decodeAllowList(src)
	if err != nil {
		return nil, err
	}

	return decoded.actions, nil
}

// AddExecAllowAction returns src with name appended to [runner.exec]
// allow_actions. It edits only the array's text, since go-toml cannot write a
// document back with its comments. added is false when name is already
// listed. Both versions are decoded, and any difference beyond the one
// appended name is an error.
func AddExecAllowAction(src []byte, name string) ([]byte, bool, error) {
	before, err := decodeAllowList(src)
	if err != nil {
		return nil, false, err
	}

	if before.exec == nil {
		return nil, false, fmt.Errorf("%w: add a [runner.exec] block with a timeout first", ErrNoExecTable)
	}

	if slices.Contains(before.actions, name) {
		return src, false, nil
	}

	edited, err := insertAllowAction(string(src), name, before.actions != nil)
	if err != nil {
		return nil, false, err
	}

	if err := verifyAllowEdit(before, []byte(edited), name); err != nil {
		return nil, false, err
	}

	return []byte(edited), true, nil
}

// allowList is one decode of the config: the whole document, the exec table
// inside it (nil when absent), and that table's allow_actions.
type allowList struct {
	doc     map[string]any
	exec    map[string]any
	actions []string
}

func decodeAllowList(src []byte) (allowList, error) {
	var doc map[string]any

	if err := toml.Unmarshal(src, &doc); err != nil {
		return allowList{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	runners, _ := doc[keyRunner].(map[string]any)
	execTable, _ := runners[keyExec].(map[string]any)

	actions, err := stringList(execTable[keyAllowActions])
	if err != nil {
		return allowList{}, err
	}

	return allowList{doc: doc, exec: execTable, actions: actions}, nil
}

func stringList(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}

	items, isList := raw.([]any)
	if !isList {
		return nil, fmt.Errorf("%w: [runner.exec] %s must be an array of strings, got %T",
			ErrInvalidConfig, keyAllowActions, raw)
	}

	names := make([]string, 0, len(items))

	for _, item := range items {
		text, isText := item.(string)
		if !isText {
			return nil, fmt.Errorf("%w: [runner.exec] %s must hold only strings, found %T",
				ErrInvalidConfig, keyAllowActions, item)
		}

		names = append(names, text)
	}

	return names, nil
}

// verifyAllowEdit decodes the edited text and requires it to equal the
// original document with name appended to allow_actions and nothing else
// changed, which catches any edit that landed in the wrong span.
func verifyAllowEdit(before allowList, edited []byte, name string) error {
	after, err := decodeAllowList(edited)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrAllowActionsVerify, err)
	}

	want := append(slices.Clone(before.actions), name)
	if !slices.Equal(after.actions, want) {
		return fmt.Errorf("%w: got %q, want %q", ErrAllowActionsVerify, after.actions, want)
	}

	wantList := make([]any, 0, len(want))
	for _, action := range want {
		wantList = append(wantList, action)
	}

	before.exec[keyAllowActions] = wantList

	if !reflect.DeepEqual(before.doc, after.doc) {
		return fmt.Errorf("%w: a key other than allow_actions changed", ErrAllowActionsVerify)
	}

	return nil
}

// insertAllowAction finds the [runner.exec] section and either rewrites its
// allow_actions array or, when the key is absent, adds one under the header.
func insertAllowAction(text, name string, listed bool) (string, error) {
	bodyStart, bodyEnd, found := execSection(text)
	if !found {
		return "", fmt.Errorf("%w: [runner.exec] is not written as a plain table header", ErrAllowActionsShape)
	}

	loc := allowActionsKey.FindStringIndex(text[bodyStart:bodyEnd])
	if loc == nil && listed {
		return "", fmt.Errorf("%w: allow_actions is set outside a plain key line under [runner.exec]", ErrAllowActionsShape)
	}

	quoted := strconv.Quote(name)
	eol := lineEnding(text)

	if loc == nil {
		line := keyAllowActions + " = [" + quoted + "]" + eol

		// A header on the last line with no newline needs one before the key.
		if bodyStart == len(text) && !strings.HasSuffix(text, "\n") {
			line = eol + line
		}

		return text[:bodyStart] + line + text[bodyStart:], nil
	}

	return appendToArray(text, bodyStart+loc[1], quoted, eol)
}

// lineEnding answers the newline an inserted line uses, so a CRLF file gains
// CRLF lines instead of ending up with mixed endings.
func lineEnding(text string) string {
	if strings.Contains(text, "\r\n") {
		return "\r\n"
	}

	return "\n"
}

// execSection locates the lines under the [runner.exec] header: bodyStart is
// just past the header line and bodyEnd is the next header or the end.
func execSection(text string) (int, int, bool) {
	bodyStart := -1

	var offset int

	for offset < len(text) {
		line, next := lineAt(text, offset)

		// The header pattern anchors at the line end, so a CRLF line's \r would
		// otherwise stop a plain header from matching.
		name, isHeader := headerName(strings.TrimSuffix(line, "\r"))
		if isHeader && bodyStart >= 0 {
			return bodyStart, offset, true
		}

		if isHeader && name == keyRunner+"."+keyExec {
			bodyStart = next
		}

		offset = next
	}

	return bodyStart, len(text), bodyStart >= 0
}

// headerName reports whether line is a table header and, if so, its dotted
// name with the optional spaces around each dot removed.
func headerName(line string) (string, bool) {
	match := tableHeaderLine.FindStringSubmatch(line)
	if match == nil {
		return "", false
	}

	return headerSpaces.ReplaceAllString(match[1], "."), true
}

// lineAt returns the line starting at offset without its newline, and the
// offset of the line after it.
func lineAt(text string, offset int) (string, int) {
	end := strings.IndexByte(text[offset:], '\n')
	if end < 0 {
		return text[offset:], len(text)
	}

	return text[offset : offset+end], offset + end + 1
}

// appendToArray adds quoted to the array opening at open. A one-line array
// gets ", name" before its bracket; a multi-line array gets a new element line
// above its closing bracket, matching the last element's indent and comma.
func appendToArray(text string, open int, quoted, eol string) (string, error) {
	if open >= len(text) || text[open] != '[' {
		return "", fmt.Errorf("%w: allow_actions is not an inline array", ErrAllowActionsShape)
	}

	scan := arrayScan{text: text, pos: open, lastValueEnd: -1}

	closeAt, err := scan.run()
	if err != nil {
		return "", err
	}

	if !strings.Contains(text[open:closeAt], "\n") {
		return appendOneLine(text, open, closeAt, scan.lastValueEnd, quoted), nil
	}

	return appendMultiLine(text, open, closeAt, scan.lastValueEnd, quoted, eol)
}

func appendOneLine(text string, open, closeAt, lastValueEnd int, quoted string) string {
	switch {
	case lastValueEnd < 0:
		return text[:open] + "[" + quoted + "]" + text[closeAt+1:]
	case text[lastValueEnd-1] == ',':
		return text[:lastValueEnd] + " " + quoted + text[lastValueEnd:]
	default:
		return text[:lastValueEnd] + ", " + quoted + text[lastValueEnd:]
	}
}

func appendMultiLine(text string, open, closeAt, lastValueEnd int, quoted, eol string) (string, error) {
	closeLine := strings.LastIndexByte(text[:closeAt], '\n') + 1
	closeIndent := text[closeLine:closeAt]

	if strings.TrimLeft(closeIndent, " \t") != "" {
		return "", fmt.Errorf("%w: a multi-line allow_actions needs its closing bracket on a line of its own",
			ErrAllowActionsShape)
	}

	if lastValueEnd < 0 {
		return text[:closeLine] + closeIndent + newElementIndent + quoted + "," + eol + text[closeLine:], nil
	}

	valueLine := strings.LastIndexByte(text[:lastValueEnd], '\n') + 1

	indent := closeIndent + newElementIndent
	if valueLine > open {
		indent = text[valueLine : valueLine+len(text[valueLine:])-len(strings.TrimLeft(text[valueLine:], " \t"))]
	}

	// The new element keeps the list's trailing-comma habit: after a trailing
	// comma it gets one too, otherwise the old last element gains the comma.
	if text[lastValueEnd-1] == ',' {
		return text[:closeLine] + indent + quoted + "," + eol + text[closeLine:], nil
	}

	return text[:lastValueEnd] + "," + text[lastValueEnd:closeLine] + indent + quoted + eol + text[closeLine:], nil
}

// arrayScan walks one TOML array, skipping strings and comments, to find its
// closing bracket and the end of its last value or comma.
type arrayScan struct {
	text         string
	pos          int
	depth        int
	lastValueEnd int
}

func (s *arrayScan) run() (int, error) {
	for s.pos < len(s.text) {
		closed, err := s.step()
		if err != nil || closed {
			return s.pos, err
		}
	}

	return 0, fmt.Errorf("%w: allow_actions has no closing bracket", ErrAllowActionsShape)
}

func (s *arrayScan) step() (bool, error) {
	switch char := s.text[s.pos]; char {
	case '[':
		s.depth++
		s.pos++
	case ']':
		s.depth--
		if s.depth == 0 {
			return true, nil
		}

		s.pos++
	case '#':
		_, s.pos = lineAt(s.text, s.pos)
	case '"', '\'':
		return false, s.skipString(char)
	case ' ', '\t', '\r', '\n':
		s.pos++
	default:
		s.pos++
		s.lastValueEnd = s.pos
	}

	return false, nil
}

// skipString moves past one single-line string. Multi-line strings are
// refused rather than scanned, since no action name needs one.
func (s *arrayScan) skipString(quote byte) error {
	rest := s.text[s.pos:]
	if strings.HasPrefix(rest, `"""`) || strings.HasPrefix(rest, "'''") {
		return fmt.Errorf("%w: allow_actions holds a multi-line string", ErrAllowActionsShape)
	}

	end := s.pos + 1
	for end < len(s.text) && s.text[end] != quote && s.text[end] != '\n' {
		if quote == '"' && s.text[end] == '\\' {
			end++
		}

		end++
	}

	if end >= len(s.text) || s.text[end] != quote {
		return fmt.Errorf("%w: allow_actions holds an unterminated string", ErrAllowActionsShape)
	}

	s.pos = end + 1
	s.lastValueEnd = s.pos

	return nil
}
