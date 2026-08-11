package flowstore

import (
	"testing"

	"goflow/pkg/model"
	"goflow/pkg/runstore"
)

// pausedStateFrom runs fv (expected to pause, e.g. approvalFlow() from
// resume_test.go) and fails the test if it doesn't — mirrors
// failedStateFrom/succeededStateFrom in failure_test.go.
func pausedStateFrom(t *testing.T, fv model.FlowVersion) *model.ExecutionState {
	t.Helper()
	state, _, err := Run(&fv, approvalRegistry, nil, map[string]any{}, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Verdict.Status != model.FlowRunPaused {
		t.Fatalf("flow did not pause: %+v", state.Verdict)
	}
	return state
}

func TestTriggerOnPause_NoopWhenStateNil(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	triggerOnPause(store, "main", "notify", nil, "whatever-id", hist, emptyRegistry, nil)
	if summaries, _ := hist.List(); len(summaries) != 0 {
		t.Fatalf("history = %+v, want empty — state was nil", summaries)
	}
}

func TestTriggerOnPause_NoopWhenNotPaused(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "notify", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	hist := runstore.NewMemoryStore()
	succeeded := succeededStateFrom(t, validCodeFlow())
	triggerOnPause(store, "main", "notify", succeeded, "whatever-id", hist, emptyRegistry, nil)
	if summaries, _ := hist.List(); len(summaries) != 0 {
		t.Fatalf("history = %+v, want empty — the original run succeeded, not paused", summaries)
	}
}

func TestTriggerOnPause_NoopWhenOnPauseFlowEmpty(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	paused := pausedStateFrom(t, approvalFlow())
	runID, err := hist.Save(runstore.Record{FlowName: "main", State: paused})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	triggerOnPause(store, "main", "", paused, runID, hist, approvalRegistry, nil)
	summaries, _ := hist.List()
	if len(summaries) != 1 {
		t.Fatalf("history = %+v, want exactly 1 (only the pre-seeded paused record) — OnPauseFlow not configured", summaries)
	}
}

func TestTriggerOnPause_RunsNamedFlowOnPause_PayloadHasRunIDAndResumeToken(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "notify", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	hist := runstore.NewMemoryStore()
	paused := pausedStateFrom(t, approvalFlow())
	runID, err := hist.Save(runstore.Record{FlowName: "main", State: paused})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	pausedRec, ok, err := hist.Get(runID)
	if err != nil || !ok {
		t.Fatalf("Get(%q): ok=%v err=%v", runID, ok, err)
	}
	if pausedRec.ResumeToken == "" {
		t.Fatalf("pre-seeded record has no ResumeToken — Save should have generated one for a PAUSED record")
	}

	triggerOnPause(store, "main", "notify", paused, runID, hist, approvalRegistry, nil)

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("history = %+v, want exactly 2 (the pre-seeded paused record + the notify run)", summaries)
	}
	var notifySummary *runstore.Summary
	for i, s := range summaries {
		if s.FlowName == "notify" {
			notifySummary = &summaries[i]
		}
	}
	if notifySummary == nil {
		t.Fatal("no \"notify\" run recorded")
	}

	notifyRec, ok, err := hist.Get(notifySummary.ID)
	if err != nil || !ok {
		t.Fatalf("Get(%q): ok=%v err=%v", notifySummary.ID, ok, err)
	}
	payload, ok := notifyRec.Trigger.(map[string]any)
	if !ok {
		t.Fatalf("Trigger = %#v, want a map", notifyRec.Trigger)
	}
	if payload["flowName"] != "main" {
		t.Fatalf("Trigger[flowName] = %v, want \"main\"", payload["flowName"])
	}
	if payload["runId"] != runID {
		t.Fatalf("Trigger[runId] = %v, want %q", payload["runId"], runID)
	}
	if payload["resumeToken"] != pausedRec.ResumeToken {
		t.Fatalf("Trigger[resumeToken] = %v, want %q", payload["resumeToken"], pausedRec.ResumeToken)
	}
	if payload["pausedStepName"] != "approve" {
		t.Fatalf("Trigger[pausedStepName] = %v, want \"approve\"", payload["pausedStepName"])
	}
}

