package engine_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goflow/pkg/engine"
	"goflow/pkg/model"
	"goflow/pkg/piece"
)

func codeAction(name, source string, input map[string]any) *model.FlowAction {
	return &model.FlowAction{
		Name: name, DisplayName: name, Type: model.ActionCode,
		Code: &model.CodeSettings{Input: input, Source: source},
	}
}

const echoSource = `(params) => params`

func trigger(next *model.FlowAction) *model.FlowTrigger {
	return &model.FlowTrigger{Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty, NextAction: next}
}

func TestTwoStepCodeFlow(t *testing.T) {
	step2 := codeAction("step_2", echoSource, map[string]any{"doubled": "{{ step_1.output.key * 2 }}"})
	step1 := codeAction("step_1", echoSource, map[string]any{"key": "{{ 1 + 2 }}"})
	step1.NextAction = step2

	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(step1)}
	state := engine.New(piece.NewRegistry()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	assertOutput(t, state, "step_1", map[string]any{"key": int64(3)})
	assertOutput(t, state, "step_2", map[string]any{"doubled": int64(6)})
}

func TestRouterBranching(t *testing.T) {
	buildFlow := func() *model.FlowVersion {
		router := &model.FlowAction{
			Name: "router", DisplayName: "Router", Type: model.ActionRouter,
			Router: &model.RouterSettings{
				ExecutionType: model.RouterExecuteFirstMatch,
				Branches: []model.RouterBranch{
					{Name: "urgent", Type: model.BranchCondition, Conditions: [][]model.Condition{{
						{Operator: model.OpTextExactlyMatches, FirstValue: "{{ trigger_1.output.status }}", SecondValue: "urgent"},
					}}},
					{Name: "fallback", Type: model.BranchFallback},
				},
				Children: []*model.FlowAction{
					codeAction("urgent_step", echoSource, map[string]any{"handled": "urgent"}),
					codeAction("normal_step", echoSource, map[string]any{"handled": "normal"}),
				},
			},
		}
		return &model.FlowVersion{ID: "fv-router", Trigger: trigger(router)}
	}

	t.Run("condition matches", func(t *testing.T) {
		state := engine.New(piece.NewRegistry()).ExecuteBegin(buildFlow(), engine.BeginInput{TriggerPayload: map[string]any{"status": "urgent"}})
		if state.Verdict.Status != model.FlowRunSucceeded {
			t.Fatalf("verdict = %+v", state.Verdict)
		}
		assertOutput(t, state, "urgent_step", map[string]any{"handled": "urgent"})
		if _, ok := state.Steps["normal_step"]; ok {
			t.Fatal("normal_step should not have run")
		}
	})

	t.Run("falls through to fallback", func(t *testing.T) {
		state := engine.New(piece.NewRegistry()).ExecuteBegin(buildFlow(), engine.BeginInput{TriggerPayload: map[string]any{"status": "low"}})
		if state.Verdict.Status != model.FlowRunSucceeded {
			t.Fatalf("verdict = %+v", state.Verdict)
		}
		assertOutput(t, state, "normal_step", map[string]any{"handled": "normal"})
		if _, ok := state.Steps["urgent_step"]; ok {
			t.Fatal("urgent_step should not have run")
		}
	})
}

// TestRouterCondition_AndWithinGroup and TestRouterCondition_OrAcrossGroups
// exercise the part of a conditional branch that TestRouterBranching never
// touched: a branch's Conditions is [][]Condition — an OR of AND-groups
// (activepieces: conditionGroups.some(group => group.every(cond => ...))).
// Every router test so far used exactly one group with exactly one
// condition, so the AND-within-a-group and OR-across-groups paths in
// evaluateConditionGroups had never actually been run.
func TestRouterCondition_AndWithinGroup(t *testing.T) {
	buildFlow := func() *model.FlowVersion {
		router := &model.FlowAction{
			Name: "router", DisplayName: "Router", Type: model.ActionRouter,
			Router: &model.RouterSettings{
				ExecutionType: model.RouterExecuteFirstMatch,
				Branches: []model.RouterBranch{
					// one group, two conditions — must ALL be true (AND).
					{Name: "big_and_active", Type: model.BranchCondition, Conditions: [][]model.Condition{{
						{Operator: model.OpNumberGreaterThan, FirstValue: "{{ trigger_1.output.amount }}", SecondValue: "10"},
						{Operator: model.OpTextExactlyMatches, FirstValue: "{{ trigger_1.output.status }}", SecondValue: "active"},
					}}},
					{Name: "fallback", Type: model.BranchFallback},
				},
				Children: []*model.FlowAction{
					codeAction("matched", echoSource, map[string]any{"branch": "big_and_active"}),
					codeAction("fell_through", echoSource, map[string]any{"branch": "fallback"}),
				},
			},
		}
		return &model.FlowVersion{ID: "fv-router-and", Trigger: trigger(router)}
	}

	cases := []struct {
		name          string
		amount        int64
		status        string
		wantAndBranch bool
	}{
		{"both conditions true", 20, "active", true},
		{"amount fails, status passes", 5, "active", false},
		{"amount passes, status fails", 20, "inactive", false},
		{"both conditions false", 5, "inactive", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := engine.New(piece.NewRegistry()).ExecuteBegin(buildFlow(), engine.BeginInput{
				TriggerPayload: map[string]any{"amount": c.amount, "status": c.status},
			})
			if state.Verdict.Status != model.FlowRunSucceeded {
				t.Fatalf("verdict = %+v", state.Verdict)
			}
			_, matchedRan := state.Steps["matched"]
			_, fallbackRan := state.Steps["fell_through"]
			if matchedRan != c.wantAndBranch || fallbackRan == c.wantAndBranch {
				t.Fatalf("matched=%v fallback=%v, want AND-branch match=%v (both conditions must be true)", matchedRan, fallbackRan, c.wantAndBranch)
			}
		})
	}
}

func TestRouterCondition_OrAcrossGroups(t *testing.T) {
	buildFlow := func() *model.FlowVersion {
		router := &model.FlowAction{
			Name: "router", DisplayName: "Router", Type: model.ActionRouter,
			Router: &model.RouterSettings{
				ExecutionType: model.RouterExecuteFirstMatch,
				Branches: []model.RouterBranch{
					// two groups, one condition each — EITHER makes the branch match (OR).
					{Name: "vip_or_big_spender", Type: model.BranchCondition, Conditions: [][]model.Condition{
						{{Operator: model.OpTextExactlyMatches, FirstValue: "{{ trigger_1.output.tier }}", SecondValue: "vip"}},
						{{Operator: model.OpNumberGreaterThan, FirstValue: "{{ trigger_1.output.amount }}", SecondValue: "1000"}},
					}},
					{Name: "fallback", Type: model.BranchFallback},
				},
				Children: []*model.FlowAction{
					codeAction("matched", echoSource, map[string]any{"branch": "vip_or_big_spender"}),
					codeAction("fell_through", echoSource, map[string]any{"branch": "fallback"}),
				},
			},
		}
		return &model.FlowVersion{ID: "fv-router-or", Trigger: trigger(router)}
	}

	cases := []struct {
		name       string
		tier       string
		amount     int64
		wantOrHits bool
	}{
		{"first group matches (vip, low amount)", "vip", 1, true},
		{"second group matches (not vip, high amount)", "basic", 5000, true},
		{"both groups match", "vip", 5000, true},
		{"neither group matches", "basic", 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := engine.New(piece.NewRegistry()).ExecuteBegin(buildFlow(), engine.BeginInput{
				TriggerPayload: map[string]any{"tier": c.tier, "amount": c.amount},
			})
			if state.Verdict.Status != model.FlowRunSucceeded {
				t.Fatalf("verdict = %+v", state.Verdict)
			}
			_, matchedRan := state.Steps["matched"]
			_, fallbackRan := state.Steps["fell_through"]
			if matchedRan != c.wantOrHits || fallbackRan == c.wantOrHits {
				t.Fatalf("matched=%v fallback=%v, want OR-branch match=%v (either group being true is enough)", matchedRan, fallbackRan, c.wantOrHits)
			}
		})
	}
}

func TestLoopOnItems(t *testing.T) {
	loop := &model.FlowAction{
		Name: "loop", DisplayName: "Loop", Type: model.ActionLoopOnItems,
		Loop: &model.LoopSettings{
			Items:           "{{ [10, 20, 30] }}",
			FirstLoopAction: codeAction("echo_step", echoSource, map[string]any{"item": "{{ loop.output.item }}", "index": "{{ loop.output.index }}"}),
		},
	}
	fv := &model.FlowVersion{ID: "fv-loop", Trigger: trigger(loop)}
	state := engine.New(piece.NewRegistry()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	loopOut := state.Steps["loop"]
	if loopOut.Status != model.StepSucceeded || len(loopOut.Iterations) != 3 {
		t.Fatalf("loop step = %+v", loopOut)
	}
	if loopOut.LastItem != int64(30) || loopOut.LastIndex != 3 {
		t.Fatalf("last item/index = %v/%v", loopOut.LastItem, loopOut.LastIndex)
	}
	wantItems := []int64{10, 20, 30}
	for i, want := range wantItems {
		got := loopOut.Iterations[i]["echo_step"].Output.(map[string]any)
		if got["item"] != want || got["index"] != int64(i+1) {
			t.Fatalf("iteration %d = %+v, want item=%d index=%d", i, got, want, i+1)
		}
	}
}

func TestContinueOnFailure(t *testing.T) {
	echoStep := codeAction("echo_step", echoSource, map[string]any{"key": "{{ 1 + 2 }}"})
	throwing := &model.FlowAction{
		Name: "throwing", DisplayName: "Throwing", Type: model.ActionCode,
		Code:       &model.CodeSettings{Input: map[string]any{}, Source: `() => { throw new Error("boom") }`},
		Error:      &model.ErrorHandling{ContinueOnFailure: true},
		NextAction: echoStep,
	}
	fv := &model.FlowVersion{ID: "fv-cof", Trigger: trigger(throwing)}
	state := engine.New(piece.NewRegistry()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED (continueOnFailure should absorb the failure)", state.Verdict)
	}
	if state.Steps["throwing"].Status != model.StepFailed {
		t.Fatalf("throwing status = %v", state.Steps["throwing"].Status)
	}
	assertOutput(t, state, "echo_step", map[string]any{"key": int64(3)})
}

// TestRetryOnFailureSucceedsOnThirdAttempt proves the action is really
// re-invoked on retry, not just re-reported as failed. Uses a PIECE action
// (a plain Go closure) rather than CODE: sandbox.Run deliberately gives CODE
// a fresh goja.Runtime per call for isolation, so a JS-side counter can't
// persist across attempts by design — a Go closure counter sidesteps that
// entirely and is simpler than the TS extraction's equivalent test, which
// had to persist its counter to a sibling file because each retry there
// forks a real child process.
func TestRetryOnFailureSucceedsOnThirdAttempt(t *testing.T) {
	registry := piece.NewRegistry()
	attempts := 0
	registry.Register(piece.Piece{
		Name: "test", DisplayName: "Test",
		Actions: map[string]piece.Action{
			"flaky": {
				Name: "flaky", DisplayName: "Flaky",
				Run: func(ctx piece.ActionContext) (any, error) {
					attempts++
					if attempts < 3 {
						return nil, fmt.Errorf("flaky failure, attempt %d", attempts)
					}
					return map[string]any{"attempts": attempts}, nil
				},
			},
		},
	})

	flaky := &model.FlowAction{
		Name: "flaky", DisplayName: "Flaky", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "test", ActionName: "flaky", Input: map[string]any{}},
		Error: &model.ErrorHandling{RetryOnFailure: true},
	}
	e := engine.New(registry)
	e.Retry = engine.RetryConstants{MaxAttempts: 5, ExponentialBase: 1, IntervalMs: 1}
	fv := &model.FlowVersion{ID: "fv-retry-ok", Trigger: trigger(flaky)}
	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED on the 3rd attempt", state.Verdict)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want exactly 3", attempts)
	}
	assertOutput(t, state, "flaky", map[string]any{"attempts": 3})
}

func TestRetryOnFailureExhausts(t *testing.T) {
	flaky := &model.FlowAction{
		Name: "flaky", DisplayName: "Flaky", Type: model.ActionCode,
		Code:  &model.CodeSettings{Input: map[string]any{}, Source: `() => { throw new Error("still failing") }`},
		Error: &model.ErrorHandling{RetryOnFailure: true},
	}
	e := engine.New(piece.NewRegistry())
	e.Retry = engine.RetryConstants{MaxAttempts: 2, ExponentialBase: 1, IntervalMs: 1}
	fv := &model.FlowVersion{ID: "fv-retry", Trigger: trigger(flaky)}
	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED after exhausting retries", state.Verdict)
	}
	if state.Verdict.FailedStep.Name != "flaky" {
		t.Fatalf("failedStep = %+v", state.Verdict.FailedStep)
	}
}

