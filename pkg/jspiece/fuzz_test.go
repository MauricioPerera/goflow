package jspiece_test

import (
	"testing"
	"time"

	"goflow/pkg/jspiece"
	"goflow/pkg/model"
	"goflow/pkg/piece"
)

// FuzzJSPieceSource feeds arbitrary strings as an action's JS source — the
// most direct stand-in for what an agent generating a piece at runtime
// might produce (Phase 2's whole point). Every hook is wired (not a
// zero-value ActionContext) so a fuzzer-discovered call to
// ctx.run.stop/respond/waitForWaitpoint or ctx.files.write exercises the
// real path, not the "hook unavailable" error path
// TestAdversarial_RunHooksNilByDefaultDoNotPanic already covers. Contract
// under test: never panic, never hang past DefaultTimeout, regardless of
// input — dialed down here so a fuzzer-found infinite loop costs
// milliseconds per execution, not DefaultTimeout's normal 5s (which would
// make any real exploration impractically slow).
func FuzzJSPieceSource(f *testing.F) {
	original := jspiece.DefaultTimeout
	jspiece.DefaultTimeout = 30 * time.Millisecond
	f.Cleanup(func() { jspiece.DefaultTimeout = original })

	seeds := []string{
		`(ctx) => ctx.input`,
		`(ctx) => { throw new Error("x"); }`,
		`not a function`,
		``,
		`(ctx) => { while(true) {} }`,
		`(ctx) => ctx.run.stop({status: 200})`,
		`(ctx) => ctx.run.respond({status: 200})`,
		`(ctx) => ctx.run.waitForWaitpoint("wp")`,
		`(ctx) => ctx.files.write("f.txt", "data")`,
		`(ctx) => ctx.fetch({url: "not a url"})`,
		`(ctx) => ctx.auth.accessToken`,
		`(ctx) => Promise.resolve(1)`,
		`(ctx) => { const a = {}; a.self = a; return a; }`,
		`(ctx) => { function f(n){return f(n+1);} return f(0); }`,
		`(ctx) => JSON.stringify(ctx.input)`,
		`function() {}`,
		`{}`,
		`() =>`,
		`(ctx) => { Object.prototype.x = 1; return {}; }`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, source string) {
		act := jspiece.NewAction(jspiece.ActionSource{Name: "fuzzed", Source: source})

		done := make(chan struct{})
		var panicVal any
		go func() {
			defer close(done)
			defer func() { panicVal = recover() }()
			act.Run(piece.ActionContext{
				Input: map[string]any{"x": 1},
				Auth:  "token",
				Files: piece.NewMemoryFileWriter(),
				Run: piece.RunHooks{
					Stop:             func(*model.WebhookResponse) {},
					Respond:          func(*model.WebhookResponse) {},
					WaitForWaitpoint: func(string) {},
				},
			})
		}()

		select {
		case <-done:
			if panicVal != nil {
				t.Fatalf("action panicked on source %q: %v", source, panicVal)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("action did not return within 2s (DefaultTimeout=%v) on source %q — likely hung", jspiece.DefaultTimeout, source)
		}
	})
}
