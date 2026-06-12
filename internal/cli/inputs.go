package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/chadit/CairnOS/internal/workflow"
)

// inputFlags collects repeated --input key=value pairs in order.
type inputFlags []string

// String implements flag.Value.
func (f *inputFlags) String() string {
	return strings.Join(*f, ",")
}

// Set implements flag.Value.
func (f *inputFlags) Set(value string) error {
	*f = append(*f, value)

	return nil
}

// CoerceInputs turns --input key=value text into the native types the input
// declarations expect, because the engine validates typed values and a CLI
// flag only carries text. Undeclared keys pass through as strings so the
// engine reports them with its canonical unknown-input error.
func CoerceInputs(decls map[string]workflow.InputDecl, pairs []string) (map[string]any, error) {
	values := make(map[string]any, len(pairs))

	for _, pair := range pairs {
		key, raw, found := strings.Cut(pair, "=")
		if !found || key == "" {
			return nil, fmt.Errorf("%w: %q is not key=value", ErrBadInput, pair)
		}

		decl, declared := decls[key]
		if !declared {
			values[key] = raw

			continue
		}

		value, err := coerceValue(decl.Type, raw)
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", key, err)
		}

		values[key] = value
	}

	return values, nil
}

// coerceValue parses one raw flag value into a declared input type. The
// string[] form splits on commas, matching how list-valued flags are
// usually typed on a shell line.
func coerceValue(typeName, raw string) (any, error) {
	switch typeName {
	case workflow.TypeInt:
		num, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not an int", ErrBadInput, raw)
		}

		return num, nil
	case workflow.TypeFloat:
		num, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a float", ErrBadInput, raw)
		}

		return num, nil
	case workflow.TypeBool:
		truth, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a bool", ErrBadInput, raw)
		}

		return truth, nil
	case workflow.TypeStringSlice:
		return strings.Split(raw, ","), nil
	default:
		return raw, nil
	}
}
