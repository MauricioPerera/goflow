package flowstore

import (
	"log"
	"time"

	"goflow/pkg/credentials"
	"goflow/pkg/flowvalidate"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/runstore"
)

// RunWithHistory wraps RunWithCredentials with the same "one shared path so
// every transport behaves identically" reasoning pkg/flowstore already
// applies to credential resolution: httpapi's runFlowVersion (backing both
// POST /flows/run and POST /flows/{name}/run) and mcpapi's tools/call both
// call this now, so a run recorded in history is recorded the same way
// regardless of which transport triggered it.
//
// flowName is the persisted flow's name for a named run, or "" for an ad-hoc
// POST /flows/run body — pure metadata alongside the record, never affects
// execution. historyStore may be nil (recording disabled), the same
// nil-means-off convention credStore already uses here.
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
func RunWithHistory(fv *model.FlowVersion, buildRegistry func() (*piece.Registry, error), credStore credentials.Store, historyStore runstore.Store, flowName string, trigger any, executeTrigger bool) (*model.ExecutionState, []flowvalidate.ValidationError, error) {
	started := time.Now()
	state, validationErrs, err := RunWithCredentials(fv, buildRegistry, credStore, trigger, executeTrigger)
	if err != nil || len(validationErrs) > 0 || state == nil || historyStore == nil {
		return state, validationErrs, err
	}

	if _, saveErr := historyStore.Save(runstore.Record{
		FlowName:   flowName,
		Trigger:    trigger,
		State:      state,
		StartedAt:  started,
		FinishedAt: time.Now(),
	}); saveErr != nil {
		log.Printf("flowstore: recording run history: %v", saveErr)
	}

	return state, validationErrs, nil
}
