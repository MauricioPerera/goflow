// Package jspiece lets a piece.Action or piece.Trigger be authored as JS
// source instead of Go code — Phase 2 of the "AI-first" direction started
// by model.ParseFlowVersion (Phase 1: flows as JSON data): the point of
// this package is that an agent can produce a genuinely new, reusable
// catalog piece and use it immediately, with no Go recompile. New/NewAction
// build a real piece.Piece/piece.Action that goes through the exact same
// piece.Validate/RegisterValidated path as any hand-written Go piece — the
// engine cannot tell the difference. NewTrigger does the same for
// piece.Trigger, and NewDropdown for piece.DropdownProperty.
//
// Scope, deliberately: this package does not solve where the JS source
// text comes from or how it survives a process restart (no persistence —
// see pkg/catalog, which does, though only for actions so far;
// catalog.Definition has no trigger or Dropdown equivalent yet). What's
// proven here is narrower and load-bearing: given JS source text from
// anywhere, the engine can run it as a first-class piece.
package jspiece

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dop251/goja"

	"goflow/pkg/model"
	"goflow/pkg/piece"
)

// DefaultTimeout bounds how long a single JS action's INTERPRETED execution
// (loops, JS function calls) may run before being forcibly interrupted via
// goja.Runtime.Interrupt. pkg/sandbox's CODE step has no such limit — but
// that's code a human wrote and could review before it ships. A JS piece
// is explicitly meant for code an agent generates and registers at runtime
// with no human review gate; goja has no CPU/memory sandboxing of its own
// (see pkg/sandbox's doc comment), so an execution deadline is the one
// cheap backstop available against an infinite loop or a runaway piece.
//
// Confirmed limitation (see pkg/expr's identical DefaultTimeout and its
// TestEval_TimeoutDoesNotBoundNativeBuiltInWallClockTime, since this
// package shares the exact same goja.Interrupt mechanism): a native
// built-in call already in progress — a huge string/array allocation via
// String.prototype.repeat, new Array(n), etc. — is NOT preempted. It runs
// to full completion, paying its entire CPU/memory cost, before the
// pending interrupt is even noticed; only the RESULT gets discarded
// afterward. This timeout bounds runaway loops/recursion, not a single
// expensive native call.
var DefaultTimeout = 5 * time.Second

// DefaultFetchTimeout bounds a single ctx.fetch(...) call made from JS
// piece code — same reasoning as pkg/pieces/http's DefaultTimeout.
var DefaultFetchTimeout = 10 * time.Second

// ActionSource is one JS-authored action.
type ActionSource struct {
	Name, DisplayName string

	// Source must evaluate to a JS function `(ctx) => value`, synchronous
	// only — same rule as pkg/sandbox's CODE steps and the same reason
	// (goja supports the Promise constructor, but draining microtasks
	// needs an explicit event-loop pump this project doesn't wire up).
	//
	// ctx exposes: ctx.input (the action's resolved Input map), ctx.auth
	// (Input[piece.AuthInputKey] — a *piece.OAuth2Auth becomes
	// {accessToken, data, props}, a []byte becomes a string, anything else
	// passes through as-is), ctx.executionType ("BEGIN"/"RESUME"),
	// ctx.resumePayload, ctx.files.write(fileName, content) -> fileURL,
	// ctx.run.stop(response)/respond(response)/waitForWaitpoint(id), and
	// ctx.fetch({url, method, headers, body}) -> {status, headers, body}
	// (a real, synchronous HTTP call — the one capability beyond pure
	// logic and the ActionContext hooks; see this package's doc comment).
	Source string

	// Dropdowns declares this action's dynamically-loaded properties,
	// keyed by property name — see DropdownSource.
	Dropdowns map[string]DropdownSource
}

// New builds a piece.Piece whose actions are all JS-backed.
func New(name, displayName string, actions []ActionSource) piece.Piece {
	acts := make(map[string]piece.Action, len(actions))
	for _, a := range actions {
		acts[a.Name] = NewAction(a)
	}
	return piece.Piece{Name: name, DisplayName: displayName, Actions: acts}
}