func TestPauseAndResume(t *testing.T) {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "test", DisplayName: "Test",
		Actions: map[string]piece.Action{
			"pause_and_resume": {
				Name: "pause_and_resume", DisplayName: "Pause And Resume",
				Run: func(ctx piece.ActionContext) (any, error) {
					if ctx.ExecutionType == model.ExecutionBegin {
						ctx.Run.WaitForWaitpoint("wp-1")
						return map[string]any{"paused": true}, nil
					}
					return map[string]any{"resumed": true, "resumePayload": ctx.ResumePayload}, nil
				},
			},
		},
	})

	echoStep := codeAction("echo_step", echoSource, map[string]any{"resumed": "{{ pause_and_resume.output.resumed }}"})
	pauseAction := &model.FlowAction{
		Name: "pause_and_resume", DisplayName: "Pause And Resume", Type: model.ActionPiece,
		Piece:      &model.PieceSettings{PieceName: "test", ActionName: "pause_and_resume", Input: map[string]any{}},
		NextAction: echoStep,
	}
	fv := &model.FlowVersion{ID: "fv-pause", Trigger: trigger(pauseAction)}
	e := engine.New(registry)

	begun := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	if begun.Verdict.Status != model.FlowRunPaused {
		t.Fatalf("verdict = %+v, want PAUSED", begun.Verdict)
	}
	assertOutput(t, begun, "pause_and_resume", map[string]any{"paused": true})
	if _, ok := begun.Steps["echo_step"]; ok {
		t.Fatal("echo_step should not have run before resume")
	}

	resumed := e.ExecuteResume(fv, engine.ResumeInput{PriorState: begun, ResumePayload: map[string]any{"approvedBy": "test-user"}})
	if resumed.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED", resumed.Verdict)
	}
	got := resumed.Steps["pause_and_resume"].Output.(map[string]any)
	if got["resumed"] != true {
		t.Fatalf("resumed output = %+v", got)
	}
	if payload, ok := got["resumePayload"].(map[string]any); !ok || payload["approvedBy"] != "test-user" {
		t.Fatalf("resumePayload = %+v", got["resumePayload"])
	}
	assertOutput(t, resumed, "echo_step", map[string]any{"resumed": true})
}

func TestRealTrigger(t *testing.T) {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "test", DisplayName: "Test",
		Triggers: map[string]piece.Trigger{
			"real_trigger": {
				Name: "real_trigger", DisplayName: "Real Trigger",
				Run: func(ctx piece.TriggerContext) ([]any, error) {
					return []any{map[string]any{"receivedFrom": "real_trigger", "payload": ctx.Payload}}, nil
				},
			},
		},
	})

	echoStep := codeAction("echo_step", echoSource, map[string]any{"from": "{{ trigger_1.output.receivedFrom }}"})
	fv := &model.FlowVersion{ID: "fv-trig", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Real Trigger", Type: model.TriggerPiece,
		PieceName: "test", TriggerName: "real_trigger", Input: map[string]any{},
		NextAction: echoStep,
	}}
	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{
		TriggerPayload: map[string]any{"raw": "event-data", "id": int64(42)},
		ExecuteTrigger: true,
	})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	got := state.Steps["trigger_1"].Output.(map[string]any)
	if got["receivedFrom"] != "real_trigger" {
		t.Fatalf("trigger output = %+v, want run() to have actually executed", got)
	}
	assertOutput(t, state, "echo_step", map[string]any{"from": "real_trigger"})
}

// TestPollingTrigger_DiscoversNewItemsAcrossPolls simulates a POLLING-style
// trigger end-to-end: an external scheduler (not part of this engine — see
// Trigger's doc comment in pkg/piece) calls Run() repeatedly against a
// shared Store; Run() compares a fake data source against the cursor it
// left last time and returns only what's new; each returned item becomes
// its own separate flow run (ExecuteBegin with ExecuteTrigger: false, since
// the trigger already "ran" at the polling layer, not inside any one flow
// run — mirrors how activepieces' worker fans out a trigger's discovered
// items into one engine BEGIN call per item).
func TestPollingTrigger_DiscoversNewItemsAcrossPolls(t *testing.T) {
	var dataSource []map[string]any // grows between polls, simulating new external records arriving
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "test", DisplayName: "Test",
		Triggers: map[string]piece.Trigger{
			"poll_records": {
				Name: "poll_records", DisplayName: "Poll Records",
				Run: func(ctx piece.TriggerContext) ([]any, error) {
					lastIDVal, _ := ctx.Store.Get("last_id")
					lastID, _ := lastIDVal.(int64)
					maxID := lastID
					var fresh []any
					for _, rec := range dataSource {
						id := rec["id"].(int64)
						if id > lastID {
							fresh = append(fresh, rec)
							if id > maxID {
								maxID = id
							}
						}
					}
					ctx.Store.Put("last_id", maxID)
					return fresh, nil
				},
			},
		},
	})

	processed := codeAction("processed", echoSource, map[string]any{"id": "{{ trigger_1.output.id }}"})
	fv := &model.FlowVersion{ID: "fv-poll", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Poll Records", Type: model.TriggerPiece,
		PieceName: "test", TriggerName: "poll_records", Input: map[string]any{},
		NextAction: processed,
	}}

	store := piece.NewMemoryStore()
	e := engine.New(registry)

	poll := func() []any {
		trig, ok := registry.GetTrigger("test", "poll_records")
		if !ok {
			t.Fatal("trigger not found")
		}
		items, err := trig.Run(piece.TriggerContext{Store: store})
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		return items
	}
	runForEach := func(items []any) []*model.ExecutionState {
		var results []*model.ExecutionState
		for _, item := range items {
			results = append(results, e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: item, ExecuteTrigger: false}))
		}
		return results
	}

	// Poll 1: two records already exist when polling starts.
	dataSource = []map[string]any{{"id": int64(1), "name": "a"}, {"id": int64(2), "name": "b"}}
	firstBatch := poll()
	if len(firstBatch) != 2 {
		t.Fatalf("first poll = %d items, want 2", len(firstBatch))
	}
	firstRuns := runForEach(firstBatch)
	for i, wantID := range []int64{1, 2} {
		if firstRuns[i].Verdict.Status != model.FlowRunSucceeded {
			t.Fatalf("run %d verdict = %+v", i, firstRuns[i].Verdict)
		}
		assertOutput(t, firstRuns[i], "processed", map[string]any{"id": wantID})
	}

	// Poll 2, before any new data arrives: the cursor must prevent
	// re-discovering records 1 and 2.
	if second := poll(); len(second) != 0 {
		t.Fatalf("second poll (no new data) = %d items, want 0 — the store cursor should suppress records already seen", len(second))
	}

	// A new record arrives externally.
	dataSource = append(dataSource, map[string]any{"id": int64(3), "name": "c"})

	// Poll 3: must discover ONLY record 3, not re-discover 1 and 2.
	thirdBatch := poll()
	if len(thirdBatch) != 1 {
		t.Fatalf("third poll = %d items, want 1 (only the new record)", len(thirdBatch))
	}
	thirdRuns := runForEach(thirdBatch)
	if thirdRuns[0].Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("third run verdict = %+v", thirdRuns[0].Verdict)
	}
	assertOutput(t, thirdRuns[0], "processed", map[string]any{"id": int64(3)})
}

// TestTrigger_NoEngineLevelRetry_FailsOnFirstAttempt and
// TestTrigger_InternalRetrySucceedsAfterTransientFailures cover trigger
// retries — checked against trigger-helper.ts/flow.operation.ts first:
// there's no backoff-retry wrapper anywhere around trigger execution in
// activepieces, unlike CODE/PIECE actions (runWithExponentialBackoff is
// only ever called from codeExecutor/pieceExecutor). A trigger's run()
// either succeeds on the one attempt ExecuteBegin makes, or the run fails
// immediately — same asymmetry ExecuteBegin here already had (no test had
// ever pinned it down explicitly). "Retrying a trigger" in practice means
// exactly what "rate limiting" and "encryption" meant earlier in this
// series: ordinary logic inside the piece's own Run, not an engine feature
// — the second test proves a trigger can retry its own upstream call
// internally with zero engine involvement.

func TestTrigger_NoEngineLevelRetry_FailsOnFirstAttempt(t *testing.T) {
	registry := piece.NewRegistry()
	var attempts int32
	registry.Register(piece.Piece{
		Name: "flaky_source", DisplayName: "Flaky Source",
		Triggers: map[string]piece.Trigger{
			"poll": {
				Name: "poll", DisplayName: "Poll",
				Run: func(ctx piece.TriggerContext) ([]any, error) {
					atomic.AddInt32(&attempts, 1)
					return nil, fmt.Errorf("upstream API unavailable")
				},
			},
		},
	})
	fv := &model.FlowVersion{ID: "fv-trigger-no-retry", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Poll", Type: model.TriggerPiece,
		PieceName: "flaky_source", TriggerName: "poll", Input: map[string]any{},
		NextAction: codeAction("never_reached", echoSource, map[string]any{}),
	}}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}, ExecuteTrigger: true})

	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED", state.Verdict)
	}
	if state.Verdict.FailedStep == nil || state.Verdict.FailedStep.Message != "upstream API unavailable" {
		t.Fatalf("failedStep = %+v", state.Verdict.FailedStep)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want exactly 1 — the engine must not retry a failed trigger on its own", got)
	}
	if _, ok := state.Steps["never_reached"]; ok {
		t.Fatal("never_reached ran — the flow should never start past a failed trigger")
	}
}

func TestTrigger_InternalRetrySucceedsAfterTransientFailures(t *testing.T) {
	registry := piece.NewRegistry()
	var attempts int32
	registry.Register(piece.Piece{
		Name: "flaky_source", DisplayName: "Flaky Source",
		Triggers: map[string]piece.Trigger{
			"poll": {
				Name: "poll", DisplayName: "Poll",
				Run: func(ctx piece.TriggerContext) ([]any, error) {
					const maxAttempts = 4
					var lastErr error
					for attempt := 1; attempt <= maxAttempts; attempt++ {
						atomic.AddInt32(&attempts, 1)
						if attempt < 3 {
							lastErr = fmt.Errorf("attempt %d: upstream API unavailable", attempt)
							time.Sleep(time.Millisecond)
							continue
						}
						return []any{map[string]any{"fetched": true}}, nil
					}
					return nil, lastErr
				},
			},
		},
	})
	fv := &model.FlowVersion{ID: "fv-trigger-internal-retry", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Poll", Type: model.TriggerPiece,
		PieceName: "flaky_source", TriggerName: "poll", Input: map[string]any{},
		NextAction: codeAction("after_trigger", echoSource, map[string]any{"fetched": "{{ trigger_1.output.fetched }}"}),
	}}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}, ExecuteTrigger: true})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED — the trigger's own retries should have recovered before ExecuteBegin's single call returns", state.Verdict)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("internal attempts = %d, want 3 (2 failures + 1 success) — proof the retry loop actually ran, not that it just succeeded on luck", got)
	}
	assertOutput(t, state, "after_trigger", map[string]any{"fetched": true})
}

// TestMultipleFlowsSameTrigger_* prove state isolation between multiple
// flows using the exact same trigger — checked against activepieces'
// store.ts first: createContextStore's createKey namespaces every key as
// 'flow_' + flowId + '/' + key for the default StoreScope.FLOW, so two
// flows sharing a piece+trigger (a very ordinary setup — e.g. the same
// Slack channel triggering two independent flows) never clash even though
// activepieces itself has exactly ONE store-entries API backing every flow
// in the project. piece.MemoryStore had no such namespacing until this
// request — a real, previously-untested gap: two flows sharing a raw
// MemoryStore instance for the same trigger's cursor genuinely would step
// on each other. The first test below demonstrates that failure mode
// directly (proving the gap was real, not hypothetical); the second shows
// piece.ScopedStore fixing it.

func multiFlowPollingRegistry() (*piece.Registry, *[]map[string]any) {
	var dataSource []map[string]any
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "shared_source", DisplayName: "Shared Source",
		Triggers: map[string]piece.Trigger{
			"poll_records": {
				Name: "poll_records", DisplayName: "Poll Records",
				Run: func(ctx piece.TriggerContext) ([]any, error) {
					lastIDVal, _ := ctx.Store.Get("last_id")
					lastID, _ := lastIDVal.(int64)
					maxID := lastID
					var fresh []any
					for _, rec := range dataSource {
						id := rec["id"].(int64)
						if id > lastID {
							fresh = append(fresh, rec)
							if id > maxID {
								maxID = id
							}
						}
					}
					ctx.Store.Put("last_id", maxID)
					return fresh, nil
				},
			},
		},
	})
	return registry, &dataSource
}

func TestMultipleFlowsSameTrigger_UnscopedSharedStoreClashes(t *testing.T) {
	registry, dataSource := multiFlowPollingRegistry()
	*dataSource = []map[string]any{{"id": int64(1)}, {"id": int64(2)}, {"id": int64(3)}}
	trig, ok := registry.GetTrigger("shared_source", "poll_records")
	if !ok {
		t.Fatal("trigger not found")
	}

	sharedBackend := piece.NewMemoryStore()

	// Flow A polls first, using the raw shared backend directly (no scoping)
	// — advances the literal "last_id" key to 3.
	itemsA, err := trig.Run(piece.TriggerContext{Store: sharedBackend})
	if err != nil || len(itemsA) != 3 {
		t.Fatalf("flow A's first poll = %v, %v, want 3 items", itemsA, err)
	}

	// Flow B polls next, ALSO using the raw shared backend — its very first
	// poll ever. Without per-flow scoping it reads the SAME "last_id" key
	// flow A just wrote, so it wrongly believes it has already seen records
	// 1-3, even though flow B itself has never run before.
	itemsB, err := trig.Run(piece.TriggerContext{Store: sharedBackend})
	if err != nil {
		t.Fatalf("flow B's first poll: %v", err)
	}
	if len(itemsB) != 0 {
		t.Fatalf("flow B's first poll = %d items, want 0 — this assertion documents the BUG this test exists to demonstrate: an unscoped shared store leaks flow A's cursor into flow B", len(itemsB))
	}
}

