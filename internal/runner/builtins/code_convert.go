package builtins

import (
	"fmt"
	"maps"
	"slices"

	"go.starlark.net/starlark"
)

// toStarlarkDict maps a params or inputs table into a fresh Starlark dict. It
// never returns nil, so an absent table still predeclares an empty dict the
// script can index. Keys go in sorted, because a Starlark dict iterates in
// insertion order and Go map order would hand a script a new order each run.
func toStarlarkDict(table map[string]any) (*starlark.Dict, error) {
	dict := starlark.NewDict(len(table))

	for _, key := range slices.Sorted(maps.Keys(table)) {
		err := putStarlark(table[key], func(converted starlark.Value) error {
			if err := dict.SetKey(starlark.String(key), converted); err != nil {
				return fmt.Errorf("set dict key %q: %w", key, err)
			}

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return dict, nil
}

// toStarlarkList maps a Go slice into a Starlark list element by element.
func toStarlarkList(items []any) (*starlark.List, error) {
	list := starlark.NewList(make([]starlark.Value, 0, len(items)))

	for _, item := range items {
		if err := putStarlark(item, list.Append); err != nil {
			return nil, err
		}
	}

	return list, nil
}

// putStarlark maps one JSON-shaped Go value into Starlark per the contract
// table and hands the result to put, the dict or list slot it fills. Handing it
// over keeps the recursion on the concrete *starlark.Dict and *starlark.List
// builders. An unmapped type is a permanent params error, never a silent drop.
func putStarlark(value any, put func(starlark.Value) error) error {
	switch typed := value.(type) {
	case nil:
		return put(starlark.None)
	case bool:
		return put(starlark.Bool(typed))
	case int:
		return put(starlark.MakeInt(typed))
	case int64:
		return put(starlark.MakeInt64(typed))
	case float64:
		return put(starlark.Float(typed))
	case string:
		return put(starlark.String(typed))
	case []any:
		list, err := toStarlarkList(typed)
		if err != nil {
			return err
		}

		return put(list)
	case map[string]any:
		dict, err := toStarlarkDict(typed)
		if err != nil {
			return err
		}

		return put(dict)
	default:
		return fmt.Errorf("%w: %T", errUnmappableValue, value)
	}
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
