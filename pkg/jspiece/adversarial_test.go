package jspiece_test

// This file deliberately attacks pkg/jspiece the way untrusted, agent-
// generated JS piece code might: escape attempts, resource exhaustion,
// prototype pollution across calls, malformed thrown values, network abuse
// against internal addresses, and heavy concurrent load. The goal isn't
// just "does it return an error" — several of these tests exist to confirm
// (not just assume) the risk tradeoffs already written into README.md's
// "Design decisions" (no SSRF protection, no memory limit) are real and
// haven't silently gotten worse, and that nothing here can crash or hang
// the calling Go process regardless of what the JS does.
//
// Several sub-tests run the risky operation in a goroutine against a
// bounded deadline instead of calling it inline — a genuine infinite hang
// or a runaway allocation inside goja must fail the TEST, not freeze the
// whole suite.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"goflow/pkg/jspiece"
	"goflow/pkg/piece"
)

// runWithDeadline runs fn in a goroutine and fails the test if it doesn't
// return within deadline — used only for the handful of cases below where
// the JS under test could, in principle, hang goja forever (e.g. a
// pathological Export() on a circular structure) and a normal act.Run()
// call would take the whole test binary down with it.
func runWithDeadline(t *testing.T, deadline time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(deadline):
		t.Fatalf("operation did not return within %v — likely hung", deadline)
	}
}

func TestAdversarial_DeepRecursionFailsCleanlyNotACrash(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "recurse",
		Source: `(ctx) => {
			function boom(n) { return boom(n + 1); }
			return boom(0);
		}`,
	})

	runWithDeadline(t, jspiece.DefaultTimeout+5*time.Second, func() {
		_, err := act.Run(piece.ActionContext{})
		if err == nil {
			t.Error("Run() error = nil, want a stack-overflow/timeout error for unbounded recursion")
		}
	})
}

func TestAdversarial_LargeAllocationIsNotRejectedByAnyMemoryLimit(t *testing.T) {
	// Documents the accepted risk from README's "Design decisions": goja has
	// no memory cap, so a JS piece can allocate as much as it wants within
	// its time budget. This test uses a BOUNDED allocation (tens of MB, well
	// short of anything that could OOM a dev/CI machine) specifically to
	// prove the absence of a limit without actually being a memory bomb —
	// writing a real OOM attempt here would just crash the test runner, not
	// produce a useful signal.
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "allocate",
		Source: `(ctx) => {
			const chunks = [];
			for (let i = 0; i < 50; i++) {
				chunks.push("x".repeat(1000000)); // 1MB per chunk, 50MB total
			}
			return { chunks: chunks.length, totalLen: chunks.reduce((a, c) => a + c.length, 0) };
		}`,
	})
	out, err := act.Run(piece.ActionContext{})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — a 50MB allocation should not be rejected, there is no memory limit", err)
	}
	m := out.(map[string]any)
	if m["chunks"] != int64(50) {
		t.Fatalf("chunks = %v, want 50", m["chunks"])
	}
}

