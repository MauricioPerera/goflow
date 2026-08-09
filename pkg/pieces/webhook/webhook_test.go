package webhook_test

import (
	"fmt"
	"testing"

	"goflow/pkg/piece"
	"goflow/pkg/pieces/webhook"
)

func TestWebhook_CatchHookPassesPayloadThrough(t *testing.T) {
	p := webhook.New()
	trig, ok := p.Triggers["catch_hook"]
	if !ok {
		t.Fatal("catch_hook trigger not registered")
	}

	payload := map[string]any{"event": "order.created", "id": int64(42)}
	items, err := trig.Run(piece.TriggerContext{Payload: payload})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %#v, want exactly 1", items)
	}
	got, ok := items[0].(map[string]any)
	if !ok || got["event"] != "order.created" || got["id"] != int64(42) {
		t.Fatalf("items[0] = %#v, want the raw payload passed through untouched", items[0])
	}
}

func TestWebhook_CatchHookPassesNonMapPayloadsThrough(t *testing.T) {
	p := webhook.New()
	trig := p.Triggers["catch_hook"]

	cases := []any{nil, "raw-string-body", []any{"a", "b"}, float64(42)}
	for _, payload := range cases {
		items, err := trig.Run(piece.TriggerContext{Payload: payload})
		if err != nil {
			t.Fatalf("Run(%#v) error = %v", payload, err)
		}
		if len(items) != 1 {
			t.Fatalf("Run(%#v) items = %#v, want exactly 1", payload, items)
		}
		got := items[0]
		if fmt.Sprint(got) != fmt.Sprint(payload) {
			t.Fatalf("items[0] = %#v, want payload %#v passed through untouched, regardless of shape", got, payload)
		}
	}
}
