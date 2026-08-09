// Package delay provides goflow's "Delay" catalog piece: pauses execution
// for a given duration then continues. This is a REAL synchronous
// time.Sleep inside the action's Run — a different mechanism from
// activepieces' real "Delay" piece, which pauses the whole FLOW via
// context.run.waitForWaitpoint (see the pause/resume tests in
// pkg/engine/engine_test.go) so the worker can free up the goroutine/process
// while waiting, potentially for days. A sleeping goroutine here is fine for
// short waits and keeps this piece trivially simple; for a wait long enough
// that blocking a goroutine would matter, use WaitForWaitpoint directly in
// your own piece instead (see the goflow-piece-authoring skill).
package delay

import (
	"fmt"
	"time"

	"goflow/pkg/piece"
)

const PieceName = "delay"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Delay",
		Actions: map[string]piece.Action{
			"wait": waitAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func waitAction() piece.Action {
	return piece.Action{
		Name: "wait", DisplayName: "Wait",
		Run: func(ctx piece.ActionContext) (any, error) {
			ms, ok := toMilliseconds(ctx.Input["milliseconds"])
			if !ok {
				return nil, fmt.Errorf("missing or invalid required input: milliseconds (number)")
			}
			if ms < 0 {
				return nil, fmt.Errorf("milliseconds must be >= 0, got %v", ms)
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
			return map[string]any{"waitedMilliseconds": ms}, nil
		},
	}
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
