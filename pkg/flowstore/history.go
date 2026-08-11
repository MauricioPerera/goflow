package flowstore

import (
	"fmt"
	"log"
	"time"

	"goflow/pkg/credentials"
	"goflow/pkg/engine"
	"goflow/pkg/flowvalidate"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/runstore"
)

// maxCallFlowDepth bounds how deep a CALL_FLOW chain can nest — a backstop
// beyond the cycle detection in runWithHistoryAndCallPath (which catches
// any flow name repeating in the call path) for a long-but-non-cyclic
// chain that would otherwise recurse unbounded.
const maxCallFlowDepth = 10

// RunWithHistory wraps RunWithCredentials with the same "one shared path so
// every transport behaves identically" reasoning pkg/flowstore already
// applies to credential resolution: httpapi's runFlowVersion (backing both
// POST /flows/run and POST /flows/{name}/run), mcpapi's tools/call, and
// pkg/scheduler's tick loop all call this now, so a run recorded in history
// is recorded the same way regardless of which transport triggered it.
//
// flowName is the persisted flow's name for a named run, or "" for an ad-hoc
// POST /flows/run body — pure metadata alongside the record, never affects
// execution. historyStore may be nil (recording disabled), the same
// nil-means-off convention credStore already uses here. flowStore may also
// be nil — disabling CALL_FLOW support entirely (a CALL_FLOW action inside
// fv, if any, then fails clearly rather than panicking, per
// engine.CallFlowFunc's own doc comment); every production caller passes a
// real one, since all of them already hold a flowstore.Store for other
// reasons (looking up the flow being run, in the named case).
//
// A run is recorded whenever it actually produced a state — success or
// failure alike; a FAILED run belongs in history exactly as much as a
// SUCCEEDED one. It is NOT recorded when err != nil (a server-side fault,
// not a completed run to remember) or when validationErrs is non-empty (the
// flow never actually ran — there is no ExecutionState to record, the same
// reason a rejected malformed request elsewhere in this project doesn't get
// logged as one). A failure to WRITE the record is logged and otherwise
// swallowed — the flow itself already finished on its own terms by the time
// history recording runs; a history-store hiccup must not turn an
// already-completed run into an error for the caller.
//
// A CALL_FLOW action inside fv is what actually makes this function
// recursive — see runWithHistoryAndCallPath below for the sub-flow lookup,
// cycle detection, and per-sub-flow history recording that backs it.
func RunWithHistory(fv *model.FlowVersion, buildRegistry func() (*piece.Registry, error), credStore credentials.Store, historyStore runstore.Store, flowStore Store, flowName string, trigger any, executeTrigger bool) (*model.ExecutionState, []flowvalidate.ValidationError, error) {
	var callPath []string
	if flowName != "" {
		callPath = []string{flowName}
	}
	return runWithHistoryAndCallPath(fv, buildRegistry, credStore, historyStore, flowStore, flowName, trigger, executeTrigger, callPath, "")
}

