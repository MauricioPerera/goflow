package catalog

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"goflow/pkg/model"
	"goflow/pkg/piece"
)

// Example is one worked Input/Output case for an ActionDefinition —
// Validate actually runs it against the real action before a piece is
// allowed into the catalog. This is the only kind of "correctness" check
// available here: there's no engine-enforced schema to check a JS action
// against (Input is untyped map[string]any, same reasoning as
// ActionDefinition.InputSchema being free text), so a worked example
// supplied by the piece's own author (agent or human) is what stands in
// for a real contract.
type Example struct {
	// Description is an optional human/agent-readable label for what this
	// example demonstrates (e.g. "amount over 1000 classifies as high").
	Description string

	Input map[string]any
	Auth  any

	// WantError, if true, means running Input must produce an error —
	// e.g. an example proving missing/invalid input is rejected clearly.
	// Mutually exclusive with CheckOutput in intent (an errored Run has no
	// output to check), though nothing stops both being set; WantError is
	// checked first.
	WantError bool

	// CheckOutput enables comparing Run's actual output against WantOutput.
	// A separate bool from "WantOutput != nil" specifically so an example
	// CAN assert the output must be nil/null — Go's `any` zero value being
	// nil makes "not checking" and "expecting nil" otherwise
	// indistinguishable.
	CheckOutput bool
	WantOutput  any
}

// TriggerExample is one worked Payload/Items case for a
// TriggerDefinition — the symmetric counterpart of Example for a trigger
// instead of an action. Validate actually runs it against the real
// (JS-backed) trigger before a piece is allowed into the catalog, the
// same quality gate Example provides for actions.
type TriggerExample struct {
	// Description is an optional human/agent-readable label for what this
	// example demonstrates (e.g. "two new orders both map through").
	Description string

	// Payload is the raw trigger payload passed to the trigger's Run as
	// piece.TriggerContext.Payload — the same field Engine.ExecuteBegin
	// feeds a PIECE trigger from BeginInput.TriggerPayload.
	Payload any

	// Input is the trigger's resolved Input map, passed to Run as
	// piece.TriggerContext.Input.
	Input map[string]any

	// WantError, if true, means running the trigger with Payload/Input must
	// produce an error. Mutually exclusive with CheckOutput in intent (an
	// errored Run has no items to check), though nothing stops both being
	// set; WantError is checked first. Same semantics as Example.WantError.
	WantError bool

	// CheckOutput enables comparing Run's actual returned items against
	// WantItems. A separate bool from "WantItems != nil" specifically so an
	// example CAN assert the trigger must return an empty array — Go's
	// `any`/slice zero values would otherwise make "not checking" and
	// "expecting empty" indistinguishable. Same reasoning as
	// Example.CheckOutput.
	CheckOutput bool
	WantItems   []any
}

// DropdownExample is one worked PropsValue/SearchValue case for a
// DropdownDefinition — the symmetric counterpart of Example for a dropdown
// instead of an action. Validate actually runs it against the real
// (JS-backed) dropdown's LoadOptions before a piece is allowed into the
// catalog, the same quality gate Example provides for actions and
// TriggerExample for triggers.
type DropdownExample struct {
	// Description is an optional human/agent-readable label for what this
	// example demonstrates (e.g. "search filters to us-east-1").
	Description string

	// PropsValue is the already-resolved sibling input values passed to
	// LoadOptions as its first argument — the same map piece.Action.Dropdowns
	// receives, and what Engine.LoadOptions forwards from LoadOptionsInput.
	// Input. May be nil for a dropdown that ignores its siblings.
	PropsValue map[string]any

	// SearchValue is passed to LoadOptions as piece.PropertyContext.SearchValue
	// — the editor's current search term, same field Engine.LoadOptions sets.
	SearchValue string

	// WantError, if true, means running LoadOptions with PropsValue/
	// SearchValue must produce an error. Mutually exclusive with CheckOutput
	// in intent (an errored LoadOptions has no state to check), though nothing
	// stops both being set; WantError is checked first. Same semantics as
	// Example.WantError.
	WantError bool

	// CheckOutput enables comparing LoadOptions' actual returned state
	// against WantDisabled, WantPlaceholder, and WantOptions. A separate bool
	// from "WantOptions != nil" specifically so an example CAN assert the
	// dropdown must return an empty options slice — Go's slice zero value
	// would otherwise make "not checking" and "expecting empty"
	// indistinguishable. Same reasoning as Example.CheckOutput.
	CheckOutput bool

	// WantDisabled, WantPlaceholder, and WantOptions are the expected
	// piece.DropdownState fields compared (only) when CheckOutput is set.
	// WantOptions is compared with reflect.DeepEqual so an exact match —
	// both Label and Value of every option, in order — is required.
	WantDisabled    bool
	WantPlaceholder string
	WantOptions     []piece.DropdownOption
}

