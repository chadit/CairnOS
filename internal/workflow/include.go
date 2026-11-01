package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// CodeBadInclude marks a stdin_file param that cannot be read into the action:
// one on a runner other than exec, a path that is not a plain relative one, a file that is missing, oversized, or
// not UTF-8 text, or a workflow parsed from bytes with no directory to resolve
// against.
const CodeBadInclude = "workflow.bad_include"

const (
	// paramStdinFile names a task param holding a path to the text the stdin
	// param would otherwise carry inline. A long prompt then lives in one
	// editable file instead of a TOML string and a copy kept beside it.
	paramStdinFile = "stdin_file"
	paramStdin     = "stdin"

	// runnerExec is the only runner that reads stdin (builtins.ExecName, which
	// this package cannot import). Any other runner would drop the file's text.
	runnerExec = "exec"

	// rootEscapeText is the text of the error os.Root returns for a symlink
	// that leaves the directory. os keeps that sentinel unexported, so the text
	// is the only handle on it; a stdin_file test pins it.
	rootEscapeText = "path escapes from parent"

	// maxIncludeBytes caps one included file. A prompt is a few kilobytes; the
	// cap stops a mistyped path at a log or a dump from riding into every run.
	maxIncludeBytes = 256 << 10 // 256 KiB
)

// includedFile is one stdin_file read at load, kept so the definition hash
// covers the text as well as the workflow bytes that name it.
type includedFile struct {
	action string
	path   string
	text   string
}

// DefinitionHash is the identity a run records for this workflow: HashBytes of
// data when nothing was included, otherwise a SHA-256 over data and every
// included file, so editing a prompt file changes the hash the way editing an
// inline prompt does.
func (flow *Workflow) DefinitionHash(data []byte) string {
	if len(flow.includes) == 0 {
		return HashBytes(data)
	}

	hasher := sha256.New()
	hasher.Write(data)

	// Length-prefixing each part keeps two different splits of the same bytes
	// from hashing alike.
	for _, file := range flow.includes {
		hasher.Write([]byte("\x00" + strconv.Itoa(len(file.path)) + ":" + file.path))
		hasher.Write([]byte("\x00" + strconv.Itoa(len(file.text)) + ":" + file.text))
	}

	return hex.EncodeToString(hasher.Sum(nil))
}

// resolveIncludes swaps every stdin_file param for the stdin text it names.
// It runs before validate, so references inside the file get the same
// load-time checks as references written inline.
func (b *builder) resolveIncludes(flow *Workflow) {
	for _, name := range slices.Sorted(maps.Keys(flow.Actions)) {
		b.resolveStdinFile(flow, flow.Actions[name])
	}
}

// resolveStdinFile handles one action. The helpers below return a problem
// sentence rather than an error, because each becomes one cairnerr entry and
// nothing upstream branches on its cause.
func (b *builder) resolveStdinFile(flow *Workflow, action *Action) {
	raw, present := action.Params[paramStdinFile]
	if !present {
		return
	}

	delete(action.Params, paramStdinFile)

	rel, problem := b.includePath(action, raw)
	if problem == "" {
		var text string

		text, problem = readInclude(b.dir, rel)
		if problem == "" {
			action.Params[paramStdin] = text
			flow.includes = append(flow.includes, includedFile{action: action.Name, path: rel, text: text})

			return
		}
	}

	b.addf(CodeBadInclude, "workflow %q action %q: %s: %s", flow.Name, action.Name, paramStdinFile, problem)
}

// includePath checks everything about the param that needs no disk access.
func (b *builder) includePath(action *Action, raw any) (string, string) {
	rel, isText := raw.(string)

	switch {
	case action.Runner != runnerExec:
		return "", fmt.Sprintf("only the %s runner reads stdin, and this action runs on %q", runnerExec, action.Runner)
	case !isText:
		return "", fmt.Sprintf("must be a path string, got %T", raw)
	case b.dir == "":
		return "", strconv.Quote(rel) + " needs the workflow loaded from a file, since the path is relative to that file's directory"
	}

	if _, inline := action.Params[paramStdin]; inline {
		return "", strconv.Quote(rel) + " cannot be set alongside " + paramStdin + "; pick one"
	}

	return rel, checkIncludePath(rel)
}

// checkIncludePath refuses any path that is not a plain relative one. The
// directory root in readInclude already stops an escape; refusing ".." and
// absolute paths up front gives the author a message that says why.
func checkIncludePath(rel string) string {
	switch {
	case rel == "":
		return "path is empty"
	case strings.Contains(rel, refOpen):
		return strconv.Quote(rel) + " must be a literal path; " + refOpen + " is not expanded here"
	case !filepath.IsLocal(rel):
		return strconv.Quote(rel) + " must be a path relative to the workflow file's directory"
	case slices.Contains(strings.Split(filepath.ToSlash(rel), "/"), ".."):
		return strconv.Quote(rel) + " must not contain a .. element"
	}

	return ""
}

// readInclude reads rel through an os.Root on dir, which resolves symlinks and
// refuses any that lead outside it. O_NONBLOCK lets a FIFO open at once, and
// checking the open handle rather than the path leaves no window to swap one
// in. The limited read catches a file that grew after the check.
func readInclude(dir, rel string) (string, string) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", "open workflow directory: " + err.Error()
	}

	defer func() { _ = root.Close() }()

	file, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", describeIncludeErr(rel, dir, err)
	}

	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return "", "stat " + strconv.Quote(rel) + ": " + err.Error()
	}

	if problem := checkIncludeInfo(rel, info); problem != "" {
		return "", problem
	}

	return readIncludeText(rel, file)
}

func readIncludeText(rel string, file io.Reader) (string, string) {
	data, err := io.ReadAll(io.LimitReader(file, maxIncludeBytes+1))

	switch {
	case err != nil:
		return "", "read " + strconv.Quote(rel) + ": " + err.Error()
	case len(data) > maxIncludeBytes:
		return "", fmt.Sprintf("%q grew past the %d byte limit while it was read", rel, maxIncludeBytes)
	case !utf8.Valid(data):
		return "", strconv.Quote(rel) + " is not UTF-8 text"
	}

	return string(data), ""
}

func checkIncludeInfo(rel string, info fs.FileInfo) string {
	if !info.Mode().IsRegular() {
		return strconv.Quote(rel) + " is not a regular file"
	}

	if info.Size() > maxIncludeBytes {
		return fmt.Sprintf("%q is %d bytes, over the %d byte limit", rel, info.Size(), maxIncludeBytes)
	}

	return ""
}

// describeIncludeErr names the directory for a missing file, since the
// operator most often has the path right and the working directory wrong. An
// escape can only come from a symlink, since checkIncludePath already refused
// ".." and absolute paths, and os.Root's own wording does not say so.
func describeIncludeErr(rel, dir string, err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return strconv.Quote(rel) + " not found in " + dir
	}

	if pathErr, isPath := errors.AsType[*fs.PathError](err); isPath && pathErr.Err.Error() == rootEscapeText {
		return strconv.Quote(rel) + " is a symlink that leaves the workflow directory (absolute symlink targets are refused)"
	}

	return "open " + strconv.Quote(rel) + ": " + err.Error()
}