// runWithHistoryAndCallPath is RunWithHistory's actual implementation,
// carrying callPath — the chain of flow names already running in THIS call
// stack (starting with the top-level named run, if any). Before recursing
// into a CALL_FLOW target, its name is checked against callPath: a name
// already there means flow A (directly, or through B, C, ...) is trying to
// call itself again — the cross-flow equivalent of the NextAction-cycle
// check flowvalidate.Validate already does for a single flow's OWN chain,
// resolved here at run time instead, because the cycle only exists once
// two-or-more flows' definitions are combined, which flowvalidate (given
// only one *model.FlowVersion at a time) can never see coming.
// maxCallFlowDepth backstops a long-but-non-cyclic chain.
//
// Each sub-flow call recorded in history separately, under its OWN name
// (def.Name) — a CALL_FLOW chain three deep produces three history
// records, not one, so every level of the chain is independently
// inspectable via GET /runs/{id}, the same as if each had been triggered
// directly.
//
// replayOfRunID is threaded through only by ReplayRun's own top-level
// call — every recursive CALL_FLOW sub-call always passes "" (a sub-flow
// triggered from inside a replay is a normal run, not itself a replay;
// only the record ReplayRun directly produces carries this).
func runWithHistoryAndCallPath(fv *model.FlowVersion, buildRegistry func() (*piece.Registry, error), credStore credentials.Store, historyStore runstore.Store, flowStore Store, flowName string, trigger any, executeTrigger bool, callPath []string, replayOfRunID string) (*model.ExecutionState, []flowvalidate.ValidationError, error) {
	var callFlow engine.CallFlowFunc
	if flowStore != nil {
		callFlow = func(subFlowName string, subTrigger any) (*model.ExecutionState, error) {
			if len(callPath) >= maxCallFlowDepth {
				return nil, fmt.Errorf("CALL_FLOW depth limit (%d) exceeded — call path: %v", maxCallFlowDepth, callPath)
			}
			for _, seen := range callPath {
				if seen == subFlowName {
					return nil, fmt.Errorf("CALL_FLOW cycle detected: %q already in call path %v", subFlowName, callPath)
				}
			}
			def, ok, err := flowStore.Get(subFlowName)
			if err != nil {
				return nil, fmt.Errorf("resolving CALL_FLOW target %q: %w", subFlowName, err)
			}
			if !ok {
				return nil, fmt.Errorf("no flow named %q", subFlowName)
			}
			nextPath := make([]string, len(callPath), len(callPath)+1)
			copy(nextPath, callPath)
			nextPath = append(nextPath, subFlowName)

			subState, subValidationErrs, err := runWithHistoryAndCallPath(&def.Flow, buildRegistry, credStore, historyStore, flowStore, def.Name, subTrigger, false, nextPath, "")
			if err != nil {
				return nil, err
			}
			if len(subValidationErrs) > 0 {
				return nil, fmt.Errorf("CALL_FLOW target %q failed validation: %s", subFlowName, FormatValidationErrors(subValidationErrs))
			}
			return subState, nil
		}
	}

	started := time.Now()
	state, validationErrs, err := RunWithCredentials(fv, buildRegistry, credStore, callFlow, trigger, executeTrigger)
	if err != nil || len(validationErrs) > 0 || state == nil || historyStore == nil {
		return state, validationErrs, err
	}

	if _, saveErr := historyStore.Save(runstore.Record{
		FlowName:       flowName,
		Trigger:        trigger,
		State:          state,
		StartedAt:      started,
		FinishedAt:     time.Now(),
		ExecuteTrigger: executeTrigger,
		ReplayOfRunID:  replayOfRunID,
	}); saveErr != nil {
		log.Printf("flowstore: recording run history: %v", saveErr)
	}

	return state, validationErrs, nil
}

// ReplayRun re-runs a past run's Trigger/ExecuteTrigger against
// runID's flow's CURRENT definition — not the definition it ran against
// originally, which is the whole point: proving whether an edit changed
// behavior against real historical traffic, the natural complement to
// FlowDefinition.Examples (hand-written cases) and flow versioning
// (undo an edit) already shipped in this package. No automatic diff
// against the original run — the caller already has both run ids (the
// one it started with, and the new one this returns inside State) and
// can compare them directly; building a structural diff here would be
// real added scope nothing has asked for yet.
//
// Three ways this can fail before ever reaching Run:
//   - runID doesn't resolve (runStore.Get ok=false), or resolving it errors.
//   - the run has no FlowName (an ad-hoc POST /flows/run or
//     goflow_run_flow run) — there is no "current definition" to replay
//     against without a name to look one up by.
//   - the flow was deleted since the original run — flowStore.Get
//     ok=false.
//
// Otherwise behaves exactly like a normal named run through
// RunWithHistory (credentials resolved, CALL_FLOW supported, recorded in
// history), except the saved record's ReplayOfRunID is set to runID, so
// it's traceable as a replay rather than looking like an organic run.
func ReplayRun(runStore runstore.Store, flowStore Store, buildRegistry func() (*piece.Registry, error), credStore credentials.Store, historyStore runstore.Store, runID string) (*model.ExecutionState, []flowvalidate.ValidationError, error) {
	rec, ok, err := runStore.Get(runID)
	if err != nil {
		return nil, nil, fmt.Errorf("flowstore: resolving run %q: %w", runID, err)
	}
	if !ok {
		return nil, nil, fmt.Errorf("flowstore: no run %q", runID)
	}
	if rec.FlowName == "" {
		return nil, nil, fmt.Errorf("flowstore: run %q is ad-hoc (no flow name) and can't be replayed", runID)
	}
	def, ok, err := flowStore.Get(rec.FlowName)
	if err != nil {
		return nil, nil, fmt.Errorf("flowstore: resolving flow %q: %w", rec.FlowName, err)
	}
	if !ok {
		return nil, nil, fmt.Errorf("flowstore: flow %q no longer exists", rec.FlowName)
	}
	return runWithHistoryAndCallPath(&def.Flow, buildRegistry, credStore, historyStore, flowStore, rec.FlowName, rec.Trigger, rec.ExecuteTrigger, []string{rec.FlowName}, runID)
}
