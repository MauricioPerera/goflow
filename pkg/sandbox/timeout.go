package sandbox

import (
	"time"

	"github.com/dop251/goja"
)

// InterruptAfter arms a timer that calls vm.Interrupt(reason) after d
// elapses, and returns a stop function that cancels it — the shared
// timeout-arming pattern already used inline in pkg/expr and pkg/jspiece,
// extracted here so pkg/sandbox's CODE steps can use the identical
// mechanism instead of duplicating it a third time. Returns immediately;
// never blocks. Calling stop before d elapses prevents the interrupt from
// ever firing; calling it after (or multiple times, in any order) is a
// safe no-op, since it's backed directly by *time.Timer.Stop().
func InterruptAfter(vm *goja.Runtime, d time.Duration, reason string) func() {
	timer := time.AfterFunc(d, func() {
		vm.Interrupt(reason)
	})
	return func() {
		timer.Stop()
	}
}
