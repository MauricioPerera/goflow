// Package jspiece lets a piece.Action be authored as JS source instead of
// Go code — Phase 2 of the "AI-first" direction started by
// model.ParseFlowVersion (Phase 1: flows as JSON data): the point of this
// package is that an agent can produce a genuinely new, reusable catalog
// piece and use it immediately, with no Go recompile. New builds a real
// piece.Piece that goes through the exact same piece.Validate/
// RegisterValidated path as any hand-written Go piece — the engine cannot
// tell the difference.
//
// Scope, deliberately: ACTIONS only (no JS triggers yet — same pattern,
// not built), no Dropdowns for JS actions yet, and this package does not
// solve where the JS source text comes from or how it survives a process
// restart (no persistence, matching this whole project's "no
// persistence" boundary — see README's "Explicitly NOT in v1"). What's
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
	return piece.Action{
		Name: src.Name, DisplayName: src.DisplayName,
		Run: func(ctx piece.ActionContext) (any, error) {
			return runJS(src.Source, ctx)
		},
	}
}

func runJS(source string, ctx piece.ActionContext) (any, error) {
	vm := goja.New()

	fnValue, err := vm.RunString("(" + source + ")")
	if err != nil {
		return nil, fmt.Errorf("jspiece: parsing source: %w", err)
	}
	fn, ok := goja.AssertFunction(fnValue)
	if !ok {
		return nil, fmt.Errorf("jspiece: source must evaluate to a function, got %s", fnValue.ExportType())
	}

	jsCtx := buildContext(ctx)

	timer := time.AfterFunc(DefaultTimeout, func() {
		vm.Interrupt("jspiece: execution timed out")
	})
	defer timer.Stop()

	result, err := fn(goja.Undefined(), vm.ToValue(jsCtx))
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
		return nil, fmt.Errorf("jspiece: action returned a Promise — async/await is not supported, return a value synchronously")
	}
	return exported, nil
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
