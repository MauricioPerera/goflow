// Package model defines the flow/action/execution-state types shared by the
// whole engine. Deliberately not a 1:1 port of activepieces' TypeScript types —
// simplified where Go idioms differ (no zod, no separate on-disk code
// artifacts, discriminated unions become typed pointer fields instead of a
// tagged union type).
package model

// FlowRunStatus is the terminal (or in-progress) status of a whole flow run.
type FlowRunStatus string

const (
	FlowRunRunning         FlowRunStatus = "RUNNING"
	FlowRunSucceeded       FlowRunStatus = "SUCCEEDED"
	FlowRunFailed          FlowRunStatus = "FAILED"
	FlowRunPaused          FlowRunStatus = "PAUSED"
	FlowRunLogSizeExceeded FlowRunStatus = "LOG_SIZE_EXCEEDED"
)

// StepOutputStatus is the status of a single step within a run.
type StepOutputStatus string

const (
	StepRunning   StepOutputStatus = "RUNNING"
	StepSucceeded StepOutputStatus = "SUCCEEDED"
	StepFailed    StepOutputStatus = "FAILED"
	StepPaused    StepOutputStatus = "PAUSED"
)

// FlowActionType discriminates which settings field on FlowAction is populated.
type FlowActionType string

const (
	ActionCode        FlowActionType = "CODE"
	ActionPiece       FlowActionType = "PIECE"
	ActionRouter      FlowActionType = "ROUTER"
	ActionLoopOnItems FlowActionType = "LOOP_ON_ITEMS"
	ActionCallFlow    FlowActionType = "CALL_FLOW"
)

// FlowTriggerType discriminates a trigger: EMPTY passes its payload through
// untouched; PIECE invokes a real Trigger's Run hook (see pkg/piece).
type FlowTriggerType string

const (
	TriggerEmpty FlowTriggerType = "EMPTY"
	TriggerPiece FlowTriggerType = "PIECE_TRIGGER"
)

// ExecutionType mirrors what a step/trigger sees: first run vs. resumed after
// a pause.
type ExecutionType string

const (
	ExecutionBegin  ExecutionType = "BEGIN"
	ExecutionResume ExecutionType = "RESUME"
)

// RouterExecutionType controls how many branches of a ROUTER action run.
type RouterExecutionType string

const (
	RouterExecuteFirstMatch RouterExecutionType = "EXECUTE_FIRST_MATCH"
	RouterExecuteAllMatch   RouterExecutionType = "EXECUTE_ALL_MATCH"
)

// BranchType discriminates a RouterBranch: CONDITION is evaluated; FALLBACK
// always matches if reached.
type BranchType string

const (
	BranchCondition BranchType = "CONDITION"
	BranchFallback  BranchType = "FALLBACK"
)

// BranchOperator is a small, deliberately non-exhaustive subset of
// activepieces' 24 operators — enough to prove routing works; add more as
// real flows need them.
type BranchOperator string

const (
	OpTextExactlyMatches BranchOperator = "TEXT_EXACTLY_MATCHES"
	OpTextContains       BranchOperator = "TEXT_CONTAINS"
	OpNumberGreaterThan  BranchOperator = "NUMBER_IS_GREATER_THAN"
	OpNumberLessThan     BranchOperator = "NUMBER_IS_LESS_THAN"
	OpBooleanIsTrue      BranchOperator = "BOOLEAN_IS_TRUE"
)

// Condition is one comparison inside a branch's condition group.
//
// json tags on this and every other flow-DEFINITION type below (down to
// FlowVersion) exist so a flow can be authored as JSON data — see
// ParseFlowVersion in json.go — instead of only as Go struct literals. The
// runtime/result types further down (StepOutput, Verdict, ExecutionState,
// etc.) deliberately have no tags: nothing has asked for those to be
// caller-authored data yet, and Go's default field-name marshaling already
// covers ad hoc inspection (see Engine's own use of json.Marshal for log
// size estimation).
type Condition struct {
	Operator      BranchOperator `json:"operator"`
	FirstValue    string         `json:"firstValue"`  // template string, resolved with {{ }} before evaluating
	SecondValue   string         `json:"secondValue"` // template string, resolved with {{ }} before evaluating
	CaseSensitive bool           `json:"caseSensitive"`
}