// ValidationError is one problem Validate found — same {Path, Message}
// shape as piece.ValidationError, deliberately, for a consistent error
// style across both validators.
type ValidationError struct {
	Path    string
	Message string
}

// Validate checks def structurally (via piece.Validate on its ToPiece())
// and functionally: every action must have at least one Example, and
// every Example is actually run against the real (JS-backed) action,
// checking WantError and, if CheckOutput is set, WantOutput; likewise
// every trigger must have at least one TriggerExample, and every
// TriggerExample is actually run against the real (JS-backed) trigger,
// checking WantError and, if CheckOutput is set, WantItems; likewise every
// dropdown on every action must have at least one DropdownExample, and
// every DropdownExample is actually run against the real (JS-backed)
// dropdown's LoadOptions, checking WantError and, if CheckOutput is set,
// WantDisabled/WantPlaceholder/WantOptions. Returns nil only if def is
// both structurally sound and every example behaves as asserted.
func Validate(def Definition) []ValidationError {
	var errs []ValidationError

	p := def.ToPiece()
	for _, e := range piece.Validate(p) {
		errs = append(errs, ValidationError{Path: e.Path, Message: e.Message})
	}

	for _, a := range def.Actions {
		path := fmt.Sprintf("actions[%s]", a.Name)
		if len(a.Examples) == 0 {
			errs = append(errs, ValidationError{
				Path:    path,
				Message: "no Examples provided — at least one is required so Validate can confirm the action actually works, not just that it's structurally well-formed",
			})
			continue
		}
		act, ok := p.Actions[a.Name]
		if !ok {
			// Already reported by piece.Validate above (Name/key
			// mismatch) — nothing further to run.
			continue
		}
		for i, ex := range a.Examples {
			errs = append(errs, runExample(path, i, act, ex)...)
		}
	}

	for _, tr := range def.Triggers {
		path := fmt.Sprintf("triggers[%s]", tr.Name)
		if len(tr.Examples) == 0 {
			errs = append(errs, ValidationError{
				Path:    path,
				Message: "no Examples provided — at least one is required so Validate can confirm the trigger actually works, not just that it's structurally well-formed",
			})
			continue
		}
		trig, ok := p.Triggers[tr.Name]
		if !ok {
			// Already reported by piece.Validate above (Name/key
			// mismatch) — nothing further to run.
			continue
		}
		for i, ex := range tr.Examples {
			errs = append(errs, runTriggerExample(path, i, trig, ex)...)
		}
	}

	// Dropdowns hang off each action (same ownership as piece.Action.Dropdowns),
	// so this is a second pass over def.Actions rather than a top-level loop.
	// Deliberately separate from the action-examples pass above so an action
	// with zero Examples is still reported on its own (the pass above `continue`s
	// in that case) while a dropdown with zero Examples is still caught here.
	for _, a := range def.Actions {
		path := fmt.Sprintf("actions[%s]", a.Name)
		act, ok := p.Actions[a.Name]
		if !ok {
			// Already reported by piece.Validate above (Name/key mismatch) —
			// nothing further to run.
			continue
		}
		for ddName, dd := range a.Dropdowns {
			ddPath := fmt.Sprintf("%s.dropdowns[%s]", path, ddName)
			if len(dd.Examples) == 0 {
				errs = append(errs, ValidationError{
					Path:    ddPath,
					Message: "no Examples provided — at least one is required so Validate can confirm the dropdown actually works, not just that it's structurally well-formed",
				})
				continue
			}
			prop, ok := act.Dropdowns[ddName]
			if !ok {
				// Already reported by piece.Validate above (LoadOptions is
				// nil / dropdown missing on the built piece) — nothing further
				// to run.
				continue
			}
			for i, ex := range dd.Examples {
				errs = append(errs, runDropdownExample(ddPath, i, prop, ex)...)
			}
		}
	}

	return errs
}

func runExample(actionPath string, index int, act piece.Action, ex Example) []ValidationError {
	path := fmt.Sprintf("%s.examples[%d]", actionPath, index)
	if ex.Description != "" {
		path = fmt.Sprintf("%s (%s)", path, ex.Description)
	}

	out, err := act.Run(piece.ActionContext{
		Input: ex.Input,
		Auth:  ex.Auth,
		Files: piece.NewMemoryFileWriter(),
		Store: piece.NewMemoryStore(),
		Run: piece.RunHooks{
			Stop:             func(*model.WebhookResponse) {},
			Respond:          func(*model.WebhookResponse) {},
			WaitForWaitpoint: func(string) {},
		},
	})

	if ex.WantError {
		if err == nil {
			return []ValidationError{{Path: path, Message: fmt.Sprintf("expected an error, got a successful result: %#v", out)}}
		}
		return nil
	}
	if err != nil {
		return []ValidationError{{Path: path, Message: fmt.Sprintf("unexpected error: %v", err)}}
	}
	if ex.CheckOutput && !jsonEqual(out, ex.WantOutput) {
		return []ValidationError{{Path: path, Message: fmt.Sprintf("output = %#v, want %#v", out, ex.WantOutput)}}
	}
	return nil
}

