// Package engine is goflow's flow orchestrator — the Go analogue of
// activepieces' flow-executor.ts + operations/flow.operation.ts, ported to
// native Go control flow (goroutine-free; the retry/backoff and loop
// iteration are plain synchronous loops, which is all this needs).
package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"goflow/pkg/expr"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/sandbox"
)

// RetryConstants controls CODE/PIECE action retry backoff:
// delay = ExponentialBase^attempt * IntervalMs.
type RetryConstants struct {
	MaxAttempts     int
	ExponentialBase float64
	IntervalMs      float64
}

var DefaultRetryConstants = RetryConstants{MaxAttempts: 4, ExponentialBase: 2, IntervalMs: 2000}

// Engine executes flows. Safe to reuse across runs (holds no per-run state).
type Engine struct {
	Registry        *piece.Registry
	Retry           RetryConstants
	MaxLogSizeBytes int // 0 disables the check
	Files           piece.FileWriter
	Store           piece.Store // wired into every PIECE action's ActionContext.Store — see its doc comment
}

func New(registry *piece.Registry) *Engine {
	return &Engine{
		Registry: registry, Retry: DefaultRetryConstants,
		Files: piece.NewMemoryFileWriter(), Store: piece.NewMemoryStore(),
	}
}

// BeginInput configures a fresh (non-resumed) run.
type BeginInput struct {
	TriggerPayload any
	// ExecuteTrigger mirrors activepieces: false passes TriggerPayload straight
	// through as the trigger step's output; true actually invokes a PIECE
	// trigger's Run hook (a no-op for FlowTriggerType EMPTY either way).
	ExecuteTrigger bool
}

// ResumeInput configures a resumed run, continuing from a previously PAUSED step.
type ResumeInput struct {
	PriorState    *model.ExecutionState
	ResumePayload any
}

// ExecuteBegin runs fv from its trigger. Returns the final ExecutionState;
// never returns an error — engine-level failures are recorded as a FAILED
// verdict on the returned state, same as activepieces' EngineResponse pattern
// (a run failing is not the same as the engine call itself erroring).
func (e *Engine) ExecuteBegin(fv *model.FlowVersion, in BeginInput) *model.ExecutionState {
	state := model.NewExecutionState()

	triggerOutput := in.TriggerPayload
	if in.ExecuteTrigger && fv.Trigger.Type == model.TriggerPiece {
		trig, ok := e.Registry.GetTrigger(fv.Trigger.PieceName, fv.Trigger.TriggerName)
		if !ok {
			return failRun(state, fv.Trigger.Name, fv.Trigger.DisplayName,
				fmt.Sprintf("trigger not found: %s.%s", fv.Trigger.PieceName, fv.Trigger.TriggerName))
		}
		items, err := trig.Run(piece.TriggerContext{Payload: in.TriggerPayload, Input: fv.Trigger.Input})
		if err != nil {
			return failRun(state, fv.Trigger.Name, fv.Trigger.DisplayName, err.Error())
		}
		if len(items) > 0 {
			triggerOutput = items[0]
		} else {
			triggerOutput = nil
		}
	}

	state.Steps[fv.Trigger.Name] = &model.StepOutput{
		Type:   model.FlowActionType(fv.Trigger.Type),
		Status: model.StepSucceeded,
		Output: triggerOutput,
	}

	e.executeChain(fv.Trigger.NextAction, state)
	finalizeVerdict(state)
	return state
}

// ExecuteResume restores previously SUCCEEDED/PAUSED steps from
// in.PriorState and re-walks the chain from the trigger — steps already
// SUCCEEDED short-circuit immediately (see isCompleted checks below); a
// PAUSED step re-runs and sees ExecutionType RESUME. This includes a
// LOOP_ON_ITEMS step paused mid-iteration: its own StepOutput carries
// Status PAUSED plus the iterations recorded so far, so it passes this
// restore filter, and executeLoop resumes the specific paused iteration
// (not the whole loop from scratch) — see executeLoop's doc comment.
func (e *Engine) ExecuteResume(fv *model.FlowVersion, in ResumeInput) *model.ExecutionState {
	state := model.NewExecutionState()
	state.ResumePayload = in.ResumePayload
	for name, step := range in.PriorState.Steps {
		if step.Status == model.StepSucceeded || step.Status == model.StepPaused {
			cp := *step
			state.Steps[name] = &cp
		}
	}
	e.executeChain(fv.Trigger.NextAction, state)
	finalizeVerdict(state)
	return state
}

