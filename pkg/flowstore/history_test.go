package flowstore

import (
	"encoding/json"
	"strings"
	"testing"

	"goflow/pkg/model"
	"goflow/pkg/runstore"
)

// failingCodeFlow is a CODE-only flow whose step throws, so ExecuteBegin
// finishes with a FAILED verdict — used to prove a failed run is recorded
// exactly as readily as a succeeded one.
func failingCodeFlow() model.FlowVersion {
	return model.FlowVersion{
		ID: "fv-fail",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "boom", DisplayName: "Boom", Type: model.ActionCode,
				Code: &model.CodeSettings{
					Source: `(params) => { throw new Error("boom"); }`,
				},
			},
		},
	}
}

func TestRunWithHistory_NilHistoryStore_BehavesLikeRunWithCredentials(t *testing.T) {
	fv := validCodeFlow()
	state, vErrs, err := RunWithHistory(&fv, emptyRegistry, nil, nil, "some-flow", nil, false)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(vErrs) != 0 {
		t.Fatalf("validationErrs = %v, want none", vErrs)
	}
	if state == nil || state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("state = %+v, want a SUCCEEDED verdict", state)
	}
}

func TestRunWithHistory_SuccessfulRun_Recorded(t *testing.T) {
	fv := validCodeFlow()
	hist := runstore.NewMemoryStore()
	trigger := map[string]any{"seed": "value"}

	state, vErrs, err := RunWithHistory(&fv, emptyRegistry, nil, hist, "double-it", trigger, false)
	if err != nil || len(vErrs) != 0 {
		t.Fatalf("err=%v vErrs=%v, want a clean successful run", err, vErrs)
	}
	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("state.Verdict.Status = %v, want SUCCEEDED", state.Verdict.Status)
	}

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("List() = %+v, want exactly 1 recorded run", summaries)
	}
	s := summaries[0]
	if s.FlowName != "double-it" {
		t.Fatalf("recorded FlowName = %q, want %q", s.FlowName, "double-it")
	}
	if s.Status != model.FlowRunSucceeded {
		t.Fatalf("recorded Status = %v, want SUCCEEDED", s.Status)
	}
	if s.FinishedAt.Before(s.StartedAt) {
		t.Fatalf("FinishedAt %v is before StartedAt %v", s.FinishedAt, s.StartedAt)
	}

	rec, ok, err := hist.Get(s.ID)
	if err != nil || !ok {
		t.Fatalf("Get(%q) = ok=%v err=%v, want ok=true", s.ID, ok, err)
	}
	if rec.Trigger == nil {
		t.Fatal("recorded Trigger is nil, want the trigger payload preserved")
	}
	if rec.State == nil || len(rec.State.Steps) == 0 {
		t.Fatalf("recorded State = %+v, want the full ExecutionState with its steps", rec.State)
	}
}

func TestRunWithHistory_FailedRun_StillRecorded(t *testing.T) {
	fv := failingCodeFlow()
	hist := runstore.NewMemoryStore()

	state, vErrs, err := RunWithHistory(&fv, emptyRegistry, nil, hist, "flaky-flow", nil, false)
	if err != nil || len(vErrs) != 0 {
		t.Fatalf("err=%v vErrs=%v, want a clean call (the FLOW fails, not the call)", err, vErrs)
	}
	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("state.Verdict.Status = %v, want FAILED", state.Verdict.Status)
	}

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 || summaries[0].Status != model.FlowRunFailed {
		t.Fatalf("List() = %+v, want exactly 1 FAILED run — a failed run belongs in history too", summaries)
	}
}

func TestRunWithHistory_ValidationFailure_NotRecorded(t *testing.T) {
	fv := pieceReferencingFlow()
	hist := runstore.NewMemoryStore()

	state, vErrs, err := RunWithHistory(&fv, emptyRegistry, nil, hist, "broken-flow", nil, false)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(vErrs) == 0 {
		t.Fatal("validationErrs is empty, want the piece-referencing flow to fail validation")
	}
	if state != nil {
		t.Fatalf("state = %v, want nil on a validation failure", state)
	}

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 0 {
		t.Fatalf("List() = %+v, want nothing recorded — the flow never actually ran", summaries)
	}
}

