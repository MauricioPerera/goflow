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
	"time"

	"github.com/dop251/goja"
)

// DefaultTimeout bounds how long a single CODE step's INTERPRETED
// execution (loops, JS function calls) may run before being forcibly
// interrupted via InterruptAfter. Added late — pkg/expr and pkg/jspiece
// both got this same backstop first, on the reasoning that they run
// agent-generated code with no human review gate. That reasoning turns
// out to apply here too: Phase 1 (model.ParseFlowVersion) lets a flow's
// CODE action Source arrive as external/agent-authored JSON data exactly
// like a JS piece's or a template expression's source does, and
// pkg/flowvalidate only checks that Source *compiles* (via goja.Compile,
// which never executes anything) — nothing bounded its RUNTIME behavior
// until this. A CODE step with a "(params) => { while(true){} }" Source
// used to hang the executing goroutine forever, on this package's own
// tests never having caught it, same class of gap this project already
// found and fixed twice elsewhere.
//
// Same confirmed limitation as pkg/expr's and pkg/jspiece's identical
// DefaultTimeout (they share this exact goja.Interrupt mechanism): a
// native built-in call already in progress is not preempted — it runs to
// full completion, paying its entire CPU/memory cost, before the pending
// interrupt is even noticed.
var DefaultTimeout = 5 * time.Second

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

	stop := InterruptAfter(vm, DefaultTimeout, "sandbox: execution timed out")
	defer stop()

	result, err := fn(goja.Undefined(), vm.ToValue(params))
	if err != nil {
		if exc, ok := err.(*goja.Exception); ok {
			return nil, fmt.Errorf("%s", exc.Value().String())
		}
		if interrupted, ok := err.(*goja.InterruptedError); ok {
			return nil, fmt.Errorf("%v", interrupted)
		}
		return nil, err
	}

	exported := result.Export()
	if _, ok := exported.(*goja.Promise); ok {
		return nil, fmt.Errorf("sandbox: code returned a Promise — async/await is not supported in v1, return a value synchronously")
	}
	return exported, nil
}
