// Package expr resolves {{ }} templates against a flow's execution state.
//
// Deliberately implemented on top of goja (already a dependency for the CODE
// sandbox) rather than a hand-rolled expression parser: activepieces itself
// uses jsep + a custom evaluator to approximate JS expression semantics
// ({{ 1 + 2 }}, {{ Math.min(a, b) }}, {{ a.b[0] }}); running the trimmed
// expression as real JS through goja gets full JS semantics for free instead
// of re-approximating a subset of them.
package expr

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dop251/goja"

	"goflow/pkg/model"
)

// wholeTemplate matches a string that is ENTIRELY one {{ ... }} block, e.g.
// "{{ 1 + 2 }}" — the whole-match case returns the raw JS-evaluated value
// (e.g. the number 3), not its string form, matching activepieces' behavior
// where `{"key": "{{ 1 + 2 }}"}` resolves to `{"key": 3}`.
var wholeTemplate = regexp.MustCompile(`^\{\{(.*)\}\}$`)

// anyTemplate matches one or more {{ ... }} blocks anywhere in a string, for
// the mixed-text interpolation case (e.g. "hello {{ name }}!").
var anyTemplate = regexp.MustCompile(`\{\{(.*?)\}\}`)

// Scope is the set of named values {{ }} expressions can reference — one
// entry per step name (trigger and actions), each exposing .output/.input/.status,
// matching activepieces' `{{ stepName.output.field }}` convention.
type Scope map[string]any

// NewScope builds a Scope from an ExecutionState: every recorded step becomes
// scope[stepName] = {output, input, status}.
func NewScope(state *model.ExecutionState) Scope {
	scope := Scope{}
	for name, step := range state.Steps {
		scope[name] = map[string]any{
			"output": step.Output,
			"input":  step.Input,
			"status": string(step.Status),
		}
	}
	return scope
}

// Eval runs a single JS expression (no {{ }} delimiters) against scope and
// returns the raw Go value (goja's Export()).
func Eval(expression string, scope Scope) (any, error) {
	vm := goja.New()
	for name, value := range scope {
		if err := vm.Set(name, value); err != nil {
			return nil, fmt.Errorf("expr: binding %q: %w", name, err)
		}
	}
	v, err := vm.RunString(expression)
	if err != nil {
		return nil, fmt.Errorf("expr: evaluating %q: %w", expression, err)
	}
	return v.Export(), nil
}

// Resolve walks value recursively (map[string]any, []any, string, or a
// scalar) and resolves every {{ }} template it finds against scope.
// Non-string, non-container values pass through unchanged.
func Resolve(value any, scope Scope) (any, error) {
	switch v := value.(type) {
	case string:
		return resolveString(v, scope)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			resolved, err := Resolve(val, scope)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, val := range v {
			resolved, err := Resolve(val, scope)
			if err != nil {
				return nil, err
			}
			out[i] = resolved
		}
		return out, nil
	default:
		return value, nil
	}
}

// ResolveMap is a Resolve convenience wrapper for the common case (a step's
// whole Input map).
func ResolveMap(input map[string]any, scope Scope) (map[string]any, error) {
	resolved, err := Resolve(input, scope)
	if err != nil {
		return nil, err
	}
	return resolved.(map[string]any), nil
}

func resolveString(s string, scope Scope) (any, error) {
	if m := wholeTemplate.FindStringSubmatch(s); m != nil {
		return Eval(strings.TrimSpace(m[1]), scope)
	}
	if !anyTemplate.MatchString(s) {
		return s, nil
	}
	var evalErr error
	result := anyTemplate.ReplaceAllStringFunc(s, func(match string) string {
		if evalErr != nil {
			return match
		}
		inner := wholeTemplate.FindStringSubmatch(match)[1]
		val, err := Eval(strings.TrimSpace(inner), scope)
		if err != nil {
			evalErr = err
			return match
		}
		return stringify(val)
	})
	if evalErr != nil {
		return nil, evalErr
	}
	return result, nil
}

func stringify(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
