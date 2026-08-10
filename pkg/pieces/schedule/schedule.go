// Package schedule provides goflow's "Schedule" catalog piece: a real
// (non-EMPTY) POLLING-style trigger that fires at most once per configured
// interval. This piece alone does nothing on a timer by itself — Trigger's
// own doc comment in pkg/piece is explicit that "what makes a trigger a
// polling trigger is entirely a convention of how it's invoked", and an
// external caller is responsible for calling Run() repeatedly. That caller
// is pkg/scheduler, which is what actually makes a flow using this piece
// fire on its own inside the real server; see that package for the other
// half of this feature.
package schedule

import (
	"fmt"
	"time"

	"goflow/pkg/piece"
)

const PieceName = "schedule"

// TriggerName is the one trigger this piece exposes — exported so
// pkg/scheduler can identify a schedule-triggered flow (FlowTrigger.PieceName
// == PieceName && FlowTrigger.TriggerName == TriggerName) without importing
// anything beyond this piece's own package.
const TriggerName = "interval"

// storeKey is the single key this trigger reads/writes on whatever Store its
// caller supplies — always via a piece.ScopedStore keyed per flow in
// production (pkg/scheduler does this), so two different schedule-triggered
// flows never clash over this same literal key, the same reasoning
// ScopedStore's own doc comment gives for a "last_id"-style cursor.
const storeKey = "last_fired_at"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Schedule",
		Triggers: map[string]piece.Trigger{
			TriggerName: {
				Name: TriggerName, DisplayName: "Interval",
				Run: Run,
			},
		},
	}
	piece.MustValidate(p)
	return p
}

// Run is the trigger's Run function, exported directly (not only reachable
// via New().Triggers[TriggerName].Run) so pkg/scheduler can call it without
// building a whole Piece/Registry just to get one function — the same
// "export what a real caller outside this package actually needs" reasoning
// pkg/flowstore's Run/RunWithCredentials already follow for the run path.
//
// ctx.Input must carry intervalSeconds (a positive number — the flow
// author's configured firing interval). ctx.Store is required: a polling
// trigger with nowhere to remember when it last fired can't know whether
// it's due, so a nil Store is a clear error here rather than silently
// firing every single call.
//
// The very first call for a given Store/key pair (no cursor yet) fires
// immediately and seeds the cursor — matching the shape
// TestPollingTrigger_DiscoversNewItemsAcrossPolls already establishes for
// polling triggers in general (existing data counts as "new" on the first
// poll), rather than making a freshly-created schedule wait a full interval
// before its first run.
func Run(ctx piece.TriggerContext) ([]any, error) {
	seconds, err := intervalSeconds(ctx.Input)
	if err != nil {
		return nil, err
	}
	if ctx.Store == nil {
		return nil, fmt.Errorf("schedule.interval: no Store configured — a polling trigger requires one to track when it last fired")
	}

	now := time.Now()
	lastVal, ok := ctx.Store.Get(storeKey)
	if !ok {
		ctx.Store.Put(storeKey, now)
		return []any{fireEvent(now)}, nil
	}

	last, ok := lastVal.(time.Time)
	if !ok {
		return nil, fmt.Errorf("schedule.interval: stored cursor is not a time.Time (got %T) — Store is corrupted or shared with an incompatible trigger", lastVal)
	}
	if now.Sub(last) < time.Duration(seconds)*time.Second {
		return nil, nil // not due yet — same "nothing new" shape a polling trigger returns when its data source has no fresh items
	}

	ctx.Store.Put(storeKey, now)
	return []any{fireEvent(now)}, nil
}

// fireEvent is the payload a fired schedule hands to the flow as its trigger
// output — deliberately minimal (there is no external event data, unlike a
// real polling trigger like a Slack channel), but non-empty so a flow has
// something to reference via {{ trigger_1.output.firedAt }} if it wants to.
func fireEvent(t time.Time) map[string]any {
	return map[string]any{"firedAt": t.Format(time.RFC3339)}
}

// intervalSeconds accepts int64/int/float64 for the same reason
// pkg/pieces/delay's toMilliseconds does: a JSON-defined flow's Input always
// decodes numbers as float64, but a Go-literal FlowVersion (or one built by
// another piece's Output) may hand this an int64 or int instead.
func intervalSeconds(input map[string]any) (int64, error) {
	raw, ok := input["intervalSeconds"]
	if !ok {
		return 0, fmt.Errorf("missing required input: intervalSeconds (number)")
	}
	var seconds int64
	switch v := raw.(type) {
	case int64:
		seconds = v
	case int:
		seconds = int64(v)
	case float64:
		seconds = int64(v)
	default:
		return 0, fmt.Errorf("missing required input: intervalSeconds (number)")
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("intervalSeconds must be > 0, got %d", seconds)
	}
	return seconds, nil
}