// runTriggerExample is the symmetric counterpart of runExample for a
// trigger: it runs the real (JS-backed) trigger with the example's
// Payload and Input — deliberately WITHOUT a Store, matching
// Engine.ExecuteBegin, which constructs a piece.TriggerContext as
// {Payload, Input} only and never sets Store when invoking a PIECE
// trigger (confirmed by reading engine.go). A trigger example that
// needs Store to behave correctly therefore can't be validated here,
// the same way it can't actually run through ExecuteBegin — that
// polling-cursor pattern only works when something else calls trig.Run
// directly and reuses one Store across calls itself.
func runTriggerExample(triggerPath string, index int, trig piece.Trigger, ex TriggerExample) []ValidationError {
	path := fmt.Sprintf("%s.examples[%d]", triggerPath, index)
	if ex.Description != "" {
		path = fmt.Sprintf("%s (%s)", path, ex.Description)
	}

	items, err := trig.Run(piece.TriggerContext{
		Payload: ex.Payload,
		Input:   ex.Input,
		// Store intentionally left nil — see the doc comment above.
	})

	if ex.WantError {
		if err == nil {
			return []ValidationError{{Path: path, Message: fmt.Sprintf("expected an error, got a successful result: %#v", items)}}
		}
		return nil
	}
	if err != nil {
		return []ValidationError{{Path: path, Message: fmt.Sprintf("unexpected error: %v", err)}}
	}
	if ex.CheckOutput && !jsonEqual(items, ex.WantItems) {
		return []ValidationError{{Path: path, Message: fmt.Sprintf("items = %#v, want %#v", items, ex.WantItems)}}
	}
	return nil
}

// runDropdownExample is the symmetric counterpart of runExample for a
// dropdown: it runs the real (JS-backed) dropdown's LoadOptions with the
// example's PropsValue and SearchValue — the exact pair Engine.LoadOptions
// passes (propsValue first, piece.PropertyContext{SearchValue} second), so a
// passing example here is the same call path a real editor UI would make.
func runDropdownExample(dropdownPath string, index int, dd piece.DropdownProperty, ex DropdownExample) []ValidationError {
	path := fmt.Sprintf("%s.examples[%d]", dropdownPath, index)
	if ex.Description != "" {
		path = fmt.Sprintf("%s (%s)", path, ex.Description)
	}

	state, err := dd.LoadOptions(ex.PropsValue, piece.PropertyContext{SearchValue: ex.SearchValue})

	if ex.WantError {
		if err == nil {
			return []ValidationError{{Path: path, Message: fmt.Sprintf("expected an error, got a successful result: %#v", state)}}
		}
		return nil
	}
	if err != nil {
		return []ValidationError{{Path: path, Message: fmt.Sprintf("unexpected error: %v", err)}}
	}
	if ex.CheckOutput {
		if state.Disabled != ex.WantDisabled {
			return []ValidationError{{Path: path, Message: fmt.Sprintf("disabled = %v, want %v", state.Disabled, ex.WantDisabled)}}
		}
		if state.Placeholder != ex.WantPlaceholder {
			return []ValidationError{{Path: path, Message: fmt.Sprintf("placeholder = %q, want %q", state.Placeholder, ex.WantPlaceholder)}}
		}
		if !reflect.DeepEqual(state.Options, ex.WantOptions) {
			return []ValidationError{{Path: path, Message: fmt.Sprintf("options = %#v, want %#v", state.Options, ex.WantOptions)}}
		}
	}
	return nil
}

// jsonEqual compares a and b via their JSON encoding rather than
// reflect.DeepEqual — a JS action's output can come back as int64 where a
// hand-written WantOutput uses float64 (or vice versa; see this project's
// other notes on goja's number-export quirks), and Go's json.Marshal
// normalizes both to the same numeral text, sidestepping the mismatch
// entirely. Map keys are also sorted by encoding/json, making this stable
// regardless of original map iteration order.
func jsonEqual(a, b any) bool {
	aJSON, errA := json.Marshal(a)
	bJSON, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(aJSON) == string(bJSON)
}

// FormatValidationErrors joins errs into one human-readable message —
// what GatedStore.Save wraps its rejection error with.
func FormatValidationErrors(errs []ValidationError) string {
	lines := make([]string, len(errs))
	for i, e := range errs {
		lines[i] = fmt.Sprintf("%s: %s", e.Path, e.Message)
	}
	return strings.Join(lines, "; ")
}