// ActionRunInput configures a standalone action run — no flow, no trigger.
type ActionRunInput struct {
	// Context seeds the templating scope with pre-made step outputs, e.g. to
	// feed an action mock data for a step it would normally read via {{ }}
	// in a real flow (activepieces' "test this step with sample data").
	// Keys become step names exactly as in a real flow's Steps map.
	Context map[string]*model.StepOutput
}

// ExecuteActionRun runs a single CODE or PIECE action in isolation — the Go
// analogue of activepieces' actionRunStepRunner, which reuses the exact same
// per-action executor (codeExecutor/pieceExecutor) with an empty execution
// context and actionRunMode: true. Same here: this calls the identical
// executeCode/executePiece used inside a real flow, just seeded with an
// otherwise-empty ExecutionState — same retry/continueOnFailure behavior,
// no flow-level concepts (no trigger, no chain, no LOG_SIZE_EXCEEDED).
//
// ROUTER and LOOP_ON_ITEMS are not valid here (mirrors activepieces:
// actionRunStepRunner's own type signature only ever accepted
// PieceAction | CodeAction) — a router/loop has no meaning without a
// surrounding flow to branch or iterate within.
//
// Simplification vs. activepieces for a PIECE action that tries to pause: TS
// throws from inside the WaitForWaitpoint hook itself, aborting the piece's
// run() immediately at that call. Here the hook just flips a flag (see
// executePiece), so the piece's Run still finishes and its return value is
// computed before being discarded in favor of a FAILED step — observably
// identical for a well-behaved piece that pauses-then-returns, different
// only if the piece has side effects between calling WaitForWaitpoint and
// returning (those still happen here; they wouldn't in activepieces).
func (e *Engine) ExecuteActionRun(action *model.FlowAction, in ActionRunInput) *model.ExecutionState {
	state := model.NewExecutionState()
	state.ActionRunMode = true
	for name, step := range in.Context {
		cp := *step
		state.Steps[name] = &cp
	}

	switch action.Type {
	case model.ActionCode:
		e.executeCode(action, state)
	case model.ActionPiece:
		e.executePiece(action, state)
	default:
		return failRun(state, action.Name, action.DisplayName,
			fmt.Sprintf("action type %s cannot run standalone (only CODE and PIECE can)", action.Type))
	}
	finalizeVerdict(state)
	return state
}

// LoadOptionsInput configures a dynamic-dropdown load — a config-time
// (editor) operation, not a flow run.
type LoadOptionsInput struct {
	PieceName    string
	ActionName   string
	PropertyName string
	// Input is the ALREADY-RESOLVED values of the action's other inputs
	// (including AuthInputKey, if the piece needs to authenticate to fetch
	// the list) — activepieces' resolvedInput. Unlike ExecuteBegin/
	// ExecuteActionRun, this does NOT run {{ }} templating: LoadOptions
	// input comes straight from a UI form (already-literal values), not
	// from step outputs in a running flow — deliberately simpler than
	// activepieces, which does resolve templates here too (its editor lets
	// a field reference sample data even at config time). Pass already-
	// resolved values if that matters for your use.
	Input       map[string]any
	SearchValue string
}

// LoadOptions invokes a dropdown property's LoadOptions function — the Go
// analogue of activepieces' propertyOperation/pieceHelper.executeProps,
// narrowed to DROPDOWN only (activepieces also has MULTI_SELECT_DROPDOWN and
// DYNAMIC; same "prove the mechanism, not the whole catalog" scope as the
// router's 5-of-24 operators — add more property kinds if a real flow needs
// them).
func (e *Engine) LoadOptions(in LoadOptionsInput) (piece.DropdownState, error) {
	act, ok := e.Registry.GetAction(in.PieceName, in.ActionName)
	if !ok {
		return piece.DropdownState{}, fmt.Errorf("piece action not found: %s.%s", in.PieceName, in.ActionName)
	}
	dd, ok := act.Dropdowns[in.PropertyName]
	if !ok {
		return piece.DropdownState{}, fmt.Errorf("%q is not a loadable dropdown property on %s.%s", in.PropertyName, in.PieceName, in.ActionName)
	}
	return dd.LoadOptions(in.Input, piece.PropertyContext{SearchValue: in.SearchValue})
}

