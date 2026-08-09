// Package sandbox runs a CODE action's user-supplied JS source.
//
// Contract: Source must evaluate to a JS function `(params) => value`
// (arrow or plain function expression). Deliberately synchronous-only for
// v1 — goja supports the Promise constructor but draining microtasks
// requires an explicit event-loop pump (goja_nodejs/eventloop); until that's
// wired up, an async function's Promise return value would resolve to
// "[object Promise]" instead of being awaited, which is a worse trap than
// just rejecting it outright. See README "Known limitations".
package sandbox

import (
	"fmt"

	"github.com/dop251/goja"
)

// Run executes source with params bound as the sole argument to the
// function it must evaluate to, in a fresh goja.Runtime (one per call — no
// state, no shared globals leak between steps or between retry attempts,
// which is the property that matters for isolation here, distinct from
// activepieces' isolated-vm which additionally sandboxes CPU/memory via a
// real V8 isolate; goja has no such resource-limiting knobs).
func Run(source string, params map[string]any) (any, error) {
	vm := goja.New()

	fnValue, err := vm.RunString("(" + source + ")")
	if err != nil {
		return nil, fmt.Errorf("sandbox: parsing code: %w", err)
	}
	fn, ok := goja.AssertFunction(fnValue)
	if !ok {
		return nil, fmt.Errorf("sandbox: code must evaluate to a function, got %s", fnValue.ExportType())
	}

	result, err := fn(goja.Undefined(), vm.ToValue(params))
	if err != nil {
		if exc, ok := err.(*goja.Exception); ok {
			return nil, fmt.Errorf("%s", exc.Value().String())
		}
		return nil, err
	}

	exported := result.Export()
	if isPromiseLike(result) {
		return nil, fmt.Errorf("sandbox: code returned a Promise — async/await is not supported in v1, return a value synchronously")
	}
	return exported, nil
}

func isPromiseLike(v goja.Value) bool {
	obj, ok := v.(*goja.Object)
	if !ok {
		return false
	}
	return obj.ClassName() == "Promise"
}
