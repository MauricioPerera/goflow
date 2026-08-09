package model_test

import (
	"encoding/json"
	"testing"

	"goflow/pkg/model"
)

func TestParseFlowVersion_SimpleTwoStepFlow(t *testing.T) {
	data := []byte(`{
		"id": "fv-1",
		"trigger": {
			"name": "trigger_1",
			"displayName": "Trigger",
			"type": "EMPTY",
			"nextAction": {
				"name": "step_1",
				"displayName": "Step 1",
				"type": "CODE",
				"code": {
					"source": "(params) => params",
					"input": {"key": 3}
				},
				"nextAction": {
					"name": "step_2",
					"displayName": "Step 2",
					"type": "CODE",
					"code": {
						"source": "(params) => params",
						"input": {"doubled": "{{ step_1.output.key * 2 }}"}
					}
				}
			}
		}
	}`)

	fv, err := model.ParseFlowVersion(data)
	if err != nil {
		t.Fatalf("ParseFlowVersion() error = %v", err)
	}
	if fv.ID != "fv-1" {
		t.Fatalf("ID = %q", fv.ID)
	}
	if fv.Trigger.Type != model.TriggerEmpty {
		t.Fatalf("Trigger.Type = %q", fv.Trigger.Type)
	}
	step1 := fv.Trigger.NextAction
	if step1 == nil || step1.Name != "step_1" || step1.Type != model.ActionCode {
		t.Fatalf("step1 = %+v", step1)
	}
	if step1.Code == nil || step1.Code.Source != "(params) => params" {
		t.Fatalf("step1.Code = %+v", step1.Code)
	}
	if step1.Code.Input["key"] != float64(3) {
		t.Fatalf("step1.Code.Input[key] = %#v, want float64(3) (JSON numbers decode as float64)", step1.Code.Input["key"])
	}
	step2 := step1.NextAction
	if step2 == nil || step2.Name != "step_2" {
		t.Fatalf("step2 = %+v", step2)
	}
	if step2.Code.Input["doubled"] != "{{ step_1.output.key * 2 }}" {
		t.Fatalf("step2.Code.Input[doubled] = %#v", step2.Code.Input["doubled"])
	}
}

func TestParseFlowVersion_PieceTriggerFields(t *testing.T) {
	data := []byte(`{
		"id": "fv-2",
		"trigger": {
			"name": "trigger_1",
			"displayName": "Catch Webhook",
			"type": "PIECE_TRIGGER",
			"pieceName": "webhook",
			"triggerName": "catch_hook",
			"input": {}
		}
	}`)

	fv, err := model.ParseFlowVersion(data)
	if err != nil {
		t.Fatalf("ParseFlowVersion() error = %v", err)
	}
	if fv.Trigger.Type != model.TriggerPiece {
		t.Fatalf("Trigger.Type = %q", fv.Trigger.Type)
	}
	if fv.Trigger.PieceName != "webhook" || fv.Trigger.TriggerName != "catch_hook" {
		t.Fatalf("Trigger = %+v", fv.Trigger)
	}
}

func TestParseFlowVersion_RouterAndLoopNesting(t *testing.T) {
	data := []byte(`{
		"id": "fv-3",
		"trigger": {
			"name": "trigger_1",
			"displayName": "Trigger",
			"type": "EMPTY",
			"nextAction": {
				"name": "route",
				"displayName": "Route",
				"type": "ROUTER",
				"router": {
					"executionType": "EXECUTE_FIRST_MATCH",
					"branches": [
						{
							"name": "branch_a",
							"type": "CONDITION",
							"conditions": [[
								{"operator": "TEXT_EXACTLY_MATCHES", "firstValue": "{{ trigger_1.output.kind }}", "secondValue": "a", "caseSensitive": true}
							]]
						},
						{"name": "fallback", "type": "FALLBACK"}
					],
					"children": [
						{
							"name": "loop_body",
							"displayName": "Loop",
							"type": "LOOP_ON_ITEMS",
							"loop": {
								"items": "{{ [1, 2, 3] }}",
								"firstLoopAction": {
									"name": "inner",
									"displayName": "Inner",
									"type": "CODE",
									"code": {"source": "(params) => params"}
								}
							}
						},
						{
							"name": "fallback_step",
							"displayName": "Fallback Step",
							"type": "CODE",
							"code": {"source": "(params) => params"}
						}
					]
				}
			}
		}
	}`)

	fv, err := model.ParseFlowVersion(data)
	if err != nil {
		t.Fatalf("ParseFlowVersion() error = %v", err)
	}
	route := fv.Trigger.NextAction
	if route.Router == nil {
		t.Fatal("route.Router is nil")
	}
	if len(route.Router.Branches) != 2 || len(route.Router.Children) != 2 {
		t.Fatalf("Router = %+v", route.Router)
	}
	if route.Router.Branches[0].Conditions[0][0].SecondValue != "a" {
		t.Fatalf("Branches[0] = %+v", route.Router.Branches[0])
	}
	loopChild := route.Router.Children[0]
	if loopChild.Type != model.ActionLoopOnItems || loopChild.Loop == nil {
		t.Fatalf("loopChild = %+v", loopChild)
	}
	if loopChild.Loop.FirstLoopAction == nil || loopChild.Loop.FirstLoopAction.Name != "inner" {
		t.Fatalf("Loop.FirstLoopAction = %+v", loopChild.Loop.FirstLoopAction)
	}
}

func TestParseFlowVersion_MalformedJSONFailsClearly(t *testing.T) {
	_, err := model.ParseFlowVersion([]byte(`{not valid json`))
	if err == nil {
		t.Fatal("ParseFlowVersion() error = nil, want a parse failure")
	}
}

func TestFlowVersion_MarshalUnmarshalRoundTrip(t *testing.T) {
	original := &model.FlowVersion{
		ID: "fv-roundtrip", FlowID: "flow-1", DisplayName: "Round Trip",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "step_1", DisplayName: "Step 1", Type: model.ActionPiece,
				Error: &model.ErrorHandling{RetryOnFailure: true},
				Piece: &model.PieceSettings{
					PieceName: "http", ActionName: "request",
					Input: map[string]any{"url": "https://example.com", "failOnErrorStatus": true},
				},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parsed, err := model.ParseFlowVersion(data)
	if err != nil {
		t.Fatalf("ParseFlowVersion: %v", err)
	}

	if parsed.ID != original.ID || parsed.Trigger.NextAction.Name != "step_1" {
		t.Fatalf("parsed = %+v", parsed)
	}
	if parsed.Trigger.NextAction.Error.RetryOnFailure != true {
		t.Fatalf("Error.RetryOnFailure not preserved: %+v", parsed.Trigger.NextAction.Error)
	}
	if parsed.Trigger.NextAction.Piece.Input["url"] != "https://example.com" {
		t.Fatalf("Piece.Input not preserved: %+v", parsed.Trigger.NextAction.Piece.Input)
	}
}