func finalizeVerdict(state *model.ExecutionState) {
	if state.Verdict.Status == model.FlowRunRunning {
		state.Verdict.Status = model.FlowRunSucceeded
	}
}

func failRun(state *model.ExecutionState, stepName, displayName, message string) *model.ExecutionState {
	state.Verdict = model.Verdict{
		Status:     model.FlowRunFailed,
		FailedStep: &model.FailedStep{Name: stepName, DisplayName: displayName, Message: message},
	}
	return state
}

// executeChain walks the nextAction chain, dispatching each action to its
// executor and stopping as soon as the run leaves RUNNING (FAILED, PAUSED, or
// LOG_SIZE_EXCEEDED) — the Go analogue of flow-executor.ts's execute() loop.
func (e *Engine) executeChain(action *model.FlowAction, state *model.ExecutionState) {
	for action != nil {
		if action.Skip {
			action = action.NextAction
			continue
		}
		switch action.Type {
		case model.ActionCode:
			e.executeCode(action, state)
		case model.ActionPiece:
			e.executePiece(action, state)
		case model.ActionRouter:
			e.executeRouter(action, state)
		case model.ActionLoopOnItems:
			e.executeLoop(action, state)
		}
		e.checkLogSize(action, state)
		if state.Verdict.Status != model.FlowRunRunning {
			return
		}
		action = action.NextAction
	}
}

// checkLogSize is the Go analogue of applyLogSizeLimitIfExceeded — checked
// after every step, cutting the run short with a distinct verdict distinct
// from FAILED (LOG_SIZE_EXCEEDED), same as activepieces.
func (e *Engine) checkLogSize(action *model.FlowAction, state *model.ExecutionState) {
	if e.MaxLogSizeBytes <= 0 || state.Verdict.Status != model.FlowRunRunning {
		return
	}
	encoded, err := json.Marshal(state.Steps)
	if err != nil {
		return
	}
	if len(encoded) <= e.MaxLogSizeBytes {
		return
	}
	state.Verdict = model.Verdict{
		Status: model.FlowRunLogSizeExceeded,
		FailedStep: &model.FailedStep{
			Name:        action.Name,
			DisplayName: action.DisplayName,
			Message:     "Flow run logs size exceeded",
		},
	}
}

func ms(since time.Time) float64 {
	return float64(time.Since(since)) / float64(time.Millisecond)
}

// --- CODE -------------------------------------------------------------

func (e *Engine) executeCode(action *model.FlowAction, state *model.ExecutionState) {
	if state.IsCompleted(action.Name) {
		return
	}
	out := e.runWithRetry(action, func() *model.StepOutput {
		start := time.Now()
		resolvedInput, err := expr.ResolveMap(action.Code.Input, expr.NewScope(state))
		if err != nil {
			return failedStep(model.ActionCode, resolvedInput, err, ms(start))
		}
		result, err := sandbox.Run(action.Code.Source, resolvedInput)
		if err != nil {
			return failedStep(model.ActionCode, resolvedInput, err, ms(start))
		}
		return &model.StepOutput{Type: model.ActionCode, Status: model.StepSucceeded, Input: resolvedInput, Output: result, DurationMs: ms(start)}
	})
	recordStep(state, action, out)
}

// --- PIECE --------------------------------------------------------------

