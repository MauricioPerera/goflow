// Package exportjs renders a SUBSET of goflow flows as a single,
// self-contained JavaScript module that runs with no goflow, no goja, and
// no Go runtime at all — just a modern JS environment (Node.js, a browser,
// or a platform like a Cloudflare Worker or AWS Lambda's Node runtime).
// Built for the case this project's engine can't help with directly: a
// flow that needs to run somewhere goflow itself can't be deployed to.
//
// Deliberately narrow, not a general flow-to-JS compiler: only an EMPTY
// trigger followed by a linear chain of CODE actions is exportable (see
// Supported). A ROUTER, a LOOP_ON_ITEMS, a PIECE, or a CALL_FLOW action
// all need either engine control-flow this package doesn't reimplement,
// a specific piece's own logic (native Go code, or a JS-authored one
// whose source isn't even passed to Export), or a flowstore.Store to
// look another flow up in (CALL_FLOW) — none of it re-hostable in the
// target runtime without real added scope a "lightweight" exporter
// doesn't take on. Export rejects
// anything outside the supported subset outright, with every violation
// listed (mirroring flowvalidate.Validate's "report everything, not just
// the first problem" convention) — never a partial or silently-wrong
// export.
//
// What IS faithfully replicated for the supported subset, matching the
// real engine exactly rather than approximating it:
//   - pkg/expr's {{ }} template resolution: a {{ }} block's content is
//     evaluated as a REAL JS expression against named step-scope bindings
//     ({stepName: {output, input, status}}) — pkg/expr itself already
//     takes this approach (running the trimmed expression through goja
//     instead of a hand-rolled evaluator, see its own doc comment); the
//     generated JS just re-hosts the identical idea in the target runtime
//     via `new Function(...)` instead of goja, so the same {{ }} syntax
//     goflow already uses needs no translation at all.
//   - pkg/engine's exact per-step retry/backoff (RetryOnFailure,
//     MaxAttempts=4, ExponentialBase=2, IntervalMs=2000) and
//     ContinueOnFailure semantics, and the exact chain-walking/verdict
//     rules (a Skip'd action is passed over but its NextAction still
//     walked; a failed step without ContinueOnFailure stops the run).
//   - The output shape: {Steps, Verdict} with the same field names
//     (capitalized, matching model.ExecutionState's own untagged JSON
//     marshaling) a real goflow run already produces, so tooling built
//     against goflow's normal run output works against an export's output
//     unchanged.
//
// Known, disclosed differences from running the same flow through goflow
// itself — not oversights, documented in the generated file's own header
// comment too:
//   - No per-step execution timeout. goja.Interrupt (what bounds a
//     runaway CODE step inside goflow) has no equivalent for a
//     synchronous call in a normal JS engine; the target runtime's own
//     limits (e.g. a serverless platform's CPU-time cap), if any, are
//     what bound a runaway step in an export instead.
//   - Dynamic code evaluation (`new Function`) is required to run a
//     CODE step's source and evaluate {{ }} expressions, the same way
//     goflow's own goja-based engine does. Node.js and browsers allow
//     this unconditionally; a specific deployment target's policy should
//     be checked before relying on this (some CSP-restricted contexts
//     disallow it) — this package does not verify that for you.
//   - An async CODE step (one whose function returns a Promise) is
//     rejected, matching pkg/sandbox.Run's own "synchronous only" rule
//     exactly — a deliberate fidelity choice, not a technical limitation
//     of the export target, which could easily support real async.
//
// Everything here is encoding/json + fmt + strings — no new dependency,
// matching the rest of this project's stance. The generated JS itself has
// zero dependencies of its own either.
package exportjs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"goflow/pkg/model"
)

// UnsupportedError is one reason fv can't be exported — Path names the
// trigger or the offending action, Message explains why.
type UnsupportedError struct {
	Path    string
	Message string
}

func (e UnsupportedError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Message)
}

// Supported reports every reason fv falls outside this package's
// exportable subset — empty if fv can be exported. Checked by Export
// itself; exported separately so a caller (an HTTP handler, an MCP tool)
// can offer a clear "why not" without generating anything.
func Supported(fv *model.FlowVersion) []UnsupportedError {
	if fv == nil || fv.Trigger == nil {
		return []UnsupportedError{{Path: "trigger", Message: "flow has no trigger"}}
	}

	var errs []UnsupportedError
	if fv.Trigger.Type != model.TriggerEmpty {
		errs = append(errs, UnsupportedError{
			Path:    "trigger",
			Message: fmt.Sprintf("only an EMPTY trigger can be exported to JS (a PIECE_TRIGGER needs its piece's own Go/JS logic re-hosted in the target runtime); got %s", fv.Trigger.Type),
		})
	}

	for action := fv.Trigger.NextAction; action != nil; action = action.NextAction {
		if action.Type != model.ActionCode {
			errs = append(errs, UnsupportedError{
				Path:    action.Name,
				Message: fmt.Sprintf("only CODE actions can be exported to JS (a %s needs engine control-flow or a piece's own logic this package doesn't reimplement); got %s", action.Type, action.Type),
			})
		}
	}
	return errs
}

