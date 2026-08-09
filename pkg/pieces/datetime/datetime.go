// Package datetime provides goflow's "Datetime" catalog piece: parse,
// format, shift, and diff timestamps. All timestamps in/out are RFC3339
// strings unless an action's own doc says otherwise — pure logic, no
// network, no auth.
package datetime

import (
	"fmt"
	"time"

	"goflow/pkg/piece"
)

const PieceName = "datetime"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Datetime",
		Actions: map[string]piece.Action{
			"now":    nowAction(),
			"parse":  parseAction(),
			"format": formatAction(),
			"add":    addAction(),
			"diff":   diffAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func nowAction() piece.Action {
	return piece.Action{
		Name: "now", DisplayName: "Now",
		Run: func(ctx piece.ActionContext) (any, error) {
			return map[string]any{"iso": time.Now().UTC().Format(time.RFC3339)}, nil
		},
	}
}

func parseAction() piece.Action {
	return piece.Action{
		Name: "parse", DisplayName: "Parse",
		Run: func(ctx piece.ActionContext) (any, error) {
			text, ok := ctx.Input["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: text (string)")
			}
			layout, ok := ctx.Input["layout"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: layout (string)")
			}
			t, err := time.Parse(layout, text)
			if err != nil {
				return nil, fmt.Errorf("parsing %q with layout %q: %w", text, layout, err)
			}
			return map[string]any{
				"iso":    t.Format(time.RFC3339),
				"unixMs": t.UnixMilli(),
			}, nil
		},
	}
}

func formatAction() piece.Action {
	return piece.Action{
		Name: "format", DisplayName: "Format",
		Run: func(ctx piece.ActionContext) (any, error) {
			t, err := parseISO(ctx.Input["iso"])
			if err != nil {
				return nil, err
			}
			layout, ok := ctx.Input["layout"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: layout (string)")
			}
			return map[string]any{"text": t.Format(layout)}, nil
		},
	}
}

func addAction() piece.Action {
	return piece.Action{
		Name: "add", DisplayName: "Add",
		Run: func(ctx piece.ActionContext) (any, error) {
			t, err := parseISO(ctx.Input["iso"])
			if err != nil {
				return nil, err
			}
			amountMs, ok := toMilliseconds(ctx.Input["amountMs"])
			if !ok {
				return nil, fmt.Errorf("missing or invalid required input: amountMs (number)")
			}
			shifted := t.Add(time.Duration(amountMs) * time.Millisecond)
			return map[string]any{"iso": shifted.Format(time.RFC3339)}, nil
		},
	}
}

func diffAction() piece.Action {
	return piece.Action{
		Name: "diff", DisplayName: "Diff",
		Run: func(ctx piece.ActionContext) (any, error) {
			a, err := parseISO(ctx.Input["a"])
			if err != nil {
				return nil, fmt.Errorf("input \"a\": %w", err)
			}
			b, err := parseISO(ctx.Input["b"])
			if err != nil {
				return nil, fmt.Errorf("input \"b\": %w", err)
			}
			return map[string]any{"diffMs": a.Sub(b).Milliseconds()}, nil
		},
	}
}

func parseISO(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}, fmt.Errorf("missing required input: a valid RFC3339 timestamp string")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid RFC3339 timestamp %q: %w", s, err)
	}
	return t, nil
}

func toMilliseconds(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
