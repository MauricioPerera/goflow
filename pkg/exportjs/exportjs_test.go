package exportjs_test

import (
	"strings"
	"testing"

	"github.com/dop251/goja"

	"goflow/pkg/exportjs"
	"goflow/pkg/model"
)

func emptyTriggerFlow(chain *model.FlowAction) *model.FlowVersion {
	return &model.FlowVersion{
		ID: "fv-test",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: chain,
		},
	}
}

func codeAction(name string, next *model.FlowAction) *model.FlowAction {
	return &model.FlowAction{
		Name: name, DisplayName: name, Type: model.ActionCode,
		Code:       &model.CodeSettings{Source: `(params) => params`},
		NextAction: next,
	}
}

// --- Supported --------------------------------------------------------------

func TestSupported_NilTrigger(t *testing.T) {
	errs := exportjs.Supported(&model.FlowVersion{ID: "fv"})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "no trigger") {
		t.Fatalf("errs = %v, want exactly one \"no trigger\" violation", errs)
	}
}

func TestSupported_RejectsNonEmptyTrigger(t *testing.T) {
	fv := &model.FlowVersion{
		ID: "fv",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", Type: model.TriggerPiece,
			PieceName: "webhook", TriggerName: "catch_hook",
		},
	}
	errs := exportjs.Supported(fv)
	if len(errs) != 1 || errs[0].Path != "trigger" || !strings.Contains(errs[0].Message, "EMPTY trigger") {
		t.Fatalf("errs = %v, want exactly one trigger violation mentioning EMPTY", errs)
	}
}

func TestSupported_RejectsRouterAction(t *testing.T) {
	router := &model.FlowAction{Name: "route", Type: model.ActionRouter, Router: &model.RouterSettings{}}
	fv := emptyTriggerFlow(router)
	errs := exportjs.Supported(fv)
	if len(errs) != 1 || errs[0].Path != "route" || !strings.Contains(errs[0].Message, "ROUTER") {
		t.Fatalf("errs = %v, want exactly one violation naming the ROUTER action", errs)
	}
}

func TestSupported_RejectsLoopAction(t *testing.T) {
	loop := &model.FlowAction{Name: "loop", Type: model.ActionLoopOnItems, Loop: &model.LoopSettings{}}
	fv := emptyTriggerFlow(loop)
	errs := exportjs.Supported(fv)
	if len(errs) != 1 || errs[0].Path != "loop" || !strings.Contains(errs[0].Message, "LOOP_ON_ITEMS") {
		t.Fatalf("errs = %v, want exactly one violation naming the LOOP_ON_ITEMS action", errs)
	}
}

func TestSupported_RejectsPieceAction(t *testing.T) {
	pieceAction := &model.FlowAction{
		Name: "call", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "http", ActionName: "request"},
	}
	fv := emptyTriggerFlow(pieceAction)
	errs := exportjs.Supported(fv)
	if len(errs) != 1 || errs[0].Path != "call" || !strings.Contains(errs[0].Message, "PIECE") {
		t.Fatalf("errs = %v, want exactly one violation naming the PIECE action", errs)
	}
}

func TestSupported_ReportsEveryViolationNotJustFirst(t *testing.T) {
	router := &model.FlowAction{Name: "route", Type: model.ActionRouter, Router: &model.RouterSettings{}}
	loop := &model.FlowAction{Name: "loop", Type: model.ActionLoopOnItems, Loop: &model.LoopSettings{}, NextAction: router}
	fv := &model.FlowVersion{
		ID: "fv",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", Type: model.TriggerPiece, PieceName: "webhook", TriggerName: "catch_hook",
			NextAction: loop,
		},
	}
	errs := exportjs.Supported(fv)
	if len(errs) != 3 {
		t.Fatalf("errs = %v, want 3 violations (trigger, loop, router) — Supported must report every one, not stop at the first", errs)
	}
}

func TestSupported_AcceptsLinearCodeChain(t *testing.T) {
	chain := codeAction("first", codeAction("second", nil))
	fv := emptyTriggerFlow(chain)
	if errs := exportjs.Supported(fv); len(errs) != 0 {
		t.Fatalf("errs = %v, want none for a valid EMPTY-trigger/CODE-only chain", errs)
	}
}

// --- Export -------------------------------------------------------------

