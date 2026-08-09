// Package webhook provides goflow's "Webhook" catalog piece: a generic,
// pass-through real (non-EMPTY) trigger — the reusable version of the
// "hub"/"catch_any_event" fixtures built ad hoc across
// pkg/engine/engine_test.go's real-trigger and combined-events tests. Use it
// as the entry point for a flow that should react to whatever arrives at
// its webhook, then dispatch on event shape with a ROUTER (see
// TestFlow_CombinedEvents_OneTriggerDispatchesByEventType for the pattern).
package webhook

import "goflow/pkg/piece"

const PieceName = "webhook"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Webhook",
		Triggers: map[string]piece.Trigger{
			"catch_hook": catchHookTrigger(),
		},
	}
	piece.MustValidate(p)
	return p
}

func catchHookTrigger() piece.Trigger {
	return piece.Trigger{
		Name: "catch_hook", DisplayName: "Catch Webhook",
		Run: func(ctx piece.TriggerContext) ([]any, error) {
			return []any{ctx.Payload}, nil
		},
	}
}