// RouterBranch is one arm of a ROUTER action. Conditions is an OR-of-AND
// group list, matching activepieces: Conditions[i] (a group) must ALL be
// true; any group matching makes the branch match.
type RouterBranch struct {
	Name       string        `json:"name"`
	Type       BranchType    `json:"type"`
	Conditions [][]Condition `json:"conditions,omitempty"` // ignored when Type == BranchFallback
}

// ErrorHandling controls retry/continue behavior for CODE and PIECE actions.
type ErrorHandling struct {
	RetryOnFailure    bool `json:"retryOnFailure"`
	ContinueOnFailure bool `json:"continueOnFailure"`
}

// FlowAction is one node in the action chain. Type selects exactly one of
// Code/Piece/Router/Loop/CallFlow to be non-nil.
type FlowAction struct {
	Name        string         `json:"name"`
	DisplayName string         `json:"displayName"`
	Type        FlowActionType `json:"type"`
	Skip        bool           `json:"skip,omitempty"`
	NextAction  *FlowAction    `json:"nextAction,omitempty"`
	Error       *ErrorHandling `json:"error,omitempty"`

	Code     *CodeSettings     `json:"code,omitempty"`
	Piece    *PieceSettings    `json:"piece,omitempty"`
	Router   *RouterSettings   `json:"router,omitempty"`
	Loop     *LoopSettings     `json:"loop,omitempty"`
	CallFlow *CallFlowSettings `json:"callFlow,omitempty"`
}

// CodeSettings holds a CODE action's inputs and JS source. Unlike
// activepieces (which compiles user code to an on-disk artifact ahead of
// time), the source lives inline — see pkg/sandbox for the execution
// contract.
type CodeSettings struct {
	Input  map[string]any `json:"input,omitempty"` // values may contain {{ }} templates, resolved before running
	Source string         `json:"source"`          // JS source: must evaluate to a function (params) => value
}

// PieceSettings holds a PIECE action's inputs and which registered piece/action to invoke.
type PieceSettings struct {
	PieceName  string         `json:"pieceName"`
	ActionName string         `json:"actionName"`
	Input      map[string]any `json:"input,omitempty"`
}

// RouterSettings holds a ROUTER action's branches and their child actions.
// Children is index-aligned with Branches: Children[i] runs if Branches[i] matches.
type RouterSettings struct {
	Branches      []RouterBranch      `json:"branches"`
	Children      []*FlowAction       `json:"children"`
	ExecutionType RouterExecutionType `json:"executionType"`
}

// LoopSettings holds a LOOP_ON_ITEMS action's item source and per-iteration body.
type LoopSettings struct {
	Items           string      `json:"items"` // template string resolving to an array
	FirstLoopAction *FlowAction `json:"firstLoopAction,omitempty"`
}

// CallFlowSettings holds a CALL_FLOW action's target and the trigger payload
// to pass it — the Go analogue of n8n's "Execute Workflow" node. FlowName
// names ANOTHER saved flow (looked up the same way OnFailureFlow is);
// Input is that sub-flow's trigger payload, {{ }}-templated like every
// other action's Input before running. See Engine.CallFlow's doc comment
// for how the sub-flow actually gets invoked (pkg/engine itself has no
// concept of a flow store — this is wired in from pkg/flowstore).
type CallFlowSettings struct {
	FlowName string         `json:"flowName"`
	Input    map[string]any `json:"input,omitempty"`
}

// FlowTrigger is the entry point of a flow.
type FlowTrigger struct {
	Name        string          `json:"name"`
	DisplayName string          `json:"displayName"`
	Type        FlowTriggerType `json:"type"`
	NextAction  *FlowAction     `json:"nextAction,omitempty"`

	// Populated only when Type == TriggerPiece.
	PieceName   string         `json:"pieceName,omitempty"`
	TriggerName string         `json:"triggerName,omitempty"`
	Input       map[string]any `json:"input,omitempty"`
}

