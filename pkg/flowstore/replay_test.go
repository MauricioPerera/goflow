package flowstore

import (
	"strings"
	"testing"

	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/runstore"
)

func TestReplayRun_RunsAgainstCurrentDefinition_MarksReplayOfRunID(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()

	original := validCodeFlow() // hardcoded n=21 -> {"doubled": 42}
	if err := fs.Save(FlowDefinition{Name: "flow", Flow: original}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	state, _, err := RunWithHistory(&original, emptyRegistry, nil, hist, fs, "flow", map[string]any{"x": 1}, false)
	if err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("original verdict = %+v", state.Verdict)
	}
	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %+v, want exactly 1", summaries)
	}
	originalRunID := summaries[0].ID
	if summaries[0].ReplayOfRunID != "" {
		t.Fatalf("original run's ReplayOfRunID = %q, want empty — it's organic", summaries[0].ReplayOfRunID)
	}

	// Edit the flow's CURRENT saved definition to produce a different shape.
	edited := model.FlowVersion{
		ID: "fv-edited",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "double", DisplayName: "Double", Type: model.ActionCode,
				Code: &model.CodeSettings{Input: map[string]any{"n": 21}, Source: `(params) => ({ tripled: params.n * 3 })`},
			},
		},
	}
	if err := fs.Save(FlowDefinition{Name: "flow", Flow: edited}); err != nil {
		t.Fatalf("Save edited: %v", err)
	}

	newState, vErrs, err := ReplayRun(hist, fs, emptyRegistry, nil, hist, originalRunID)
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}
	if len(vErrs) != 0 {
		t.Fatalf("vErrs = %+v", vErrs)
	}
	if newState.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("replay verdict = %+v", newState.Verdict)
	}
	doubleOut, ok := newState.Steps["double"].Output.(map[string]any)
	if !ok || doubleOut["tripled"] == nil {
		t.Fatalf("Output = %#v, want the EDITED definition's shape (tripled), not the original (doubled) — replay must run against the CURRENT definition", newState.Steps["double"].Output)
	}

	summaries, err = hist.List()
	if err != nil {
		t.Fatalf("List after replay: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %+v, want exactly 2 (original + replay)", summaries)
	}
	var replaySummary *runstore.Summary
	for i, s := range summaries {
		if s.ID != originalRunID {
			replaySummary = &summaries[i]
		}
	}
	if replaySummary == nil {
		t.Fatal("no second summary found")
	}
	if replaySummary.ReplayOfRunID != originalRunID {
		t.Fatalf("replay's ReplayOfRunID = %q, want %q", replaySummary.ReplayOfRunID, originalRunID)
	}
}

func TestReplayRun_PreservesExecuteTrigger(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()

	registry := func() (*piece.Registry, error) {
		r := piece.NewRegistry()
		r.Register(piece.Piece{
			Name: "probe", DisplayName: "Probe",
			Triggers: map[string]piece.Trigger{
				"fire": {Name: "fire", DisplayName: "Fire", Run: func(piece.TriggerContext) ([]any, error) {
					return []any{"hook-was-invoked"}, nil
				}},
			},
		})
		return r, nil
	}
	fv := model.FlowVersion{
		ID: "fv-trigger-probe",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerPiece,
			PieceName: "probe", TriggerName: "fire",
			NextAction: &model.FlowAction{
				Name: "echo", DisplayName: "Echo", Type: model.ActionCode,
				Code: &model.CodeSettings{
					Input:  map[string]any{"got": "{{ trigger_1.output }}"},
					Source: `(params) => params`,
				},
			},
		},
	}
	if err := fs.Save(FlowDefinition{Name: "probed", Flow: fv}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Original run: ExecuteTrigger=true, so the payload passed in is
	// IGNORED — the hook's own return value becomes the trigger output.
	state, _, err := RunWithHistory(&fv, registry, nil, hist, fs, "probed", "payload-passed-in", true)
	if err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	echoOut, _ := state.Steps["echo"].Output.(map[string]any)
	if echoOut["got"] != "hook-was-invoked" {
		t.Fatalf("original echo.Output = %#v, want the hook's own output (ExecuteTrigger=true)", echoOut)
	}
	summaries, _ := hist.List()
	runID := summaries[0].ID

	newState, _, err := ReplayRun(hist, fs, registry, nil, hist, runID)
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}
	newEchoOut, _ := newState.Steps["echo"].Output.(map[string]any)
	if newEchoOut["got"] != "hook-was-invoked" {
		t.Fatalf("replay echo.Output = %#v, want the hook invoked again (ExecuteTrigger must be preserved as true, not default to false)", newEchoOut)
	}
}

func TestReplayRun_AdHocRun_Rejected(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	fv := validCodeFlow()
	if _, _, err := RunWithHistory(&fv, emptyRegistry, nil, hist, fs, "", nil, false); err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	summaries, _ := hist.List()
	runID := summaries[0].ID

	_, _, err = ReplayRun(hist, fs, emptyRegistry, nil, hist, runID)
	if err == nil || !strings.Contains(err.Error(), "ad-hoc") {
		t.Fatalf("err = %v, want an ad-hoc rejection", err)
	}
}

func TestReplayRun_UnknownRunID_Rejected(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	_, _, err = ReplayRun(hist, fs, emptyRegistry, nil, hist, "never-existed")
	if err == nil || !strings.Contains(err.Error(), "no run") {
		t.Fatalf("err = %v, want a \"no run\" rejection", err)
	}
}

func TestReplayRun_FlowDeletedSince_Rejected(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	fv := validCodeFlow()
	if err := fs.Save(FlowDefinition{Name: "gone", Flow: fv}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, err := RunWithHistory(&fv, emptyRegistry, nil, hist, fs, "gone", nil, false); err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	summaries, _ := hist.List()
	runID := summaries[0].ID
	if err := fs.Delete("gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, _, err = ReplayRun(hist, fs, emptyRegistry, nil, hist, runID)
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("err = %v, want a \"no longer exists\" rejection", err)
	}
}

func TestReplayRun_SubFlowCallsNotMarkedAsReplays(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	hist := runstore.NewMemoryStore()
	if err := fs.Save(FlowDefinition{Name: "leaf", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save leaf: %v", err)
	}
	root := callFlowFlow("fv-root", "leaf")
	if err := fs.Save(FlowDefinition{Name: "root", Flow: root}); err != nil {
		t.Fatalf("Save root: %v", err)
	}
	if _, _, err := RunWithHistory(&root, emptyRegistry, nil, hist, fs, "root", nil, false); err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	summaries, _ := hist.List()
	var rootRunID string
	for _, s := range summaries {
		if s.FlowName == "root" {
			rootRunID = s.ID
		}
	}
	if rootRunID == "" {
		t.Fatal("no root run recorded")
	}

	if _, _, err := ReplayRun(hist, fs, emptyRegistry, nil, hist, rootRunID); err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}

	summaries, _ = hist.List()
	for _, s := range summaries {
		if s.FlowName == "leaf" && s.ReplayOfRunID != "" {
			t.Fatalf("leaf sub-run has ReplayOfRunID = %q, want empty — only the top-level replay is marked", s.ReplayOfRunID)
		}
	}
	rootReplayCount := 0
	for _, s := range summaries {
		if s.FlowName == "root" && s.ReplayOfRunID == rootRunID {
			rootReplayCount++
		}
	}
	if rootReplayCount != 1 {
		t.Fatalf("root replay count = %d, want exactly 1", rootReplayCount)
	}
}