func (e *Engine) executePiece(action *model.FlowAction, state *model.ExecutionState) {
	if state.IsCompleted(action.Name) {
		return
	}
	// Declared here (not inside the retry callback) so executePiece can still
	// see the outcome after runWithRetry returns; reset at the top of each
	// attempt below so a stale value from an earlier failed retry can't leak
	// into how a later attempt's outcome is read.
	var stopResp, respondResp *model.WebhookResponse

	out := e.runWithRetry(action, func() *model.StepOutput {
		start := time.Now()
		stopResp, respondResp = nil, nil
		resolvedInput, err := expr.ResolveMap(action.Piece.Input, expr.NewScope(state))
		if err != nil {
			return failedStep(model.ActionPiece, resolvedInput, err, ms(start))
		}
		act, ok := e.Registry.GetAction(action.Piece.PieceName, action.Piece.ActionName)
		if !ok {
			return failedStep(model.ActionPiece, resolvedInput,
				fmt.Errorf("piece action not found: %s.%s", action.Piece.PieceName, action.Piece.ActionName), ms(start))
		}

		execType := model.ExecutionBegin
		if state.IsPaused(action.Name) {
			execType = model.ExecutionResume
		}
		paused := false
		ctx := piece.ActionContext{
			ExecutionType: execType,
			Input:         resolvedInput,
			Auth:          resolvedInput[piece.AuthInputKey],
			ResumePayload: state.ResumePayload,
			Files:         e.Files,
			Store:         e.Store,
			Run: piece.RunHooks{
				WaitForWaitpoint: func(string) { paused = true },
				Stop:             func(resp *model.WebhookResponse) { stopResp = resp },
				Respond:          func(resp *model.WebhookResponse) { respondResp = resp },
			},
		}
		result, err := act.Run(ctx)
		if err != nil {
			return failedStep(model.ActionPiece, resolvedInput, err, ms(start))
		}
		// Priority mirrors activepieces' piece-executor.ts: stopped is checked
		// before paused (both end in a SUCCEEDED/PAUSED step either way; the
		// distinction only matters for how the surrounding run's Verdict ends
		// up, handled below after runWithRetry returns).
		if stopResp != nil {
			return &model.StepOutput{Type: model.ActionPiece, Status: model.StepSucceeded, Input: resolvedInput, Output: result, DurationMs: ms(start)}
		}
		if paused {
			if state.ActionRunMode {
				// Mirrors activepieces' assertActionRunCannotSuspend: a standalone
				// action run has no flow to persist against, so pausing can never be
				// resumed — fail clearly instead of returning a PAUSED step nothing
				// will ever pick back up. See ExecuteActionRun's doc comment for how
				// this differs from TS's immediate in-hook throw.
				return failedStep(model.ActionPiece, resolvedInput,
					fmt.Errorf("action run cannot pause (waitpoint) — there is no flow to resume it in"), ms(start))
			}
			return &model.StepOutput{Type: model.ActionPiece, Status: model.StepPaused, Input: resolvedInput, Output: result, DurationMs: ms(start)}
		}
		return &model.StepOutput{Type: model.ActionPiece, Status: model.StepSucceeded, Input: resolvedInput, Output: result, DurationMs: ms(start)}
	})
	recordStep(state, action, out)

	if stopResp != nil {
		// Ends the run right here — activepieces sets exactly this verdict
		// shape for a stop (SUCCEEDED, never FAILED: stopping isn't an error).
		// executeChain's caller sees Verdict.Status != Running and does not
		// walk into action.NextAction.
		state.Verdict = model.Verdict{Status: model.FlowRunSucceeded, StopResponse: stopResp}
	} else if respondResp != nil && state.RespondedEarly == nil {
		// Unlike Stop, does not touch Verdict — the chain keeps going after
		// this. Only the first Respond in a run is recorded: a second one
		// would be sending a reply to an HTTP request that's already been
		// answered, same as in a real webhook handler.
		state.RespondedEarly = respondResp
	}
}

// --- ROUTER ---------------------------------------------------------------

