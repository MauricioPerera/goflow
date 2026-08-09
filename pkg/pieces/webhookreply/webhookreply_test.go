package webhookreply_test

import (
	"testing"

	"goflow/pkg/engine"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	webhookreplypiece "goflow/pkg/pieces/webhookreply"
)

func TestWebhookReply_Respond_CallsHookWithBuiltResponse(t *testing.T) {
	p := webhookreplypiece.New()
	act := p.Actions["respond"]

	var got *model.WebhookResponse
	out, err := act.Run(piece.ActionContext{
		Input: map[string]any{
			"status":  int64(201),
			"body":    map[string]any{"ok": true},
			"headers": map[string]any{"X-Trace": "abc", "X-Ignored": 5},
		},
		Run: piece.RunHooks{
			Respond: func(resp *model.WebhookResponse) { got = resp },
			Stop:    func(resp *model.WebhookResponse) {},
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got == nil {
		t.Fatal("Respond hook was never called")
	}
	if got.Status != 201 {
		t.Fatalf("Status = %d, want 201", got.Status)
	}
	if got.Headers["X-Trace"] != "abc" {
		t.Fatalf("Headers = %+v, want X-Trace=abc", got.Headers)
	}
	if _, ok := got.Headers["X-Ignored"]; ok {
		t.Fatalf("Headers = %+v, want non-string values dropped", got.Headers)
	}
	if out.(map[string]any)["status"] != 201 {
		t.Fatalf("out = %+v", out)
	}
}

func TestWebhookReply_Stop_CallsHookWithBuiltResponse(t *testing.T) {
	p := webhookreplypiece.New()
	act := p.Actions["stop"]

	var got *model.WebhookResponse
	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"status": int64(400), "body": "bad request"},
		Run: piece.RunHooks{
			Stop: func(resp *model.WebhookResponse) { got = resp },
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got == nil || got.Status != 400 || got.Body != "bad request" {
		t.Fatalf("got = %+v", got)
	}
}

func TestWebhookReply_StatusDefaultsTo200(t *testing.T) {
	p := webhookreplypiece.New()
	act := p.Actions["respond"]

	var got *model.WebhookResponse
	act.Run(piece.ActionContext{
		Input: map[string]any{},
		Run:   piece.RunHooks{Respond: func(resp *model.WebhookResponse) { got = resp }},
	})
	if got.Status != 200 {
		t.Fatalf("Status = %d, want 200 when omitted", got.Status)
	}
}

func markerAction(name string) *model.FlowAction {
	return &model.FlowAction{
		Name: name, DisplayName: name, Type: model.ActionCode,
		Code: &model.CodeSettings{Source: "(params) => ({ ran: true })"},
	}
}

func registryWithMarker() *piece.Registry {
	r := piece.NewRegistry()
	r.Register(webhookreplypiece.New())
	return r
}

func TestWebhookReply_Respond_FlowContinuesToNextAction(t *testing.T) {
	next := markerAction("next")
	respond := &model.FlowAction{
		Name: "reply", DisplayName: "Reply", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: webhookreplypiece.PieceName, ActionName: "respond", Input: map[string]any{
			"status": int64(200),
		}},
		NextAction: next,
	}
	fv := &model.FlowVersion{ID: "fv-respond", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty, NextAction: respond,
	}}

	state := engine.New(registryWithMarker()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	if state.RespondedEarly == nil || state.RespondedEarly.Status != 200 {
		t.Fatalf("RespondedEarly = %+v, want a recorded response", state.RespondedEarly)
	}
	if _, ran := state.Steps["next"]; !ran {
		t.Fatal("next step did not run — respond must not halt the flow")
	}
}

func TestWebhookReply_Stop_FlowDoesNotReachNextAction(t *testing.T) {
	next := markerAction("next")
	stop := &model.FlowAction{
		Name: "halt", DisplayName: "Halt", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: webhookreplypiece.PieceName, ActionName: "stop", Input: map[string]any{
			"status": int64(200),
		}},
		NextAction: next,
	}
	fv := &model.FlowVersion{ID: "fv-stop", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty, NextAction: stop,
	}}

	state := engine.New(registryWithMarker()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	if state.Verdict.StopResponse == nil || state.Verdict.StopResponse.Status != 200 {
		t.Fatalf("StopResponse = %+v, want a recorded response", state.Verdict.StopResponse)
	}
	if _, ran := state.Steps["next"]; ran {
		t.Fatal("next step ran — stop must halt the flow before NextAction")
	}
}