func TestRunWithHistory_BuildRegistryFails_NotRecorded(t *testing.T) {
	fv := validCodeFlow()
	hist := runstore.NewMemoryStore()

	_, _, err := RunWithHistory(&fv, failingRegistry, nil, hist, "some-flow", nil, false)
	if err == nil {
		t.Fatal("err = nil, want the simulated registry-build failure")
	}

	summaries, listErr := hist.List()
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(summaries) != 0 {
		t.Fatalf("List() = %+v, want nothing recorded — a server-side fault is not a completed run", summaries)
	}
}

func TestRunWithHistory_AdHocRun_RecordedWithEmptyFlowName(t *testing.T) {
	fv := validCodeFlow()
	hist := runstore.NewMemoryStore()

	if _, _, err := RunWithHistory(&fv, emptyRegistry, nil, hist, "", nil, false); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 || summaries[0].FlowName != "" {
		t.Fatalf("List() = %+v, want exactly 1 run with FlowName=\"\" (ad-hoc)", summaries)
	}
}

// TestRunWithHistory_CredentialRedactedInRecordedState proves the recorded
// Record never carries the raw secret — RunWithHistory calls
// RunWithCredentials internally, whose redaction pass runs BEFORE
// RunWithHistory's own Save, the same ordering that already keeps a secret
// out of the HTTP/MCP response (see TestRunWithCredentials_SecretNotInStateJSON).
func TestRunWithHistory_CredentialRedactedInRecordedState(t *testing.T) {
	credStore := newCredStore(t)
	const secret = "sk-live-super-secret-key"
	if err := credStore.Save("api_key", secret); err != nil {
		t.Fatalf("credStore.Save: %v", err)
	}
	fv := credMarkerFlow("use_cred", "api_key")
	hist := runstore.NewMemoryStore()

	state, vErrs, err := RunWithHistory(&fv, emptyRegistry, credStore, hist, "cred-flow", nil, false)
	if err != nil || len(vErrs) != 0 {
		t.Fatalf("err=%v vErrs=%v, want a clean successful run", err, vErrs)
	}
	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("state.Verdict.Status = %v, want SUCCEEDED", state.Verdict.Status)
	}

	summaries, err := hist.List()
	if err != nil || len(summaries) != 1 {
		t.Fatalf("List() = %+v, err=%v, want exactly 1 recorded run", summaries, err)
	}
	rec, ok, err := hist.Get(summaries[0].ID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}

	step, ok := rec.State.Steps["use_cred"]
	if !ok {
		t.Fatal("recorded state has no \"use_cred\" step")
	}
	in, ok := step.Input.(map[string]any)
	if !ok {
		t.Fatalf("recorded step Input = %#v, want a map", step.Input)
	}
	if in["auth"] != "<credential:api_key>" {
		t.Fatalf(`recorded Input["auth"] = %#v, want the redaction placeholder — the raw secret must never reach the history store`, in["auth"])
	}
	// The step actually ran against the REAL secret (authLen proves it) —
	// redaction only touches what gets recorded, not what the piece received.
	// authLen came from goja's export of a plain property access (no
	// arithmetic), which this project's own jspiece code documents as
	// sometimes int64 rather than float64 — accept either.
	out, _ := step.Output.(map[string]any)
	var authLen int
	switch n := out["authLen"].(type) {
	case float64:
		authLen = int(n)
	case int64:
		authLen = int(n)
	default:
		t.Fatalf("step Output authLen = %#v (%T), want a number", out["authLen"], out["authLen"])
	}
	if authLen != len(secret) {
		t.Fatalf("step Output authLen = %d, want %d — the real secret must still have reached the piece", authLen, len(secret))
	}
	recJSON, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal recorded Record: %v", err)
	}
	if strings.Contains(string(recJSON), secret) {
		t.Fatalf("the raw secret appears SOMEWHERE in the recorded Record — redaction was bypassed:\n%s", recJSON)
	}
}