// NewAction builds a single JS-backed piece.Action.
func NewAction(src ActionSource) piece.Action {
	var dropdowns map[string]piece.DropdownProperty
	if len(src.Dropdowns) > 0 {
		dropdowns = make(map[string]piece.DropdownProperty, len(src.Dropdowns))
		for name, d := range src.Dropdowns {
			dropdowns[name] = NewDropdown(d)
		}
	}
	return piece.Action{
		Name: src.Name, DisplayName: src.DisplayName,
		Dropdowns: dropdowns,
		Run: func(ctx piece.ActionContext) (any, error) {
			return compileAndRun(src.Source, buildContext(ctx))
		},
	}
}

// DropdownSource is one JS-authored dynamically-loaded property —
// piece.DropdownProperty authored as JS instead of a Go closure.
type DropdownSource struct {
	// Refreshers names the sibling input properties this dropdown depends
	// on — purely documentation, same as piece.DropdownProperty.Refreshers;
	// nothing enforces it, LoadOptions decides what it actually reads.
	Refreshers []string

	// Source must evaluate to a JS function `(propsValue, ctx) => value`,
	// synchronous only, same rule as ActionSource.Source. propsValue is
	// the sibling input values already resolved (matching Go's own
	// LoadOptions signature); ctx exposes ctx.searchValue
	// (piece.PropertyContext.SearchValue). The returned value must be an
	// object matching piece.DropdownState's shape:
	// {disabled?, placeholder?, options: [{label, value}, ...]}.
	Source string
}

// NewDropdown builds a single JS-backed piece.DropdownProperty.
func NewDropdown(src DropdownSource) piece.DropdownProperty {
	return piece.DropdownProperty{
		Refreshers: src.Refreshers,
		LoadOptions: func(propsValue map[string]any, ctx piece.PropertyContext) (piece.DropdownState, error) {
			exported, err := compileAndRun(src.Source, propsValue, map[string]any{"searchValue": ctx.SearchValue})
			if err != nil {
				return piece.DropdownState{}, err
			}
			return toDropdownState(exported)
		},
	}
}

// toDropdownState converts a JS dropdown function's exported return value
// into a piece.DropdownState, rejecting anything that doesn't match the
// expected shape with a clear error rather than silently defaulting.
func toDropdownState(exported any) (piece.DropdownState, error) {
	m, ok := exported.(map[string]any)
	if !ok {
		return piece.DropdownState{}, fmt.Errorf("jspiece: dropdown must return an object, got %T", exported)
	}
	var state piece.DropdownState
	if v, ok := m["disabled"].(bool); ok {
		state.Disabled = v
	}
	if v, ok := m["placeholder"].(string); ok {
		state.Placeholder = v
	}
	if raw, ok := m["options"].([]any); ok {
		state.Options = make([]piece.DropdownOption, 0, len(raw))
		for i, o := range raw {
			om, ok := o.(map[string]any)
			if !ok {
				return piece.DropdownState{}, fmt.Errorf("jspiece: dropdown option[%d] must be an object, got %T", i, o)
			}
			label, _ := om["label"].(string)
			state.Options = append(state.Options, piece.DropdownOption{Label: label, Value: om["value"]})
		}
	}
	return state, nil
}

// TriggerSource is one JS-authored trigger.
type TriggerSource struct {
	Name, DisplayName string

	// Source must evaluate to a JS function `(ctx) => value` returning an
	// ARRAY of items — piece.Trigger.Run's []any, exactly as a Go trigger
	// would return it (e.g. pkg/pieces/webhook's catch_hook returns a
	// single-element array). Synchronous only, same rule and reason as
	// ActionSource.Source.
	//
	// ctx exposes: ctx.payload (the raw trigger payload), ctx.input (the
	// trigger's resolved Input map), and ctx.store.get(key)/put(key,
	// value) bound to the real piece.Store the caller supplied — nil
	// unless one was, same as piece.TriggerContext.Store itself; a
	// polling trigger uses this to remember a cursor across separate
	// invocations, the same job pkg/engine's own hand-rolled polling
	// trigger tests use a Store for.
	Source string
}

