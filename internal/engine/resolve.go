package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/workflow"
)

// resolver builds the lookup behind every ${{ }} reference: upstream outputs,
// validated run inputs, allowlisted environment variables, and the secret
// source. The same resolver backs param interpolation and condition
// evaluation, so both see identical values.
func (e *Engine) resolver(ctx context.Context, inputs, outputs map[string]any) workflow.Resolver {
	return func(ref workflow.Ref) (any, error) {
		switch ref.Kind {
		case workflow.RefAction:
			return resolveActionRef(ref, outputs)
		case workflow.RefInput:
			return resolveInputRef(ref, inputs)
		case workflow.RefEnv:
			return e.resolveEnv(ref)
		case workflow.RefSecret:
			return e.resolveSecret(ctx, ref)
		case workflow.RefSystem:
			return e.resolveSystem(ref)
		case workflow.RefItem, workflow.RefIndex:
			// item and index only exist inside a fan-out, layered on by
			// foreachResolver; the base resolver never binds them. Validation
			// rejects them elsewhere, so reaching here is a precondition bug.
			return nil, errf(CodePreconditionFailed, "%s is only available inside a foreach action", ref.Raw)
		default:
			return nil, errf(CodePreconditionFailed, "unsupported reference %s", ref.Raw)
		}
	}
}

// foreachResolver layers the current element and its position over the base
// resolver so ${{ item }} and ${{ index }} bind during a fan-out; every other
// reference falls through unchanged.
func (e *Engine) foreachResolver(ctx context.Context, inputs, outputs map[string]any, item any, index int) workflow.Resolver {
	base := e.resolver(ctx, inputs, outputs)

	return func(ref workflow.Ref) (any, error) {
		if ref.Kind == workflow.RefItem {
			return resolveItemRef(ref, item)
		}

		if ref.Kind == workflow.RefIndex {
			return index, nil
		}

		return base(ref)
	}
}

// resolveItemRef reads the current foreach element, traversing the reference
// path through structured fields the same way an action output does.
func resolveItemRef(ref workflow.Ref, item any) (any, error) {
	value := item

	for _, field := range ref.Path {
		object, isObject := value.(map[string]any)
		if !isObject {
			return nil, errf(CodePreconditionFailed,
				"%s: cannot read field %q of a %T value", ref.Raw, field, value)
		}

		next, exists := object[field]
		if !exists {
			return nil, errf(CodePreconditionFailed, "%s: item has no field %q", ref.Raw, field)
		}

		value = next
	}

	return value, nil
}

// resolveParams interpolates every reference in an action's params at launch
// time, after upstream results are persisted
// (.agents/spec/02-workflow-schema.md, "Interpolation and data passing").
func (e *Engine) resolveParams(ctx context.Context, params, inputs, outputs map[string]any) (map[string]any, error) {
	return resolveParamsWith(params, e.resolver(ctx, inputs, outputs))
}

// resolveParamsWith interpolates params against a supplied resolver, so a
// foreach item can reuse the same walk with item and index bound on top.
func resolveParamsWith(params map[string]any, resolve workflow.Resolver) (map[string]any, error) {
	resolved := make(map[string]any, len(params))

	for key, value := range params {
		item, err := resolveValue(value, resolve)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", key, err)
		}

		resolved[key] = item
	}

	return resolved, nil
}

// resolveGoal interpolates an agent action's goal at launch time under the
// same reference rules as task params: a goal that is exactly one reference
// takes the referenced value, embedded references render scalars into the
// surrounding text. A non-string whole value renders through the scalar
// rules so the loop always receives text.
func (e *Engine) resolveGoal(ctx context.Context, action *workflow.Action, inputs, outputs map[string]any) (string, error) {
	value, err := resolveString(action.Goal, e.resolver(ctx, inputs, outputs))
	if err != nil {
		return "", fmt.Errorf("goal: %w", err)
	}

	if text, isText := value.(string); isText {
		return text, nil
	}

	rendered, err := renderScalar(value)
	if err != nil {
		return "", errf(CodePreconditionFailed, "agent action %q goal: %v", action.Name, err)
	}

	return rendered, nil
}

// resolveSystem answers a system.* reference at launch, so a run reports the host
// it actually ran on rather than the one that loaded the workflow.
func (e *Engine) resolveSystem(ref workflow.Ref) (any, error) {
	if ref.Target != workflow.SystemHostname {
		return nil, errf(CodePreconditionFailed, "unknown system value %s", ref.Raw)
	}

	hostname, err := e.cfg.Hostname()
	if err != nil {
		return nil, errf(CodePreconditionFailed, "%s: read hostname: %v", ref.Raw, err)
	}

	return hostname, nil
}

// resolveEnv reads an environment variable, but only when its name is in the
// engine allowlist; everything else is a configuration error, never a leak.
func (e *Engine) resolveEnv(ref workflow.Ref) (any, error) {
	if !slices.Contains(e.cfg.EnvAllowlist, ref.Target) {
		return nil, errf(CodeConfigInvalid,
			"environment variable %q is not in the engine allowlist", ref.Target)
	}

	return os.Getenv(ref.Target), nil
}

// resolveSecret reads from the configured secret source. A missing source
// and a missing value report the same canonical code, since both mean the
// run cannot have that secret.
func (e *Engine) resolveSecret(ctx context.Context, ref workflow.Ref) (any, error) {
	if e.cfg.Secrets == nil {
		return nil, errf(CodeMissingSecret, "secret %q: %v", ref.Target, ErrNoSecretSource)
	}

	value, err := e.cfg.Secrets.Get(ctx, ref.Target)
	if err != nil {
		return nil, errf(CodeMissingSecret, "secret %q: %v", ref.Target, err)
	}

	return value, nil
}

