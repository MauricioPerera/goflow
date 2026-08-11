package flowstore

import (
	"fmt"

	"goflow/pkg/engine"
	"goflow/pkg/flowvalidate"
	"goflow/pkg/model"
	"goflow/pkg/piece"
)

// Run is the shared "validate then execute" path for a *model.FlowVersion —
// the single place that assembles a fresh piece registry, re-validates the
// flow against it, and (if it passes) runs it through the engine. Both the
// HTTP layer (POST /flows/run and POST /flows/{name}/run, via
// httpapi.Server.runFlowVersion) and the MCP layer (tools/call, via
// mcpapi.Handler) call this, so the two transports never drift apart in what
// "run a flow" means.
//
// The three return values separate the three outcomes a caller distinguishes,
// none of which is collapsed into the others:
//   - err != nil: buildRegistry failed — a server-side fault, not a problem
//     with the caller's flow. HTTP maps this to 400 {"error":...}; MCP to a
//     JSON-RPC -32603 internal error.
//   - validationErrs non-empty: the flow is structurally broken (references a
//     piece that isn't in the registry, has a cycle, bad JS, ...). This is NOT
//     returned as err: a broken flow is a normal caller-supplied input, not a
//     server fault, and the two transports surface it differently — HTTP as
//     400 {"errors":[...]}, MCP as a tool result with isError:true (the tool
//     exists, it just can't run).
//   - state non-nil: the flow ran. The caller inspects state.Verdict.Status
//     (model.FlowRunSucceeded vs FAILED/...) to decide success.
//
// buildRegistry is called fresh here (never cached), matching
// httpapi.Server.buildRegistry and GatedStore.Save: a flow must validate and
// run against the piece registry AS IT IS now, including JS pieces added to
// the catalog after this process started.
//
// callFlow backs CALL_FLOW actions (see engine.CallFlowFunc) — nil disables
// them (a flow using one fails clearly instead of panicking). Run itself
// has no idea how a sub-flow actually gets looked up and run; it only
// wires whatever it's given onto the Engine — RunWithHistory is the one
// place in this package that actually BUILDS a real callFlow closure (flow
// lookup, cycle detection, recording), since it's the one with a Store,
// credStore, and historyStore all in scope together. Run/RunWithCredentials
// stay agnostic on purpose, matching how neither of them knows about
// credentials.Store either — the caller two levels up owns that.
func Run(fv *model.FlowVersion, buildRegistry func() (*piece.Registry, error), callFlow engine.CallFlowFunc, trigger any, executeTrigger bool) (state *model.ExecutionState, validationErrs []flowvalidate.ValidationError, err error) {
	registry, err := buildRegistry()
	if err != nil {
		return nil, nil, fmt.Errorf("flowstore: building registry: %w", err)
	}
	if errs := flowvalidate.Validate(fv, registry); len(errs) > 0 {
		return nil, errs, nil
	}
	eng := engine.New(registry)
	eng.CallFlow = callFlow
	return eng.ExecuteBegin(fv, engine.BeginInput{
		TriggerPayload: trigger,
		ExecuteTrigger: executeTrigger,
	}), nil, nil
}

// Resume is Run's counterpart for continuing a PAUSED run instead of
// starting a fresh one — same "assemble registry, validate, wire
// CallFlow" shape, but calls engine.ExecuteResume with priorState
// (typically a runstore.Record.State fetched by id — see ResumeRun)
// instead of engine.ExecuteBegin with a trigger payload. fv is
// re-validated against the CURRENT registry exactly like Run does: since
// fv is normally the flow's CURRENT saved definition (see ResumeRun),
// not necessarily the one that was running when it paused, this catches
// an edit that broke the flow outright — it does NOT catch an edit that
// changed behavior more subtly (renamed/reordered steps after the
// resume point); engine.ExecuteResume itself has no guard against that
// either. resumePayload is engine-opaque, handed straight to whichever
// piece's ctx.resumePayload is waiting on it (see pkg/pieces/approval
// for the one piece in this catalog that reads it). priorState is
// assumed to already be paused — Resume does not check this itself, the
// same way Run doesn't second-guess ExecuteBegin's own preconditions;
// see ResumeRun for the actual "was this run paused" check.
func Resume(fv *model.FlowVersion, buildRegistry func() (*piece.Registry, error), callFlow engine.CallFlowFunc, priorState *model.ExecutionState, resumePayload any) (state *model.ExecutionState, validationErrs []flowvalidate.ValidationError, err error) {
	registry, err := buildRegistry()
	if err != nil {
		return nil, nil, fmt.Errorf("flowstore: building registry: %w", err)
	}
	if errs := flowvalidate.Validate(fv, registry); len(errs) > 0 {
		return nil, errs, nil
	}
	eng := engine.New(registry)
	eng.CallFlow = callFlow
	return eng.ExecuteResume(fv, engine.ResumeInput{
		PriorState:    priorState,
		ResumePayload: resumePayload,
	}), nil, nil
}