func TestAdversarial_NodeAndHostGlobalsAreNotExposed(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "probe",
		Source: `(ctx) => ({
			hasProcess: typeof process !== "undefined",
			hasRequire: typeof require !== "undefined",
			hasGlobalThis: typeof globalThis !== "undefined",
			hasImportScripts: typeof importScripts !== "undefined",
		})`,
	})
	out, err := act.Run(piece.ActionContext{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m := out.(map[string]any)
	if m["hasProcess"] != false || m["hasRequire"] != false || m["hasImportScripts"] != false {
		t.Fatalf("host/Node globals leaked into the sandbox: %+v", m)
	}
}

func TestAdversarial_PrototypePollutionDoesNotSurviveAcrossRuns(t *testing.T) {
	pollute := jspiece.NewAction(jspiece.ActionSource{
		Name: "pollute",
		Source: `(ctx) => {
			Object.prototype.polluted = "leaked";
			Array.prototype.polluted = "leaked";
			return { ok: true };
		}`,
	})
	check := jspiece.NewAction(jspiece.ActionSource{
		Name: "check",
		Source: `(ctx) => ({
			objectPolluted: ({}).polluted !== undefined,
			arrayPolluted: ([]).polluted !== undefined,
		})`,
	})

	if _, err := pollute.Run(piece.ActionContext{}); err != nil {
		t.Fatalf("pollute Run() error = %v", err)
	}
	out, err := check.Run(piece.ActionContext{})
	if err != nil {
		t.Fatalf("check Run() error = %v", err)
	}
	m := out.(map[string]any)
	if m["objectPolluted"] != false || m["arrayPolluted"] != false {
		t.Fatalf("pollution from one Run() call leaked into a later one: %+v — every Run must get a fresh goja.Runtime", m)
	}
}

func TestAdversarial_ConcurrentRunsDoNotShareState(t *testing.T) {
	// Each goroutine pollutes the global Object prototype with ITS OWN
	// distinguishing value and immediately reads it back, many times over,
	// under -race. If runJS's "fresh goja.New() per call" isolation were
	// ever broken (e.g. a VM accidentally cached/reused across calls), this
	// would either race or observe another goroutine's pollution.
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "isolated",
		Source: `(ctx) => {
			const tag = ctx.input.tag;
			Object.prototype.tag = tag;
			const seen = ({}).tag;
			delete Object.prototype.tag;
			return { tag: tag, seen: seen };
		}`,
	})

	const goroutines = 50
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag := fmt.Sprintf("tag-%d", i)
			out, err := act.Run(piece.ActionContext{Input: map[string]any{"tag": tag}})
			if err != nil {
				errs[i] = err
				return
			}
			m := out.(map[string]any)
			if m["seen"] != tag {
				errs[i] = fmt.Errorf("goroutine %d saw tag %q, want %q — cross-goroutine leakage", i, m["seen"], tag)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
}

func TestAdversarial_ThrownNonErrorValues(t *testing.T) {
	cases := []struct {
		name, source string
	}{
		{"string", `(ctx) => { throw "just a string"; }`},
		{"number", `(ctx) => { throw 42; }`},
		{"object", `(ctx) => { throw {code: "BOOM", detail: "nested"}; }`},
		{"null", `(ctx) => { throw null; }`},
		{"undefined", `(ctx) => { throw undefined; }`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			act := jspiece.NewAction(jspiece.ActionSource{Name: "thrower", Source: c.source})
			_, err := act.Run(piece.ActionContext{})
			if err == nil {
				t.Fatalf("Run() error = nil, want a surfaced error for a thrown %s", c.name)
			}
		})
	}
}

func TestAdversarial_CircularObjectReturnDoesNotHangOrCrash(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "circular",
		Source: `(ctx) => {
			const obj = {};
			obj.self = obj;
			return obj;
		}`,
	})
	runWithDeadline(t, 5*time.Second, func() {
		// Either outcome (error, or a successfully exported self-referential
		// map) is acceptable — what matters is that it returns at all.
		act.Run(piece.ActionContext{})
	})
}

// TestAdversarial_RunHooksNilByDefaultDoNotPanic is a regression test for a
// real bug this adversarial battery found: calling ctx.run.stop/respond/
// waitForWaitpoint from JS against a zero-value piece.ActionContext (no
// hooks wired — e.g. a bare call to act.Run() outside the engine, exactly
// what every other test in this package does for a plain Run/action test)
// used to panic with a Go nil-pointer dereference that escaped act.Run()
// completely uncaught, instead of returning a normal error. The engine
// itself always wires all three hooks, so this never fired in a real flow
// — but any other caller of a JS-backed piece.Action would have crashed
// its own goroutine. Fixed by guarding each hook before calling it.
func TestAdversarial_RunHooksNilByDefaultDoNotPanic(t *testing.T) {
	cases := []struct {
		name, source string
	}{
		{"stop", `(ctx) => ctx.run.stop({status: 200})`},
		{"respond", `(ctx) => ctx.run.respond({status: 200})`},
		{"waitForWaitpoint", `(ctx) => ctx.run.waitForWaitpoint("wp-1")`},
		{"files.write", `(ctx) => ctx.files.write("f.txt", "data")`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			act := jspiece.NewAction(jspiece.ActionSource{Name: "a", Source: c.source})
			_, err := act.Run(piece.ActionContext{}) // zero-value: every hook and Files is nil
			if err == nil {
				t.Fatalf("Run() error = nil, want a clean error — %s should be unavailable, not panic", c.name)
			}
		})
	}
}

