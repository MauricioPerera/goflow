package catalog

import (
	"encoding/json"
	"fmt"
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
// checking WantError and, if CheckOutput is set, WantOutput. Returns nil
// only if def is both structurally sound and every example behaves as
// asserted.
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
