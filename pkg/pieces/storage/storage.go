// Package storage provides goflow's "Storage" catalog piece: write bytes
// through ctx.Files, the first catalog piece to touch FileWriter at all.
// Write-only, matching FileWriter's own contract (see piece.FileWriter's
// doc comment) — nothing reads a file back through the engine.
package storage

import (
	"encoding/base64"
	"fmt"

	"goflow/pkg/piece"
)

const PieceName = "storage"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Storage",
		Actions: map[string]piece.Action{
			"write": writeAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func writeAction() piece.Action {
	return piece.Action{
		Name: "write", DisplayName: "Write File",
		Dropdowns: map[string]piece.DropdownProperty{
			"format": {
				LoadOptions: func(propsValue map[string]any, ctx piece.PropertyContext) (piece.DropdownState, error) {
					return piece.DropdownState{Options: []piece.DropdownOption{
						{Label: "Text", Value: "text"},
						{Label: "Base64", Value: "base64"},
					}}, nil
				},
			},
		},
		Run: func(ctx piece.ActionContext) (any, error) {
			fileName, ok := ctx.Input["fileName"].(string)
			if !ok || fileName == "" {
				return nil, fmt.Errorf("missing required input: fileName (string)")
			}
			content, ok := ctx.Input["content"].(string)
			if !ok || content == "" {
				return nil, fmt.Errorf("missing required input: content (string)")
			}
			format, _ := ctx.Input["format"].(string)

			var data []byte
			switch format {
			case "text":
				data = []byte(content)
			case "base64":
				decoded, err := base64.StdEncoding.DecodeString(content)
				if err != nil {
					return nil, fmt.Errorf("invalid base64 content: %w", err)
				}
				data = decoded
			default:
				return nil, fmt.Errorf(`invalid format %q: must be "text" or "base64"`, format)
			}

			url, err := ctx.Files.Write(fileName, data)
			if err != nil {
				return nil, fmt.Errorf("writing file: %w", err)
			}
			return map[string]any{"fileURL": url}, nil
		},
	}
}
