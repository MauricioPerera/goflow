package approval_test

import (
	"testing"

	"goflow/pkg/engine"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	approvalpiece "goflow/pkg/pieces/approval"
)

func TestApproval_Resume_InvalidPayloadFailsClearly(t *testing.T) {
	p := approvalpiece.New()
	act := p.Actions["request"]

	_, err := act.Run(piece.ActionContext{
		ExecutionType: model.ExecutionResume,
		ResumePayload: "not-a-map",
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection for a non-map resume payload")
	}
}

func registryWith(p piece.Piece) *piece.Registry {
	r := piece.NewRegistry()
	r.Register(p)
	return r
}

func requestAction() *model.FlowAction {
	return &model.FlowAction{
		Name: "approve", DisplayName: "Approve", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: approvalpiece.PieceName, ActionName: "request", Input: map[string]any{
			"message": "please approve this",
		}},
	}
}

func flowVersion(action *model.FlowAction) *model.FlowVersion {
	return &model.FlowVersion{ID: "fv-approval", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: action,
	}}
}

func TestApproval_BeginPauses(t *testing.T) {
	action := requestAction()
	fv := flowVersion(action)
	e := engine.New(registryWith(approvalpiece.New()))

	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunPaused {
		t.Fatalf("verdict = %+v, want PAUSED", state.Verdict)
	}
	out := state.Steps["approve"].Output.(map[string]any)
	if out["status"] != "pending" || out["message"] != "please approve this" {
		t.Fatalf("out = %+v", out)
	}
}

func TestApproval_ResumeApproved(t *testing.T) {
	action := requestAction()
	fv := flowVersion(action)
	e := engine.New(registryWith(approvalpiece.New()))

	begun := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	resumed := e.ExecuteResume(fv, engine.ResumeInput{
		PriorState:    begun,
		ResumePayload: map[string]any{"approved": true, "comment": "looks good"},
	})

	if resumed.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED", resumed.Verdict)
	}
	out := resumed.Steps["approve"].Output.(map[string]any)
	if out["approved"] != true || out["comment"] != "looks good" {
		t.Fatalf("out = %+v", out)
	}
}

func TestApproval_ResumeRejected(t *testing.T) {
	action := requestAction()
	fv := flowVersion(action)
	e := engine.New(registryWith(approvalpiece.New()))

	begun := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	resumed := e.ExecuteResume(fv, engine.ResumeInput{
		PriorState:    begun,
		ResumePayload: map[string]any{"approved": false},
	})

	if resumed.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED (rejection is a normal outcome, not a failure)", resumed.Verdict)
	}
	out := resumed.Steps["approve"].Output.(map[string]any)
	if out["approved"] != false {
		t.Fatalf("out = %+v, want approved=false", out)
	}
}

func TestApproval_StandaloneActionRunFailsClearly(t *testing.T) {
	action := requestAction()
	e := engine.New(registryWith(approvalpiece.New()))

	state := e.ExecuteActionRun(action, engine.ActionRunInput{})

	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED — a standalone action run has no flow to resume in", state.Verdict)
	}
}