func TestMultipleFlowsSameTrigger_ScopedStoreIsolatesCursors(t *testing.T) {
	registry, dataSource := multiFlowPollingRegistry()
	*dataSource = []map[string]any{{"id": int64(1)}, {"id": int64(2)}, {"id": int64(3)}}
	trig, ok := registry.GetTrigger("shared_source", "poll_records")
	if !ok {
		t.Fatal("trigger not found")
	}

	// One shared backend — mirrors activepieces' single store-entries API
	// serving every flow — wrapped per flow so each gets its own cursor.
	sharedBackend := piece.NewMemoryStore()
	storeForFlowA := &piece.ScopedStore{Underlying: sharedBackend, FlowID: "flow-A"}
	storeForFlowB := &piece.ScopedStore{Underlying: sharedBackend, FlowID: "flow-B"}

	itemsA1, err := trig.Run(piece.TriggerContext{Store: storeForFlowA})
	if err != nil || len(itemsA1) != 3 {
		t.Fatalf("flow A's first poll = %v, %v, want 3 items", itemsA1, err)
	}

	// Flow B's first poll — must independently see all 3 records too, since
	// its own scoped cursor has never been touched, regardless of flow A
	// having already advanced past them on the same underlying backend.
	itemsB1, err := trig.Run(piece.TriggerContext{Store: storeForFlowB})
	if err != nil || len(itemsB1) != 3 {
		t.Fatalf("flow B's first poll = %v, %v, want 3 items (independent of flow A's progress)", itemsB1, err)
	}

	*dataSource = append(*dataSource, map[string]any{"id": int64(4)})

	itemsA2, err := trig.Run(piece.TriggerContext{Store: storeForFlowA})
	if err != nil || len(itemsA2) != 1 || itemsA2[0].(map[string]any)["id"] != int64(4) {
		t.Fatalf("flow A's second poll = %v, %v, want only record 4", itemsA2, err)
	}
	itemsB2, err := trig.Run(piece.TriggerContext{Store: storeForFlowB})
	if err != nil || len(itemsB2) != 1 || itemsB2[0].(map[string]any)["id"] != int64(4) {
		t.Fatalf("flow B's second poll = %v, %v, want only record 4 (its own independent cursor also advanced to 3 after its first poll)", itemsB2, err)
	}
}

// TestMultiTenancy_* prove project-level (tenant) isolation — checked
// against activepieces first: createContextStore's signature is
// (apiUrl, prefix, flowId, engineToken) — no projectId parameter at all.
// Project/tenant isolation in the real system isn't an engine mechanism
// either; it's enforced by the SERVER validating the engineToken's implicit
// project scope on every store/connection lookup. There is no engine-level
// "ProjectScope" to port, same boundary as OAuth2 refresh and encryption
// earlier in this series. What IS worth proving: the two realistic ways to
// actually get tenant isolation with this library.

// TestMultiTenancy_ComposedProjectAndFlowScoping shows the FIRST way: reuse
// piece.ScopedStore (already built for flow-level isolation) with a
// composite key that folds the project in too — zero new engine code,
// exactly the same "already had everything needed" shape as the OAuth2 and
// file-attachment findings. Two different projects each happen to have a
// flow literally named "flow-1" (a realistic collision — e.g. both created
// from the same template) polling the same trigger; without folding project
// into the scope key this would clash exactly like TestMultipleFlowsSameTrigger_UnscopedSharedStoreClashes
// did for two flows in the same project.
func TestMultiTenancy_ComposedProjectAndFlowScoping(t *testing.T) {
	registry, dataSourceA := multiFlowPollingRegistry()
	trig, ok := registry.GetTrigger("shared_source", "poll_records")
	if !ok {
		t.Fatal("trigger not found")
	}
	*dataSourceA = []map[string]any{{"id": int64(1)}, {"id": int64(2)}}

	sharedBackend := piece.NewMemoryStore()
	storeForProjectAFlow1 := &piece.ScopedStore{Underlying: sharedBackend, FlowID: "project-A/flow-1"}
	storeForProjectBFlow1 := &piece.ScopedStore{Underlying: sharedBackend, FlowID: "project-B/flow-1"}

	itemsA, err := trig.Run(piece.TriggerContext{Store: storeForProjectAFlow1})
	if err != nil || len(itemsA) != 2 {
		t.Fatalf("project A's poll = %v, %v, want 2 items", itemsA, err)
	}

	// Project B's OWN "flow-1" — same literal flow name, different project —
	// must not inherit project A's cursor.
	itemsB, err := trig.Run(piece.TriggerContext{Store: storeForProjectBFlow1})
	if err != nil || len(itemsB) != 2 {
		t.Fatalf("project B's poll = %v, %v, want 2 items — same flow ID as project A must not mean shared state across the project boundary", itemsB, err)
	}
}

// TestMultiTenancy_SeparateEnginesFullyIsolated shows the SECOND, more
// robust way: one *engine.Engine per tenant. This is the pattern this
// project actually recommends for real multi-tenancy — not because
// composed scope keys (above) don't work, but because a single shared
// Engine.Files (piece.MemoryFileWriter) has no per-tenant access control at
// all: two tenants sharing one Engine could each read files the OTHER
// uploaded, since MemoryFileWriter.Get has no notion of "whose file is
// this." A key-prefixing wrapper wouldn't fix that (the reference would
// still resolve on the same shared Get), so — documented honestly rather
// than papered over — the real answer is separate Engine instances (or your
// own access-controlled FileWriter) per tenant, proven here to give
// complete isolation across every axis at once: Registry, Files, Steps.
func TestMultiTenancy_SeparateEnginesFullyIsolated(t *testing.T) {
	newTenantEngine := func(greeting string) *engine.Engine {
		registry := piece.NewRegistry()
		registry.Register(piece.Piece{
			Name: "tenant_piece", DisplayName: "Tenant Piece",
			Actions: map[string]piece.Action{
				"greet": {
					Name: "greet",
					Run: func(ctx piece.ActionContext) (any, error) {
						url, err := ctx.Files.Write("greeting.txt", []byte(greeting))
						if err != nil {
							return nil, err
						}
						return map[string]any{"greeting": greeting, "fileURL": url}, nil
					},
				},
			},
		})
		return engine.New(registry)
	}

	action := &model.FlowAction{
		Name: "greet", DisplayName: "Greet", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "tenant_piece", ActionName: "greet", Input: map[string]any{}},
	}
	fv := &model.FlowVersion{ID: "fv-tenant", Trigger: trigger(action)}

	tenantA := newTenantEngine("hello from tenant A")
	tenantB := newTenantEngine("hello from tenant B")

	stateA := tenantA.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	stateB := tenantB.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	outA := stateA.Steps["greet"].Output.(map[string]any)
	outB := stateB.Steps["greet"].Output.(map[string]any)
	if outA["greeting"] != "hello from tenant A" || outB["greeting"] != "hello from tenant B" {
		t.Fatalf("outA=%+v outB=%+v — same *model.FlowVersion, but each tenant's own piece registration must run its own closure", outA, outB)
	}

	// Each writer's ID counter starts independently at 1, so with identical
	// filenames the two tenants' URLs can coincidentally be the SAME string
	// (both "memfile://f1/greeting.txt") — a real, if benign, consequence of
	// per-instance-local IDs worth knowing about (a URL is only meaningful
	// within the writer instance that produced it, never globally unique),
	// but not itself a leak. The actual isolation check has to compare
	// CONTENT: tenant A's writer, looked up under whatever URL tenant B got,
	// must never return tenant B's actual bytes.
	writerA := tenantA.Files.(*piece.MemoryFileWriter)
	writerB := tenantB.Files.(*piece.MemoryFileWriter)
	if data, ok := writerA.Get(outB["fileURL"].(string)); ok && string(data) == "hello from tenant B" {
		t.Fatal("tenant A's file writer returned tenant B's actual content — engines must not share a FileWriter across tenants")
	}
	if data, ok := writerB.Get(outA["fileURL"].(string)); ok && string(data) == "hello from tenant A" {
		t.Fatal("tenant B's file writer returned tenant A's actual content")
	}
}

// A "sync webhook, immediate response" flow: a webhook-style PIECE trigger
// (real, non-EMPTY — see TestRealTrigger) feeding a PIECE action that calls
// Run.Stop or Run.Respond — activepieces' mechanism for a flow to reply to
// its own webhook caller synchronously instead of making them wait for the
// whole run to finish. Two genuinely distinct behaviors, both proven here:
// Stop ends the run immediately after replying; Respond replies but lets the
// run keep going.

func buildSyncWebhookRegistry(t *testing.T) *piece.Registry {
	t.Helper()
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "webhook", DisplayName: "Webhook",
		Triggers: map[string]piece.Trigger{
			"catch_hook": {
				Name: "catch_hook", DisplayName: "Catch Webhook",
				Run: func(ctx piece.TriggerContext) ([]any, error) {
					return []any{ctx.Payload}, nil
				},
			},
		},
		Actions: map[string]piece.Action{
			"stop_now": {
				Name: "stop_now", DisplayName: "Stop And Respond Now",
				Run: func(ctx piece.ActionContext) (any, error) {
					ctx.Run.Stop(&model.WebhookResponse{Status: 200, Body: map[string]any{"ok": true}})
					return map[string]any{"stopped": true}, nil
				},
			},
			"respond_now": {
				Name: "respond_now", DisplayName: "Respond Now",
				Run: func(ctx piece.ActionContext) (any, error) {
					ctx.Run.Respond(&model.WebhookResponse{Status: 202, Body: map[string]any{"accepted": true}})
					return map[string]any{"responded": true}, nil
				},
			},
		},
	})
	return registry
}

func webhookTrigger(next *model.FlowAction) *model.FlowTrigger {
	return &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Catch Webhook", Type: model.TriggerPiece,
		PieceName: "webhook", TriggerName: "catch_hook", Input: map[string]any{},
		NextAction: next,
	}
}

func TestSyncWebhook_StopRespondsImmediatelyAndEndsRun(t *testing.T) {
	registry := buildSyncWebhookRegistry(t)
	stopAction := &model.FlowAction{
		Name: "stop_now", DisplayName: "Stop Now", Type: model.ActionPiece,
		Piece:      &model.PieceSettings{PieceName: "webhook", ActionName: "stop_now", Input: map[string]any{}},
		NextAction: codeAction("after_stop", echoSource, map[string]any{"reached": true}),
	}
	fv := &model.FlowVersion{ID: "fv-sync-stop", Trigger: webhookTrigger(stopAction)}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{
		TriggerPayload: map[string]any{"event": "order.created"},
		ExecuteTrigger: true,
	})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED — stopping is not a failure", state.Verdict)
	}
	if state.Verdict.StopResponse == nil {
		t.Fatal("Verdict.StopResponse is nil, want the response passed to Run.Stop")
	}
	if state.Verdict.StopResponse.Status != 200 {
		t.Fatalf("StopResponse.Status = %d, want 200", state.Verdict.StopResponse.Status)
	}
	body := state.Verdict.StopResponse.Body.(map[string]any)
	if body["ok"] != true {
		t.Fatalf("StopResponse.Body = %+v, want {ok:true}", body)
	}

	assertOutput(t, state, "stop_now", map[string]any{"stopped": true})
	if _, ok := state.Steps["after_stop"]; ok {
		t.Fatal("after_stop ran — Stop should have ended the run before NextAction")
	}
}

func TestSyncWebhook_RespondContinuesRunningAfterward(t *testing.T) {
	registry := buildSyncWebhookRegistry(t)
	continues := codeAction("continues", echoSource, map[string]any{"finished": true})
	respondAction := &model.FlowAction{
		Name: "respond_now", DisplayName: "Respond Now", Type: model.ActionPiece,
		Piece:      &model.PieceSettings{PieceName: "webhook", ActionName: "respond_now", Input: map[string]any{}},
		NextAction: continues,
	}
	fv := &model.FlowVersion{ID: "fv-sync-respond", Trigger: webhookTrigger(respondAction)}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{
		TriggerPayload: map[string]any{"event": "order.created"},
		ExecuteTrigger: true,
	})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED (normal completion — Respond doesn't force an early verdict)", state.Verdict)
	}
	if state.Verdict.StopResponse != nil {
		t.Fatalf("Verdict.StopResponse = %+v, want nil — Respond is not Stop", state.Verdict.StopResponse)
	}
	if state.RespondedEarly == nil {
		t.Fatal("RespondedEarly is nil, want the response passed to Run.Respond")
	}
	if state.RespondedEarly.Status != 202 {
		t.Fatalf("RespondedEarly.Status = %d, want 202", state.RespondedEarly.Status)
	}
	body := state.RespondedEarly.Body.(map[string]any)
	if body["accepted"] != true {
		t.Fatalf("RespondedEarly.Body = %+v, want {accepted:true}", body)
	}

	// the run kept going PAST the respond action, unlike Stop.
	assertOutput(t, state, "continues", map[string]any{"finished": true})
}

func TestLogSizeExceeded(t *testing.T) {
	echoStep := codeAction("echo_step", echoSource, map[string]any{"key": bigString(10000)})
	fv := &model.FlowVersion{ID: "fv-log", Trigger: trigger(echoStep)}
	e := engine.New(piece.NewRegistry())
	e.MaxLogSizeBytes = 100

	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunLogSizeExceeded {
		t.Fatalf("verdict = %+v, want LOG_SIZE_EXCEEDED", state.Verdict)
	}
	if state.Verdict.FailedStep == nil || state.Verdict.FailedStep.Message != "Flow run logs size exceeded" {
		t.Fatalf("failedStep = %+v", state.Verdict.FailedStep)
	}
}

