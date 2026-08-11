package flowstore

import (
	"fmt"
	"strings"
	"testing"

	"goflow/pkg/model"
	"goflow/pkg/runstore"
)

// callFlowFlow is a FlowVersion whose single action calls another saved
// flow by name (targetName), passing {"from": id} as that sub-flow's
// trigger payload.
func callFlowFlow(id, targetName string) model.FlowVersion {
	return model.FlowVersion{
		ID: id,
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "call", DisplayName: "Call", Type: model.ActionCallFlow,
				CallFlow: &model.CallFlowSettings{FlowName: targetName, Input: map[string]any{"from": id}},
			},
		},
	}
}

func TestCallFlow_ChainOfTwoFlows_Succeeds(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "leaf", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save leaf: %v", err)
	}
	root := FlowDefinition{Name: "root", Flow: callFlowFlow("fv-root", "leaf")}
	if err := store.Save(root); err != nil {
		t.Fatalf("Save root: %v", err)
	}
	hist := runstore.NewMemoryStore()

	state, vErrs, err := RunWithHistory(&root.Flow, emptyRegistry, nil, hist, store, "root", nil, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(vErrs) != 0 {
		t.Fatalf("validationErrs = %+v", vErrs)
	}
	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}

	callOut := state.Steps["call"]
	subState, ok := callOut.Output.(*model.ExecutionState)
	if !ok {
		t.Fatalf("Output = %#v (%T), want the sub-flow's *model.ExecutionState", callOut.Output, callOut.Output)
	}
	if subState.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("nested sub-state verdict = %+v, want SUCCEEDED", subState.Verdict)
	}

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := map[string]int{}
	for _, s := range summaries {
		names[s.FlowName]++
	}
	if names["root"] != 1 || names["leaf"] != 1 {
		t.Fatalf("history = %v, want exactly one record each for \"root\" and \"leaf\" — every hop is independently recorded", names)
	}
}

func TestCallFlow_CycleDetected_FailsCleanlyNotInfinitely(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "a", Flow: callFlowFlow("fv-a", "b")}); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "b", Flow: callFlowFlow("fv-b", "a")}); err != nil {
		t.Fatalf("Save b: %v", err)
	}
	hist := runstore.NewMemoryStore()

	aDef, ok, err := store.Get("a")
	if err != nil || !ok {
		t.Fatalf("Get(a): ok=%v err=%v", ok, err)
	}
	state, vErrs, err := RunWithHistory(&aDef.Flow, emptyRegistry, nil, hist, store, "a", nil, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(vErrs) != 0 {
		t.Fatalf("validationErrs = %+v", vErrs)
	}
	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED (a->b->a is a cycle)", state.Verdict)
	}
	if !strings.Contains(state.Steps["call"].ErrorMessage, "cycle") {
		t.Fatalf("ErrorMessage = %q, want it to mention the detected cycle", state.Steps["call"].ErrorMessage)
	}

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("history = %+v, want exactly 2 records (a, b) — the cycle must be caught before a third", summaries)
	}
}

func TestCallFlow_DepthLimitExceeded_StopsShortOfTheWholeChain(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	const chainLen = 15 // deliberately longer than maxCallFlowDepth, all UNIQUE names (no cycle)
	names := make([]string, chainLen)
	for i := range names {
		names[i] = fmt.Sprintf("f%d", i)
	}
	for i := 0; i < chainLen; i++ {
		var flow model.FlowVersion
		if i == chainLen-1 {
			flow = validCodeFlow() // terminal leaf, no further CALL_FLOW
		} else {
			flow = callFlowFlow(fmt.Sprintf("fv-%s", names[i]), names[i+1])
		}
		if err := store.Save(FlowDefinition{Name: names[i], Flow: flow}); err != nil {
			t.Fatalf("Save %q: %v", names[i], err)
		}
	}
	hist := runstore.NewMemoryStore()

	rootDef, ok, err := store.Get(names[0])
	if err != nil || !ok {
		t.Fatalf("Get(%q): ok=%v err=%v", names[0], ok, err)
	}
	state, vErrs, err := RunWithHistory(&rootDef.Flow, emptyRegistry, nil, hist, store, names[0], nil, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(vErrs) != 0 {
		t.Fatalf("validationErrs = %+v", vErrs)
	}
	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED once the depth limit is hit", state.Verdict)
	}
	if !strings.Contains(state.Steps["call"].ErrorMessage, "depth limit") {
		t.Fatalf("ErrorMessage = %q, want it to mention the depth limit", state.Steps["call"].ErrorMessage)
	}

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) == 0 || len(summaries) >= chainLen {
		t.Fatalf("history has %d records, want somewhere between 1 and %d — the chain must stop well short of completing all %d hops", len(summaries), chainLen-1, chainLen)
	}
}