// executeRouter's own IsCompleted check only short-circuits a router whose
// LAST recorded status is truly done (SUCCEEDED/FAILED) — see containerStepStatus
// below, which this now shares with executeLoop: if a child action inside
// the matched branch previously paused, the router's own StepOutput status
// is PAUSED, so IsCompleted (status != Paused) is false and this re-enters
// runBranch on resume. That re-entry is safe and cheap even for a branch
// that's already fully done: branch selection is a pure re-evaluation over
// already-resolved step outputs (deterministic — same branch every time),
// and the branch's own child executors (CODE/PIECE/LOOP) have their own
// IsCompleted short-circuits, so an already-succeeded grandchild just no-ops.
//
// This fixes a real bug (found by testing a ROUTER nested inside a
// LOOP_ON_ITEMS with a pause in one branch): the router's status used to be
// hardcoded StepSucceeded regardless of what its branch did, which made
// IsCompleted always short-circuit it on resume — silently skipping back
// into the paused branch entirely. The run reported SUCCEEDED while a step
// inside it was still actually PAUSED and had never really resumed.
func (e *Engine) executeRouter(action *model.FlowAction, state *model.ExecutionState) {
	if state.IsCompleted(action.Name) {
		return
	}
	scope := expr.NewScope(state)
	type evaluation struct {
		BranchName string `json:"branchName"`
		Matched    bool   `json:"matched"`
	}
	var evaluations []evaluation
	matchedIdx := -1
	for i, branch := range action.Router.Branches {
		matched := branch.Type == model.BranchFallback
		if branch.Type == model.BranchCondition {
			var err error
			matched, err = evaluateConditionGroups(branch.Conditions, scope)
			if err != nil {
				recordStep(state, action, failedStep(model.ActionRouter, nil, err, 0))
				return
			}
		}
		evaluations = append(evaluations, evaluation{BranchName: branch.Name, Matched: matched})
		if matched && matchedIdx == -1 {
			matchedIdx = i
		}
	}

	runBranch := func(i int) {
		if i < 0 || i >= len(action.Router.Children) {
			return
		}
		e.executeChain(action.Router.Children[i], state)
	}

	if action.Router.ExecutionType == model.RouterExecuteAllMatch {
		for i, ev := range evaluations {
			if !ev.Matched {
				continue
			}
			runBranch(i)
			if state.Verdict.Status != model.FlowRunRunning {
				break
			}
		}
	} else {
		runBranch(matchedIdx)
	}

	status := model.StepSucceeded
	if state.Verdict.Status != model.FlowRunRunning {
		status = containerStepStatus(state.Verdict.Status)
	}
	state.Steps[action.Name] = &model.StepOutput{
		Type:   model.ActionRouter,
		Status: status,
		Input:  map[string]any{"branches": evaluations},
	}
}

func evaluateConditionGroups(groups [][]model.Condition, scope expr.Scope) (bool, error) {
	for _, group := range groups {
		allTrue := true
		for _, cond := range group {
			ok, err := evaluateCondition(cond, scope)
			if err != nil {
				return false, err
			}
			if !ok {
				allTrue = false
				break
			}
		}
		if allTrue {
			return true, nil
		}
	}
	return false, nil
}

func evaluateCondition(cond model.Condition, scope expr.Scope) (bool, error) {
	first, err := expr.Resolve(cond.FirstValue, scope)
	if err != nil {
		return false, err
	}
	var second any
	if cond.SecondValue != "" {
		second, err = expr.Resolve(cond.SecondValue, scope)
		if err != nil {
			return false, err
		}
	}
	switch cond.Operator {
	case model.OpTextExactlyMatches:
		return toText(first, cond.CaseSensitive) == toText(second, cond.CaseSensitive), nil
	case model.OpTextContains:
		return strings.Contains(toText(first, cond.CaseSensitive), toText(second, cond.CaseSensitive)), nil
	case model.OpNumberGreaterThan:
		return toNumber(first) > toNumber(second), nil
	case model.OpNumberLessThan:
		return toNumber(first) < toNumber(second), nil
	case model.OpBooleanIsTrue:
		b, _ := first.(bool)
		return b, nil
	default:
		return false, fmt.Errorf("router: unsupported operator %q", cond.Operator)
	}
}

// --- LOOP_ON_ITEMS ----------------------------------------------------------