// Export renders fv as a single, self-contained JavaScript module. Returns
// an error wrapping every UnsupportedError Supported finds if fv falls
// outside the exportable subset — never a partial or silently-approximate
// export.
func Export(fv *model.FlowVersion) (string, error) {
	if errs := Supported(fv); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return "", fmt.Errorf("exportjs: flow is not exportable to JS: %s", strings.Join(msgs, "; "))
	}

	flowJSON, err := encodeFlowJSON(fv)
	if err != nil {
		return "", fmt.Errorf("exportjs: encoding flow: %w", err)
	}

	return fmt.Sprintf(jsTemplate, flowJSON), nil
}

// encodeFlowJSON is json.MarshalIndent with HTML-escaping turned off —
// encoding/json's default (SetEscapeHTML(true), on by MarshalIndent
// unconditionally) exists for safely embedding JSON inside an HTML
// <script> tag, which is not what's happening here: this JSON becomes JS
// SOURCE, not a string embedded in HTML, so escaping "<"/">"/"&" as
// "<"/">"/"&" only makes an action's own JS source (e.g. an
// arrow function's "=>") unreadable in the generated file for no benefit —
// the escaped and unescaped forms are equally valid inside a JS string
// literal either way, so this is a readability fix, not a correctness one.
func encodeFlowJSON(fv *model.FlowVersion) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(fv); err != nil {
		return nil, err
	}
	// Encode appends a trailing newline; trim it so the embedded `const
	// FLOW = <json>;` line ends cleanly instead of with a blank line
	// before the semicolon.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// jsTemplate is the generated module's full source, with %s standing in