// TestPauseInsideLoop_BeginPausesOnSecondItem and
// TestPauseInsideLoop_ResumeContinuesFromPausedIteration together prove the
// scenario the mislabeling bug (loop step hardcoded to StepFailed on any
// nested non-success, including a pause) silently broke: RESUME used to
// restart the whole loop from item 1 instead of continuing from the paused
// iteration, discarding the resume payload entirely. Split into two tests
// sharing one flow builder so each phase's assertions are easy to read
// independently, mirroring how the TS extraction split BEGIN/RESUME into
// separate `it()` blocks for its own pause/resume test.
func buildLoopPauseFlow(t *testing.T) (*model.FlowVersion, *engine.Engine) {
	t.Helper()
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "test", DisplayName: "Test",
		Actions: map[string]piece.Action{
			"handle_item": {
				Name: "handle_item", DisplayName: "Handle Item",
				Run: func(ctx piece.ActionContext) (any, error) {
					item, _ := ctx.Input["item"].(int64)
					if item == 20 && ctx.ExecutionType == model.ExecutionBegin {
						ctx.Run.WaitForWaitpoint("wp-loop-item-2")
						return map[string]any{"paused": true, "item": item}, nil
					}
					return map[string]any{"handled": item, "resumed": ctx.ExecutionType == model.ExecutionResume}, nil
				},
			},
		},
	})

	afterLoop := codeAction("after_loop", echoSource, map[string]any{"finished": true})
	handleItem := &model.FlowAction{
		Name: "handle_item", DisplayName: "Handle Item", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "test", ActionName: "handle_item", Input: map[string]any{"item": "{{ loop.output.item }}"}},
	}
	loop := &model.FlowAction{
		Name: "loop", DisplayName: "Loop", Type: model.ActionLoopOnItems,
		Loop:       &model.LoopSettings{Items: "{{ [10, 20, 30] }}", FirstLoopAction: handleItem},
		NextAction: afterLoop,
	}
	fv := &model.FlowVersion{ID: "fv-loop-pause", Trigger: trigger(loop)}
	return fv, engine.New(registry)
}

func TestPauseInsideLoop_BeginPausesOnSecondItem(t *testing.T) {
	fv, e := buildLoopPauseFlow(t)
	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunPaused {
		t.Fatalf("verdict = %+v, want PAUSED", state.Verdict)
	}

	loopOut := state.Steps["loop"]
	if loopOut.Status != model.StepPaused {
		t.Fatalf("loop step status = %v, want PAUSED (this is the bug this test was written to catch)", loopOut.Status)
	}
	if loopOut.LastIndex != 2 || loopOut.LastItem != int64(20) {
		t.Fatalf("last item/index = %v/%v, want 20/2", loopOut.LastItem, loopOut.LastIndex)
	}
	if len(loopOut.Iterations) != 2 {
		t.Fatalf("iterations = %d, want 2 (item 10 succeeded, item 20 paused)", len(loopOut.Iterations))
	}

	firstIter := loopOut.Iterations[0]["handle_item"]
	if firstIter.Status != model.StepSucceeded || firstIter.Output.(map[string]any)["handled"] != int64(10) {
		t.Fatalf("iteration 1 (item 10) = %+v, want it to have already succeeded", firstIter)
	}
	pausedIter := loopOut.Iterations[1]["handle_item"]
	if pausedIter.Status != model.StepPaused {
		t.Fatalf("iteration 2 (item 20) status = %v, want PAUSED", pausedIter.Status)
	}

	if _, ok := state.Steps["after_loop"]; ok {
		t.Fatal("after_loop should not run before the loop finishes resuming")
	}
}

func TestPauseInsideLoop_ResumeContinuesFromPausedIteration(t *testing.T) {
	fv, e := buildLoopPauseFlow(t)
	begun := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	if begun.Verdict.Status != model.FlowRunPaused {
		t.Fatalf("setup: BEGIN verdict = %+v, want PAUSED", begun.Verdict)
	}

	resumed := e.ExecuteResume(fv, engine.ResumeInput{PriorState: begun, ResumePayload: map[string]any{"approved": true}})

	if resumed.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED", resumed.Verdict)
	}

	loopOut := resumed.Steps["loop"]
	if loopOut.Status != model.StepSucceeded {
		t.Fatalf("loop step status = %v, want SUCCEEDED", loopOut.Status)
	}
	if loopOut.LastIndex != 3 || loopOut.LastItem != int64(30) {
		t.Fatalf("last item/index = %v/%v, want 30/3 (the loop must continue past the resumed iteration)", loopOut.LastItem, loopOut.LastIndex)
	}
	if len(loopOut.Iterations) != 3 {
		t.Fatalf("iterations = %d, want 3", len(loopOut.Iterations))
	}

	// iteration 1 (item 10) was already done before the pause — untouched by resume.
	firstIter := loopOut.Iterations[0]["handle_item"].Output.(map[string]any)
	if firstIter["handled"] != int64(10) || firstIter["resumed"] != false {
		t.Fatalf("iteration 1 = %+v, should be unchanged from BEGIN", firstIter)
	}

	// iteration 2 (item 20) is the one that paused — must have actually re-run
	// with ExecutionType RESUME, not just been left as PAUSED or silently
	// restarted from BEGIN.
	secondIter := loopOut.Iterations[1]["handle_item"]
	if secondIter.Status != model.StepSucceeded {
		t.Fatalf("iteration 2 status = %v, want SUCCEEDED after resume", secondIter.Status)
	}
	secondOut := secondIter.Output.(map[string]any)
	if secondOut["handled"] != int64(20) || secondOut["resumed"] != true {
		t.Fatalf("iteration 2 output = %+v, want {handled:20, resumed:true}", secondOut)
	}

	// iteration 3 (item 30) never got a chance to run before the pause — proves
	// the loop actually continued past the resumed iteration, not just replayed it.
	thirdOut := loopOut.Iterations[2]["handle_item"].Output.(map[string]any)
	if thirdOut["handled"] != int64(30) || thirdOut["resumed"] != false {
		t.Fatalf("iteration 3 = %+v, want {handled:30, resumed:false}", thirdOut)
	}

	// and the chain continued past the loop step itself.
	assertOutput(t, resumed, "after_loop", map[string]any{"finished": true})
}

// TestRouterInsideLoop_Pause and TestRouterInsideLoop_Resume prove a ROUTER
// nested inside a LOOP_ON_ITEMS handles a pause inside its matched branch —
// the scenario that surfaced the router bug fixed above: before the fix,
// resuming this exact shape silently reported SUCCEEDED while the piece
// inside the branch was still actually PAUSED and had never re-run.
//
// Flow per iteration: ROUTER routes item>10 to a PIECE that pauses the first
// time it sees item 20; everything else (including item 20 on resume) falls
// to either the same PIECE past its pause, or a CODE fallback branch for
// small items. Items: [5, 20, 5] — exercises both branches AND the pause,
// in one loop.
func buildRouterInLoopFlow(t *testing.T) (*model.FlowVersion, *engine.Engine) {
	t.Helper()
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "test", DisplayName: "Test",
		Actions: map[string]piece.Action{
			"big_handler": {
				Name: "big_handler", DisplayName: "Big Handler",
				Run: func(ctx piece.ActionContext) (any, error) {
					item, _ := ctx.Input["item"].(int64)
					if ctx.ExecutionType == model.ExecutionBegin {
						ctx.Run.WaitForWaitpoint("wp-router-loop")
						return map[string]any{"paused": true, "item": item}, nil
					}
					return map[string]any{"handled": item, "big": true, "resumed": true}, nil
				},
			},
		},
	})

	bigHandler := &model.FlowAction{
		Name: "big_handler", DisplayName: "Big Handler", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "test", ActionName: "big_handler", Input: map[string]any{"item": "{{ loop.output.item }}"}},
	}
	smallHandler := codeAction("small_handler", echoSource, map[string]any{"handled": "{{ loop.output.item }}", "small": true})

	router := &model.FlowAction{
		Name: "router", DisplayName: "Router", Type: model.ActionRouter,
		Router: &model.RouterSettings{
			ExecutionType: model.RouterExecuteFirstMatch,
			Branches: []model.RouterBranch{
				{Name: "big", Type: model.BranchCondition, Conditions: [][]model.Condition{{
					{Operator: model.OpNumberGreaterThan, FirstValue: "{{ loop.output.item }}", SecondValue: "10"},
				}}},
				{Name: "small", Type: model.BranchFallback},
			},
			Children: []*model.FlowAction{bigHandler, smallHandler},
		},
	}
	loop := &model.FlowAction{
		Name: "loop", DisplayName: "Loop", Type: model.ActionLoopOnItems,
		Loop:       &model.LoopSettings{Items: "{{ [5, 20, 5] }}", FirstLoopAction: router},
		NextAction: codeAction("after_loop", echoSource, map[string]any{"finished": true}),
	}
	fv := &model.FlowVersion{ID: "fv-router-loop", Trigger: trigger(loop)}
	return fv, engine.New(registry)
}

func TestRouterInsideLoop_Pause(t *testing.T) {
	fv, e := buildRouterInLoopFlow(t)
	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunPaused {
		t.Fatalf("verdict = %+v, want PAUSED", state.Verdict)
	}
	loopOut := state.Steps["loop"]
	if loopOut.Status != model.StepPaused {
		t.Fatalf("loop step status = %v, want PAUSED", loopOut.Status)
	}
	if loopOut.LastIndex != 2 || loopOut.LastItem != int64(20) {
		t.Fatalf("last item/index = %v/%v, want 20/2", loopOut.LastItem, loopOut.LastIndex)
	}
	if len(loopOut.Iterations) != 2 {
		t.Fatalf("iterations = %d, want 2", len(loopOut.Iterations))
	}

	// iteration 1 (item 5): took the fallback branch, ran to completion —
	// its own router step must be SUCCEEDED (not skipped, not mislabeled).
	firstRouter := loopOut.Iterations[0]["router"]
	if firstRouter.Status != model.StepSucceeded {
		t.Fatalf("iteration 1 router status = %v, want SUCCEEDED", firstRouter.Status)
	}
	firstSmall := loopOut.Iterations[0]["small_handler"].Output.(map[string]any)
	if firstSmall["handled"] != int64(5) {
		t.Fatalf("iteration 1 small_handler = %+v", firstSmall)
	}
	if _, ok := loopOut.Iterations[0]["big_handler"]; ok {
		t.Fatal("iteration 1 (item 5) should not have taken the big branch")
	}

	// iteration 2 (item 20): took the big/conditional branch, and the router
	// itself must report PAUSED (this is the bug this test exists to catch) —
	// not SUCCEEDED with the branch's paused child silently orphaned.
	secondRouter := loopOut.Iterations[1]["router"]
	if secondRouter.Status != model.StepPaused {
		t.Fatalf("iteration 2 router status = %v, want PAUSED (this is the bug this test was written to catch)", secondRouter.Status)
	}
	if loopOut.Iterations[1]["big_handler"].Status != model.StepPaused {
		t.Fatalf("iteration 2 big_handler status = %v, want PAUSED", loopOut.Iterations[1]["big_handler"].Status)
	}

	if _, ok := state.Steps["after_loop"]; ok {
		t.Fatal("after_loop should not run before the loop finishes resuming")
	}
}

func TestRouterInsideLoop_Resume(t *testing.T) {
	fv, e := buildRouterInLoopFlow(t)
	begun := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	if begun.Verdict.Status != model.FlowRunPaused {
		t.Fatalf("setup: BEGIN verdict = %+v, want PAUSED", begun.Verdict)
	}

	resumed := e.ExecuteResume(fv, engine.ResumeInput{PriorState: begun, ResumePayload: map[string]any{"ok": true}})

	if resumed.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED", resumed.Verdict)
	}
	loopOut := resumed.Steps["loop"]
	if loopOut.Status != model.StepSucceeded {
		t.Fatalf("loop step status = %v, want SUCCEEDED", loopOut.Status)
	}
	if loopOut.LastIndex != 3 || loopOut.LastItem != int64(5) {
		t.Fatalf("last item/index = %v/%v, want 5/3", loopOut.LastItem, loopOut.LastIndex)
	}
	if len(loopOut.Iterations) != 3 {
		t.Fatalf("iterations = %d, want 3", len(loopOut.Iterations))
	}

	// iteration 2 (item 20) is the one that paused inside its router branch —
	// must have actually re-run the branch with ExecutionType RESUME, not
	// been silently skipped by the router's own IsCompleted short-circuit.
	secondRouter := loopOut.Iterations[1]["router"]
	if secondRouter.Status != model.StepSucceeded {
		t.Fatalf("iteration 2 router status = %v, want SUCCEEDED after resume", secondRouter.Status)
	}
	secondHandler := loopOut.Iterations[1]["big_handler"]
	if secondHandler.Status != model.StepSucceeded {
		t.Fatalf("iteration 2 big_handler status = %v, want SUCCEEDED after resume", secondHandler.Status)
	}
	secondOut := secondHandler.Output.(map[string]any)
	if secondOut["resumed"] != true || secondOut["handled"] != int64(20) {
		t.Fatalf("iteration 2 big_handler output = %+v, want {handled:20, big:true, resumed:true}", secondOut)
	}

	// iteration 3 (item 5, fallback branch again) never ran before the pause —
	// proves the loop actually continued past the resumed iteration.
	thirdOut := loopOut.Iterations[2]["small_handler"].Output.(map[string]any)
	if thirdOut["handled"] != int64(5) {
		t.Fatalf("iteration 3 small_handler = %+v", thirdOut)
	}

	assertOutput(t, resumed, "after_loop", map[string]any{"finished": true})
}

