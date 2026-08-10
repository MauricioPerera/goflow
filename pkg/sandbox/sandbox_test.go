package sandbox_test

import (
	"strings"
	"testing"
	"time"

	"goflow/pkg/sandbox"
)

// This file didn't exist before this request — sandbox.Run had never had a
// direct unit test of its own; every prior test of CODE-step behavior went
// through pkg/engine's codeAction() helper indirectly. try/catch is
// standard JS, not a goflow-specific mechanism, so these tests are really
// verifying goja's fidelity as a JS engine (nested scopes, exception
// propagation across function-call boundaries, custom Error subclasses) —
// worth checking directly rather than assuming, same discipline as every
// other "checked before claiming" finding in this project's README.

func TestRun_TryCatchRecoversFromThrow(t *testing.T) {
	source := `(params) => {
		try {
			throw new Error("boom")
		} catch (err) {
			return { recovered: true, message: err.message }
		}
	}`
	out, err := sandbox.Run(source, map[string]any{})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — the try/catch should have handled the throw internally", err)
	}
	m, ok := out.(map[string]any)
	if !ok || m["recovered"] != true || m["message"] != "boom" {
		t.Fatalf("out = %#v, want {recovered:true, message:\"boom\"}", out)
	}
}

func TestRun_NestedTryCatch_InnerHandlesOuterUnreached(t *testing.T) {
	source := `(params) => {
		const attempts = []
		try {
			try {
				throw new Error("inner failure")
			} catch (innerErr) {
				attempts.push("inner caught: " + innerErr.message)
			}
			attempts.push("after inner try — outer try body continues normally")
		} catch (outerErr) {
			attempts.push("outer caught: " + outerErr.message)
		}
		return { attempts }
	}`
	out, err := sandbox.Run(source, map[string]any{})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	attempts, _ := out.(map[string]any)["attempts"].([]any)
	want := []string{
		"inner caught: inner failure",
		"after inner try — outer try body continues normally",
	}
	if len(attempts) != len(want) {
		t.Fatalf("attempts = %#v, want %d entries", attempts, len(want))
	}
	for i, w := range want {
		if attempts[i] != w {
			t.Fatalf("attempts[%d] = %q, want %q", i, attempts[i], w)
		}
	}
}

func TestRun_NestedTryCatch_RethrowIsCaughtByOuter(t *testing.T) {
	source := `(params) => {
		try {
			try {
				throw new Error("original")
			} catch (innerErr) {
				throw new Error("wrapped: " + innerErr.message)
			}
		} catch (outerErr) {
			return { caughtByOuter: outerErr.message }
		}
	}`
	out, err := sandbox.Run(source, map[string]any{})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — the outer catch should have handled the rethrow", err)
	}
	if got := out.(map[string]any)["caughtByOuter"]; got != "wrapped: original" {
		t.Fatalf("caughtByOuter = %v, want %q", got, "wrapped: original")
	}
}

func TestRun_ErrorEscapingEveryCatchStillFailsTheStep(t *testing.T) {
	// The catch block itself throws a NEW error that nothing catches —
	// proves an error surviving past all try/catch blocks still correctly
	// surfaces as a Go error with the right message, not silently swallowed
	// or misreported as the original error.
	source := `(params) => {
		try {
			throw new Error("first")
		} catch (err) {
			throw new Error("second, from inside the catch: " + err.message)
		}
	}`
	_, err := sandbox.Run(source, map[string]any{})
	if err == nil {
		t.Fatal("Run() error = nil, want the uncaught rethrow to surface as an error")
	}
	if !strings.Contains(err.Error(), "second, from inside the catch: first") {
		t.Fatalf("err = %v, want it to contain the final uncaught error's message", err)
	}
}

func TestRun_CustomErrorClassPreservesTypeAndMessage(t *testing.T) {
	source := `(params) => {
		class ValidationError extends Error {
			constructor(field) {
				super("invalid field: " + field)
				this.name = "ValidationError"
				this.field = field
			}
		}
		try {
			throw new ValidationError("email")
		} catch (err) {
			return {
				isValidationError: err instanceof ValidationError,
				isError: err instanceof Error,
				name: err.name,
				field: err.field,
				message: err.message,
			}
		}
	}`
	out, err := sandbox.Run(source, map[string]any{})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	m := out.(map[string]any)
	if m["isValidationError"] != true || m["isError"] != true {
		t.Fatalf("instanceof checks = %+v, want both true (custom Error subclasses must work)", m)
	}
	if m["name"] != "ValidationError" || m["field"] != "email" || m["message"] != "invalid field: email" {
		t.Fatalf("caught error fields = %+v", m)
	}
}

// TestRun_PromiseReturnIsRejected proves the behavior sandbox.go's own doc
// comment has claimed since it was written: a returned Promise is rejected,
// not silently resolved to "[object Promise]" or awaited. No test ever
// exercised this before — the original implementation detected a Promise
// via `goja.Object.ClassName() == "Promise"`, but goja's actual ClassName()
// for a Promise object is "Object", not "Promise" (confirmed by inspecting
// what Export() returns instead: a *goja.Promise). That check never once
// matched, so Run() was silently returning the *goja.Promise value itself
// (exported) on every async function — exactly the bad-trap behavior the
// doc comment says this guards against. Fixed by checking
// `result.Export().(*goja.Promise)` instead of ClassName().
func TestRun_PromiseReturnIsRejected(t *testing.T) {
	source := `(params) => Promise.resolve(42)`
	_, err := sandbox.Run(source, map[string]any{})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection — a returned Promise must never resolve to a Go value")
	}
	if !strings.Contains(err.Error(), "Promise") {
		t.Fatalf("err = %v, want it to mention the Promise rejection", err)
	}
}

// TestRun_InfiniteLoopIsInterrupted proves DefaultTimeout actually bounds
// a runaway CODE step through the real Run() entry point — not just
// InterruptAfter in isolation (see timeout_test.go). Overrides
// DefaultTimeout so the test doesn't wait the real 5s.
func TestRun_InfiniteLoopIsInterrupted(t *testing.T) {
	original := sandbox.DefaultTimeout
	sandbox.DefaultTimeout = 50 * time.Millisecond
	defer func() { sandbox.DefaultTimeout = original }()

	start := time.Now()
	_, err := sandbox.Run(`(params) => { while (true) {} }`, map[string]any{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run() error = nil, want a timeout error for an infinite loop")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Run() took %v, want it to be interrupted quickly after the 50ms timeout", elapsed)
	}
}

func TestRun_FinallyRunsOnBothPaths(t *testing.T) {
	source := `(params) => {
		const log = []
		function attempt(shouldThrow) {
			try {
				log.push("try:" + shouldThrow)
				if (shouldThrow) {
					throw new Error("nope")
				}
				log.push("try-succeeded:" + shouldThrow)
			} catch (err) {
				log.push("catch:" + shouldThrow)
			} finally {
				log.push("finally:" + shouldThrow)
			}
		}
		attempt(true)
		attempt(false)
		return { log }
	}`
	out, err := sandbox.Run(source, map[string]any{})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	log, _ := out.(map[string]any)["log"].([]any)
	want := []string{"try:true", "catch:true", "finally:true", "try:false", "try-succeeded:false", "finally:false"}
	if len(log) != len(want) {
		t.Fatalf("log = %#v, want %d entries", log, len(want))
	}
	for i, w := range want {
		if log[i] != w {
			t.Fatalf("log[%d] = %q, want %q", i, log[i], w)
		}
	}
}