// for the embedded flow definition (valid JSON is valid JS object/array
// syntax — embedded directly, no re-escaping into a string literal to
// parse at runtime). No `export`/`import` on purpose: a plain script
// defining top-level functions runs anywhere synchronously-loadable JS
// runs (a <script> tag, Node's `vm`/`require` machinery, a Worker's global
// scope, or this package's own goja-based tests) without assuming ES
// module support; wrap it in an export/module.exports yourself if your
// target needs one.
const jsTemplate = `// Generated by goflow's JS exporter (pkg/exportjs) from a single flow —
// self-contained, no goflow/goja/Go runtime needed to run it. Executes the
// SAME subset pkg/exportjs supports: an EMPTY trigger + a linear chain of
// CODE steps, replicating pkg/engine's exact execution order, retry/
// backoff, and ContinueOnFailure semantics, and pkg/expr's exact {{ }}
// template resolution (a {{ }} block's content runs as a real JS
// expression against named step-scope bindings — the same approach
// pkg/expr itself takes, just re-hosted here instead of in goja).
//
// KNOWN DIFFERENCES from running this same flow through goflow itself:
//   - No 5-second per-step execution timeout — goja.Interrupt has no
//     equivalent for a synchronous call in a normal JS engine. Your
//     runtime's own limits (if any) bound a runaway step here instead.
//   - Uses new Function(...) to evaluate {{ }} expressions and CODE step
//     sources — dynamic code evaluation. Node.js and browsers allow this
//     unconditionally; verify your specific deployment target's policy
//     (some CSP-restricted environments disallow it) before relying on
//     this in production.
//   - An async CODE step (one whose function returns a Promise) is
//     rejected, matching goflow's own pkg/sandbox "synchronous only" rule
//     exactly — a deliberate fidelity choice, not a limitation of this
//     runtime.

const FLOW = %s;

const RETRY = { maxAttempts: 4, exponentialBase: 2, intervalMs: 2000 };

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// Mirrors pkg/expr's wholeTemplate/anyTemplate regexes exactly, including
// that "." does not match a newline in either Go's RE2 or JS by default —
// a {{ }} block spanning multiple lines is deliberately NOT matched here
// either, same as the engine this was exported from.
const WHOLE_TEMPLATE = /^\{\{(.*)\}\}$/;
const ANY_TEMPLATE = /\{\{(.*?)\}\}/g;

function evalExpr(expr, scope) {
  const names = Object.keys(scope);
  const values = names.map((n) => scope[n]);
  const fn = new Function(...names, ` + "`return (${expr});`" + `);
  return fn(...values);
}

function stringifyForTemplate(v) {
  if (v === null || v === undefined) return "";
  if (typeof v === "string") return v;
  return String(v);
}

function resolveString(s, scope) {
  const whole = WHOLE_TEMPLATE.exec(s);
  if (whole) return evalExpr(whole[1].trim(), scope);
  if (!ANY_TEMPLATE.test(s)) return s;
  ANY_TEMPLATE.lastIndex = 0;
  return s.replace(ANY_TEMPLATE, (_match, inner) => stringifyForTemplate(evalExpr(inner.trim(), scope)));
}

function resolveValue(value, scope) {
  if (typeof value === "string") return resolveString(value, scope);
  if (Array.isArray(value)) return value.map((v) => resolveValue(v, scope));
  if (value !== null && typeof value === "object") {
    const out = {};
    for (const k of Object.keys(value)) out[k] = resolveValue(value[k], scope);
    return out;
  }
  return value;
}

function buildScope(steps) {
  const scope = {};
  for (const name of Object.keys(steps)) {
    const step = steps[name];
    scope[name] = { output: step.Output, input: step.Input, status: step.Status };
  }
  return scope;
}

function errorMessage(err) {
  if (err && err.message !== undefined) return String(err.message);
  return String(err);
}

// stepOutput builds a step record with the FULL field set
// model.StepOutput always has, even though Iterations/LastItem/LastIndex
// are only ever meaningful for a LOOP_ON_ITEMS step (never a CODE or EMPTY
// one, the only two kinds this exporter produces) — Go's own json.Marshal
// includes them regardless (no omitempty tag on those three fields), so a
// consumer expecting the exact same shape a real goflow run already
// produces gets it here too, not a step object missing keys it would
// otherwise always see.
function stepOutput(type, status, input, output, errorMsg, start) {
  return {
    Type: type,
    Status: status,
    Input: input,
    Output: output === undefined ? null : output,
    ErrorMessage: errorMsg,
    DurationMs: Date.now() - start,
    Iterations: null,
    LastItem: null,
    LastIndex: 0,
  };
}

async function runCodeStepOnce(action, steps) {
  const start = Date.now();
  const rawInput = (action.code && action.code.input) || {};
  let resolvedInput;
  try {
    resolvedInput = resolveValue(rawInput, buildScope(steps));
  } catch (err) {
    return stepOutput("CODE", "FAILED", rawInput, null, errorMessage(err), start);
  }

  let fn;
  try {
    fn = new Function(` + "`return (${action.code.source});`" + `)();
    if (typeof fn !== "function") {
      throw new Error(` + "`code must evaluate to a function, got ${typeof fn}`" + `);
    }
  } catch (err) {
    return stepOutput("CODE", "FAILED", resolvedInput, null, errorMessage(err), start);
  }

  try {
    const output = fn(resolvedInput);
    if (output && typeof output.then === "function") {
      throw new Error("code returned a Promise — async/await is not supported, return a value synchronously");
    }
    return stepOutput("CODE", "SUCCEEDED", resolvedInput, output, "", start);
  } catch (err) {
    return stepOutput("CODE", "FAILED", resolvedInput, null, errorMessage(err), start);
  }
}

async function runCodeStepWithRetry(action, steps) {
  const retryEnabled = !!(action.error && action.error.retryOnFailure);
  let attempt = 1;
  for (;;) {
    const out = await runCodeStepOnce(action, steps);
    if (out.Status !== "FAILED") return out;
    if (!retryEnabled || attempt >= RETRY.maxAttempts) return out;
    const backoffMs = Math.pow(RETRY.exponentialBase, attempt) * RETRY.intervalMs;
    await sleep(backoffMs);
    attempt++;
  }
}

// runFlow mirrors pkg/engine's ExecuteBegin + executeChain + recordStep +
// finalizeVerdict, for this exporter's supported subset only: triggerPayload
// becomes the trigger step's output verbatim (an EMPTY trigger never
// invokes anything), each action runs in order with retry/ContinueOnFailure
// applied exactly like the original engine, and the run stops the moment a
// step fails without ContinueOnFailure set — matching executeChain's own
// early return the instant the run leaves RUNNING.
async function runFlow(triggerPayload) {
  const steps = {};
  // DurationMs: 0 always, not measured — matching ExecuteBegin, which
  // constructs the trigger's StepOutput directly with no timing at all
  // (an EMPTY trigger never actually invokes anything to time).
  const triggerOutput = stepOutput("EMPTY", "SUCCEEDED", null, triggerPayload, "", Date.now());
  triggerOutput.DurationMs = 0;
  steps[FLOW.trigger.name] = triggerOutput;

  let verdict = { Status: "RUNNING", FailedStep: null };
  let action = FLOW.trigger.nextAction || null;

  while (action) {
    if (action.skip) {
      action = action.nextAction || null;
      continue;
    }
    const out = await runCodeStepWithRetry(action, steps);
    steps[action.name] = out;
    if (out.Status === "FAILED") {
      const continueOnFailure = !!(action.error && action.error.continueOnFailure);
      if (!continueOnFailure) {
        verdict = {
          Status: "FAILED",
          FailedStep: { Name: action.name, DisplayName: action.displayName, Message: out.ErrorMessage },
        };
        break;
      }
    }
    action = action.nextAction || null;
  }

  if (verdict.Status === "RUNNING") verdict = { Status: "SUCCEEDED", FailedStep: null };
  return { Steps: steps, Verdict: verdict };
}

if (typeof module !== "undefined" && module.exports) {
  module.exports = { runFlow: runFlow, flow: FLOW };
}
`