// --- ExecuteActionRun (standalone, no flow) --------------------------------
//
// This whole section didn't exist before this feature request — unlike the
// loop/router pause bugs above, there was no partial/broken implementation
// to find; ExecuteActionRun and the ActionRunMode guard were added from
// scratch, modeled directly on activepieces' actionRunStepRunner.ts (which
// reuses the exact same per-action executor with an empty context and
// actionRunMode: true — see engine.go's doc comment on ExecuteActionRun).

func TestActionRun_Code(t *testing.T) {
	action := codeAction("standalone", echoSource, map[string]any{"key": "{{ 1 + 2 }}"})
	state := engine.New(piece.NewRegistry()).ExecuteActionRun(action, engine.ActionRunInput{})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	if len(state.Steps) != 1 {
		t.Fatalf("steps = %+v, want exactly the one standalone step (no trigger, no flow)", state.Steps)
	}
	assertOutput(t, state, "standalone", map[string]any{"key": int64(3)})
}

func TestActionRun_PieceWithMockContext(t *testing.T) {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "test", DisplayName: "Test",
		Actions: map[string]piece.Action{
			"greet": {
				Name: "greet",
				Run: func(ctx piece.ActionContext) (any, error) {
					name, _ := ctx.Input["name"].(string)
					return map[string]any{"greeting": "hello, " + name}, nil
				},
			},
		},
	})
	action := &model.FlowAction{
		Name: "greet_step", DisplayName: "Greet", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "test", ActionName: "greet", Input: map[string]any{
			// references mock sample data for a step that would exist in a real
			// flow but doesn't here — activepieces' "test this step with sample data".
			"name": "{{ some_prior_step.output.name }}",
		}},
	}
	state := engine.New(registry).ExecuteActionRun(action, engine.ActionRunInput{
		Context: map[string]*model.StepOutput{
			"some_prior_step": {Status: model.StepSucceeded, Output: map[string]any{"name": "sample-data"}},
		},
	})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	assertOutput(t, state, "greet_step", map[string]any{"greeting": "hello, sample-data"})
}

func TestActionRun_PauseIsRejected(t *testing.T) {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "test", DisplayName: "Test",
		Actions: map[string]piece.Action{
			"tries_to_pause": {
				Name: "tries_to_pause",
				Run: func(ctx piece.ActionContext) (any, error) {
					ctx.Run.WaitForWaitpoint("wp-standalone")
					return map[string]any{"paused": true}, nil
				},
			},
		},
	})
	action := &model.FlowAction{
		Name: "tries_to_pause", DisplayName: "Tries To Pause", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "test", ActionName: "tries_to_pause", Input: map[string]any{}},
	}
	state := engine.New(registry).ExecuteActionRun(action, engine.ActionRunInput{})

	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED — a standalone action run has no flow to pause within", state.Verdict)
	}
	step := state.Steps["tries_to_pause"]
	if step.Status != model.StepFailed {
		t.Fatalf("step status = %v, want FAILED (not left as PAUSED — nothing will ever resume it)", step.Status)
	}
	if step.ErrorMessage == "" {
		t.Fatal("expected a clear error message explaining why the pause was rejected")
	}
}

func TestActionRun_RetryStillApplies(t *testing.T) {
	registry := piece.NewRegistry()
	attempts := 0
	registry.Register(piece.Piece{
		Name: "test", DisplayName: "Test",
		Actions: map[string]piece.Action{
			"flaky": {
				Name: "flaky",
				Run: func(ctx piece.ActionContext) (any, error) {
					attempts++
					if attempts < 2 {
						return nil, fmt.Errorf("attempt %d failed", attempts)
					}
					return map[string]any{"attempts": attempts}, nil
				},
			},
		},
	})
	action := &model.FlowAction{
		Name: "flaky", DisplayName: "Flaky", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "test", ActionName: "flaky", Input: map[string]any{}},
		Error: &model.ErrorHandling{RetryOnFailure: true},
	}
	e := engine.New(registry)
	e.Retry = engine.RetryConstants{MaxAttempts: 3, ExponentialBase: 1, IntervalMs: 1}

	state := e.ExecuteActionRun(action, engine.ActionRunInput{})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED after retry — actionRunStepRunner reuses the same executor unchanged, retry included", state.Verdict)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestActionRun_RouterAndLoopAreRejected(t *testing.T) {
	router := &model.FlowAction{Name: "router", DisplayName: "Router", Type: model.ActionRouter, Router: &model.RouterSettings{}}
	loop := &model.FlowAction{Name: "loop", DisplayName: "Loop", Type: model.ActionLoopOnItems, Loop: &model.LoopSettings{Items: "{{ [] }}"}}

	e := engine.New(piece.NewRegistry())
	for _, action := range []*model.FlowAction{router, loop} {
		state := e.ExecuteActionRun(action, engine.ActionRunInput{})
		if state.Verdict.Status != model.FlowRunFailed {
			t.Fatalf("%s: verdict = %+v, want FAILED (ROUTER/LOOP_ON_ITEMS have no meaning standalone)", action.Type, state.Verdict)
		}
	}
}

// TestActionRun_PieceRequiringAuth_* prove ActionContext.Auth end-to-end:
// auth is not a separate channel from a normal input — it's the reserved
// piece.AuthInputKey ("auth") within the same Input map, resolved through
// the identical {{ }} templating pipeline as any other field, and merely
// surfaced as a convenience ctx.Auth for the piece to read. The engine does
// NOT enforce its presence (mirrors activepieces: props-processor.ts only
// conditionally validates an auth sub-object if one was actually supplied,
// and never blocks execution over a missing one) — a piece that "needs auth"
// enforces that itself by checking ctx.Auth and failing clearly, which is
// exactly what these tests prove: the engine faithfully delivers whatever
// was (or wasn't) provided, and gets out of the way.
func authRequiringPiece() *piece.Registry {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "authed", DisplayName: "Authed Service",
		Actions: map[string]piece.Action{
			"send_message": {
				Name: "send_message", DisplayName: "Send Message",
				Run: func(ctx piece.ActionContext) (any, error) {
					apiKey, _ := ctx.Auth.(string)
					if apiKey == "" {
						return nil, fmt.Errorf("missing required auth: apiKey")
					}
					return map[string]any{"sent": true, "usedKey": apiKey}, nil
				},
			},
		},
	})
	return registry
}

func TestActionRun_PieceRequiringAuth_MissingAuthFailsClearly(t *testing.T) {
	action := &model.FlowAction{
		Name: "send_message", DisplayName: "Send Message", Type: model.ActionPiece,
		// no "auth" key at all in Input — ctx.Auth will be nil.
		Piece: &model.PieceSettings{PieceName: "authed", ActionName: "send_message", Input: map[string]any{}},
	}
	state := engine.New(authRequiringPiece()).ExecuteActionRun(action, engine.ActionRunInput{})

	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED — the piece itself should reject missing auth", state.Verdict)
	}
	msg := state.Steps["send_message"].ErrorMessage
	if msg != "missing required auth: apiKey" {
		t.Fatalf("errorMessage = %q, want the piece's own clear rejection message", msg)
	}
}

func TestActionRun_PieceRequiringAuth_LiteralAuthSucceeds(t *testing.T) {
	action := &model.FlowAction{
		Name: "send_message", DisplayName: "Send Message", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "authed", ActionName: "send_message", Input: map[string]any{
			piece.AuthInputKey: "secret-key-123",
		}},
	}
	state := engine.New(authRequiringPiece()).ExecuteActionRun(action, engine.ActionRunInput{})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	assertOutput(t, state, "send_message", map[string]any{"sent": true, "usedKey": "secret-key-123"})
}

func TestActionRun_PieceRequiringAuth_TemplatedAuthFromMockContextSucceeds(t *testing.T) {
	action := &model.FlowAction{
		Name: "send_message", DisplayName: "Send Message", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "authed", ActionName: "send_message", Input: map[string]any{
			// auth resolved the same way as any other input — no special-casing.
			piece.AuthInputKey: "{{ my_connection.output.apiKey }}",
		}},
	}
	state := engine.New(authRequiringPiece()).ExecuteActionRun(action, engine.ActionRunInput{
		Context: map[string]*model.StepOutput{
			"my_connection": {Status: model.StepSucceeded, Output: map[string]any{"apiKey": "templated-key-456"}},
		},
	})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	assertOutput(t, state, "send_message", map[string]any{"sent": true, "usedKey": "templated-key-456"})
}

// TestActionRun_PieceWithFileAttachment_* prove the file-handling shape
// ported from activepieces' file-uploader.ts (context.files.write) and
// ApFile (the input side: a file-typed prop resolves to filename/data/
// extension, base64 available on demand). *piece.ApFile is just an Input
// value like any other — expr.Resolve only ever recurses into strings, maps,
// and slices; anything else (including *piece.ApFile) passes through its
// "default" case untouched, so no engine-level special-casing was needed to
// carry an attachment through to the action.
func attachmentPieceRegistry() *piece.Registry {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "files", DisplayName: "Files",
		Actions: map[string]piece.Action{
			"uppercase_file": {
				Name: "uppercase_file", DisplayName: "Uppercase File",
				Run: func(ctx piece.ActionContext) (any, error) {
					attachment, ok := ctx.Input["attachment"].(*piece.ApFile)
					if !ok || attachment == nil {
						return nil, fmt.Errorf("missing required attachment")
					}
					transformed := bytes.ToUpper(attachment.Data)
					url, err := ctx.Files.Write("uppercase_"+attachment.Filename, transformed)
					if err != nil {
						return nil, err
					}
					return map[string]any{
						"originalFilename":  attachment.Filename,
						"originalExtension": attachment.Extension,
						"originalBase64":    attachment.Base64(),
						"resultUrl":         url,
					}, nil
				},
			},
		},
	})
	return registry
}

func TestActionRun_PieceWithFileAttachment_UploadsATransformedFile(t *testing.T) {
	attachment := &piece.ApFile{Filename: "notes.txt", Data: []byte("hello world"), Extension: "txt"}
	action := &model.FlowAction{
		Name: "uppercase_file", DisplayName: "Uppercase File", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "files", ActionName: "uppercase_file", Input: map[string]any{
			"attachment": attachment,
		}},
	}

	e := engine.New(attachmentPieceRegistry())
	state := e.ExecuteActionRun(action, engine.ActionRunInput{})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	out := state.Steps["uppercase_file"].Output.(map[string]any)
	if out["originalFilename"] != "notes.txt" || out["originalExtension"] != "txt" {
		t.Fatalf("output = %+v", out)
	}
	wantB64 := base64.StdEncoding.EncodeToString([]byte("hello world"))
	if out["originalBase64"] != wantB64 {
		t.Fatalf("originalBase64 = %v, want %v", out["originalBase64"], wantB64)
	}

	url, _ := out["resultUrl"].(string)
	if url == "" {
		t.Fatal("resultUrl is empty")
	}
	writer, ok := e.Files.(*piece.MemoryFileWriter)
	if !ok {
		t.Fatalf("e.Files = %T, want the default *piece.MemoryFileWriter", e.Files)
	}
	written, ok := writer.Get(url)
	if !ok {
		t.Fatalf("no file found at %q in the file writer", url)
	}
	if string(written) != "HELLO WORLD" {
		t.Fatalf("written file content = %q, want %q", written, "HELLO WORLD")
	}
}

func TestActionRun_PieceWithFileAttachment_MissingAttachmentFailsClearly(t *testing.T) {
	action := &model.FlowAction{
		Name: "uppercase_file", DisplayName: "Uppercase File", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "files", ActionName: "uppercase_file", Input: map[string]any{}},
	}
	state := engine.New(attachmentPieceRegistry()).ExecuteActionRun(action, engine.ActionRunInput{})

	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED", state.Verdict)
	}
	if msg := state.Steps["uppercase_file"].ErrorMessage; msg != "missing required attachment" {
		t.Fatalf("errorMessage = %q", msg)
	}
}

// TestFlow_OAuth2Auth_* prove piece.OAuth2Auth flowing through a real flow
// (not a standalone action-run, unlike the other auth tests) via the exact
// same generic ctx.Auth mechanism as TestActionRun_PieceRequiringAuth_* —
// no OAuth2-specific engine code exists or was added; see OAuth2Auth's doc
// comment for why (activepieces itself doesn't refresh OAuth2 tokens inside
// the engine either). The interesting case an OAuth2 auth value uniquely
// raises is auth being PRESENT but stale — a piece rejecting an expired
// token is a different failure mode than the missing-auth case already
// covered elsewhere.
func oauth2PieceRegistry() *piece.Registry {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "crm", DisplayName: "CRM",
		Actions: map[string]piece.Action{
			"fetch_contacts": {
				Name: "fetch_contacts", DisplayName: "Fetch Contacts",
				Run: func(ctx piece.ActionContext) (any, error) {
					oauth, ok := ctx.Auth.(*piece.OAuth2Auth)
					if !ok || oauth.AccessToken == "" {
						return nil, fmt.Errorf("missing OAuth2 auth")
					}
					if expired, _ := oauth.Data["expired"].(bool); expired {
						return nil, fmt.Errorf("token expired, please reconnect")
					}
					return map[string]any{
						"contacts":  []any{"alice", "bob"},
						"tokenType": oauth.Data["token_type"],
					}, nil
				},
			},
		},
	})
	return registry
}

func TestFlow_OAuth2Auth_Succeeds(t *testing.T) {
	fetchContacts := &model.FlowAction{
		Name: "fetch_contacts", DisplayName: "Fetch Contacts", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "crm", ActionName: "fetch_contacts", Input: map[string]any{
			piece.AuthInputKey: &piece.OAuth2Auth{
				AccessToken: "valid-access-token",
				Data:        map[string]any{"refresh_token": "rt-123", "expires_in": 3600, "token_type": "Bearer"},
			},
		}},
	}
	summarize := codeAction("summarize", echoSource, map[string]any{
		"count": "{{ fetch_contacts.output.contacts.length }}",
	})
	fetchContacts.NextAction = summarize
	fv := &model.FlowVersion{ID: "fv-oauth2", Trigger: trigger(fetchContacts)}

	state := engine.New(oauth2PieceRegistry()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	assertOutput(t, state, "fetch_contacts", map[string]any{"tokenType": "Bearer"})
	assertOutput(t, state, "summarize", map[string]any{"count": int64(2)})
}

