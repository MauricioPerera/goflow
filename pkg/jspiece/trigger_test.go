package jspiece_test

import (
	"testing"

	"goflow/pkg/jspiece"
	"goflow/pkg/piece"
)

func TestJSTrigger_BasicPayloadPassthrough(t *testing.T) {
	trig := jspiece.NewTrigger(jspiece.TriggerSource{
		Name: "catch", Source: `(ctx) => [ctx.payload]`,
	})
	items, err := trig.Run(piece.TriggerContext{Payload: map[string]any{"event": "order.created"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %#v, want exactly 1", items)
	}
	got := items[0].(map[string]any)
	if got["event"] != "order.created" {
		t.Fatalf("got = %#v", got)
	}
}

func TestJSTrigger_MustReturnAnArray(t *testing.T) {
	trig := jspiece.NewTrigger(jspiece.TriggerSource{
		Name: "bad", Source: `(ctx) => ({ not: "an array" })`,
	})
	_, err := trig.Run(piece.TriggerContext{})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection — the trigger returned an object, not an array")
	}
}

func TestJSTrigger_EmptyArrayIsValid(t *testing.T) {
	trig := jspiece.NewTrigger(jspiece.TriggerSource{
		Name: "empty", Source: `(ctx) => []`,
	})
	items, err := trig.Run(piece.TriggerContext{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %#v, want empty", items)
	}
}

func TestJSTrigger_ThrownExceptionBecomesError(t *testing.T) {
	trig := jspiece.NewTrigger(jspiece.TriggerSource{
		Name: "thrower", Source: `(ctx) => { throw new Error("boom"); }`,
	})
	_, err := trig.Run(piece.TriggerContext{})
	if err == nil {
		t.Fatal("Run() error = nil, want the thrown error surfaced")
	}
}

func TestJSTrigger_StoreGetPutRoundTrip(t *testing.T) {
	trig := jspiece.NewTrigger(jspiece.TriggerSource{
		Name: "counter",
		Source: `(ctx) => {
			const prev = ctx.store.get("count") || 0;
			ctx.store.put("count", prev + 1);
			return [{ count: prev + 1 }];
		}`,
	})
	store := piece.NewMemoryStore()

	first, err := trig.Run(piece.TriggerContext{Store: store})
	if err != nil {
		t.Fatalf("Run() (1st) error = %v", err)
	}
	if first[0].(map[string]any)["count"] != int64(1) {
		t.Fatalf("first = %#v, want count=1", first[0])
	}

	second, err := trig.Run(piece.TriggerContext{Store: store})
	if err != nil {
		t.Fatalf("Run() (2nd) error = %v", err)
	}
	if second[0].(map[string]any)["count"] != int64(2) {
		t.Fatalf("second = %#v, want count=2 — the store must have persisted across calls", second[0])
	}
}

func TestJSTrigger_StoreUnavailableFailsClearlyNotPanic(t *testing.T) {
	trig := jspiece.NewTrigger(jspiece.TriggerSource{
		Name: "needs_store", Source: `(ctx) => { ctx.store.put("x", 1); return []; }`,
	})
	// Zero-value TriggerContext: Store is nil.
	_, err := trig.Run(piece.TriggerContext{})
	if err == nil {
		t.Fatal("Run() error = nil, want a clean error — ctx.store should be unavailable, not panic")
	}
}

func TestJSTrigger_PollingCursorFiltersOnlyNewItems(t *testing.T) {
	// Mirrors the polling-trigger pattern pkg/engine's own hand-rolled
	// trigger tests use a Store cursor for: each call sees the FULL
	// payload again (a real poll re-fetches everything), filters to what's
	// new relative to a persisted "lastId", and advances the cursor.
	trig := jspiece.NewTrigger(jspiece.TriggerSource{
		Name: "new_orders",
		Source: `(ctx) => {
			const lastId = ctx.store.get("lastId") || 0;
			const items = ctx.payload.filter(item => item.id > lastId);
			if (items.length > 0) {
				const maxId = Math.max(...items.map(item => item.id));
				ctx.store.put("lastId", maxId);
			}
			return items;
		}`,
	})
	store := piece.NewMemoryStore()
	payload := []any{
		map[string]any{"id": int64(1), "amount": int64(10)},
		map[string]any{"id": int64(2), "amount": int64(20)},
		map[string]any{"id": int64(3), "amount": int64(30)},
	}

	items, err := trig.Run(piece.TriggerContext{Payload: payload, Store: store})
	if err != nil {
		t.Fatalf("Run() (1st poll) error = %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("1st poll items = %#v, want all 3 (nothing seen yet)", items)
	}

	// A second poll with the SAME full payload (a real poll re-fetches
	// everything each time) must find nothing new — the cursor advanced.
	items, err = trig.Run(piece.TriggerContext{Payload: payload, Store: store})
	if err != nil {
		t.Fatalf("Run() (2nd poll) error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("2nd poll items = %#v, want empty — cursor should have advanced past all 3", items)
	}
}
