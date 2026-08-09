// Package approval provides goflow's "Approval" catalog piece: pauses a
// flow via ctx.Run.WaitForWaitpoint until resumed with a decision. The
// first catalog piece to actually pause a flow — every prior pause/resume
// test in this project used a hand-rolled fixture piece.
package approval

import (
	"fmt"

	"goflow/pkg/model"
	"goflow/pkg/piece"
)

const PieceName = "approval"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Approval",
		Actions: map[string]piece.Action{
			"request": requestAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func requestAction() piece.Action {
	return piece.Action{
		Name: "request", DisplayName: "Request Approval",
		Run: func(ctx piece.ActionContext) (any, error) {
			if ctx.ExecutionType == model.ExecutionResume {
				resume, ok := ctx.ResumePayload.(map[string]any)
				if !ok {
					return nil, fmt.Errorf(`expected resume payload to be a map with an "approved" bool, got %T`, ctx.ResumePayload)
				}
				approved, _ := resume["approved"].(bool)
				comment, _ := resume["comment"].(string)
				return map[string]any{"approved": approved, "comment": comment}, nil
			}

			message, _ := ctx.Input["message"].(string)
			ctx.Run.WaitForWaitpoint("approval")
			return map[string]any{"status": "pending", "message": message}, nil
		},
	}
}
