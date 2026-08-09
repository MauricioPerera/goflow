// Package json provides goflow's "JSON" catalog piece: parse a JSON string
// into a value, or stringify a value into JSON text. Pure logic, no
// network, no auth — the simplest possible catalog citizen, and a good
// first piece to read if you're learning the SDK from pkg/pieces rather
// than the goflow-piece-authoring skill.
package json

import (
	"encoding/json"
	"fmt"

	"goflow/pkg/piece"
)

const PieceName = "json"

// New builds and validates the piece — see the goflow-piece-authoring
// skill for why every catalog piece does this (piece.MustValidate panics
// immediately on a structural mistake, at package-init time, instead of
// letting it surface later as a runtime panic mid-flow).
func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "JSON",
		Actions: map[string]piece.Action{
			"parse":     parseAction(),
			"stringify": stringifyAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func parseAction() piece.Action {
	return piece.Action{
		Name: "parse", DisplayName: "Parse JSON",
		Run: func(ctx piece.ActionContext) (any, error) {
			text, ok := ctx.Input["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: text (string)")
			}
			var data any
			if err := json.Unmarshal([]byte(text), &data); err != nil {
				return nil, fmt.Errorf("invalid JSON: %w", err)
			}
			return map[string]any{"data": data}, nil
		},
	}
}

func stringifyAction() piece.Action {
	return piece.Action{
		Name: "stringify", DisplayName: "Stringify JSON",
		Run: func(ctx piece.ActionContext) (any, error) {
			data, ok := ctx.Input["data"]
			if !ok {
				return nil, fmt.Errorf("missing required input: data")
			}
			pretty, _ := ctx.Input["pretty"].(bool)
			var (
				encoded []byte
				err     error
			)
			if pretty {
				encoded, err = json.MarshalIndent(data, "", "  ")
			} else {
				encoded, err = json.Marshal(data)
			}
			if err != nil {
				return nil, fmt.Errorf("value cannot be encoded as JSON: %w", err)
			}
			return map[string]any{"text": string(encoded)}, nil
		},
	}
}