// FlowVersion is the full definition of one flow.
type FlowVersion struct {
	ID          string       `json:"id"`
	FlowID      string       `json:"flowId,omitempty"`
	DisplayName string       `json:"displayName,omitempty"`
	Trigger     *FlowTrigger `json:"trigger"`
}

// StepOutput is the recorded result of one executed step (action or trigger).
type StepOutput struct {
	Type         FlowActionType
	Status       StepOutputStatus
	Input        any
	Output       any
	ErrorMessage string
	DurationMs   float64

	// LOOP_ON_ITEMS only.
	Iterations []map[string]*StepOutput
	LastItem   any
	LastIndex  int
}

// FailedStep identifies which step ended a run.
type FailedStep struct {
	Name        string
	DisplayName string
	Message     string
}

// WebhookResponse is what a PIECE action synchronously hands back to whoever
// triggered the flow via a webhook — the Go analogue of activepieces'
// RespondResponse (StopHookParams/RespondHookParams's inner `response`).
type WebhookResponse struct {
	Status  int
	Body    any
	Headers map[string]string
}

// Verdict is the terminal (or in-progress) outcome of a run.
type Verdict struct {
	Status     FlowRunStatus
	FailedStep *FailedStep

	// StopResponse is set when a PIECE action calls Run.Stop(...) —
	// activepieces' context.run.stop(...). It ends the run right there
	// (Status becomes FlowRunSucceeded, matching activepieces: a stop is not
	// a failure) with this as the synchronous webhook reply.
	StopResponse *WebhookResponse
}

// ExecutionState is the accumulated result of a flow run so far — the Go
// analogue of activepieces' FlowExecutorContext. Unlike the TS engine's
// immutable-context-returned-from-every-call style, this is mutated in place;
// callers that need a snapshot should Clone() first.
type ExecutionState struct {
	Steps   map[string]*StepOutput
	Verdict Verdict
	LogSize int // running byte-count approximation, for LOG_SIZE_EXCEEDED

	// ResumePayload is set only for a RESUME run; a PIECE action reads it via
	// ActionContext.ResumePayload after being re-invoked past a pause.
	ResumePayload any

	// ActionRunMode marks a standalone single-action run (Engine.ExecuteActionRun) —
	// no trigger, no flow, no persistence to ever resume from. A PIECE action
	// calling WaitForWaitpoint in this mode fails the step instead of pausing,
	// since there's nothing to resume — the Go analogue of activepieces'
	// assertActionRunCannotSuspend.
	ActionRunMode bool

	// RespondedEarly is set the first time a PIECE action calls Run.Respond(...)
	// — activepieces' context.run.respond(...): unlike Stop, the run keeps
	// executing afterward (Verdict.Status is untouched), but a synchronous
	// webhook reply has already effectively gone out. There is deliberately no
	// simulated HTTP layer here to actually deliver it — this field is the
	// observable proof that Respond fired, at the point it fired, distinct
	// from whatever the run's live-later Verdict/Steps end up being.
	RespondedEarly *WebhookResponse
}

func NewExecutionState() *ExecutionState {
	return &ExecutionState{
		Steps:   map[string]*StepOutput{},
		Verdict: Verdict{Status: FlowRunRunning},
	}
}

// IsCompleted mirrors activepieces' FlowExecutorContext.isCompleted: a step
// counts as completed (skip re-running it) if it exists and is not PAUSED —
// SUCCEEDED and even FAILED (already handled by continueOnFailure) both skip.
func (s *ExecutionState) IsCompleted(name string) bool {
	step, ok := s.Steps[name]
	if !ok {
		return false
	}
	return step.Status != StepPaused
}

func (s *ExecutionState) IsPaused(name string) bool {
	step, ok := s.Steps[name]
	return ok && step.Status == StepPaused
}

func (s *ExecutionState) Clone() *ExecutionState {
	stepsCopy := make(map[string]*StepOutput, len(s.Steps))
	for k, v := range s.Steps {
		vCopy := *v
		stepsCopy[k] = &vCopy
	}
	return &ExecutionState{Steps: stepsCopy, Verdict: s.Verdict, LogSize: s.LogSize}
}
