// Command example runs a small end-to-end flow through the engine and prints
// the result as JSON — the Go analogue of the TS extraction's
// example-standalone.ts. Not a CLI meant for real use; a runnable proof the
// pieces (pun intended) fit together.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"goflow/pkg/engine"
	"goflow/pkg/model"
	"goflow/pkg/piece"
)

func main() {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "demo", DisplayName: "Demo",
		Actions: map[string]piece.Action{
			"greet": {
				Name: "greet", DisplayName: "Greet",
				Run: func(ctx piece.ActionContext) (any, error) {
					name, _ := ctx.Input["name"].(string)
					return map[string]any{"greeting": "hello, " + name}, nil
				},
			},
		},
	})

	// trigger (payload: {name}) -> greet (PIECE) -> double (CODE, reads greet's output)
	doubleStep := &model.FlowAction{
		Name: "double", DisplayName: "Double", Type: model.ActionCode,
		Code: &model.CodeSettings{
			Input:  map[string]any{"length": "{{ greet.output.greeting.length }}"},
			Source: `(params) => ({ length: params.length, doubled: params.length * 2 })`,
		},
	}
	greetStep := &model.FlowAction{
		Name: "greet", DisplayName: "Greet", Type: model.ActionPiece,
		Piece:      &model.PieceSettings{PieceName: "demo", ActionName: "greet", Input: map[string]any{"name": "{{ trigger_1.output.name }}"}},
		NextAction: doubleStep,
	}
	fv := &model.FlowVersion{
		ID: "fv-demo",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: greetStep,
		},
	}

	e := engine.New(registry)
	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{"name": "world"}})

	out, err := json.MarshalIndent(map[string]any{"verdict": state.Verdict, "steps": state.Steps}, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(out))

	if state.Verdict.Status != model.FlowRunSucceeded {
		os.Exit(1)
	}
}
