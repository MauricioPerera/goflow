// Package webhookreply provides goflow's "Webhook Reply" catalog piece:
// ctx.Run.Respond and ctx.Run.Stop, the two ways a PIECE action can reply
// synchronously to whatever triggered the flow over a webhook. The first
// catalog piece to touch either hook — every prior respond/stop test in
// this project used a hand-rolled fixture piece. See model.WebhookResponse
// and ActionContext.RunHooks' doc comments for the exact semantics: Respond
// lets the flow keep running afterward, Stop ends the run right there.
package webhookreply

import (
	"goflow/pkg/model"
	"goflow/pkg/piece"
)

const PieceName = "webhook_reply"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Webhook Reply",
		Actions: map[string]piece.Action{
			"respond": respondAction(),
			"stop":    stopAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func respondAction() piece.Action {
	return piece.Action{
		Name: "respond", DisplayName: "Respond",
		Run: func(ctx piece.ActionContext) (any, error) {
			resp := buildResponse(ctx.Input)
			ctx.Run.Respond(resp)
			return map[string]any{"status": resp.Status}, nil
		},
	}
}

func stopAction() piece.Action {
	return piece.Action{
		Name: "stop", DisplayName: "Stop",
		Run: func(ctx piece.ActionContext) (any, error) {
			resp := buildResponse(ctx.Input)
			ctx.Run.Stop(resp)
			return map[string]any{"status": resp.Status}, nil
		},
	}
}

func buildResponse(input map[string]any) *model.WebhookResponse {
	status := 200
	if n, ok := toInt(input["status"]); ok {
		status = n
	}
	headers := map[string]string{}
	if raw, ok := input["headers"].(map[string]any); ok {
		for k, v := range raw {
			if s, ok := v.(string); ok {
				headers[k] = s
			}
		}
	}
	return &model.WebhookResponse{Status: status, Body: input["body"], Headers: headers}
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}
