package flowstore

import (
	"strings"
	"testing"

	"goflow/pkg/model"
	"goflow/pkg/piece"
	approvalpiece "goflow/pkg/pieces/approval"
	"goflow/pkg/runstore"
)

// approvalRegistry mirrors pkg/pieces/approval's own test fixtures — a
// registry with just the approval piece, enough for a flow whose single
// action pauses on ExecuteBegin and completes on ExecuteResume.
func approvalRegistry() (*piece.Registry, error) {
	r := piece.NewRegistry()
	r.Register(approvalpiece.New())
	return r, nil
}

func approvalFlow() model.FlowVersion {
	return model.FlowVersion{
		ID: "fv-approval", Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "approve", DisplayName: "Approve", Type: model.ActionPiece,
				Piece: &model.PieceSettings{PieceName: approvalpiece.PieceName, ActionName: "request", Input: map[string]any{
					"message": "please approve this",
				}},
			},
		},
	}
}

func TestResumeRun_ContinuesPausedRun_MarksResumeOfRunID(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()

	fv := approvalFlow()
	if err := fs.Save(FlowDefinition{Name: "flow", Flow: fv}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	state, _, err := RunWithHistory(&fv, approvalRegistry, nil, hist, fs, "flow", map[string]any{}, false, "")
	if err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	if state.Verdict.Status != model.FlowRunPaused {
		t.Fatalf("original verdict = %+v, want PAUSED", state.Verdict)
	}
	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %+v, want exactly 1", summaries)
	}
	pausedRunID := summaries[0].ID
	if summaries[0].ResumeOfRunID != "" {
		t.Fatalf("original run's ResumeOfRunID = %q, want empty — it's organic", summaries[0].ResumeOfRunID)
	}

	newState, vErrs, err := ResumeRun(hist, fs, approvalRegistry, nil, hist, pausedRunID, map[string]any{"approved": true, "comment": "looks good"})
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	if len(vErrs) != 0 {
		t.Fatalf("vErrs = %+v", vErrs)
	}
	if newState.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("resumed verdict = %+v, want SUCCEEDED", newState.Verdict)
	}
	out, ok := newState.Steps["approve"].Output.(map[string]any)
	if !ok || out["approved"] != true || out["comment"] != "looks good" {
		t.Fatalf("Output = %#v, want the resume payload reflected", newState.Steps["approve"].Output)
	}

	summaries, err = hist.List()
	if err != nil {
		t.Fatalf("List after resume: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %+v, want exactly 2 (paused + resumed)", summaries)
	}
	var resumeSummary *runstore.Summary
	for i, s := range summaries {
		if s.ID != pausedRunID {
			resumeSummary = &summaries[i]
		}
	}
	if resumeSummary == nil {
		t.Fatal("no second summary found")
	}
	if resumeSummary.ResumeOfRunID != pausedRunID {
		t.Fatalf("resume's ResumeOfRunID = %q, want %q", resumeSummary.ResumeOfRunID, pausedRunID)
	}
}

func TestResumeRun_RunsAgainstCurrentDefinition(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()

	fv := approvalFlow()
	if err := fs.Save(FlowDefinition{Name: "flow", Flow: fv}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, err := RunWithHistory(&fv, approvalRegistry, nil, hist, fs, "flow", map[string]any{}, false, ""); err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	summaries, _ := hist.List()
	pausedRunID := summaries[0].ID

	// Edit the flow's CURRENT saved definition: a different request message.
	edited := approvalFlow()
	edited.Trigger.NextAction.Piece.Input = map[string]any{"message": "an EDITED message"}
	if err := fs.Save(FlowDefinition{Name: "flow", Flow: edited}); err != nil {
		t.Fatalf("Save edited: %v", err)
	}

	newState, _, err := ResumeRun(hist, fs, approvalRegistry, nil, hist, pausedRunID, map[string]any{"approved": true})
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	if newState.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("resumed verdict = %+v, want SUCCEEDED", newState.Verdict)
	}
}

