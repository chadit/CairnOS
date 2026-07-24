package workflow

import (
	"fmt"
	"strings"
)

// RefKind says which namespace an interpolation reference reads from.
type RefKind int

const (
	// RefAction reads an upstream action's output.
	RefAction RefKind = iota
	// RefInput reads a declared run input.
	RefInput
	// RefEnv reads an allowlisted environment variable.
	RefEnv
	// RefSecret reads a value from the configured secret source.
	RefSecret
	// RefItem reads the current element of a foreach fan-out (${{ item }} or
	// ${{ item.<field> }}); only valid inside an action that declares foreach.
	RefItem
	// RefIndex reads the zero-based position of the current foreach element
	// (${{ index }}); only valid inside a foreach action.
	RefIndex
)

// Ref is one parsed interpolation reference from a params value, a goal, or a
// condition expression.
type Ref struct {
	// Raw is the full reference text including the delimiters.
	Raw string
	// Kind says which namespace Target lives in.
	Kind RefKind
	// Target is the action, input, env, or secret name being read.
	Target string
	// Path is the field path into an action's output (the part after
	// "output"), empty when the whole output is referenced.
	Path []string
}

const (
	// refOpen and refClose delimit references. The doubled braces were chosen
	// so interpolation never collides with shell or TOML escaping in values
	// that legitimately contain a dollar sign.
	refOpen  = "${{"
	refClose = "}}"

	minRefParts = 2
	nsInputs    = "inputs"
	nsEnv       = "env"
	nsSecrets   = "secrets"
	nsItem      = "item"
	nsIndex     = "index"
	fieldOutput = "output"
)

// span is one located reference occurrence inside a string value.
type span struct {
	raw   string
	body  string
	start int
	end   int // index just past the closing delimiter
}

// ExtractRefs finds every interpolation reference in text. Malformed
// references come back as errors wrapping ErrBadReference so validation can
// report all of them with their raw text.
func ExtractRefs(text string) ([]Ref, []error) {
	spans, scanErrs := scanRefs(text)

	var (
		refs []Ref
		errs []error
	)

	errs = append(errs, scanErrs...)

	for _, sp := range spans {
		ref, err := parseRef(sp.raw, sp.body)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		refs = append(refs, ref)
	}

	return refs, errs
}

// WholeRef reports whether text is exactly one reference and nothing else,
// which is the case where the resolved value keeps its native type instead of
// being rendered into a string.
func WholeRef(text string) (Ref, bool) {
	trimmed := strings.TrimSpace(text)

	spans, scanErrs := scanRefs(trimmed)
	if len(scanErrs) > 0 || len(spans) != 1 {
		return Ref{}, false
	}

	if spans[0].start != 0 || spans[0].end != len(trimmed) {
		return Ref{}, false
	}

	ref, err := parseRef(spans[0].raw, spans[0].body)
	if err != nil {
		return Ref{}, false
	}

	return ref, true
}

// scanRefs locates every delimiter pair without interpreting the bodies. An
// unterminated opener is reported as an error rather than silently skipped,
// since it is almost always a typo the author wants to hear about.
func scanRefs(text string) ([]span, []error) {
	var (
		spans []span
		errs  []error
	)

	var offset int

	rest := text

	for {
		openAt := strings.Index(rest, refOpen)
		if openAt < 0 {
			return spans, errs
		}

		afterOpen := rest[openAt+len(refOpen):]

		closeAt := strings.Index(afterOpen, refClose)
		if closeAt < 0 {
			errs = append(errs, fmt.Errorf("%w: unterminated %q", ErrBadReference, refOpen))

			return spans, errs
		}

		rawEnd := openAt + len(refOpen) + closeAt + len(refClose)
		spans = append(spans, span{
			raw:   rest[openAt:rawEnd],
			body:  strings.TrimSpace(afterOpen[:closeAt]),
			start: offset + openAt,
			end:   offset + rawEnd,
		})

		offset += rawEnd
		rest = rest[rawEnd:]
	}
}

// parseRef classifies one reference body. The foreach namespaces (item,
// index) are single-part; namespaced references (inputs, env, secrets) take
// exactly two parts; everything else must be an action output reference of
// the shape name.output or name.output.field...
func parseRef(raw, body string) (Ref, error) {
	if body == "" {
		return Ref{}, fmt.Errorf("%w: %q", ErrBadReference, raw)
	}

	parts := strings.Split(body, ".")

	switch parts[0] {
	case nsItem:
		return parseItemRef(raw, parts), nil
	case nsIndex:
		return parseIndexRef(raw, parts)
	}

	if len(parts) < minRefParts {
		return Ref{}, fmt.Errorf("%w: %q", ErrBadReference, raw)
	}

	switch parts[0] {
	case nsInputs, nsEnv, nsSecrets:
		return parseNamespacedRef(raw, parts)
	default:
		return parseActionRef(raw, parts)
	}
}

// parseItemRef reads ${{ item }} or ${{ item.<field>... }}: the whole element
// or a path into it. The path mirrors action-output refs, so a bad field
// surfaces at resolution the same way a bad output field does.
func parseItemRef(raw string, parts []string) Ref {
	return Ref{Raw: raw, Kind: RefItem, Target: nsItem, Path: parts[1:]}
}

// parseIndexRef reads ${{ index }}. The index carries no path, so anything
// after it (index.foo) is malformed.
func parseIndexRef(raw string, parts []string) (Ref, error) {
	if len(parts) != 1 {
		return Ref{}, fmt.Errorf("%w: %q", ErrBadReference, raw)
	}

	return Ref{Raw: raw, Kind: RefIndex, Target: nsIndex}, nil
}

func parseNamespacedRef(raw string, parts []string) (Ref, error) {
	if len(parts) != minRefParts || parts[1] == "" {
		return Ref{}, fmt.Errorf("%w: %q", ErrBadReference, raw)
	}

	kind := RefInput

	switch parts[0] {
	case nsEnv:
		kind = RefEnv
	case nsSecrets:
		kind = RefSecret
	}

	return Ref{Raw: raw, Kind: kind, Target: parts[1]}, nil
}

func parseActionRef(raw string, parts []string) (Ref, error) {
	if parts[0] == "" || parts[1] != fieldOutput {
		return Ref{}, fmt.Errorf("%w: %q", ErrBadReference, raw)
	}

	return Ref{Raw: raw, Kind: RefAction, Target: parts[0], Path: parts[2:]}, nil
}