// executeLoop supports resuming a loop that previously paused mid-iteration
// (a PIECE action inside the loop body called WaitForWaitpoint): if the
// loop's own prior StepOutput is PAUSED, already-succeeded iterations before
// the pause are kept as-is, the paused iteration is re-run seeded with its
// own prior (PAUSED) steps — so the nested action sees ExecutionType RESUME
// and the real ResumePayload, exactly like a top-level PIECE action does —
// and, if that iteration now completes, the loop continues with the
// remaining items. This mirrors what ExecuteResume's doc comment used to
// flag as unimplemented; see engine_test.go's TestPauseInsideLoop* for the
// end-to-end proof.
func (e *Engine) executeLoop(action *model.FlowAction, state *model.ExecutionState) {
	if state.IsCompleted(action.Name) {
		return
	}
	prior, resuming := state.Steps[action.Name]
	resuming = resuming && prior.Status == model.StepPaused

	scope := expr.NewScope(state)
	itemsVal, err := expr.Eval(trimTemplate(action.Loop.Items), scope)
	if err != nil {
		recordStep(state, action, failedStep(model.ActionLoopOnItems, nil, err, 0))
		return
	}
	items, ok := itemsVal.([]any)
	if !ok {
		recordStep(state, action, failedStep(model.ActionLoopOnItems, nil,
			fmt.Errorf("loop: items expression did not resolve to an array, got %T", itemsVal), 0))
		return
	}

	var iterations []map[string]*model.StepOutput
	var lastItem any
	var lastIndex int
	startAt := 0 // 0-based index of the first NEW iteration to run

	if resuming {
		pausedIdx := prior.LastIndex - 1 // 0-based
		iterations = append(iterations, prior.Iterations[:pausedIdx]...)

		iterState := e.newLoopIterationState(state, prior.Iterations[pausedIdx], action.Name, items[pausedIdx], prior.LastIndex)
		e.executeChain(action.Loop.FirstLoopAction, iterState)
		iterations = append(iterations, e.extractIterationSteps(iterState, state, action.Name))

		if iterState.Verdict.Status != model.FlowRunRunning {
			e.finishLoop(state, action, iterState.Verdict, iterations, items[pausedIdx], prior.LastIndex)
			return
		}
		startAt = prior.LastIndex // resume succeeded; continue with the item after the one that paused
		lastItem, lastIndex = items[pausedIdx], prior.LastIndex
	}

	for i := startAt; i < len(items); i++ {
		index := i + 1 // 1-based, matches activepieces
		item := items[i]
		lastItem, lastIndex = item, index

		iterState := e.newLoopIterationState(state, nil, action.Name, item, index)
		e.executeChain(action.Loop.FirstLoopAction, iterState)
		iterations = append(iterations, e.extractIterationSteps(iterState, state, action.Name))

		if iterState.Verdict.Status != model.FlowRunRunning {
			e.finishLoop(state, action, iterState.Verdict, iterations, lastItem, lastIndex)
			return
		}
	}

	e.finishLoop(state, action, model.Verdict{Status: model.FlowRunSucceeded}, iterations, lastItem, lastIndex)
}

// newLoopIterationState builds the isolated state one loop iteration runs
// in: a copy of the outer (pre-loop) steps, so the body can reference prior
// top-level steps, plus — when seed is non-nil (resuming a specific paused
// iteration) — that iteration's own previously-recorded steps layered on
// top, so e.g. a paused PIECE action's IsPaused() check still finds it.
func (e *Engine) newLoopIterationState(outer *model.ExecutionState, seed map[string]*model.StepOutput, loopName string, item any, index int) *model.ExecutionState {
	iterState := &model.ExecutionState{
		Steps:         map[string]*model.StepOutput{},
		Verdict:       model.Verdict{Status: model.FlowRunRunning},
		ResumePayload: outer.ResumePayload,
	}
	for k, v := range outer.Steps {
		cp := *v
		iterState.Steps[k] = &cp
	}
	for k, v := range seed {
		cp := *v
		iterState.Steps[k] = &cp
	}
	iterState.Steps[loopName] = &model.StepOutput{Output: map[string]any{"item": item, "index": index}}
	return iterState
}

// extractIterationSteps returns only the steps that belong to this
// iteration's own body — i.e. iterState.Steps minus the loop's own synthetic
// item/index entry and minus everything already present in the outer state
// (prior top-level steps, which are visible for templating but aren't part
// of the iteration's own recorded result).
func (e *Engine) extractIterationSteps(iterState, outer *model.ExecutionState, loopName string) map[string]*model.StepOutput {
	delete(iterState.Steps, loopName)
	for k := range outer.Steps {
		delete(iterState.Steps, k)
	}
	return iterState.Steps
}