func TestResumeRun_AdHocRun_Rejected(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	fv := approvalFlow()
	if _, _, err := RunWithHistory(&fv, approvalRegistry, nil, hist, fs, "", map[string]any{}, false, ""); err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	summaries, _ := hist.List()
	runID := summaries[0].ID

	_, _, err = ResumeRun(hist, fs, approvalRegistry, nil, hist, runID, map[string]any{"approved": true})
	if err == nil || !strings.Contains(err.Error(), "ad-hoc") {
		t.Fatalf("err = %v, want an ad-hoc rejection", err)
	}
}

func TestResumeRun_UnknownRunID_Rejected(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	_, _, err = ResumeRun(hist, fs, approvalRegistry, nil, hist, "never-existed", map[string]any{"approved": true})
	if err == nil || !strings.Contains(err.Error(), "no run") {
		t.Fatalf("err = %v, want a \"no run\" rejection", err)
	}
}

func TestResumeRun_NotPaused_Rejected(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	fv := validCodeFlow() // succeeds outright, never pauses
	if err := fs.Save(FlowDefinition{Name: "flow", Flow: fv}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, err := RunWithHistory(&fv, emptyRegistry, nil, hist, fs, "flow", map[string]any{"x": 1}, false, ""); err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	summaries, _ := hist.List()
	runID := summaries[0].ID

	_, _, err = ResumeRun(hist, fs, emptyRegistry, nil, hist, runID, map[string]any{"approved": true})
	if err == nil || !strings.Contains(err.Error(), "not paused") {
		t.Fatalf("err = %v, want a \"not paused\" rejection", err)
	}
}

func TestResumeRun_SameRunIDTwice_BothSucceed_NoIdempotencyGuard(t *testing.T) {
	// runstore.Record is append-only/immutable, same as every other Record
	// in this project — ResumeRun only reads the ORIGINAL paused record by
	// id, it never marks it "consumed." So resuming the same pausedRunID
	// twice is currently ALLOWED, each call producing its own independent
	// resumed record — exactly the same lack-of-guard ReplayRun already
	// has (nothing stops replaying the same run id twice either). This
	// test documents that as current behavior, not a bug: a caller that
	// wants at-most-once resume semantics (e.g. an approval whose
	// downstream steps have real side effects) has to enforce that
	// itself today.
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	fv := approvalFlow()
	if err := fs.Save(FlowDefinition{Name: "flow", Flow: fv}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, err := RunWithHistory(&fv, approvalRegistry, nil, hist, fs, "flow", map[string]any{}, false, ""); err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	summaries, _ := hist.List()
	pausedRunID := summaries[0].ID

	if _, _, err := ResumeRun(hist, fs, approvalRegistry, nil, hist, pausedRunID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("first ResumeRun: %v", err)
	}
	if _, _, err := ResumeRun(hist, fs, approvalRegistry, nil, hist, pausedRunID, map[string]any{"approved": false}); err != nil {
		t.Fatalf("second ResumeRun (same pausedRunID): %v", err)
	}

	summaries, _ = hist.List()
	if len(summaries) != 3 {
		t.Fatalf("summaries = %+v, want exactly 3 (paused + two independent resumes)", summaries)
	}
}

func TestResumeRun_FlowDeletedSince_Rejected(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	fv := approvalFlow()
	if err := fs.Save(FlowDefinition{Name: "gone", Flow: fv}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, err := RunWithHistory(&fv, approvalRegistry, nil, hist, fs, "gone", map[string]any{}, false, ""); err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	summaries, _ := hist.List()
	runID := summaries[0].ID
	if err := fs.Delete("gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, _, err = ResumeRun(hist, fs, approvalRegistry, nil, hist, runID, map[string]any{"approved": true})
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("err = %v, want a \"no longer exists\" rejection", err)
	}
}
