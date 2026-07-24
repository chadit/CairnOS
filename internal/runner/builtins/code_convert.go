package builtins

import (
	"fmt"
	"slices"

	"go.starlark.net/starlark"
)

// toStarlarkDict maps a params or inputs table into a fresh Starlark dict. It
// never returns nil, so an absent table still predeclares an empty dict the
// script can index.
func toStarlarkDict(table map[string]any) (*starlark.Dict, error) {
	dict := starlark.NewDict(len(table))

	for key, value := range table {
		converted, err := toStarlarkValue(value)
		if err != nil {
			return nil, err
		}

		if err := dict.SetKey(starlark.String(key), converted); err != nil {
			return nil, fmt.Errorf("set dict key %q: %w", key, err)
		}
	}

	return dict, nil
}

// toStarlarkValue maps one JSON-shaped Go value into Starlark per the contract
// table. An unmapped type is a permanent params error, never a silent drop.
func toStarlarkValue(value any) (starlark.Value, error) {
	switch typed := value.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(typed), nil
	case int:
		return starlark.MakeInt(typed), nil
	case int64:
		return starlark.MakeInt64(typed), nil
	case float64:
		return starlark.Float(typed), nil
	case string:
		return starlark.String(typed), nil
	case []any:
		return toStarlarkList(typed)
	case map[string]any:
		return toStarlarkDict(typed)
	default:
		return nil, fmt.Errorf("%w: %T", errUnmappableValue, value)
	}
}

// toStarlarkList maps a Go slice into a Starlark list element by element.
func toStarlarkList(items []any) (starlark.Value, error) {
	elems := make([]starlark.Value, 0, len(items))

	for _, item := range items {
		converted, err := toStarlarkValue(item)
		if err != nil {
			return nil, err
		}

		elems = append(elems, converted)
	}

	return starlark.NewList(elems), nil
}

// fromStarlarkValue maps one Starlark value back to a JSON-shaped Go value. A
// value outside the contract table (function, set, builtin, module) or an int
// outside int64 is a permanent output error.
func fromStarlarkValue(value starlark.Value) (any, error) {
	switch typed := value.(type) {
	case starlark.NoneType:
		return nil, nil //nolint:nilnil // Starlark None maps to Go nil; the nil here is the value, not an error
	case starlark.Bool:
		return bool(typed), nil
	case starlark.Int:
		return fromStarlarkInt(typed)
	case starlark.Float:
		return float64(typed), nil
	case starlark.String:
		return string(typed), nil
	case *starlark.List:
		return fromStarlarkElems(slices.Collect(typed.Elements()))
	case starlark.Tuple:
		return fromStarlarkElems(typed)
	case *starlark.Dict:
		return fromStarlarkDict(typed)
	default:
		return nil, fmt.Errorf("%w: %s is not a data value", errBadOutput, value.Type())
	}
}

// fromStarlarkInt converts a Starlark int, rejecting anything outside int64
// rather than truncating to a downstream value that differs from the script's.
func fromStarlarkInt(value starlark.Int) (any, error) {
	number, fits := value.Int64()
	if !fits {
		return nil, fmt.Errorf("%w: int %s exceeds int64 range", errBadOutput, value.String())
	}

	return number, nil
}

// fromStarlarkElems converts a list or tuple's elements into a Go slice. Both
// out-forms map to []any so downstream references read a uniform shape.
func fromStarlarkElems(elems []starlark.Value) (any, error) {
	out := make([]any, 0, len(elems))

	for _, elem := range elems {
		converted, err := fromStarlarkValue(elem)
		if err != nil {
			return nil, err
		}

		out = append(out, converted)
	}

	return out, nil
}

// fromStarlarkDict converts a Starlark dict into a Go map, rejecting non-string
// keys because the persistence contract in pkg/runner is JSON-shaped and JSON
// object keys are strings.
func fromStarlarkDict(dict *starlark.Dict) (any, error) {
	out := make(map[string]any, dict.Len())

	for key, value := range dict.Entries() {
		text, isString := key.(starlark.String)
		if !isString {
			return nil, fmt.Errorf("%w: dict key of type %s is not a string", errBadOutput, key.Type())
		}

		converted, err := fromStarlarkValue(value)
		if err != nil {
			return nil, err
		}

		out[string(text)] = converted
	}

	return out, nil
}