func TestCallFlow_UnknownTarget_FailsCleanly(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	root := FlowDefinition{Name: "root", Flow: callFlowFlow("fv-root", "never-saved")}
	if err := store.Save(root); err != nil {
		t.Fatalf("Save: %v", err)
	}
	hist := runstore.NewMemoryStore()

	state, vErrs, err := RunWithHistory(&root.Flow, emptyRegistry, nil, hist, store, "root", nil, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(vErrs) != 0 {
		t.Fatalf("validationErrs = %+v", vErrs)
	}
	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED", state.Verdict)
	}
	if !strings.Contains(state.Steps["call"].ErrorMessage, `no flow named "never-saved"`) {
		t.Fatalf("ErrorMessage = %q, want it to name the missing target", state.Steps["call"].ErrorMessage)
	}
}

func TestCallFlow_FlowStoreNil_DisablesCallFlowCleanly(t *testing.T) {
	root := callFlowFlow("fv-root", "leaf")
	hist := runstore.NewMemoryStore()

	state, vErrs, err := RunWithHistory(&root, emptyRegistry, nil, hist, nil, "root", nil, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(vErrs) != 0 {
		t.Fatalf("validationErrs = %+v", vErrs)
	}
	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED", state.Verdict)
	}
	if !strings.Contains(state.Steps["call"].ErrorMessage, "not enabled") {
		t.Fatalf("ErrorMessage = %q, want it to say sub-flow calls aren't enabled", state.Steps["call"].ErrorMessage)
	}
}

// TestCallFlow_SubFlowCredentialResolved proves a sub-flow invoked via
// CALL_FLOW gets its OWN $credential markers resolved exactly like a
// top-level run would — recursion goes back through RunWithCredentials,
// not a raw engine.ExecuteBegin, so this isn't a special case.
func TestCallFlow_SubFlowCredentialResolved(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	credStore := newCredStore(t)
	if err := credStore.Save("api-key", "sk-live-secret"); err != nil {
		t.Fatalf("credStore.Save: %v", err)
	}

	credFlow := model.FlowVersion{
		ID: "fv-cred-leaf",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "use_cred", DisplayName: "Use Cred", Type: model.ActionCode,
				Code: &model.CodeSettings{
					Input:  map[string]any{"auth": map[string]any{"$credential": "api-key"}},
					Source: `(params) => ({ authLen: params.auth.length })`,
				},
			},
		},
	}
	if err := store.Save(FlowDefinition{Name: "cred-leaf", Flow: credFlow}); err != nil {
		t.Fatalf("Save cred-leaf: %v", err)
	}
	root := FlowDefinition{Name: "root", Flow: callFlowFlow("fv-root", "cred-leaf")}
	if err := store.Save(root); err != nil {
		t.Fatalf("Save root: %v", err)
	}
	hist := runstore.NewMemoryStore()

	state, _, err := RunWithHistory(&root.Flow, emptyRegistry, credStore, hist, store, "root", nil, false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	subState, ok := state.Steps["call"].Output.(*model.ExecutionState)
	if !ok {
		t.Fatalf("Output = %#v, want *model.ExecutionState", state.Steps["call"].Output)
	}
	credStep := subState.Steps["use_cred"]
	output, _ := credStep.Output.(map[string]any)
	if output["authLen"] != int64(len("sk-live-secret")) {
		t.Fatalf("sub-flow's use_cred.Output = %#v, want authLen=%d (the real secret's length) — credential resolution must reach INTO a CALL_FLOW target", credStep.Output, len("sk-live-secret"))
	}
	input, _ := credStep.Input.(map[string]any)
	if input["auth"] != "<credential:api-key>" {
		t.Fatalf("sub-flow's use_cred.Input[auth] = %v, want the redaction placeholder — the raw secret must never leak into the recorded state", input["auth"])
	}
}
