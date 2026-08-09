package delay_test

import (
	"testing"
	"time"

	"goflow/pkg/piece"
	"goflow/pkg/pieces/delay"
)

func TestDelay_WaitsAtLeastTheRequestedDuration(t *testing.T) {
	p := delay.New()
	act, ok := p.Actions["wait"]
	if !ok {
		t.Fatal("wait action not registered")
	}

	const ms = 15
	start := time.Now()
	out, err := act.Run(piece.ActionContext{Input: map[string]any{"milliseconds": int64(ms)}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if elapsed < ms*time.Millisecond {
		t.Fatalf("elapsed = %v, want at least %dms", elapsed, ms)
	}
	if out.(map[string]any)["waitedMilliseconds"] != int64(ms) {
		t.Fatalf("out = %#v", out)
	}
}

func TestDelay_ZeroIsValid(t *testing.T) {
	p := delay.New()
	act := p.Actions["wait"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"milliseconds": int64(0)}})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for a zero-length wait", err)
	}
}

func TestDelay_NegativeFailsClearly(t *testing.T) {
	p := delay.New()
	act := p.Actions["wait"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"milliseconds": int64(-5)}})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection for a negative duration")
	}
}

func TestDelay_MissingInputFailsClearly(t *testing.T) {
	p := delay.New()
	act := p.Actions["wait"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{}})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-input error")
	}
}

func TestDelay_NonNumericInputFailsClearly(t *testing.T) {
	p := delay.New()
	act := p.Actions["wait"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"milliseconds": "10"}})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection — a numeric string is not a number, unlike toNumber elsewhere in this project")
	}
}

func TestDelay_AcceptsFloat64AndIntInputs(t *testing.T) {
	p := delay.New()
	act := p.Actions["wait"]

	// float64 is what arrives after a {{ }}-templated JSON round-trip; int is
	// what a Go caller (e.g. a standalone action-run) would naturally pass.
	for _, v := range []any{float64(5), int(5)} {
		out, err := act.Run(piece.ActionContext{Input: map[string]any{"milliseconds": v}})
		if err != nil {
			t.Fatalf("Run(%T) error = %v", v, err)
		}
		if out.(map[string]any)["waitedMilliseconds"] != int64(5) {
			t.Fatalf("Run(%T) out = %#v", v, out)
		}
	}
}
