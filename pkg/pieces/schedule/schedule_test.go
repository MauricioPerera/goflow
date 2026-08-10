package schedule_test

import (
	"testing"
	"time"

	"goflow/pkg/piece"
	"goflow/pkg/pieces/schedule"
)

func TestSchedule_FirstCallFiresImmediatelyAndSeedsCursor(t *testing.T) {
	store := piece.NewMemoryStore()
	items, err := schedule.Run(piece.TriggerContext{
		Input: map[string]any{"intervalSeconds": int64(60)},
		Store: store,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("first call: got %d items, want 1 (fresh cursor should fire immediately)", len(items))
	}
	if _, ok := store.Get("last_fired_at"); !ok {
		t.Fatal("first call did not seed a cursor in Store")
	}
}

func TestSchedule_SecondCallBeforeIntervalElapsedDoesNotFire(t *testing.T) {
	store := piece.NewMemoryStore()
	input := map[string]any{"intervalSeconds": int64(3600)} // an hour — won't elapse during this test

	if _, err := schedule.Run(piece.TriggerContext{Input: input, Store: store}); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	items, err := schedule.Run(piece.TriggerContext{Input: input, Store: store})
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("second call before the interval elapsed: got %d items, want 0", len(items))
	}
}

func TestSchedule_FiresAgainAfterIntervalElapses(t *testing.T) {
	// intervalSeconds is whole seconds (matches the piece's real use case —
	// "poll every N seconds", not sub-second precision), so this test pays
	// real wall-clock time for it — same discipline as pkg/pieces/http's
	// TestHTTP_RespectsRetryAfterHeader, the one other place in this project
	// that deliberately sleeps for real instead of faking the clock.
	store := piece.NewMemoryStore()
	input := map[string]any{"intervalSeconds": int64(1)}

	if _, err := schedule.Run(piece.TriggerContext{Input: input, Store: store}); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	items, err := schedule.Run(piece.TriggerContext{Input: input, Store: store})
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("call after the interval elapsed: got %d items, want 1", len(items))
	}
}

func TestSchedule_TwoFlowsWithScopedStoresDontClash(t *testing.T) {
	// Same shape as ScopedStore's own doc comment warns about: two
	// schedule-triggered flows sharing one underlying Store must not see
	// each other's cursor.
	underlying := piece.NewMemoryStore()
	storeA := &piece.ScopedStore{Underlying: underlying, FlowID: "flow-a"}
	storeB := &piece.ScopedStore{Underlying: underlying, FlowID: "flow-b"}
	input := map[string]any{"intervalSeconds": int64(3600)}

	itemsA1, err := schedule.Run(piece.TriggerContext{Input: input, Store: storeA})
	if err != nil || len(itemsA1) != 1 {
		t.Fatalf("flow A first call: items=%d err=%v, want 1 item, no error", len(itemsA1), err)
	}
	// flow B's first-ever call must still fire (its own cursor is unset),
	// not silently inherit flow A's just-seeded one.
	itemsB1, err := schedule.Run(piece.TriggerContext{Input: input, Store: storeB})
	if err != nil || len(itemsB1) != 1 {
		t.Fatalf("flow B first call: items=%d err=%v, want 1 item, no error (should not inherit flow A's cursor)", len(itemsB1), err)
	}
}

func TestSchedule_MissingIntervalFailsClearly(t *testing.T) {
	_, err := schedule.Run(piece.TriggerContext{Input: map[string]any{}, Store: piece.NewMemoryStore()})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-input error")
	}
}

func TestSchedule_ZeroOrNegativeIntervalFailsClearly(t *testing.T) {
	for _, v := range []any{int64(0), int64(-5), float64(0)} {
		_, err := schedule.Run(piece.TriggerContext{
			Input: map[string]any{"intervalSeconds": v},
			Store: piece.NewMemoryStore(),
		})
		if err == nil {
			t.Fatalf("Run(intervalSeconds=%v) error = nil, want a rejection", v)
		}
	}
}

func TestSchedule_AcceptsFloat64AndIntInputs(t *testing.T) {
	for _, v := range []any{float64(60), int(60)} {
		store := piece.NewMemoryStore()
		items, err := schedule.Run(piece.TriggerContext{
			Input: map[string]any{"intervalSeconds": v},
			Store: store,
		})
		if err != nil {
			t.Fatalf("Run(intervalSeconds=%T) error = %v", v, err)
		}
		if len(items) != 1 {
			t.Fatalf("Run(intervalSeconds=%T): got %d items, want 1", v, len(items))
		}
	}
}

func TestSchedule_NilStoreFailsClearly(t *testing.T) {
	_, err := schedule.Run(piece.TriggerContext{Input: map[string]any{"intervalSeconds": int64(60)}, Store: nil})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection — a polling trigger needs somewhere to track its cursor")
	}
}

func TestSchedule_RegisteredAsRealPiece(t *testing.T) {
	p := schedule.New()
	if p.Name != schedule.PieceName {
		t.Fatalf("p.Name = %q, want %q", p.Name, schedule.PieceName)
	}
	trig, ok := p.Triggers[schedule.TriggerName]
	if !ok {
		t.Fatalf("trigger %q not registered", schedule.TriggerName)
	}
	if trig.Run == nil {
		t.Fatal("trigger Run is nil")
	}
}