// NewTrigger builds a single JS-backed piece.Trigger.
func NewTrigger(src TriggerSource) piece.Trigger {
	return piece.Trigger{
		Name: src.Name, DisplayName: src.DisplayName,
		Run: func(ctx piece.TriggerContext) ([]any, error) {
			exported, err := compileAndRun(src.Source, buildTriggerContext(ctx))
			if err != nil {
				return nil, err
			}
			items, ok := exported.([]any)
			if !ok {
				return nil, fmt.Errorf("jspiece: trigger must return an array, got %T", exported)
			}
			return items, nil
		},
	}
}

// compileAndRun is the shared core for NewAction, NewTrigger, and
// NewDropdown: parse source as `(source)`, confirm it's a function, invoke
// it with args under DefaultTimeout, and return its exported Go value.
// Callers interpret the returned value differently (an action's is passed
// through as-is; a trigger's must be a []any; a dropdown's must be an
// object matching piece.DropdownState's shape).
func compileAndRun(source string, args ...any) (any, error) {
	vm := goja.New()

	fnValue, err := vm.RunString("(" + source + ")")
	if err != nil {
		return nil, fmt.Errorf("jspiece: parsing source: %w", err)
	}
	fn, ok := goja.AssertFunction(fnValue)
	if !ok {
		return nil, fmt.Errorf("jspiece: source must evaluate to a function, got %s", fnValue.ExportType())
	}

	timer := time.AfterFunc(DefaultTimeout, func() {
		vm.Interrupt("jspiece: execution timed out")
	})
	defer timer.Stop()

	argValues := make([]goja.Value, len(args))
	for i, a := range args {
		argValues[i] = vm.ToValue(a)
	}
	result, err := fn(goja.Undefined(), argValues...)
	if err != nil {
		if exc, ok := err.(*goja.Exception); ok {
			return nil, fmt.Errorf("%s", exc.Value().String())
		}
		if interrupted, ok := err.(*goja.InterruptedError); ok {
			return nil, fmt.Errorf("jspiece: %v", interrupted)
		}
		return nil, err
	}

	exported := result.Export()
	if _, ok := exported.(*goja.Promise); ok {
		return nil, fmt.Errorf("jspiece: returned a Promise — async/await is not supported, return a value synchronously")
	}
	return exported, nil
}

// buildTriggerContext assembles the JS-facing ctx object for one trigger
// Run call — see TriggerSource.Source's doc comment for what it exposes.
func buildTriggerContext(ctx piece.TriggerContext) map[string]any {
	return map[string]any{
		"payload": ctx.Payload,
		"input":   ctx.Input,
		"store": map[string]any{
			"get": func(key string) (any, error) {
				if ctx.Store == nil {
					return nil, fmt.Errorf("ctx.store is not available in this execution context")
				}
				v, _ := ctx.Store.Get(key)
				return v, nil
			},
			"put": func(key string, value any) error {
				if ctx.Store == nil {
					return fmt.Errorf("ctx.store is not available in this execution context")
				}
				ctx.Store.Put(key, value)
				return nil
			},
		},
	}
}

