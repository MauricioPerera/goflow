package engine_test

// Scales the existing concurrency proofs (TestConcurrentFlowRuns_*, further
// up in this package) up by an order of magnitude and adds shapes those
// tests don't cover: real catalog pieces (not just a synthetic worker),
// large LOOP_ON_ITEMS counts, deep ROUTER nesting, and a shared registry
// under simultaneous read+execute pressure. The goal isn't new correctness
// claims — it's confirming the same isolation/race-freedom guarantees hold
// at a scale closer to a real deployment, not just at the small N the
// original tests used to keep the suite fast.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"goflow/pkg/engine"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/pieces"
	"goflow/pkg/sandbox"
)

func TestStress_ManyConcurrentFlowsWithRealCatalogPieces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"n": %s}`, r.URL.Query().Get("n"))
	}))
	defer server.Close()

	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	parseStep := &model.FlowAction{
		Name: "parsed", DisplayName: "Parse", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "parse", Input: map[string]any{
			"text": "{{ fetch.output.body }}",
		}},
	}
	fetchStep := &model.FlowAction{
		Name: "fetch", DisplayName: "Fetch", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "http", ActionName: "request", Input: map[string]any{
			"url": "{{ trigger_1.output.url }}",
		}},
		NextAction: parseStep,
	}
	fv := &model.FlowVersion{ID: "fv-stress-catalog", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: fetchStep,
	}}

	e := engine.New(registry)
	// 150, not something larger like 500: hammering ONE local httptest.Server
	// with hundreds of simultaneous raw TCP connections runs into the OS's
	// own listen-backlog/accept-queue limits (observed on Windows as
	// WSAECONNREFUSED under -count repeats at 500) — that's a limit of this
	// test's single-listener setup, not of the engine or the http piece. 150
	// concurrent runs against one listener is stable across repeated runs
	// and still a real order-of-magnitude jump over the original
	// TestConcurrentFlowRuns_* tests' 50.
	const runs = 150
	var wg sync.WaitGroup
	results := make([]*model.ExecutionState, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			url := fmt.Sprintf("%s?n=%d", server.URL, i)
			results[i] = e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{"url": url}})
		}(i)
	}
	wg.Wait()

	for i, state := range results {
		if state.Verdict.Status != model.FlowRunSucceeded {
			t.Fatalf("run %d verdict = %+v failedStep=%+v fetchStep=%+v", i, state.Verdict, state.Verdict.FailedStep, state.Steps["fetch"])
		}
		data := state.Steps["parsed"].Output.(map[string]any)["data"].(map[string]any)
		if data["n"] != float64(i) {
			t.Fatalf("run %d: n = %v, want %d — cross-run contamination under load", i, data["n"], i)
		}
	}
}

func TestStress_LargeLoopOnItemsCompletesCorrectly(t *testing.T) {
	const itemCount = 5000

	sumSoFar := &accumulator{}
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "acc", DisplayName: "Accumulator",
		Actions: map[string]piece.Action{
			"add": {
				Name: "add", DisplayName: "Add",
				Run: func(ctx piece.ActionContext) (any, error) {
					n, _ := ctx.Input["n"].(int64)
					sumSoFar.Add(n)
					return map[string]any{"n": n}, nil
				},
			},
		},
	})

	addStep := &model.FlowAction{
		Name: "add", DisplayName: "Add", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "acc", ActionName: "add", Input: map[string]any{
			"n": "{{ loop.output.item }}",
		}},
	}
	loop := &model.FlowAction{
		Name: "loop", DisplayName: "Loop", Type: model.ActionLoopOnItems,
		Loop: &model.LoopSettings{Items: "{{ trigger_1.output.items }}", FirstLoopAction: addStep},
	}
	fv := &model.FlowVersion{ID: "fv-stress-loop", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: loop,
	}}

	items := make([]any, itemCount)
	var want int64
	for i := 0; i < itemCount; i++ {
		items[i] = int64(i)
		want += int64(i)
	}

	start := time.Now()
	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{"items": items}})
	elapsed := time.Since(start)

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	loopOut := state.Steps["loop"]
	if len(loopOut.Iterations) != itemCount {
		t.Fatalf("Iterations count = %d, want %d", len(loopOut.Iterations), itemCount)
	}
	if sumSoFar.Value() != want {
		t.Fatalf("accumulated sum = %d, want %d — an iteration ran zero or more than once", sumSoFar.Value(), want)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("%d-item loop took %v, want well under 10s", itemCount, elapsed)
	}
}

func TestStress_DeeplyNestedRouterChainCompletesCorrectly(t *testing.T) {
	const depth = 200

	countStep := func(name string, next *model.FlowAction) *model.FlowAction {
		return &model.FlowAction{
			Name: name, DisplayName: name, Type: model.ActionCode,
			Code:       &model.CodeSettings{Source: echoSource, Input: map[string]any{"depth": name}},
			NextAction: next,
		}
	}

	// Build depth-many nested single-branch ROUTERs, innermost first.
	var innermost *model.FlowAction
	for i := depth - 1; i >= 0; i-- {
		leaf := countStep(fmt.Sprintf("leaf_%d", i), nil)
		router := &model.FlowAction{
			Name: fmt.Sprintf("router_%d", i), DisplayName: fmt.Sprintf("Router %d", i), Type: model.ActionRouter,
			Router: &model.RouterSettings{
				ExecutionType: model.RouterExecuteFirstMatch,
				Branches:      []model.RouterBranch{{Name: "always", Type: model.BranchFallback}},
				Children:      []*model.FlowAction{leaf},
			},
		}
		if innermost != nil {
			leaf.NextAction = innermost
		}
		innermost = router
	}

	fv := &model.FlowVersion{ID: "fv-stress-nested-router", Trigger: trigger(innermost)}

	start := time.Now()
	state := engine.New(piece.NewRegistry()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	elapsed := time.Since(start)

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	if _, ok := state.Steps["leaf_0"]; !ok {
		t.Fatal("outermost leaf (leaf_0) never ran")
	}
	if _, ok := state.Steps[fmt.Sprintf("leaf_%d", depth-1)]; !ok {
		t.Fatalf("innermost leaf (leaf_%d) never ran", depth-1)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("%d-deep router chain took %v, want well under 10s", depth, elapsed)
	}
}

func TestStress_SharedRegistryUnderConcurrentReadsAndExecution(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	step := codeAction("step_1", echoSource, map[string]any{"key": "{{ 1 + 1 }}"})
	fv := &model.FlowVersion{ID: "fv-stress-registry", Trigger: trigger(step)}
	e := engine.New(registry)

	var wg sync.WaitGroup

	// Readers: hammer GetAction directly, simulating another part of a real
	// deployment (e.g. a UI resolving piece metadata) reading the registry
	// while flows are executing against it.
	const readers = 100
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, ok := registry.GetAction("http", "request"); !ok {
					t.Error("http.request unexpectedly not resolvable")
				}
			}
		}()
	}

	// Writers-in-effect: concurrent flow runs against the same registry.
	const runs = 200
	results := make([]*model.ExecutionState, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
		}(i)
	}
	wg.Wait()

	for i, state := range results {
		if state.Verdict.Status != model.FlowRunSucceeded {
			t.Fatalf("run %d verdict = %+v", i, state.Verdict)
		}
		if state.Steps["step_1"].Output.(map[string]any)["key"] != int64(2) {
			t.Fatalf("run %d: key = %v, want 2", i, state.Steps["step_1"].Output.(map[string]any)["key"])
		}
	}
}

func BenchmarkConcurrentFlowExecution(b *testing.B) {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "worker", DisplayName: "Worker",
		Actions: map[string]piece.Action{
			"process": {
				Name: "process",
				Run: func(ctx piece.ActionContext) (any, error) {
					n, _ := ctx.Input["n"].(int64)
					return map[string]any{"doubled": n * 2}, nil
				},
			},
		},
	})
	process := &model.FlowAction{
		Name: "process", DisplayName: "Process", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "worker", ActionName: "process", Input: map[string]any{
			"n": "{{ trigger_1.output.n }}",
		}},
	}
	fv := &model.FlowVersion{ID: "fv-bench", Trigger: trigger(process)}
	e := engine.New(registry)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int64(0)
		for pb.Next() {
			e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{"n": i}})
			i++
		}
	})
}

// accumulator is a tiny mutex-guarded int64 sum, used only to prove every
// loop iteration ran exactly once under TestStress_LargeLoopOnItemsCompletesCorrectly
// — LOOP_ON_ITEMS iterations run sequentially within one ExecuteBegin call
// (not concurrently), so this doesn't need to be lock-free, just correct.
type accumulator struct {
	mu  sync.Mutex
	sum int64
}

func (a *accumulator) Add(n int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sum += n
}

func (a *accumulator) Value() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sum
}

// TestStress_RunawayCodeStepIsInterruptedThroughRealFlow proves
// sandbox.DefaultTimeout actually bounds a CODE step's runaway execution
// through a real ExecuteBegin call — not just sandbox.Run in isolation
// (pkg/sandbox's own TestRun_InfiniteLoopIsInterrupted). A flow built
// from Phase 1's JSON data could contain exactly this: an agent-authored
// CODE action with a Source that never returns.
func TestStress_RunawayCodeStepIsInterruptedThroughRealFlow(t *testing.T) {
	original := sandbox.DefaultTimeout
	sandbox.DefaultTimeout = 50 * time.Millisecond
	defer func() { sandbox.DefaultTimeout = original }()

	runaway := codeAction("runaway", `(params) => { while (true) {} }`, map[string]any{})
	fv := &model.FlowVersion{ID: "fv-runaway-code", Trigger: trigger(runaway)}

	start := time.Now()
	state := engine.New(piece.NewRegistry()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	elapsed := time.Since(start)

	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED — the runaway CODE step must time out, not hang the run forever", state.Verdict)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ExecuteBegin took %v, want it to return quickly after the 50ms timeout", elapsed)
	}
}
