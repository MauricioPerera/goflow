package expr_test

import (
	"testing"
	"time"

	"goflow/pkg/expr"
)

// TestEval_TimeoutDoesNotBoundNativeBuiltInWallClockTime confirms a real,
// more serious-than-it-first-looked limitation found while fuzzing this
// package. goja.Runtime.Interrupt's own doc comment says it "does not
// interrupt native Go functions (which includes all built-ins)" — the
// naive reading is "a native call like String.prototype.repeat just isn't
// covered by the timeout." What a direct measurement actually shows: the
// native call still runs to FULL completion, paying its full CPU and
// memory cost, however long that naturally takes — the pending interrupt
// is only noticed and converted into an error AFTER the call returns,
// discarding the (already fully computed) result. So DefaultTimeout does
// NOT bound wall-clock execution time at all for an expression dominated
// by a slow/huge native built-in call — it only determines whether the
// caller receives the result or a timeout error once the underlying work
// is already done. A 100M-char repeat still takes ~190-200ms end to end
// whether the timeout is 10ms, 1ms, or absent — measured directly (not
// guessed) via a throwaway diagnostic comparing "no interrupt" vs.
// "interrupt fired near-immediately" for the same native call.
//
// Practical consequence for this project: DefaultTimeout (shared
// mechanism across this package, pkg/jspiece, and pkg/sandbox) protects
// against runaway INTERPRETED execution (loops, JS function calls) but
// provides no bound at all on the wall-clock cost of a single expensive
// native built-in call — an adversarial expression built entirely around
// one huge native allocation pays its full cost regardless of the
// timeout.
func TestEval_TimeoutDoesNotBoundNativeBuiltInWallClockTime(t *testing.T) {
	original := expr.DefaultTimeout
	expr.DefaultTimeout = 1 * time.Millisecond // as aggressive as a timeout can get
	defer func() { expr.DefaultTimeout = original }()

	// 100M chars naturally takes on the order of 150-250ms (machine-
	// dependent) — comfortably, unmissably longer than the 1ms timeout.
	start := time.Now()
	_, err := expr.Eval("'x'.repeat(100000000)", expr.Scope{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Eval() error = nil, want a timeout error — the 1ms timeout should have elapsed long before the native call finished")
	}
	// The point being proven: it did NOT come back quickly. If the
	// interrupt actually bounded wall-clock time, this would return in
	// ~1ms; instead it takes as long as the native call naturally does.
	if elapsed < 50*time.Millisecond {
		t.Fatalf("Eval() returned in %v, want it to take roughly as long as the uninterrupted native call — a fast return here would mean the timeout DOES bound native execution, contradicting this test's premise", elapsed)
	}
	t.Logf("Eval() with a %v timeout still took %v to return (as an error) — confirms the native call ran to completion regardless of the timeout", expr.DefaultTimeout, elapsed)
}