func TestAdversarial_FetchReachesLocalAndPrivateAddresses(t *testing.T) {
	// Confirms the documented SSRF gap: ctx.fetch has no address filtering,
	// so it reaches "internal" targets (a loopback-bound test server stands
	// in for a real private/internal address here) exactly like any public
	// one. This is expected to succeed — that success IS the risk.
	var sawInternalRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawInternalRequest = true
		w.Write([]byte("internal data"))
	}))
	defer server.Close()

	act := jspiece.NewAction(jspiece.ActionSource{
		Name:   "ssrf_probe",
		Source: `(ctx) => ctx.fetch({url: ctx.input.url})`,
	})
	out, err := act.Run(piece.ActionContext{Input: map[string]any{"url": server.URL}})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — no SSRF protection exists, this must succeed", err)
	}
	if !sawInternalRequest {
		t.Fatal("the internal-address server never received the request")
	}
	if out.(map[string]any)["body"] != "internal data" {
		t.Fatalf("out = %#v", out)
	}
}

func TestAdversarial_FetchTimeoutIsEnforced(t *testing.T) {
	original := jspiece.DefaultFetchTimeout
	jspiece.DefaultFetchTimeout = 50 * time.Millisecond
	defer func() { jspiece.DefaultFetchTimeout = original }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte("too slow"))
	}))
	defer server.Close()

	act := jspiece.NewAction(jspiece.ActionSource{
		Name:   "slow_fetch",
		Source: `(ctx) => ctx.fetch({url: ctx.input.url})`,
	})

	runWithDeadline(t, 1500*time.Millisecond, func() {
		_, err := act.Run(piece.ActionContext{Input: map[string]any{"url": server.URL}})
		if err == nil {
			t.Error("Run() error = nil, want a timeout error — the server deliberately took 2s against a 50ms fetch timeout")
		}
	})
}

func TestAdversarial_ManySequentialFetchesInOneActionComplete(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "hammer",
		Source: `(ctx) => {
			let count = 0;
			for (let i = 0; i < 20; i++) {
				const res = ctx.fetch({url: ctx.input.url});
				if (res.status === 200) count++;
			}
			return { count: count };
		}`,
	})
	out, err := act.Run(piece.ActionContext{Input: map[string]any{"url": server.URL}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.(map[string]any)["count"] != int64(20) {
		t.Fatalf("count = %v, want 20", out.(map[string]any)["count"])
	}
	if requestCount != 20 {
		t.Fatalf("requestCount = %d, want 20", requestCount)
	}
}

func TestAdversarial_HugeInputStringIsHandled(t *testing.T) {
	huge := strings.Repeat("a", 5_000_000) // 5MB string
	act := jspiece.NewAction(jspiece.ActionSource{
		Name:   "measure",
		Source: `(ctx) => ({ len: ctx.input.text.length })`,
	})
	out, err := act.Run(piece.ActionContext{Input: map[string]any{"text": huge}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.(map[string]any)["len"] != int64(5_000_000) {
		t.Fatalf("len = %v, want 5000000", out.(map[string]any)["len"])
	}
}

func TestAdversarial_ManyJSPiecesRunConcurrentlyStaysIsolated(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name:   "double",
		Source: `(ctx) => ({ doubled: Number(ctx.input.n) * 2 })`,
	})

	const runs = 200
	var wg sync.WaitGroup
	results := make([]any, runs)
	errs := make([]error, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := act.Run(piece.ActionContext{Input: map[string]any{"n": int64(i)}})
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = out.(map[string]any)["doubled"]
		}(i)
	}
	wg.Wait()

	for i := 0; i < runs; i++ {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		want := int64(i * 2)
		if results[i] != want {
			t.Fatalf("run %d: doubled = %v, want %d", i, results[i], want)
		}
	}
}