func TestExport_RejectsUnsupportedFlow_ErrorMentionsViolation(t *testing.T) {
	router := &model.FlowAction{Name: "route", Type: model.ActionRouter, Router: &model.RouterSettings{}}
	fv := emptyTriggerFlow(router)
	_, err := exportjs.Export(fv)
	if err == nil {
		t.Fatal("Export err = nil, want a rejection for a ROUTER action")
	}
	if !strings.Contains(err.Error(), "route") || !strings.Contains(err.Error(), "ROUTER") {
		t.Fatalf("err = %v, want it to name the offending action and its type", err)
	}
}

func TestExport_ProducesSyntacticallyValidJS(t *testing.T) {
	fv := emptyTriggerFlow(codeAction("double", nil))
	js, err := exportjs.Export(fv)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, err := goja.Compile("export_test.js", js, false); err != nil {
		t.Fatalf("generated JS does not compile as valid JavaScript: %v\n---\n%s", err, js)
	}
}

func TestExport_NoHTMLEscaping(t *testing.T) {
	// encoding/json's default HTML-escaping would turn "=>" into ">" —
	// harmless inside a JSON string but unreadable in what's meant to be
	// human-facing generated source. Export must have that off.
	action := codeAction("double", nil)
	action.Code.Source = `(params) => params`
	fv := emptyTriggerFlow(action)
	js, err := exportjs.Export(fv)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if strings.Contains(js, "\\u003e") {
		t.Fatalf("generated JS HTML-escapes \"=>\" as \\u003e — Export must disable json.Encoder's SetEscapeHTML:\n%s", js)
	}
	if !strings.Contains(js, "(params) => params") {
		t.Fatalf("generated JS is missing the literal, unescaped action source:\n%s", js)
	}
}

func TestExport_RejectsPromiseReturningSourceLikeSandboxDoes(t *testing.T) {
	// Not a Go-side test of runFlow (async, can't be driven synchronously
	// through bare goja — see the package doc comment on why this
	// package's own tests stop at the synchronous helpers and rely on a
	// real JS runtime, exercised manually, for the async path). This just
	// confirms the REJECTION CHECK ITSELF is present in the generated
	// source, matching pkg/sandbox.Run's exact wording.
	fv := emptyTriggerFlow(codeAction("double", nil))
	js, err := exportjs.Export(fv)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !strings.Contains(js, "async/await is not supported") {
		t.Fatalf("generated JS is missing the Promise-rejection check pkg/sandbox.Run also has:\n%s", js)
	}
}

// --- fidelity of the synchronous template-resolution core ------------------
//
// These run the ACTUAL generated JS through goja and call its own
// resolveValue/evalExpr functions directly — proving pkg/expr's {{ }}
// semantics (wholeTemplate returns the raw value; mixed text stringifies
// each block; a template spanning a newline is NOT matched, since "."
// doesn't match \n in Go's RE2 or JS by default either) survive the
// translation from Go/goja to a plain JS file, not just eyeballing the
// generated source. Deliberately stops at the SYNCHRONOUS pieces:
// runCodeStepOnce/runCodeStepWithRetry/runFlow are all `async function`s,
// and bare goja (no goja_nodejs/eventloop, which this project doesn't
// depend on — see pkg/sandbox's own doc comment on the exact same gap)
// doesn't drain microtasks after RunProgram returns, so a Promise from
// calling them here wouldn't resolve. The async path was verified for
// real instead: the generated output run under actual Node.js against a
// multi-step flow (templating chained across two prior steps, retry,
// ContinueOnFailure) produced output identical to the same flow run
// through pkg/engine, field-for-field except DurationMs.

func mustExport(t *testing.T, fv *model.FlowVersion) *goja.Runtime {
	t.Helper()
	js, err := exportjs.Export(fv)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	vm := goja.New()
	if _, err := vm.RunString(js); err != nil {
		t.Fatalf("running generated JS in goja: %v\n---\n%s", err, js)
	}
	return vm
}

func callJS(t *testing.T, vm *goja.Runtime, name string, args ...any) any {
	t.Helper()
	fnValue := vm.Get(name)
	fn, ok := goja.AssertFunction(fnValue)
	if !ok {
		t.Fatalf("generated JS has no callable %q", name)
	}
	gojaArgs := make([]goja.Value, len(args))
	for i, a := range args {
		gojaArgs[i] = vm.ToValue(a)
	}
	result, err := fn(goja.Undefined(), gojaArgs...)
	if err != nil {
		t.Fatalf("calling %s(%v): %v", name, args, err)
	}
	return result.Export()
}