func TestFlow_OAuth2Auth_ExpiredTokenFailsClearly(t *testing.T) {
	fetchContacts := &model.FlowAction{
		Name: "fetch_contacts", DisplayName: "Fetch Contacts", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "crm", ActionName: "fetch_contacts", Input: map[string]any{
			piece.AuthInputKey: &piece.OAuth2Auth{
				AccessToken: "stale-access-token",
				Data:        map[string]any{"expired": true},
			},
		}},
		NextAction: codeAction("summarize", echoSource, map[string]any{"never": "reached"}),
	}
	fv := &model.FlowVersion{ID: "fv-oauth2-expired", Trigger: trigger(fetchContacts)}

	state := engine.New(oauth2PieceRegistry()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED — auth was present but stale, that's the piece's own call to reject", state.Verdict)
	}
	if msg := state.Steps["fetch_contacts"].ErrorMessage; msg != "token expired, please reconnect" {
		t.Fatalf("errorMessage = %q", msg)
	}
	if _, ok := state.Steps["summarize"]; ok {
		t.Fatal("summarize ran — the flow should have stopped at the failed auth check")
	}
}

// TestLoadOptions_* and TestFlow_DynamicDropdown_* prove the dynamic-dropdown
// (loadOptions) mechanism end-to-end: it's a config-time operation
// (Engine.LoadOptions), separate from running the flow, whose function
// receives the ALREADY-RESOLVED values of the action's OTHER inputs
// (including auth, same AuthInputKey convention as everywhere else) so a
// dependent dropdown (e.g. "channel", depending on "workspace") can decide
// what to fetch. By the time an actual flow runs, whatever value the
// dropdown produced is just a normal input — no special runtime handling,
// proven by running a real flow with a literal channel ID as if a human had
// already picked it from the options LoadOptions returned.
func slackDropdownRegistry() *piece.Registry {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "slack", DisplayName: "Slack",
		Actions: map[string]piece.Action{
			"send_message": {
				Name: "send_message", DisplayName: "Send Message",
				Run: func(ctx piece.ActionContext) (any, error) {
					channel, _ := ctx.Input["channel"].(string)
					workspace, _ := ctx.Input["workspace"].(string)
					if channel == "" {
						return nil, fmt.Errorf("missing required channel")
					}
					return map[string]any{"sent": true, "workspace": workspace, "channel": channel}, nil
				},
				Dropdowns: map[string]piece.DropdownProperty{
					"channel": {
						Refreshers: []string{"workspace"},
						LoadOptions: func(propsValue map[string]any, ctx piece.PropertyContext) (piece.DropdownState, error) {
							auth, _ := propsValue[piece.AuthInputKey].(string)
							if auth == "" {
								return piece.DropdownState{}, fmt.Errorf("connect a Slack account first")
							}
							workspace, _ := propsValue["workspace"].(string)
							switch workspace {
							case "engineering":
								return piece.DropdownState{Options: []piece.DropdownOption{
									{Label: "#eng-general", Value: "C_ENG_GENERAL"},
									{Label: "#eng-alerts", Value: "C_ENG_ALERTS"},
								}}, nil
							case "sales":
								return piece.DropdownState{Options: []piece.DropdownOption{
									{Label: "#sales-general", Value: "C_SALES_GENERAL"},
								}}, nil
							default:
								return piece.DropdownState{Disabled: true, Placeholder: "Select a workspace first"}, nil
							}
						},
					},
				},
			},
		},
	})
	return registry
}

func TestLoadOptions_DropdownDependsOnSiblingWorkspace(t *testing.T) {
	e := engine.New(slackDropdownRegistry())

	engineering, err := e.LoadOptions(engine.LoadOptionsInput{
		PieceName: "slack", ActionName: "send_message", PropertyName: "channel",
		Input: map[string]any{piece.AuthInputKey: "token-abc", "workspace": "engineering"},
	})
	if err != nil {
		t.Fatalf("LoadOptions(engineering): %v", err)
	}
	if len(engineering.Options) != 2 || engineering.Options[0].Value != "C_ENG_GENERAL" {
		t.Fatalf("engineering options = %+v", engineering.Options)
	}

	sales, err := e.LoadOptions(engine.LoadOptionsInput{
		PieceName: "slack", ActionName: "send_message", PropertyName: "channel",
		Input: map[string]any{piece.AuthInputKey: "token-abc", "workspace": "sales"},
	})
	if err != nil {
		t.Fatalf("LoadOptions(sales): %v", err)
	}
	if len(sales.Options) != 1 || sales.Options[0].Value != "C_SALES_GENERAL" {
		t.Fatalf("sales options = %+v, want only the sales channel (not engineering's)", sales.Options)
	}

	noWorkspace, err := e.LoadOptions(engine.LoadOptionsInput{
		PieceName: "slack", ActionName: "send_message", PropertyName: "channel",
		Input: map[string]any{piece.AuthInputKey: "token-abc"},
	})
	if err != nil {
		t.Fatalf("LoadOptions(no workspace): %v", err)
	}
	if !noWorkspace.Disabled {
		t.Fatalf("no-workspace state = %+v, want Disabled with a placeholder", noWorkspace)
	}
}

func TestLoadOptions_MissingAuthFailsClearly(t *testing.T) {
	_, err := engine.New(slackDropdownRegistry()).LoadOptions(engine.LoadOptionsInput{
		PieceName: "slack", ActionName: "send_message", PropertyName: "channel",
		Input: map[string]any{"workspace": "engineering"}, // no auth key at all
	})
	if err == nil || err.Error() != "connect a Slack account first" {
		t.Fatalf("err = %v, want the piece's own auth rejection", err)
	}
}

func TestLoadOptions_UnknownPropertyFailsClearly(t *testing.T) {
	_, err := engine.New(slackDropdownRegistry()).LoadOptions(engine.LoadOptionsInput{
		PieceName: "slack", ActionName: "send_message", PropertyName: "not_a_dropdown",
	})
	if err == nil {
		t.Fatal("err = nil, want a clear rejection for a non-dropdown property name")
	}
}

func TestFlow_DynamicDropdown_SelectedValueRunsInFlow(t *testing.T) {
	// "C_ENG_GENERAL" here is exactly the Value TestLoadOptions_DropdownDependsOnSiblingWorkspace
	// proved LoadOptions(workspace="engineering") returns — this is what a
	// human picking from that dropdown in a real editor would have produced.
	sendMessage := &model.FlowAction{
		Name: "send_message", DisplayName: "Send Message", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "slack", ActionName: "send_message", Input: map[string]any{
			piece.AuthInputKey: "token-abc",
			"workspace":        "engineering",
			"channel":          "C_ENG_GENERAL",
		}},
	}
	fv := &model.FlowVersion{ID: "fv-dropdown", Trigger: trigger(sendMessage)}

	state := engine.New(slackDropdownRegistry()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	assertOutput(t, state, "send_message", map[string]any{
		"sent": true, "workspace": "engineering", "channel": "C_ENG_GENERAL",
	})
}

// TestFlow_EncryptDecrypt_* prove data encryption/decryption as ordinary
// piece business logic in a real flow — NOT an engine mechanism, because
// activepieces' engine doesn't have one: checked before writing anything
// (grep across the whole TS extraction for encrypt/decrypt/crypto turned up
// nothing in packages/server/engine; connection secrets arrive already
// decrypted, fetched from the server over an authenticated API call —
// createConnectionResolver(...).obtain(key) — same boundary as OAuth2
// refresh, polling scheduling, and flow timeout: real in the product, owned
// by the server, not the engine). So there's no new engine surface here.
// What's actually proven: an "encrypt" PIECE action feeding an "decrypt"
// PIECE action through a real flow via {{ }} templating, using AES-GCM
// (Go's standard crypto/aes+crypto/cipher) with the key carried through
// ctx.Auth exactly like every other secret in this test suite — []byte is
// just another value type that already passed through expr.Resolve's
// default case untouched, same as *piece.ApFile and *piece.OAuth2Auth
// before it; no engine change was needed for this test to be possible.
func vaultRegistry() *piece.Registry {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "vault", DisplayName: "Vault",
		Actions: map[string]piece.Action{
			"encrypt": {
				Name: "encrypt", DisplayName: "Encrypt",
				Run: func(ctx piece.ActionContext) (any, error) {
					key, ok := ctx.Auth.([]byte)
					if !ok || len(key) == 0 {
						return nil, fmt.Errorf("missing encryption key")
					}
					plaintext, _ := ctx.Input["plaintext"].(string)
					gcm, err := newGCM(key)
					if err != nil {
						return nil, err
					}
					nonce := make([]byte, gcm.NonceSize())
					if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
						return nil, err
					}
					sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
					return map[string]any{"ciphertext": base64.StdEncoding.EncodeToString(sealed)}, nil
				},
			},
			"decrypt": {
				Name: "decrypt", DisplayName: "Decrypt",
				Run: func(ctx piece.ActionContext) (any, error) {
					key, ok := ctx.Auth.([]byte)
					if !ok || len(key) == 0 {
						return nil, fmt.Errorf("missing decryption key")
					}
					ciphertextB64, _ := ctx.Input["ciphertext"].(string)
					raw, err := base64.StdEncoding.DecodeString(ciphertextB64)
					if err != nil {
						return nil, fmt.Errorf("invalid ciphertext encoding: %w", err)
					}
					gcm, err := newGCM(key)
					if err != nil {
						return nil, err
					}
					nonceSize := gcm.NonceSize()
					if len(raw) < nonceSize {
						return nil, fmt.Errorf("ciphertext too short")
					}
					nonce, sealed := raw[:nonceSize], raw[nonceSize:]
					plaintext, err := gcm.Open(nil, nonce, sealed, nil)
					if err != nil {
						return nil, fmt.Errorf("decryption failed: %w", err)
					}
					return map[string]any{"plaintext": string(plaintext)}, nil
				},
			},
		},
	})
	return registry
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func TestFlow_EncryptDecryptRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef") // AES-128
	decryptStep := &model.FlowAction{
		Name: "decrypt", DisplayName: "Decrypt", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "vault", ActionName: "decrypt", Input: map[string]any{
			piece.AuthInputKey: key,
			"ciphertext":       "{{ encrypt.output.ciphertext }}",
		}},
	}
	encryptStep := &model.FlowAction{
		Name: "encrypt", DisplayName: "Encrypt", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "vault", ActionName: "encrypt", Input: map[string]any{
			piece.AuthInputKey: key,
			"plaintext":        "the launch codes are 12345",
		}},
		NextAction: decryptStep,
	}
	fv := &model.FlowVersion{ID: "fv-crypto", Trigger: trigger(encryptStep)}

	state := engine.New(vaultRegistry()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}

	ciphertext, _ := state.Steps["encrypt"].Output.(map[string]any)["ciphertext"].(string)
	if ciphertext == "" || ciphertext == "the launch codes are 12345" {
		t.Fatalf("ciphertext = %q, want a real (non-empty, non-plaintext) encrypted value", ciphertext)
	}

	assertOutput(t, state, "decrypt", map[string]any{"plaintext": "the launch codes are 12345"})
}

func TestFlow_DecryptWithWrongKeyFailsClearly(t *testing.T) {
	rightKey := []byte("0123456789abcdef")
	wrongKey := []byte("fedcba9876543210")

	decryptStep := &model.FlowAction{
		Name: "decrypt", DisplayName: "Decrypt", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "vault", ActionName: "decrypt", Input: map[string]any{
			piece.AuthInputKey: wrongKey, // deliberately not the key encrypt used
			"ciphertext":       "{{ encrypt.output.ciphertext }}",
		}},
	}
	encryptStep := &model.FlowAction{
		Name: "encrypt", DisplayName: "Encrypt", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "vault", ActionName: "encrypt", Input: map[string]any{
			piece.AuthInputKey: rightKey,
			"plaintext":        "top secret",
		}},
		NextAction: decryptStep,
	}
	fv := &model.FlowVersion{ID: "fv-crypto-wrong-key", Trigger: trigger(encryptStep)}

	state := engine.New(vaultRegistry()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED — GCM authentication must reject data encrypted under a different key", state.Verdict)
	}
	msg := state.Steps["decrypt"].ErrorMessage
	if msg == "" || !bytes.Contains([]byte(msg), []byte("decryption failed")) {
		t.Fatalf("errorMessage = %q, want a clear decryption-failed message", msg)
	}
}

