package expr_test

import (
	"testing"
	"time"

	"goflow/pkg/expr"
)

// FuzzEval feeds arbitrary strings as {{ }} expression bodies — this is
// literally user/agent-controlled text (a flow's Input values), evaluated
// as real JS via goja (see resolve.go's doc comment for why). The only
// contract under test: a malformed or adversarial expression always comes
// back as a Go error, never a panic — goja itself, or this package's own
// wrapping, must never crash the calling goroutine over bad input text.
func FuzzEval(f *testing.F) {
	original := expr.DefaultTimeout
	expr.DefaultTimeout = 30 * time.Millisecond
	f.Cleanup(func() { expr.DefaultTimeout = original })

	seeds := []string{
		"1 + 2",
		"a.b.c",
		"[1,2,3]",
		"",
		"{{{{{{",
		"}}}}}}",
		"while(true){}",
		"(() => {})()",
		"null.x",
		"undefined()",
		"Array(-1)",
		"'x'.repeat(5000000)",
		"({}).constructor.constructor('return this')()",
		"JSON.parse('{')",
		"new Array(1000000)",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, expression string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Eval panicked on %q: %v", expression, r)
			}
		}()
		expr.Eval(expression, expr.Scope{})
	})
}

// FuzzResolveString feeds arbitrary strings through Resolve as a step
// Input value would arrive — the mixed-text interpolation path
// (resolveString's anyTemplate branch), not just a whole-template
// expression. Same contract: never panic.
func FuzzResolveString(f *testing.F) {
	seeds := []string{
		"hello {{ name }}!",
		"{{ a }} {{ b }} {{ c }}",
		"{{ unterminated",
		"unstarted }}",
		"{{ }}",
		"{{ {{ }} }}",
		"plain text, no templates at all",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Resolve panicked on %q: %v", s, r)
			}
		}()
		expr.Resolve(s, expr.Scope{"name": "world"})
	})
}