// resolveValue walks one params value, interpolating strings and recursing
// into lists and tables. Non-string scalars pass through untouched.
func resolveValue(value any, resolve workflow.Resolver) (any, error) {
	switch typed := value.(type) {
	case string:
		return resolveString(typed, resolve)
	case []any:
		return resolveList(typed, resolve)
	case map[string]any:
		return resolveTable(typed, resolve)
	default:
		return value, nil
	}
}

func resolveList(items []any, resolve workflow.Resolver) (any, error) {
	out := make([]any, len(items))

	for idx, item := range items {
		resolved, err := resolveValue(item, resolve)
		if err != nil {
			return nil, err
		}

		out[idx] = resolved
	}

	return out, nil
}

func resolveTable(table map[string]any, resolve workflow.Resolver) (any, error) {
	out := make(map[string]any, len(table))

	for key, item := range table {
		resolved, err := resolveValue(item, resolve)
		if err != nil {
			return nil, err
		}

		out[key] = resolved
	}

	return out, nil
}

// resolveString applies the schema's type rule: a string that is exactly one
// reference resolves to the native value; embedded references render scalars
// into the surrounding text.
func resolveString(text string, resolve workflow.Resolver) (any, error) {
	if ref, whole := workflow.WholeRef(text); whole {
		value, err := resolve(ref)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", ref.Raw, err)
		}

		return value, nil
	}

	refs, refErrs := workflow.ExtractRefs(text)
	if len(refErrs) > 0 {
		return nil, errf(CodePreconditionFailed, "bad reference in %q: %v", text, refErrs[0])
	}

	for _, ref := range refs {
		rendered, err := renderRef(ref, resolve)
		if err != nil {
			return nil, err
		}

		text = strings.ReplaceAll(text, ref.Raw, rendered)
	}

	return text, nil
}

// renderRef resolves one embedded reference into string form. Only scalars
// may embed: splicing an object or a list into surrounding text almost
// always hides a bug, so it fails instead.
func renderRef(ref workflow.Ref, resolve workflow.Resolver) (string, error) {
	value, err := resolve(ref)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref.Raw, err)
	}

	rendered, err := renderScalar(value)
	if err != nil {
		return "", errf(CodePreconditionFailed, "%s: %v", ref.Raw, err)
	}

	return rendered, nil
}

func renderScalar(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case bool:
		return strconv.FormatBool(typed), nil
	case int:
		return strconv.Itoa(typed), nil
	case int64:
		return strconv.FormatInt(typed, 10), nil
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("%w: %T", ErrNonScalarEmbed, value)
	}
}

// resolveActionRef reads an upstream output, traversing the reference path
// through structured fields. A missing output or field is a precondition
// failure: the runner is never called.
func resolveActionRef(ref workflow.Ref, outputs map[string]any) (any, error) {
	value, exists := outputs[ref.Target]
	if !exists {
		return nil, errf(CodePreconditionFailed, "action %q has no recorded output", ref.Target)
	}

	for _, field := range ref.Path {
		next, err := stepActionPath(value, field, ref.Raw)
		if err != nil {
			return nil, err
		}

		value = next
	}

	return value, nil
}

// stepActionPath advances one path segment. A map reads by field name even when
// the segment is numeric, so an output shaped {"0": ...} keeps its meaning. A
// non-negative in-range numeric segment on a list indexes it. Every other case
// fails as a precondition before the runner runs.
func stepActionPath(value any, field, raw string) (any, error) {
	switch container := value.(type) {
	case map[string]any:
		next, exists := container[field]
		if !exists {
			return nil, errf(CodePreconditionFailed, "%s: output has no field %q", raw, field)
		}

		return next, nil
	case []any:
		return indexActionList(container, field, raw)
	default:
		return nil, errf(CodePreconditionFailed,
			"%s: cannot read field %q of a %T value", raw, field, value)
	}
}

// indexActionList indexes a list by a numeric segment. A negative or
// non-numeric segment keeps the field-shaped error so a list and a scalar read
// alike on misuse; an out-of-range index names the index and the length.
func indexActionList(list []any, field, raw string) (any, error) {
	index, err := strconv.Atoi(field)
	if err != nil || index < 0 {
		return nil, errf(CodePreconditionFailed,
			"%s: cannot read field %q of a %T value", raw, field, list)
	}

	if index >= len(list) {
		return nil, errf(CodePreconditionFailed,
			"%s: index %d out of range for a list of length %d", raw, index, len(list))
	}

	return list[index], nil
}

// resolveInputRef reads a validated run input. A declared optional input
// with no default and no supplied value has nothing to resolve.
func resolveInputRef(ref workflow.Ref, inputs map[string]any) (any, error) {
	value, exists := inputs[ref.Target]
	if !exists {
		return nil, errf(CodePreconditionFailed, "input %q has no value for this run", ref.Target)
	}

	return value, nil
}

// asPrecondition surfaces a resolution failure as the action's error. An
// error already carrying a canonical code (config.invalid,
// config.missing_secret) keeps it; anything else becomes
// action.precondition_failed, per the interpolation rules.
func asPrecondition(err error) *cairnerr.Error {
	if cerr, isCanonical := errors.AsType[*cairnerr.Error](err); isCanonical {
		return cerr
	}

	return errf(CodePreconditionFailed, "%v", err)
}
