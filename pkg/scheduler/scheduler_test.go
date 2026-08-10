package scheduler_test

import (
	"context"
	"testing"
	"time"

	"goflow/pkg/flowstore"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/pieces/schedule"
	"goflow/pkg/runstore"
	"goflow/pkg/scheduler"
)

// scheduleRegistry returns a buildRegistry with just the schedule piece
// registered — enough for a schedule-triggered flow (with a CODE next
// action, which needs no registered piece) to validate.
func scheduleRegistry() (*piece.Registry, error) {
	r := piece.NewRegistry()
	r.Register(schedule.New())
	return r, nil
}

// scheduledFlow builds a flow whose trigger is the schedule piece
// (intervalSeconds as given) followed by one CODE action that echoes the
// trigger's firedAt — enough to prove, when the scheduler actually runs it,
// that the whole path (trigger fires -> flow executes -> history records)
// works end to end, not just that the piece alone decides "due".
func scheduledFlow(name string, intervalSeconds int64) model.FlowVersion {
	return model.FlowVersion{
		ID: "fv-" + name,
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Schedule", Type: model.TriggerPiece,
			PieceName: schedule.PieceName, TriggerName: schedule.TriggerName,
			Input: map[string]any{"intervalSeconds": intervalSeconds},
			NextAction: &model.FlowAction{
				Name: "echo", DisplayName: "Echo", Type: model.ActionCode,
				Code: &model.CodeSettings{
					Input:  map[string]any{"firedAt": "{{ trigger_1.output.firedAt }}"},
					Source: `(params) => params`,
				},
			},
		},
	}
}

// failingScheduledFlow is scheduledFlow's shape but its CODE step always
// throws — used to prove TriggerOnFailure actually fires from inside the
// scheduler's own per-flow goroutine.
func failingScheduledFlow(name string, intervalSeconds int64) model.FlowVersion {
	return model.FlowVersion{
		ID: "fv-" + name,
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Schedule", Type: model.TriggerPiece,
			PieceName: schedule.PieceName, TriggerName: schedule.TriggerName,
			Input: map[string]any{"intervalSeconds": intervalSeconds},
			NextAction: &model.FlowAction{
				Name: "boom", DisplayName: "Boom", Type: model.ActionCode,
				Code: &model.CodeSettings{Source: `(params) => { throw new Error("boom"); }`},
			},
		},
	}
}

// notifyFlow is a valid, always-succeeding, EMPTY-trigger CODE flow — the
// scheduler's own tick loop never picks it up on its own (isScheduleTriggered
// is false for it), so it should only ever run via TriggerOnFailure.
func notifyFlow() model.FlowVersion {
	return model.FlowVersion{
		ID: "fv-notify",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "ack", DisplayName: "Ack", Type: model.ActionCode,
				Code: &model.CodeSettings{Source: `(params) => ({ acked: true })`},
			},
		},
	}
}

func newFlowStore(t *testing.T) *flowstore.GatedStore {
	t.Helper()
	fs, err := flowstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("flowstore.NewFileStore: %v", err)
	}
	return &flowstore.GatedStore{Underlying: fs, BuildRegistry: scheduleRegistry}
}

// waitForRuns polls hist.List() until it has at least n entries or timeout
// elapses — a tighter, faster-on-success alternative to one blind
// time.Sleep long enough to cover the worst case.
func waitForRuns(t *testing.T, hist runstore.Store, n int, timeout time.Duration) []runstore.Summary {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		summaries, err := hist.List()
		if err != nil {
			t.Fatalf("hist.List: %v", err)
		}
		if len(summaries) >= n {
			return summaries
		}
		time.Sleep(20 * time.Millisecond)
	}
	summaries, _ := hist.List()
	t.Fatalf("timed out waiting for %d recorded run(s), got %d: %+v", n, len(summaries), summaries)
	return nil
}