func TestTriggerOnPause_UnknownOnPauseFlow_NoopNoPanic(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	paused := pausedStateFrom(t, approvalFlow())
	runID, err := hist.Save(runstore.Record{FlowName: "main", State: paused})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	triggerOnPause(store, "main", "never-saved", paused, runID, hist, approvalRegistry, nil)

	summaries, _ := hist.List()
	if len(summaries) != 1 {
		t.Fatalf("history = %+v, want exactly 1 (only the pre-seeded paused record) — the on-pause flow doesn't exist", summaries)
	}
}

func TestTriggerOnPause_DoesNotChainRecursively(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	// "notify" itself PAUSES (uses the approval piece too) AND has its
	// own OnPauseFlow pointing at "third" — must never be followed:
	// triggerOnPause only ever runs ONE hop, regardless of what the flow
	// it ran declares.
	if err := store.Save(FlowDefinition{Name: "notify", Flow: approvalFlow(), OnPauseFlow: "third"}); err != nil {
		t.Fatalf("Save notify: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "third", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save third: %v", err)
	}
	hist := runstore.NewMemoryStore()
	paused := pausedStateFrom(t, approvalFlow())
	runID, err := hist.Save(runstore.Record{FlowName: "main", State: paused})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	triggerOnPause(store, "main", "notify", paused, runID, hist, approvalRegistry, nil)

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range summaries {
		if s.FlowName == "third" {
			t.Fatalf("history = %+v, want NO \"third\" entry — a second hop would mean unbounded recursion for a circular pair", summaries)
		}
	}
	if len(summaries) != 2 {
		t.Fatalf("history = %+v, want exactly 2 (pre-seeded paused record + notify's own paused run)", summaries)
	}
}

func TestRunWithHistory_TriggersOnPauseFlow_EndToEnd(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Save(FlowDefinition{Name: "notify", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save notify: %v", err)
	}
	hist := runstore.NewMemoryStore()
	fv := approvalFlow()

	state, validationErrs, err := RunWithHistory(&fv, approvalRegistry, nil, hist, fs, "main", map[string]any{}, false, "notify")
	if err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	if len(validationErrs) != 0 {
		t.Fatalf("validationErrs = %+v", validationErrs)
	}
	if state.Verdict.Status != model.FlowRunPaused {
		t.Fatalf("verdict = %+v, want PAUSED", state.Verdict)
	}

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %+v, want exactly 2 (\"main\" paused + \"notify\" triggered on pause)", summaries)
	}
	var mainSummary, notifySummary *runstore.Summary
	for i, s := range summaries {
		switch s.FlowName {
		case "main":
			mainSummary = &summaries[i]
		case "notify":
			notifySummary = &summaries[i]
		}
	}
	if mainSummary == nil || notifySummary == nil {
		t.Fatalf("summaries = %+v, want one \"main\" and one \"notify\"", summaries)
	}

	mainRec, ok, err := hist.Get(mainSummary.ID)
	if err != nil || !ok {
		t.Fatalf("Get(main): ok=%v err=%v", ok, err)
	}
	if mainRec.ResumeToken == "" {
		t.Fatal("main's own paused record has no ResumeToken")
	}

	notifyRec, ok, err := hist.Get(notifySummary.ID)
	if err != nil || !ok {
		t.Fatalf("Get(notify): ok=%v err=%v", ok, err)
	}
	payload, ok := notifyRec.Trigger.(map[string]any)
	if !ok {
		t.Fatalf("Trigger = %#v, want a map", notifyRec.Trigger)
	}
	if payload["runId"] != mainSummary.ID {
		t.Fatalf("Trigger[runId] = %v, want %q", payload["runId"], mainSummary.ID)
	}
	if payload["resumeToken"] != mainRec.ResumeToken {
		t.Fatalf("Trigger[resumeToken] = %v, want %q", payload["resumeToken"], mainRec.ResumeToken)
	}
}
