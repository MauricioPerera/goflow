package flowstore

import (
	"log"

	"goflow/pkg/credentials"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/runstore"
)

// pausedStepName returns the name of state's one PAUSED step (the map key
// in state.Steps), or "" if none is paused. The engine guarantees at most
// one live pause point per run (see engine.ExecuteResume's own reasoning
// — a container's IsCompleted check stops the moment any step goes
// non-Running), so there is never an ambiguity to resolve here. For a
// pause nested inside a LOOP_ON_ITEMS/ROUTER, this returns the top-level
// CONTAINER step's name (the one whose own Status the engine propagates
// to PAUSED), not a path into its Iterations — good enough for a
// human-readable notification ("your flow is paused at step X"), not an
// attempt at a full nested-step address.
func pausedStepName(state *model.ExecutionState) string {
	if state == nil {
		return ""
	}
	for name, step := range state.Steps {
		if step.Status == model.StepPaused {
			return name
		}
	}
	return ""
}

// triggerOnPause runs flowName's onPauseFlow (if configured) whenever
// state's Verdict ended up PAUSED — closes the gap left after
// engine.ExecuteResume shipped with no transport ever able to call it: a
// webhook- or scheduler-fired run that pauses (e.g. the "approval" piece)
// has no human watching it, so without this the only way to learn it's
// waiting is polling GET /runs. A no-op whenever state is nil, the run
// isn't PAUSED, onPauseFlow is "" (not configured), or historyStore is
// nil — the common case for every flow, unaffected by this ever
// existing.
//
// runID is the id historyStore.Save already assigned the paused record
// (the caller fetches it back via historyStore.Get to read the
// ResumeToken Save generated — see runstore.Record.ResumeToken's own doc
// comment for why that round-trip, rather than threading the token
// through some other path). The on-pause flow's trigger payload carries
// flowName, runId, resumeToken, and pausedStepName — enough to build a
// one-click approval link (POST /public/runs/{runId}/resume with header
// X-Resume-Secret: {resumeToken}) and notify someone with it, the whole
// reason this exists.
//
// Same call-path shape as TriggerOnFailure — run through RunWithHistory,
// recorded in history under its own name, credentials resolved the same
// way — and same safety properties: deliberately synchronous (blocks the
// caller; see TriggerOnFailure's own doc comment for why), best-effort
// and one-way (any error resolving or running the on-pause flow is
// logged and swallowed — a broken notification must never change the
// ORIGINAL paused run's own outcome), and capped at exactly one hop (the
// on-pause flow's own OnPauseFlow, if it has one and also pauses, is
// never read here).
func triggerOnPause(store Store, flowName, onPauseFlow string, state *model.ExecutionState, runID string, historyStore runstore.Store, buildRegistry func() (*piece.Registry, error), credStore credentials.Store) {
	if state == nil || state.Verdict.Status != model.FlowRunPaused || onPauseFlow == "" || historyStore == nil {
		return
	}

	rec, ok, err := historyStore.Get(runID)
	if err != nil || !ok {
		log.Printf("flowstore: triggerOnPause: resolving saved run %q for %q: ok=%v err=%v", runID, flowName, ok, err)
		return
	}

	def, ok, err := store.Get(onPauseFlow)
	if err != nil || !ok {
		log.Printf("flowstore: triggerOnPause: resolving on-pause flow %q for %q: ok=%v err=%v", onPauseFlow, flowName, ok, err)
		return
	}

	payload := map[string]any{
		"flowName":       flowName,
		"runId":          runID,
		"resumeToken":    rec.ResumeToken,
		"pausedStepName": pausedStepName(state),
	}

	if _, _, runErr := RunWithHistory(&def.Flow, buildRegistry, credStore, historyStore, store, def.Name, payload, false, ""); runErr != nil {
		log.Printf("flowstore: triggerOnPause: running on-pause flow %q for %q: %v", onPauseFlow, flowName, runErr)
	}
}
