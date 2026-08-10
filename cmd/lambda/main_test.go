package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"goflow/pkg/model"
)

// TestMain runs setup() once (the exact cold-start path main() itself
// runs) before any test — main() is never invoked by `go test`, so eng/
// flow would otherwise stay unset.
func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestHandleRequest_ValidEvent_RunsEmbeddedFlow(t *testing.T) {
	executeTrigger = false
	state, err := handleRequest(context.Background(), json.RawMessage(`{"n": 21}`))
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("Verdict.Status = %v, want SUCCEEDED", state.Verdict.Status)
	}
	double, ok := state.Steps["double"]
	if !ok {
		t.Fatalf("Steps = %+v, missing \"double\"", state.Steps)
	}
	output, ok := double.Output.(map[string]any)
	if !ok {
		t.Fatalf("double.Output = %#v (%T), want a map", double.Output, double.Output)
	}
	doubled := fmt.Sprintf("%v", output["doubled"])
	if doubled != "42" {
		t.Fatalf("double.Output[\"doubled\"] = %v (%T), want 42", output["doubled"], output["doubled"])
	}
}

func TestHandleRequest_EmptyEvent_TriggerIsNil(t *testing.T) {
	executeTrigger = false
	state, err := handleRequest(context.Background(), json.RawMessage(``))
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	trigger, ok := state.Steps["trigger_1"]
	if !ok {
		t.Fatalf("Steps = %+v, missing \"trigger_1\"", state.Steps)
	}
	if trigger.Output != nil {
		t.Fatalf("trigger_1.Output = %#v, want nil for an empty event", trigger.Output)
	}
}

func TestHandleRequest_MalformedEventJSON_ReturnsError(t *testing.T) {
	executeTrigger = false
	state, err := handleRequest(context.Background(), json.RawMessage(`{not valid json`))
	if err == nil {
		t.Fatalf("err = nil, state = %+v, want an error for malformed event JSON", state)
	}
}

func TestSetup_EmbeddedFlowJSONParsesToEmptyTriggerCodeChain(t *testing.T) {
	// setup() already ran in TestMain against the real embedded flow.json —
	// this just asserts what it actually parsed into, so a future edit to
	// flow.json that breaks the EMPTY-trigger/CODE-chain shape this demo
	// relies on fails a test instead of only failing silently at deploy.
	if flow.Trigger == nil || flow.Trigger.Type != model.TriggerEmpty {
		t.Fatalf("flow.Trigger = %+v, want a non-nil EMPTY trigger", flow.Trigger)
	}
	if flow.Trigger.NextAction == nil || flow.Trigger.NextAction.Type != model.ActionCode {
		t.Fatalf("flow.Trigger.NextAction = %+v, want a CODE action", flow.Trigger.NextAction)
	}
}
