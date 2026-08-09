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
type Condition struct {
	Operator      BranchOperator
	FirstValue    string // template string, resolved with {{ }} before evaluating
	SecondValue   string // template string, resolved with {{ }} before evaluating
	CaseSensitive bool
}

// RouterBranch is one arm of a ROUTER action. Conditions is an OR-of-AND
// group list, matching activepieces: Conditions[i] (a group) must ALL be
// true; any group matching makes the branch match.
type RouterBranch struct {
	Name       string
	Type       BranchType
	Conditions [][]Condition // ignored when Type == BranchFallback
}

// ErrorHandling controls retry/continue behavior for CODE and PIECE actions.
type ErrorHandling struct {
	RetryOnFailure    bool
	ContinueOnFailure bool
}

// FlowAction is one node in the action chain. Type selects exactly one of
// Code/Piece/Router/Loop to be non-nil.
type FlowAction struct {
	Name        string
	DisplayName string
	Type        FlowActionType
	Skip        bool
	NextAction  *FlowAction
	Error       *ErrorHandling

	Code   *CodeSettings
	Piece  *PieceSettings
	Router *RouterSettings
	Loop   *LoopSettings
}

// CodeSettings holds a CODE action's inputs and JS source. Unlike
// activepieces (which compiles user code to an on-disk artifact ahead of
// time), the source lives inline — see pkg/sandbox for the execution
// contract.
type CodeSettings struct {
	Input  map[string]any // values may contain {{ }} templates, resolved before running
	Source string         // JS source: must evaluate to a function (params) => value
}

// PieceSettings holds a PIECE action's inputs and which registered piece/action to invoke.
type PieceSettings struct {
	PieceName  string
	ActionName string
	Input      map[string]any
}

// RouterSettings holds a ROUTER action's branches and their child actions.
// Children is index-aligned with Branches: Children[i] runs if Branches[i] matches.
type RouterSettings struct {
	Branches      []RouterBranch
	Children      []*FlowAction
	ExecutionType RouterExecutionType
}

// LoopSettings holds a LOOP_ON_ITEMS action's item source and per-iteration body.
type LoopSettings struct {
	Items           string // template string resolving to an array
	FirstLoopAction *FlowAction
}

// FlowTrigger is the entry point of a flow.
type FlowTrigger struct {
	Name        string
	DisplayName string
	Type        FlowTriggerType
	NextAction  *FlowAction

	// Populated only when Type == TriggerPiece.
	PieceName   string
	TriggerName string
	Input       map[string]any
}

// FlowVersion is the full definition of one flow.
type FlowVersion struct {
	ID          string
	FlowID      string
	DisplayName string
	Trigger     *FlowTrigger
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