// TestFlow_RateLimiting_LoopEventuallySucceedsViaRetry proves rate limiting
// as ordinary piece business logic, NOT an engine mechanism — activepieces
// doesn't have one either: grepped the whole TS extraction first, the only
// hit was a boolean config FLAG (projectRateLimiterEnabled) in a health/
// status schema, no actual limiter implementation anywhere in
// packages/server/engine. Same boundary as OAuth2 refresh, encryption, and
// flow timeout before it.
//
// What IS interesting and worth proving: a piece rate-limits itself by
// returning an error once its quota is used up, and leans on the engine's
// ALREADY-EXISTING retry/backoff (RetryOnFailure, tested independently
// since TestRetryOnFailureSucceedsOnThirdAttempt) to survive being
// throttled — rate limiting isn't a new engine feature, it's a piece using
// ordinary state plus a capability this engine already had.
func TestFlow_RateLimiting_LoopEventuallySucceedsViaRetry(t *testing.T) {
	const capacity = 1 // 1 call allowed per window
	const window = 8 * time.Millisecond
	registry := piece.NewRegistry()
	var attempts int32
	var mu sync.Mutex
	var windowStart time.Time
	used := 0
	registry.Register(piece.Piece{
		Name: "api", DisplayName: "API",
		Actions: map[string]piece.Action{
			"call_api": {
				Name: "call_api", DisplayName: "Call API",
				Run: func(ctx piece.ActionContext) (any, error) {
					atomic.AddInt32(&attempts, 1)
					mu.Lock()
					defer mu.Unlock()
					now := time.Now()
					if windowStart.IsZero() || now.Sub(windowStart) >= window {
						windowStart = now
						used = 0
					}
					if used >= capacity {
						return nil, fmt.Errorf("rate limit exceeded, retry later")
					}
					used++
					item, _ := ctx.Input["item"].(int64)
					return map[string]any{"called": item}, nil
				},
			},
		},
	})

	callAPI := &model.FlowAction{
		Name: "call_api", DisplayName: "Call API", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "api", ActionName: "call_api", Input: map[string]any{
			"item": "{{ loop.output.item }}",
		}},
		Error: &model.ErrorHandling{RetryOnFailure: true},
	}
	loop := &model.FlowAction{
		Name: "loop", DisplayName: "Loop", Type: model.ActionLoopOnItems,
		Loop: &model.LoopSettings{Items: "{{ [1, 2, 3, 4, 5] }}", FirstLoopAction: callAPI},
	}
	fv := &model.FlowVersion{ID: "fv-rate-limit", Trigger: trigger(loop)}

	e := engine.New(registry)
	// Generous linear backoff (ExponentialBase 1) budget: each item may need
	// to wait out a whole rate-limit window before its retry succeeds.
	e.Retry = engine.RetryConstants{MaxAttempts: 20, ExponentialBase: 1, IntervalMs: 3}

	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED — every item should eventually get through via retry", state.Verdict)
	}
	loopOut := state.Steps["loop"]
	if loopOut.Status != model.StepSucceeded || len(loopOut.Iterations) != 5 {
		t.Fatalf("loop step = %+v", loopOut)
	}
	for i, want := range []int64{1, 2, 3, 4, 5} {
		out := loopOut.Iterations[i]["call_api"].Output.(map[string]any)
		if out["called"] != want {
			t.Fatalf("iteration %d = %+v, want called=%d", i, out, want)
		}
	}
	// With capacity 1 and 5 calls, at least some must have been rejected and
	// retried — proves the rate limiter actually fired, not just that the
	// flow happened to succeed on the first try every time.
	if got := atomic.LoadInt32(&attempts); got <= 5 {
		t.Fatalf("total call attempts = %d, want > 5 (proof at least one call was rate-limited and retried)", got)
	}
}

// TestConcurrentFlowRuns_* prove something none of the ~40 tests above ever
// touched: real goroutine parallelism. Every test until now called
// ExecuteBegin/ExecuteActionRun from a single goroutine at a time (Go's
// `go test` doesn't parallelize test functions unless they call
// t.Parallel(), which none of these do). That's a real gap given this
// whole project's premise from the first message in this thread: Go's
// actual advantage over activepieces' Node engine is OS-thread parallelism
// for concurrent flow runs, not just avoiding a JS runtime.
//
// While designing this, reviewing piece.Registry to check what these
// goroutines would actually be touching concurrently turned up a real gap:
// its map had no mutex. Fixed with a sync.RWMutex (see Registry's doc
// comment) — but honestly: the tests below only call Register once, before
// any goroutine starts, then read concurrently afterward, and concurrent
// map READS with no concurrent write are not a data race Go's detector
// flags. So `go test -race` passing here doesn't prove the fix was load-
// bearing for THIS specific test — it was a correctness fix for a realistic
// but untested usage pattern (registering pieces while flows already
// running elsewhere are reading the registry), found by inspection while
// building this, not caught red-handed by -race in the test that follows.
//
// FlowAction/FlowVersion trees are safe to SHARE (not just the Engine)
// across concurrent runs: nothing in executeChain/executeCode/executePiece/
// executeRouter/executeLoop ever mutates an action or its Input map —
// expr.Resolve always builds a NEW map rather than mutating the one it was
// given — so every goroutine below reuses the exact same *model.FlowVersion.

func TestConcurrentFlowRuns_IsolatedAndRaceFree(t *testing.T) {
	registry := piece.NewRegistry()
	var totalCalls int32
	registry.Register(piece.Piece{
		Name: "worker", DisplayName: "Worker",
		Actions: map[string]piece.Action{
			"process": {
				Name: "process", DisplayName: "Process",
				Run: func(ctx piece.ActionContext) (any, error) {
					atomic.AddInt32(&totalCalls, 1)
					n, _ := ctx.Input["n"].(int64)
					time.Sleep(time.Millisecond) // encourages real goroutine interleaving, not just theoretical concurrency
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
	fv := &model.FlowVersion{ID: "fv-concurrent", Trigger: trigger(process)}
	e := engine.New(registry)

	const runs = 50
	var wg sync.WaitGroup
	results := make([]*model.ExecutionState, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Same *model.FlowVersion pointer shared across every goroutine —
			// see the doc comment above for why that's safe.
			results[i] = e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{"n": int64(i)}})
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&totalCalls); got != runs {
		t.Fatalf("totalCalls = %d, want %d — every concurrent run should have executed exactly once", got, runs)
	}
	for i, state := range results {
		if state.Verdict.Status != model.FlowRunSucceeded {
			t.Fatalf("run %d verdict = %+v", i, state.Verdict)
		}
		out := state.Steps["process"].Output.(map[string]any)
		want := int64(i * 2)
		if out["doubled"] != want {
			t.Fatalf("run %d: doubled = %v, want %d — cross-goroutine contamination of ExecutionState", i, out["doubled"], want)
		}
	}
}

// TestConcurrentFlowRuns_AreActuallyFaster is the measurable payoff: N runs
// launched as goroutines complete meaningfully faster than the same N runs
// executed one at a time, for a workload with real (if artificial) latency
// per run. Loose threshold (parallel must beat half of sequential, not the
// theoretical ~1/N) specifically to avoid flaking on a loaded CI machine —
// the point is proving real parallelism happened, not benchmarking it.
func TestConcurrentFlowRuns_AreActuallyFaster(t *testing.T) {
	const runs = 20
	const perRunLatency = 5 * time.Millisecond

	buildEngine := func() (*engine.Engine, *model.FlowVersion) {
		registry := piece.NewRegistry()
		registry.Register(piece.Piece{
			Name: "worker", DisplayName: "Worker",
			Actions: map[string]piece.Action{
				"slow_process": {
					Name: "slow_process",
					Run: func(ctx piece.ActionContext) (any, error) {
						time.Sleep(perRunLatency)
						return map[string]any{"done": true}, nil
					},
				},
			},
		})
		action := &model.FlowAction{
			Name: "slow_process", DisplayName: "Slow Process", Type: model.ActionPiece,
			Piece: &model.PieceSettings{PieceName: "worker", ActionName: "slow_process", Input: map[string]any{}},
		}
		return engine.New(registry), &model.FlowVersion{ID: "fv-speed", Trigger: trigger(action)}
	}

	seqEngine, seqFV := buildEngine()
	seqStart := time.Now()
	for i := 0; i < runs; i++ {
		seqEngine.ExecuteBegin(seqFV, engine.BeginInput{TriggerPayload: map[string]any{}})
	}
	sequential := time.Since(seqStart)

	parEngine, parFV := buildEngine()
	var wg sync.WaitGroup
	parStart := time.Now()
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			parEngine.ExecuteBegin(parFV, engine.BeginInput{TriggerPayload: map[string]any{}})
		}()
	}
	wg.Wait()
	parallel := time.Since(parStart)

	if parallel >= sequential/2 {
		t.Fatalf("parallel = %v, sequential = %v — expected parallel to be meaningfully faster (real goroutine concurrency), not just theoretically possible", parallel, sequential)
	}
}

// TestFlow_NestedTryCatch_RecoveryFeedsRouterBranch is the "flow" version of
// pkg/sandbox's try/catch tests: a CODE step's internal error handling
// (JSON.parse wrapped in try/catch, nested one level inside a helper
// function) integrates with the ENGINE's own error handling at a different
// layer — a ROUTER branching on whether the code recovered — without ever
// tripping the engine's own failure/retry machinery. Two genuinely
// different kinds of "error handling" nested inside one flow: JS-level
// try/catch inside the step, and the step's clean (non-error) output
// driving flow-level branching. Contrast with TestContinueOnFailure et al.,
// which cover an UNCAUGHT error at the engine layer — this is deliberately
// the other case: the error never reaches the engine at all, because JS
// caught it first.
func TestFlow_NestedTryCatch_RecoveryFeedsRouterBranch(t *testing.T) {
	validateSource := `(params) => {
		function tryParse(raw) {
			try {
				return { ok: true, data: JSON.parse(raw) }
			} catch (err) {
				return { ok: false, reason: err.message }
			}
		}
		const result = tryParse(params.raw)
		if (!result.ok) {
			return { valid: false, error: result.reason }
		}
		return { valid: true, data: result.data }
	}`

	buildFlow := func() *model.FlowVersion {
		router := &model.FlowAction{
			Name: "router", DisplayName: "Router", Type: model.ActionRouter,
			Router: &model.RouterSettings{
				ExecutionType: model.RouterExecuteFirstMatch,
				Branches: []model.RouterBranch{
					{Name: "valid", Type: model.BranchCondition, Conditions: [][]model.Condition{{
						{Operator: model.OpBooleanIsTrue, FirstValue: "{{ validate_and_parse.output.valid }}"},
					}}},
					{Name: "invalid", Type: model.BranchFallback},
				},
				Children: []*model.FlowAction{
					codeAction("process_valid", echoSource, map[string]any{"handled": "{{ validate_and_parse.output.data }}"}),
					codeAction("handle_invalid", echoSource, map[string]any{"reason": "{{ validate_and_parse.output.error }}"}),
				},
			},
		}
		validateAndParse := codeAction("validate_and_parse", validateSource, map[string]any{"raw": "{{ trigger_1.output.raw }}"})
		validateAndParse.NextAction = router
		return &model.FlowVersion{ID: "fv-nested-try-catch", Trigger: trigger(validateAndParse)}
	}

	t.Run("well-formed JSON recovers cleanly and routes to the valid branch", func(t *testing.T) {
		state := engine.New(piece.NewRegistry()).ExecuteBegin(buildFlow(), engine.BeginInput{
			TriggerPayload: map[string]any{"raw": `{"name":"ok"}`},
		})
		if state.Verdict.Status != model.FlowRunSucceeded {
			t.Fatalf("verdict = %+v, want SUCCEEDED", state.Verdict)
		}
		vp := state.Steps["validate_and_parse"]
		if vp.Status != model.StepSucceeded {
			t.Fatalf("validate_and_parse status = %v, want SUCCEEDED (this step never throws — its own try/catch handles everything)", vp.Status)
		}
		if _, ok := state.Steps["process_valid"]; !ok {
			t.Fatal("process_valid did not run")
		}
		if _, ok := state.Steps["handle_invalid"]; ok {
			t.Fatal("handle_invalid ran — should not have, JSON was well-formed")
		}
	})

	t.Run("malformed JSON is caught internally — step still SUCCEEDS and routes to the fallback branch", func(t *testing.T) {
		state := engine.New(piece.NewRegistry()).ExecuteBegin(buildFlow(), engine.BeginInput{
			TriggerPayload: map[string]any{"raw": `{not valid json`},
		})
		if state.Verdict.Status != model.FlowRunSucceeded {
			t.Fatalf("verdict = %+v, want SUCCEEDED — the parse error was caught inside the step, it must never reach the engine as a failure", state.Verdict)
		}
		vp := state.Steps["validate_and_parse"]
		if vp.Status != model.StepSucceeded {
			t.Fatalf("validate_and_parse status = %v, want SUCCEEDED even though the JSON was invalid — that's the whole point of its internal try/catch", vp.Status)
		}
		out := vp.Output.(map[string]any)
		if out["valid"] != false || out["error"] == "" || out["error"] == nil {
			t.Fatalf("validate_and_parse output = %+v, want {valid:false, error: <a JSON parse error message>}", out)
		}
		if _, ok := state.Steps["handle_invalid"]; !ok {
			t.Fatal("handle_invalid did not run")
		}
		if _, ok := state.Steps["process_valid"]; ok {
			t.Fatal("process_valid ran — should not have, JSON was malformed")
		}
	})
}

// TestFlow_CombinedEvents_OneTriggerDispatchesByEventType is the real,
// idiomatic answer to "a flow with multiple triggers" — checked against
// activepieces' data model first: FlowVersion.trigger is a single field
// (`trigger: FlowTrigger` in flow-version.ts), not an array, in BOTH
// codebases. There is no "multiple triggers combined" construct to add;
// building one would be a genuine deviation from the shared model, not a
// port of something real. What every platform actually does instead — and
// what this test builds — is ONE trigger receiving a union of event shapes
// (one webhook fed by two different upstream systems, in this case
// "order.created" and "user.signup") immediately dispatched by a ROUTER on
// an event-type field. Every piece of this (a real, non-EMPTY trigger;
// FIRST_MATCH branching; a fallback for an unrecognized type) already
// existed and was already tested independently — this is a composition
// test, not new engine surface.
func TestFlow_CombinedEvents_OneTriggerDispatchesByEventType(t *testing.T) {
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "hub", DisplayName: "Event Hub",
		Triggers: map[string]piece.Trigger{
			"catch_any_event": {
				Name: "catch_any_event", DisplayName: "Catch Any Event",
				// Passes the raw event through untouched — a real "combined
				// webhook" trigger like this typically does light normalization,
				// but the union-of-shapes handling is the router's job, not the
				// trigger's, same as activepieces' own webhook triggers.
				Run: func(ctx piece.TriggerContext) ([]any, error) {
					return []any{ctx.Payload}, nil
				},
			},
		},
	})

	router := &model.FlowAction{
		Name: "router", DisplayName: "Dispatch By Event Type", Type: model.ActionRouter,
		Router: &model.RouterSettings{
			ExecutionType: model.RouterExecuteFirstMatch,
			Branches: []model.RouterBranch{
				{Name: "order", Type: model.BranchCondition, Conditions: [][]model.Condition{{
					{Operator: model.OpTextExactlyMatches, FirstValue: "{{ trigger_1.output.type }}", SecondValue: "order.created"},
				}}},
				{Name: "signup", Type: model.BranchCondition, Conditions: [][]model.Condition{{
					{Operator: model.OpTextExactlyMatches, FirstValue: "{{ trigger_1.output.type }}", SecondValue: "user.signup"},
				}}},
				{Name: "unknown", Type: model.BranchFallback},
			},
			Children: []*model.FlowAction{
				codeAction("handle_order", echoSource, map[string]any{"orderId": "{{ trigger_1.output.orderId }}"}),
				codeAction("handle_signup", echoSource, map[string]any{"email": "{{ trigger_1.output.email }}"}),
				codeAction("handle_unknown", echoSource, map[string]any{"ignored": true}),
			},
		},
	}
	fv := &model.FlowVersion{ID: "fv-combined-events", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Catch Any Event", Type: model.TriggerPiece,
		PieceName: "hub", TriggerName: "catch_any_event", Input: map[string]any{},
		NextAction: router,
	}}

	handlers := []string{"handle_order", "handle_signup", "handle_unknown"}
	runWith := func(t *testing.T, payload map[string]any) *model.ExecutionState {
		t.Helper()
		state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: payload, ExecuteTrigger: true})
		if state.Verdict.Status != model.FlowRunSucceeded {
			t.Fatalf("verdict = %+v", state.Verdict)
		}
		return state
	}
	assertOnlyRan := func(t *testing.T, state *model.ExecutionState, want string) {
		t.Helper()
		for _, h := range handlers {
			_, ran := state.Steps[h]
			if h == want && !ran {
				t.Fatalf("%s did not run", h)
			}
			if h != want && ran {
				t.Fatalf("%s ran — should not have, event was routed to %s", h, want)
			}
		}
	}

	t.Run("order.created event routes to the order handler", func(t *testing.T) {
		state := runWith(t, map[string]any{"type": "order.created", "orderId": "ORD-1"})
		assertOnlyRan(t, state, "handle_order")
		assertOutput(t, state, "handle_order", map[string]any{"orderId": "ORD-1"})
	})

	t.Run("user.signup event routes to the signup handler", func(t *testing.T) {
		state := runWith(t, map[string]any{"type": "user.signup", "email": "new@example.com"})
		assertOnlyRan(t, state, "handle_signup")
		assertOutput(t, state, "handle_signup", map[string]any{"email": "new@example.com"})
	})

	t.Run("an unrecognized event type falls back to the unknown handler", func(t *testing.T) {
		state := runWith(t, map[string]any{"type": "inventory.updated"})
		assertOnlyRan(t, state, "handle_unknown")
	})
}

