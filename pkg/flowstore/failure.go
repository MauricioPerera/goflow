package flowstore

import (
	"log"

	"goflow/pkg/credentials"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/runstore"
)

// TriggerOnFailure runs flowName's OnFailureFlow (if configured) whenever
// state's Verdict ended up FAILED — the notification hook a webhook- or
// scheduler-fired run needs, since nothing else surfaces a failure to
// anyone not actively polling GET /runs. A no-op whenever state is nil,
// the run didn't fail, or onFailureFlow is "" (not configured) — the
// common case for every existing flow, unaffected by this ever existing.
//
// The on-failure flow is looked up by name in store and run through the
// SAME RunWithHistory path every other run takes — recorded in history
// under its own name, credentials resolved the same way, everything else
// identical to any other named run. Its trigger payload carries the
// minimum needed to react: flowName (which flow failed), and the failed
// step's Name/DisplayName/Message (model.FailedStep, if the verdict has
// one — a run can also fail with FailedStep nil, e.g. a broken registry
// build inside RunWithCredentials never reaching a step at all).
//
// Deliberately synchronous: this blocks the caller until the on-failure
// flow itself finishes, so a slow notification (e.g. a PIECE action
// calling a real API) adds real latency to whatever triggered the
// original failure. The alternative — firing it in a goroutine — would
// make it untestable without a synchronization hook, and this project
// has no flaky/async tests anywhere; the latency tradeoff is accepted
// deliberately, not overlooked.
//
// Best-effort and one-way: any error resolving or running the on-failure
// flow is logged and otherwise swallowed — a broken notification must
// never change the ORIGINAL run's already-final outcome. The on-failure
// flow's own OnFailureFlow (if it has one) is never read here — capping
// the chain at exactly one hop, so a circular pair (A's on-failure is B,
// B's on-failure is A) can never loop.
func TriggerOnFailure(store Store, flowName, onFailureFlow string, state *model.ExecutionState, buildRegistry func() (*piece.Registry, error), credStore credentials.Store, historyStore runstore.Store) {
	if state == nil || state.Verdict.Status != model.FlowRunFailed || onFailureFlow == "" {
		return
	}

	def, ok, err := store.Get(onFailureFlow)
	if err != nil || !ok {
		log.Printf("flowstore: TriggerOnFailure: resolving on-failure flow %q for %q: ok=%v err=%v", onFailureFlow, flowName, ok, err)
		return
	}

	var failedStepName, failedStepDisplayName, failedMessage string
	if fs := state.Verdict.FailedStep; fs != nil {
		failedStepName, failedStepDisplayName, failedMessage = fs.Name, fs.DisplayName, fs.Message
	}
	payload := map[string]any{
		"flowName":              flowName,
		"failedStepName":        failedStepName,
		"failedStepDisplayName": failedStepDisplayName,
		"failedMessage":         failedMessage,
	}

	if _, _, runErr := RunWithHistory(&def.Flow, buildRegistry, credStore, historyStore, def.Name, payload, false); runErr != nil {
		log.Printf("flowstore: TriggerOnFailure: running on-failure flow %q for %q: %v", onFailureFlow, flowName, runErr)
	}
}