func (e *Engine) finishLoop(state *model.ExecutionState, action *model.FlowAction, verdict model.Verdict, iterations []map[string]*model.StepOutput, lastItem any, lastIndex int) {
	status := model.StepSucceeded
	if verdict.Status != model.FlowRunSucceeded {
		status = containerStepStatus(verdict.Status)
		state.Verdict = verdict
	}
	state.Steps[action.Name] = &model.StepOutput{
		Type:       model.ActionLoopOnItems,
		Status:     status,
		Iterations: iterations,
		LastItem:   lastItem,
		LastIndex:  lastIndex,
	}
}

// containerStepStatus maps a nested chain's run-level verdict to a container
// action's (LOOP_ON_ITEMS or ROUTER) own StepOutputStatus. Shared by both
// because both used to hardcode StepFailed/StepSucceeded regardless of what
// actually happened inside them, which made ExecuteResume's
// SUCCEEDED-or-PAUSED restore filter skip them entirely on resume — silently
// discarding progress (loop) or orphaning a still-paused branch (router).
func containerStepStatus(runStatus model.FlowRunStatus) model.StepOutputStatus {
	if runStatus == model.FlowRunPaused {
		return model.StepPaused
	}
	return model.StepFailed
}

// --- shared helpers ---------------------------------------------------------

// runWithRetry retries run() up to Retry.MaxAttempts when the action opts in
// via ErrorHandling.RetryOnFailure, then applies ContinueOnFailure to decide
// whether a final failure stops the whole run.
func (e *Engine) runWithRetry(action *model.FlowAction, run func() *model.StepOutput) *model.StepOutput {
	retryEnabled := action.Error != nil && action.Error.RetryOnFailure
	attempt := 1
	var out *model.StepOutput
	for {
		out = run()
		if out.Status != model.StepFailed {
			return out
		}
		if !retryEnabled || attempt >= e.Retry.MaxAttempts {
			return out
		}
		backoffMs := math.Pow(e.Retry.ExponentialBase, float64(attempt)) * e.Retry.IntervalMs
		time.Sleep(time.Duration(backoffMs) * time.Millisecond)
		attempt++
	}
}

// recordStep stores out in state.Steps and updates state.Verdict:
// FAILED stops the run unless ContinueOnFailure is set (in which case the
// step stays FAILED but the run keeps going); PAUSED always stops the run.
func recordStep(state *model.ExecutionState, action *model.FlowAction, out *model.StepOutput) {
	state.Steps[action.Name] = out
	switch out.Status {
	case model.StepFailed:
		continueOnFailure := action.Error != nil && action.Error.ContinueOnFailure
		if !continueOnFailure {
			state.Verdict = model.Verdict{
				Status:     model.FlowRunFailed,
				FailedStep: &model.FailedStep{Name: action.Name, DisplayName: action.DisplayName, Message: out.ErrorMessage},
			}
		}
	case model.StepPaused:
		state.Verdict = model.Verdict{Status: model.FlowRunPaused}
	}
}

func failedStep(t model.FlowActionType, input any, err error, durationMs float64) *model.StepOutput {
	return &model.StepOutput{Type: t, Status: model.StepFailed, Input: input, ErrorMessage: err.Error(), DurationMs: durationMs}
}

func toText(v any, caseSensitive bool) string {
	var s string
	switch val := v.(type) {
	case nil:
		s = ""
	case string:
		s = val
	default:
		s = fmt.Sprintf("%v", val)
	}
	if !caseSensitive {
		return strings.ToLower(s)
	}
	return s
}

// toNumber also parses numeric strings (strconv), not just Go numeric types:
// a router condition's Value fields are plain user-entered literals more
// often than {{ }} templates (e.g. "10" typed directly, never wrapped), and
// expr.Resolve leaves a non-templated string as-is — without this, comparing
// against any un-templated numeric literal silently evaluated as 0.
func toNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}

// trimTemplate strips a {{ }} wrapper for the one place (LOOP_ON_ITEMS'
// Items field) that must evaluate to a raw array rather than going through
// expr.Resolve's string-templating/stringification path.
func trimTemplate(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{{")
	s = strings.TrimSuffix(s, "}}")
	return strings.TrimSpace(s)
}