// TestNestedLoops_BasicIteration proves a LOOP_ON_ITEMS nested inside
// another LOOP_ON_ITEMS composes correctly: the inner loop's own step
// entry lives inside each outer iteration's recorded steps, and the
// innermost body can reference BOTH loops' current item/index at once
// (outer_loop.output.item alongside inner_loop.output.item) since each
// loop injects its own synthetic scope entry without clobbering the
// other's — same mechanism, just recursed.
func TestNestedLoops_BasicIteration(t *testing.T) {
	innerBody := codeAction("combine", echoSource, map[string]any{
		"outer": "{{ outer_loop.output.item }}",
		"inner": "{{ inner_loop.output.item }}",
	})
	innerLoop := &model.FlowAction{
		Name: "inner_loop", DisplayName: "Inner Loop", Type: model.ActionLoopOnItems,
		Loop: &model.LoopSettings{Items: `{{ ["a", "b"] }}`, FirstLoopAction: innerBody},
	}
	outerLoop := &model.FlowAction{
		Name: "outer_loop", DisplayName: "Outer Loop", Type: model.ActionLoopOnItems,
		Loop: &model.LoopSettings{Items: "{{ [1, 2] }}", FirstLoopAction: innerLoop},
	}
	fv := &model.FlowVersion{ID: "fv-nested-loops", Trigger: trigger(outerLoop)}

	state := engine.New(piece.NewRegistry()).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	outerOut := state.Steps["outer_loop"]
	if outerOut.Status != model.StepSucceeded || len(outerOut.Iterations) != 2 {
		t.Fatalf("outer_loop = %+v", outerOut)
	}

	want := []struct {
		outer int64
		inner string
	}{{1, "a"}, {1, "b"}, {2, "a"}, {2, "b"}}
	got := 0
	for outerIdx, outerIter := range outerOut.Iterations {
		innerOut := outerIter["inner_loop"]
		if innerOut == nil || innerOut.Status != model.StepSucceeded {
			t.Fatalf("outer iteration %d: inner_loop = %+v", outerIdx, innerOut)
		}
		if len(innerOut.Iterations) != 2 {
			t.Fatalf("outer iteration %d: inner iterations = %d, want 2", outerIdx, len(innerOut.Iterations))
		}
		for _, innerIter := range innerOut.Iterations {
			combined := innerIter["combine"].Output.(map[string]any)
			w := want[got]
			if combined["outer"] != w.outer || combined["inner"] != w.inner {
				t.Fatalf("combination %d = %+v, want {outer:%v inner:%v}", got, combined, w.outer, w.inner)
			}
			got++
		}
	}
	if got != 4 {
		t.Fatalf("total inner iterations = %d, want 4", got)
	}
}

// TestNestedLoops_PauseAndResume proves a pause deep inside an inner loop
// (itself inside an outer loop) resumes correctly: only the specific inner
// iteration that paused re-runs with ExecutionType RESUME, the rest of that
// inner loop continues, and THEN the outer loop proceeds to its remaining
// items — two levels of the same resume mechanism composed, not a separate
// code path.
func buildNestedLoopPauseFlow(t *testing.T) (*model.FlowVersion, *engine.Engine) {
	t.Helper()
	registry := piece.NewRegistry()
	registry.Register(piece.Piece{
		Name: "test", DisplayName: "Test",
		Actions: map[string]piece.Action{
			"handle": {
				Name: "handle", DisplayName: "Handle",
				Run: func(ctx piece.ActionContext) (any, error) {
					outer, _ := ctx.Input["outer"].(int64)
					inner, _ := ctx.Input["inner"].(int64)
					if outer == 1 && inner == 20 && ctx.ExecutionType == model.ExecutionBegin {
						ctx.Run.WaitForWaitpoint("wp-nested-loop")
						return map[string]any{"paused": true}, nil
					}
					return map[string]any{"outer": outer, "inner": inner, "resumed": ctx.ExecutionType == model.ExecutionResume}, nil
				},
			},
		},
	})

	handle := &model.FlowAction{
		Name: "handle", DisplayName: "Handle", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "test", ActionName: "handle", Input: map[string]any{
			"outer": "{{ outer_loop.output.item }}",
			"inner": "{{ inner_loop.output.item }}",
		}},
	}
	innerLoop := &model.FlowAction{
		Name: "inner_loop", DisplayName: "Inner Loop", Type: model.ActionLoopOnItems,
		Loop: &model.LoopSettings{Items: "{{ [10, 20] }}", FirstLoopAction: handle},
	}
	outerLoop := &model.FlowAction{
		Name: "outer_loop", DisplayName: "Outer Loop", Type: model.ActionLoopOnItems,
		Loop:       &model.LoopSettings{Items: "{{ [1, 2] }}", FirstLoopAction: innerLoop},
		NextAction: codeAction("after_all", echoSource, map[string]any{"finished": true}),
	}
	fv := &model.FlowVersion{ID: "fv-nested-loop-pause", Trigger: trigger(outerLoop)}
	return fv, engine.New(registry)
}

func TestNestedLoops_PauseAndResume(t *testing.T) {
	fv, e := buildNestedLoopPauseFlow(t)
	begun := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if begun.Verdict.Status != model.FlowRunPaused {
		t.Fatalf("BEGIN verdict = %+v, want PAUSED", begun.Verdict)
	}
	outerOut := begun.Steps["outer_loop"]
	if outerOut.Status != model.StepPaused || outerOut.LastIndex != 1 {
		t.Fatalf("outer_loop = %+v, want PAUSED at outer index 1", outerOut)
	}
	if len(outerOut.Iterations) != 1 {
		t.Fatalf("outer iterations = %d, want 1 (still inside outer item 1)", len(outerOut.Iterations))
	}
	innerOut := outerOut.Iterations[0]["inner_loop"]
	if innerOut.Status != model.StepPaused || innerOut.LastIndex != 2 || innerOut.LastItem != int64(20) {
		t.Fatalf("inner_loop = %+v, want PAUSED at inner item 20 (index 2)", innerOut)
	}
	if len(innerOut.Iterations) != 2 {
		t.Fatalf("inner iterations = %d, want 2 (item 10 succeeded, item 20 paused)", len(innerOut.Iterations))
	}
	if innerOut.Iterations[0]["handle"].Status != model.StepSucceeded {
		t.Fatalf("inner iteration 1 (item 10) = %+v, want SUCCEEDED", innerOut.Iterations[0]["handle"])
	}
	if innerOut.Iterations[1]["handle"].Status != model.StepPaused {
		t.Fatalf("inner iteration 2 (item 20) = %+v, want PAUSED", innerOut.Iterations[1]["handle"])
	}
	if _, ok := begun.Steps["after_all"]; ok {
		t.Fatal("after_all should not run before everything resumes")
	}

	resumed := e.ExecuteResume(fv, engine.ResumeInput{PriorState: begun, ResumePayload: map[string]any{"ok": true}})

	if resumed.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("RESUME verdict = %+v, want SUCCEEDED", resumed.Verdict)
	}
	outerOut = resumed.Steps["outer_loop"]
	if outerOut.Status != model.StepSucceeded || outerOut.LastIndex != 2 {
		t.Fatalf("outer_loop = %+v, want SUCCEEDED through outer index 2", outerOut)
	}
	if len(outerOut.Iterations) != 2 {
		t.Fatalf("outer iterations = %d, want 2 (both outer items now complete)", len(outerOut.Iterations))
	}

	// outer item 1's inner loop: item 10 untouched, item 20 actually resumed.
	firstInner := outerOut.Iterations[0]["inner_loop"]
	if firstInner.Status != model.StepSucceeded || len(firstInner.Iterations) != 2 {
		t.Fatalf("outer item 1 inner_loop = %+v", firstInner)
	}
	item10 := firstInner.Iterations[0]["handle"].Output.(map[string]any)
	if item10["resumed"] != false || item10["inner"] != int64(10) {
		t.Fatalf("outer=1/inner=10 = %+v, should be unchanged from BEGIN", item10)
	}
	item20 := firstInner.Iterations[1]["handle"]
	if item20.Status != model.StepSucceeded {
		t.Fatalf("outer=1/inner=20 status = %v, want SUCCEEDED after resume", item20.Status)
	}
	item20Out := item20.Output.(map[string]any)
	if item20Out["resumed"] != true || item20Out["outer"] != int64(1) || item20Out["inner"] != int64(20) {
		t.Fatalf("outer=1/inner=20 output = %+v, want {outer:1, inner:20, resumed:true}", item20Out)
	}

	// outer item 2's ENTIRE inner loop never ran before the pause — proves the
	// outer loop actually continued past the resumed outer iteration.
	secondInner := outerOut.Iterations[1]["inner_loop"]
	if secondInner.Status != model.StepSucceeded || len(secondInner.Iterations) != 2 {
		t.Fatalf("outer item 2 inner_loop = %+v", secondInner)
	}
	for i, wantInner := range []int64{10, 20} {
		out := secondInner.Iterations[i]["handle"].Output.(map[string]any)
		if out["outer"] != int64(2) || out["inner"] != wantInner || out["resumed"] != false {
			t.Fatalf("outer=2/inner=%d = %+v, want {outer:2, inner:%d, resumed:false}", wantInner, out, wantInner)
		}
	}

	assertOutput(t, resumed, "after_all", map[string]any{"finished": true})
}

func bigString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func assertOutput(t *testing.T, state *model.ExecutionState, step string, want any) {
	t.Helper()
	got, ok := state.Steps[step]
	if !ok {
		t.Fatalf("step %q not found", step)
	}
	gotOut, ok := got.Output.(map[string]any)
	if !ok {
		t.Fatalf("step %q output = %#v, not a map", step, got.Output)
	}
	wantOut, ok := want.(map[string]any)
	if !ok {
		t.Fatalf("assertOutput: want must be a map, got %#v", want)
	}
	for k, v := range wantOut {
		if gotOut[k] != v {
			t.Fatalf("step %q output[%q] = %#v, want %#v (full output: %#v)", step, k, gotOut[k], v, gotOut)
		}
	}
}
