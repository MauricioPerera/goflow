// Package text provides goflow's "Text" catalog piece: common string
// manipulation (split/join/replace/trim/case) as pure, dependency-free
// actions — no network, no auth, same "simplest possible catalog citizen"
// spirit as pkg/pieces/json.
package text

import (
	"fmt"
	"strings"
	"unicode"

	"goflow/pkg/piece"
)

const PieceName = "text"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Text",
		Actions: map[string]piece.Action{
			"split":   splitAction(),
			"join":    joinAction(),
			"replace": replaceAction(),
			"trim":    trimAction(),
			"case":    caseAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func splitAction() piece.Action {
	return piece.Action{
		Name: "split", DisplayName: "Split",
		Run: func(ctx piece.ActionContext) (any, error) {
			s, ok := ctx.Input["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: text (string)")
			}
			sep, ok := ctx.Input["separator"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: separator (string)")
			}
			return map[string]any{"parts": strings.Split(s, sep)}, nil
		},
	}
}

func joinAction() piece.Action {
	return piece.Action{
		Name: "join", DisplayName: "Join",
		Run: func(ctx piece.ActionContext) (any, error) {
			raw, ok := ctx.Input["parts"].([]any)
			if !ok {
				return nil, fmt.Errorf("missing required input: parts ([]any)")
			}
			sep, ok := ctx.Input["separator"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: separator (string)")
			}
			parts := make([]string, len(raw))
			for i, v := range raw {
				if s, ok := v.(string); ok {
					parts[i] = s
				} else {
					parts[i] = fmt.Sprint(v)
				}
			}
			return map[string]any{"text": strings.Join(parts, sep)}, nil
		},
	}
}

func replaceAction() piece.Action {
	return piece.Action{
		Name: "replace", DisplayName: "Replace",
		Run: func(ctx piece.ActionContext) (any, error) {
			s, ok := ctx.Input["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: text (string)")
			}
			old, ok := ctx.Input["old"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: old (string)")
			}
			new_, ok := ctx.Input["new"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: new (string)")
			}
			all := true
			if v, ok := ctx.Input["all"].(bool); ok {
				all = v
			}
			n := 1
			if all {
				n = -1
			}
			return map[string]any{"text": strings.Replace(s, old, new_, n)}, nil
		},
	}
}

func trimAction() piece.Action {
	return piece.Action{
		Name: "trim", DisplayName: "Trim",
		Run: func(ctx piece.ActionContext) (any, error) {
			s, ok := ctx.Input["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: text (string)")
			}
			cutset, _ := ctx.Input["cutset"].(string)
			if cutset == "" {
				return map[string]any{"text": strings.TrimSpace(s)}, nil
			}
			return map[string]any{"text": strings.Trim(s, cutset)}, nil
		},
	}
}

func caseAction() piece.Action {
	return piece.Action{
		Name: "case", DisplayName: "Change Case",
		Dropdowns: map[string]piece.DropdownProperty{
			"mode": {
				LoadOptions: func(propsValue map[string]any, ctx piece.PropertyContext) (piece.DropdownState, error) {
					return piece.DropdownState{Options: []piece.DropdownOption{
						{Label: "UPPERCASE", Value: "upper"},
						{Label: "lowercase", Value: "lower"},
						{Label: "Title Case", Value: "title"},
					}}, nil
				},
			},
		},
		Run: func(ctx piece.ActionContext) (any, error) {
			s, ok := ctx.Input["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: text (string)")
			}
			mode, ok := ctx.Input["mode"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: mode (string)")
			}
			switch mode {
			case "upper":
				return map[string]any{"text": strings.ToUpper(s)}, nil
			case "lower":
				return map[string]any{"text": strings.ToLower(s)}, nil
			case "title":
				return map[string]any{"text": titleCase(s)}, nil
			default:
				return nil, fmt.Errorf(`invalid mode %q: must be "upper", "lower", or "title"`, mode)
			}
		},
	}
}

// titleCase capitalizes the first rune of each whitespace-separated word.
// strings.Title is deprecated (Unicode-unaware for many scripts and
// doesn't handle apostrophes/punctuation specially) — this is a
// deliberately simple, dependency-free replacement, not a full Unicode
// title-casing implementation.
func titleCase(s string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		r := []rune(f)
		r[0] = unicode.ToUpper(r[0])
		fields[i] = string(r)
	}
	return strings.Join(fields, " ")
}