func TestScheduler_FiresSavedFlowOnItsOwn(t *testing.T) {
	fs := newFlowStore(t)
	fv := scheduledFlow("ticker", 1)
	if err := fs.Save(flowstore.FlowDefinition{Name: "ticker", DisplayName: "Ticker", Flow: fv}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	hist := runstore.NewMemoryStore()
	sched := scheduler.New(fs, scheduleRegistry, nil, hist, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sched.Run(ctx)

	summaries := waitForRuns(t, hist, 1, 2*time.Second)
	if summaries[0].FlowName != "ticker" {
		t.Fatalf("recorded run FlowName = %q, want %q", summaries[0].FlowName, "ticker")
	}
	if summaries[0].Status != model.FlowRunSucceeded {
		t.Fatalf("recorded run Status = %v, want SUCCEEDED", summaries[0].Status)
	}
}

func TestScheduler_OnFailureConfigured_TriggersNamedFlow_RecordedInHistory(t *testing.T) {
	fs := newFlowStore(t)
	if err := fs.Save(flowstore.FlowDefinition{Name: "notify", DisplayName: "Notify", Flow: notifyFlow()}); err != nil {
		t.Fatalf("Save notify: %v", err)
	}
	failer := failingScheduledFlow("failer", 1)
	if err := fs.Save(flowstore.FlowDefinition{Name: "failer", DisplayName: "Failer", Flow: failer, OnFailureFlow: "notify"}); err != nil {
		t.Fatalf("Save failer: %v", err)
	}
	hist := runstore.NewMemoryStore()
	sched := scheduler.New(fs, scheduleRegistry, nil, hist, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sched.Run(ctx)

	summaries := waitForRuns(t, hist, 2, 2*time.Second)
	names := map[string]int{}
	for _, s := range summaries {
		names[s.FlowName]++
	}
	if names["failer"] != 1 || names["notify"] != 1 {
		t.Fatalf("recorded runs = %v, want exactly one for \"failer\" and one for \"notify\"", names)
	}
}

func TestScheduler_NonScheduleFlowsAreIgnored(t *testing.T) {
	fs := newFlowStore(t)
	// EMPTY-triggered flow: the scheduler must never touch this, regardless
	// of how many ticks pass.
	emptyFlow := model.FlowVersion{
		ID: "fv-empty",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Manual", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "noop", DisplayName: "Noop", Type: model.ActionCode,
				Code: &model.CodeSettings{Source: `(params) => params`},
			},
		},
	}
	if err := fs.Save(flowstore.FlowDefinition{Name: "manual-only", DisplayName: "Manual Only", Flow: emptyFlow}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	hist := runstore.NewMemoryStore()
	sched := scheduler.New(fs, scheduleRegistry, nil, hist, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	go sched.Run(ctx)
	time.Sleep(400 * time.Millisecond) // several ticks' worth
	cancel()

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("hist.List: %v", err)
	}
	if len(summaries) != 0 {
		t.Fatalf("recorded runs = %+v, want none — an EMPTY-triggered flow is not this scheduler's concern", summaries)
	}
}

func TestScheduler_TwoScheduledFlowsFireIndependently(t *testing.T) {
	fs := newFlowStore(t)
	if err := fs.Save(flowstore.FlowDefinition{Name: "fast", DisplayName: "Fast", Flow: scheduledFlow("fast", 1)}); err != nil {
		t.Fatalf("Save(fast): %v", err)
	}
	if err := fs.Save(flowstore.FlowDefinition{Name: "slow", DisplayName: "Slow", Flow: scheduledFlow("slow", 3600)}); err != nil {
		t.Fatalf("Save(slow): %v", err)
	}
	hist := runstore.NewMemoryStore()
	sched := scheduler.New(fs, scheduleRegistry, nil, hist, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sched.Run(ctx)

	// Both flows fire once immediately -- schedule.Run's own documented
	// "first call has no cursor yet, so it fires and seeds one" behavior,
	// regardless of intervalSeconds (see schedule_test.go's
	// TestSchedule_FirstCallFiresImmediatelyAndSeedsCursor). What actually
	// proves the two flows' cursors are independent (piece.ScopedStore
	// working across DIFFERENT flows sharing one underlying Store) is what
	// happens AFTER that: "fast" (1s) should fire a second time within this
	// window; "slow" (1h) should not fire a second time.
	waitForRuns(t, hist, 2, 2*time.Second) // both flows' guaranteed first fire
	waitForRuns(t, hist, 3, 2*time.Second) // "fast" firing again is the 3rd

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("hist.List: %v", err)
	}
	counts := map[string]int{}
	for _, s := range summaries {
		counts[s.FlowName]++
	}
	if counts["fast"] < 2 {
		t.Fatalf("fast fired %d time(s), want at least 2 (its 1s interval should have elapsed again): %+v", counts["fast"], summaries)
	}
	if counts["slow"] != 1 {
		t.Fatalf("slow fired %d time(s), want exactly 1 (its 1h interval should not have elapsed again — cursors must not be shared with fast): %+v", counts["slow"], summaries)
	}
}

func TestScheduler_RunReturnsWhenContextCancelled(t *testing.T) {
	fs := newFlowStore(t)
	sched := scheduler.New(fs, scheduleRegistry, nil, runstore.NewMemoryStore(), 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return within 1s of context cancellation")
	}
}
