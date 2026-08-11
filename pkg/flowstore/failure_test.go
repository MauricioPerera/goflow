package flowstore

import (
	"strings"
	"testing"

	"goflow/pkg/model"
	"goflow/pkg/runstore"
)

func failedStateFrom(fv model.FlowVersion) *model.ExecutionState {
	state, _, err := Run(&fv, emptyRegistry, nil, nil, false)
	if err != nil {
		panic(err)
	}
	if state.Verdict.Status != model.FlowRunFailed {
		panic("failedStateFrom: flow did not actually fail")
	}
	return state
}

func TestTriggerOnFailure_NoopWhenStateNil(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	TriggerOnFailure(store, "main", "notify", nil, emptyRegistry, nil, hist)
	if summaries, _ := hist.List(); len(summaries) != 0 {
		t.Fatalf("history = %+v, want empty — state was nil", summaries)
	}
}

func TestTriggerOnFailure_NoopWhenSucceeded(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "notify", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	hist := runstore.NewMemoryStore()
	fv := validCodeFlow()
	succeeded := succeededStateFrom(t, fv)
	TriggerOnFailure(store, "main", "notify", succeeded, emptyRegistry, nil, hist)
	if summaries, _ := hist.List(); len(summaries) != 0 {
		t.Fatalf("history = %+v, want empty — the original run succeeded, no notification due", summaries)
	}
}

func succeededStateFrom(t *testing.T, fv model.FlowVersion) *model.ExecutionState {
	t.Helper()
	state, _, err := Run(&fv, emptyRegistry, nil, nil, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("flow did not succeed: %+v", state.Verdict)
	}
	return state
}

func TestTriggerOnFailure_NoopWhenOnFailureFlowEmpty(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	failed := failedStateFrom(failingCodeFlow())
	TriggerOnFailure(store, "main", "", failed, emptyRegistry, nil, hist)
	if summaries, _ := hist.List(); len(summaries) != 0 {
		t.Fatalf("history = %+v, want empty — OnFailureFlow not configured", summaries)
	}
}

func TestTriggerOnFailure_RunsNamedFlowOnFailure_RecordsInHistory(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "notify", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	hist := runstore.NewMemoryStore()
	failed := failedStateFrom(failingCodeFlow())

	TriggerOnFailure(store, "main", "notify", failed, emptyRegistry, nil, hist)

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 || summaries[0].FlowName != "notify" {
		t.Fatalf("history = %+v, want exactly one run recorded under \"notify\"", summaries)
	}

	rec, ok, err := hist.Get(summaries[0].ID)
	if err != nil || !ok {
		t.Fatalf("Get(%q): ok=%v err=%v", summaries[0].ID, ok, err)
	}
	payload, ok := rec.Trigger.(map[string]any)
	if !ok {
		t.Fatalf("Trigger = %#v, want a map", rec.Trigger)
	}
	if payload["flowName"] != "main" {
		t.Fatalf("Trigger[flowName] = %v, want \"main\"", payload["flowName"])
	}
	if payload["failedStepName"] != "boom" {
		t.Fatalf("Trigger[failedStepName] = %v, want \"boom\"", payload["failedStepName"])
	}
	if msg, _ := payload["failedMessage"].(string); !strings.Contains(msg, "boom") {
		t.Fatalf("Trigger[failedMessage] = %v, want it to mention the thrown error", payload["failedMessage"])
	}
}

func TestTriggerOnFailure_UnknownOnFailureFlow_NoopNoPanic(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	failed := failedStateFrom(failingCodeFlow())

	TriggerOnFailure(store, "main", "never-saved", failed, emptyRegistry, nil, hist)

	if summaries, _ := hist.List(); len(summaries) != 0 {
		t.Fatalf("history = %+v, want empty — the on-failure flow doesn't exist", summaries)
	}
}

func TestTriggerOnFailure_DoesNotChainRecursively(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	// "notify" itself fails AND has its own OnFailureFlow pointing at
	// "main" — must never be followed: TriggerOnFailure only ever runs
	// ONE hop, regardless of what the flow it ran declares.
	if err := store.Save(FlowDefinition{Name: "notify", Flow: failingCodeFlow(), OnFailureFlow: "main"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "main", Flow: failingCodeFlow(), OnFailureFlow: "notify"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	hist := runstore.NewMemoryStore()
	failed := failedStateFrom(failingCodeFlow())

	TriggerOnFailure(store, "main", "notify", failed, emptyRegistry, nil, hist)

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 || summaries[0].FlowName != "notify" {
		t.Fatalf("history = %+v, want EXACTLY one run (\"notify\") — a second hop back to \"main\" would mean unbounded recursion for any circular pair", summaries)
	}
}