// buildContext assembles the JS-facing ctx object for one Run call. Bound
// functions close over the real Go ActionContext, so ctx.run.stop(...) in
// JS ends up calling the exact same piece.RunHooks.Stop the engine wired
// up for this step — no different from a Go-authored piece calling it
// directly.
func buildContext(ctx piece.ActionContext) map[string]any {
	return map[string]any{
		"input":         ctx.Input,
		"auth":          jsAuthValue(ctx.Auth),
		"executionType": string(ctx.ExecutionType),
		"resumePayload": ctx.ResumePayload,
		"files": map[string]any{
			"write": func(fileName, content string) (string, error) {
				if ctx.Files == nil {
					return "", fmt.Errorf("ctx.files is not available in this execution context")
				}
				return ctx.Files.Write(fileName, []byte(content))
			},
		},
		// Each hook is guarded against being nil before calling it — a real
		// Go nil-pointer panic from inside a goja-called native function
		// propagates straight out of act.Run() uncaught (confirmed directly:
		// calling ctx.run.stop with a zero-value piece.RunHooks panics, not
		// errors). The engine itself always wires all three hooks, so this
		// never fires in a normal flow run — it protects any OTHER caller of
		// a JS-backed piece.Action that doesn't (a bare unit test, a future
		// "run this piece standalone" tool). Returning a non-nil error as
		// the last return value makes goja throw a normal, catchable JS
		// exception instead — see ToValue's doc comment on multi-return Go
		// functions.
		"run": map[string]any{
			"stop": func(resp map[string]any) error {
				if ctx.Run.Stop == nil {
					return fmt.Errorf("ctx.run.stop is not available in this execution context")
				}
				ctx.Run.Stop(toWebhookResponse(resp))
				return nil
			},
			"respond": func(resp map[string]any) error {
				if ctx.Run.Respond == nil {
					return fmt.Errorf("ctx.run.respond is not available in this execution context")
				}
				ctx.Run.Respond(toWebhookResponse(resp))
				return nil
			},
			"waitForWaitpoint": func(id string) error {
				if ctx.Run.WaitForWaitpoint == nil {
					return fmt.Errorf("ctx.run.waitForWaitpoint is not available in this execution context")
				}
				ctx.Run.WaitForWaitpoint(id)
				return nil
			},
		},
		"fetch": fetchJS,
	}
}

func jsAuthValue(auth any) any {
	switch v := auth.(type) {
	case *piece.OAuth2Auth:
		if v == nil {
			return nil
		}
		return map[string]any{"accessToken": v.AccessToken, "data": v.Data, "props": v.Props}
	case []byte:
		return string(v)
	default:
		return v
	}
}

func toWebhookResponse(resp map[string]any) *model.WebhookResponse {
	status := 200
	if n, ok := toInt(resp["status"]); ok {
		status = n
	}
	headers := map[string]string{}
	if raw, ok := resp["headers"].(map[string]any); ok {
		for k, v := range raw {
			if s, ok := v.(string); ok {
				headers[k] = s
			}
		}
	}
	return &model.WebhookResponse{Status: status, Body: resp["body"], Headers: headers}
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

// fetchJS backs ctx.fetch(options) — a real, synchronous HTTP call, the one
// capability a JS piece gets beyond pure logic and the ActionContext hooks.
// Deliberate risk, not an oversight: this lets AI-generated, unreviewed
// code make outbound network calls, including to internal/private
// addresses (no SSRF protection here) — accepted so JS pieces can be
// genuinely useful (calling real APIs) rather than pure-logic-only. Revisit
// if JS pieces end up running against untrusted input at the edge of a
// real deployment.
func fetchJS(options map[string]any) (map[string]any, error) {
	url, _ := options["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("fetch: missing required option: url (string)")
	}
	method, _ := options["method"].(string)
	if method == "" {
		method = http.MethodGet
	}
	method = strings.ToUpper(method)

	var bodyReader io.Reader
	if body, ok := options["body"].(string); ok && body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("fetch: building request: %w", err)
	}
	if headers, ok := options["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
	}

	client := &http.Client{Timeout: DefaultFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetch: reading response body: %w", err)
	}
	respHeaders := map[string]any{}
	for k, v := range resp.Header {
		if len(v) > 0 {
			respHeaders[k] = v[0]
		}
	}
	return map[string]any{
		"status":  resp.StatusCode,
		"headers": respHeaders,
		"body":    string(respBody),
	}, nil
}