func TestGeneratedJS_ResolveValue_WholeTemplateReturnsRawTypedValue(t *testing.T) {
	vm := mustExport(t, emptyTriggerFlow(codeAction("noop", nil)))
	scope := map[string]any{"trigger_1": map[string]any{"output": map[string]any{"n": int64(21)}}}
	got := callJS(t, vm, "resolveValue", "{{ trigger_1.output.n }}", scope)
	n, ok := got.(int64)
	if !ok || n != 21 {
		t.Fatalf("resolveValue(whole template) = %#v (%T), want the raw number 21 — a whole-string {{ }} must return the typed value, not its string form", got, got)
	}
}

func TestGeneratedJS_ResolveValue_MixedTextInterpolatesAndStringifies(t *testing.T) {
	vm := mustExport(t, emptyTriggerFlow(codeAction("noop", nil)))
	scope := map[string]any{
		"trigger_1": map[string]any{"output": map[string]any{"n": int64(21)}},
		"double":    map[string]any{"output": map[string]any{"doubled": int64(42)}},
	}
	got := callJS(t, vm, "resolveValue", "n was {{ trigger_1.output.n }}, doubled is {{ double.output.doubled }}", scope)
	want := "n was 21, doubled is 42"
	if got != want {
		t.Fatalf("resolveValue(mixed text) = %q, want %q", got, want)
	}
}

func TestGeneratedJS_ResolveValue_PlainStringPassesThroughUnchanged(t *testing.T) {
	vm := mustExport(t, emptyTriggerFlow(codeAction("noop", nil)))
	got := callJS(t, vm, "resolveValue", "no templates here", map[string]any{})
	if got != "no templates here" {
		t.Fatalf("resolveValue(plain string) = %q, want it unchanged", got)
	}
}

func TestGeneratedJS_ResolveValue_NewlineSpanningTemplateNotMatched(t *testing.T) {
	// "." does not match \n in Go's RE2 (pkg/expr's wholeTemplate/
	// anyTemplate) or in JS by default — a template whose {{ }} content
	// spans a real newline must be left as literal text in BOTH, not
	// silently matched in one and not the other.
	vm := mustExport(t, emptyTriggerFlow(codeAction("noop", nil)))
	s := "{{ a\nb }}"
	got := callJS(t, vm, "resolveValue", s, map[string]any{})
	if got != s {
		t.Fatalf("resolveValue(newline-spanning template) = %q, want it left unchanged (unmatched), same as pkg/expr", got)
	}
}

func TestGeneratedJS_ResolveValue_NestedObjectAndArray(t *testing.T) {
	vm := mustExport(t, emptyTriggerFlow(codeAction("noop", nil)))
	scope := map[string]any{"trigger_1": map[string]any{"output": map[string]any{"n": int64(5)}}}
	value := map[string]any{
		"list": []any{"{{ trigger_1.output.n }}", "literal"},
		"nested": map[string]any{
			"x": "{{ trigger_1.output.n }}",
		},
	}
	got := callJS(t, vm, "resolveValue", value, scope)
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("resolveValue(nested) = %#v, want a map", got)
	}
	list, ok := m["list"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("resolveValue(nested).list = %#v, want a 2-element array", m["list"])
	}
	if n, ok := list[0].(int64); !ok || n != 5 {
		t.Fatalf("resolveValue(nested).list[0] = %#v, want the raw number 5", list[0])
	}
	if list[1] != "literal" {
		t.Fatalf("resolveValue(nested).list[1] = %#v, want unchanged \"literal\"", list[1])
	}
	nested, ok := m["nested"].(map[string]any)
	if !ok {
		t.Fatalf("resolveValue(nested).nested = %#v, want a map", m["nested"])
	}
	if n, ok := nested["x"].(int64); !ok || n != 5 {
		t.Fatalf("resolveValue(nested).nested.x = %#v, want the raw number 5", nested["x"])
	}
}

func TestGeneratedJS_EvalExpr_ArithmeticExpression(t *testing.T) {
	vm := mustExport(t, emptyTriggerFlow(codeAction("noop", nil)))
	got := callJS(t, vm, "evalExpr", "1 + 2", map[string]any{})
	if n, ok := got.(int64); !ok || n != 3 {
		t.Fatalf("evalExpr(\"1 + 2\") = %#v, want 3 — {{ }} content is a real JS expression, matching pkg/expr's own goja-based Eval", got)
	}
}
