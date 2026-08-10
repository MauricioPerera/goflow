package sandbox_test

import (
	"testing"
	"time"

	"github.com/dop251/goja"

	"goflow/pkg/sandbox"
)

func TestInterruptAfter_FiresAfterDuration(t *testing.T) {
	vm := goja.New()
	stop := sandbox.InterruptAfter(vm, 20*time.Millisecond, "timed out")
	defer stop()

	_, err := vm.RunString(`while (true) {}`)
	if err == nil {
		t.Fatal("RunString() error = nil, want an interrupt error")
	}
	if _, ok := err.(*goja.InterruptedError); !ok {
		t.Fatalf("err = %v (%T), want *goja.InterruptedError", err, err)
	}
}

func TestInterruptAfter_StopPreventsInterrupt(t *testing.T) {
	vm := goja.New()
	stop := sandbox.InterruptAfter(vm, 30*time.Millisecond, "timed out")
	stop()

	// Give the timer a chance to have fired if stop() didn't work.
	time.Sleep(60 * time.Millisecond)

	_, err := vm.RunString(`1 + 1`)
	if err != nil {
		t.Fatalf("RunString() error = %v, want nil — stop() should have cancelled the timer", err)
	}
}

func TestInterruptAfter_StopIsIdempotent(t *testing.T) {
	vm := goja.New()
	stop := sandbox.InterruptAfter(vm, 10*time.Millisecond, "timed out")
	stop()
	stop()
	stop()
}

func TestInterruptAfter_ReturnsImmediately(t *testing.T) {
	vm := goja.New()
	start := time.Now()
	stop := sandbox.InterruptAfter(vm, 5*time.Second, "timed out")
	defer stop()
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("InterruptAfter() took %v to return, want it to return immediately", elapsed)
	}
}
