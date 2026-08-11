package flowstore

import (
	"encoding/json"
	"fmt"

	"goflow/pkg/flowvalidate"
	"goflow/pkg/model"
	"goflow/pkg/piece"
)

// ValidateExamples runs flowvalidate.Validate against def.Flow and registry
// first — if that reports anything, def.Examples are never run at all and
// those structural errors are returned as-is. This deliberately diverges
// from catalog.Validate's own precedent (which runs a piece's examples
// even when catalog.piece.Validate already found problems): a piece
// example running against a merely mis-described piece is still safe to
// attempt, but a flow with invalid JS syntax or a malformed {{ }} template
// is not safe (or even meaningful) to actually EXECUTE — Run itself
// already refuses in that case, so this does too, rather than trying and
// producing a confusing secondary failure on top of the real one.
//
// Once structural validation passes, every example in def.Examples is run
// (all of them, not stopping at the first failure — same "report
// everything" convention flowvalidate.Validate itself already follows) via
// the plain Run path: no credentials.Store (an example's Trigger must
// carry any auth value it needs as a literal, the same limitation
// catalog.Example.Auth already has — no $credential resolution happens
// here) and no CALL_FLOW support (callFlow is nil — a CALL_FLOW action
// inside a flow being example-tested fails with "not enabled" unless the
// example sets WantError, real added scope this doesn't take on yet).
func ValidateExamples(def FlowDefinition, registry *piece.Registry) []flowvalidate.ValidationError {
	if errs := flowvalidate.Validate(&def.Flow, registry); len(errs) > 0 {
		return errs
	}
	if len(def.Examples) == 0 {
		return nil
	}

	buildRegistry := func() (*piece.Registry, error) { return registry, nil }

	var errs []flowvalidate.ValidationError
	for i, ex := range def.Examples {
		path := fmt.Sprintf("examples[%d]", i)
		if ex.Description != "" {
			path = fmt.Sprintf("%s (%s)", path, ex.Description)
		}

		state, exampleErrs, err := Run(&def.Flow, buildRegistry, nil, ex.Trigger, ex.ExecuteTrigger)
		if err != nil {
			errs = append(errs, flowvalidate.ValidationError{Path: path, Message: fmt.Sprintf("running example: %v", err)})
			continue
		}
		if len(exampleErrs) > 0 {
			// Unreachable in practice (the flow already validated once,
			// against the same registry, above) — kept defensive rather
			// than silently swallowed.
			errs = append(errs, flowvalidate.ValidationError{Path: path, Message: "running example: " + FormatValidationErrors(exampleErrs)})
			continue
		}

		failed := state.Verdict.Status == model.FlowRunFailed
		switch {
		case ex.WantError && !failed:
			errs = append(errs, flowvalidate.ValidationError{Path: path, Message: "expected the run to fail, but it succeeded"})
		case !ex.WantError && failed:
			msg := "expected the run to succeed, but it failed"
			if fs := state.Verdict.FailedStep; fs != nil {
				msg = fmt.Sprintf("expected the run to succeed, but step %q failed: %s", fs.Name, fs.Message)
			}
			errs = append(errs, flowvalidate.ValidationError{Path: path, Message: msg})
		case !ex.WantError && ex.CheckOutputs:
			for stepName, want := range ex.WantStepOutputs {
				step, ok := state.Steps[stepName]
				if !ok {
					errs = append(errs, flowvalidate.ValidationError{Path: path, Message: fmt.Sprintf("step %q not found in the run's Steps", stepName)})
					continue
				}
				if !exampleJSONEqual(step.Output, want) {
					errs = append(errs, flowvalidate.ValidationError{Path: path, Message: fmt.Sprintf("step %q Output = %#v, want %#v", stepName, step.Output, want)})
				}
			}
		}
	}
	return errs
}

// exampleJSONEqual mirrors catalog's own unexported jsonEqual — same
// "compare via JSON encoding, not reflect.DeepEqual" reasoning (a JSON
// round-trip already normalizes int vs int64 vs float64 the same way
// decoding an example's WantStepOutputs from JSON would), duplicated here
// rather than exported from pkg/catalog since it's four lines with no
// state of its own, the same call this project already made for
// goCatalogMap in both pkg/httpapi and pkg/mcpapi.
func exampleJSONEqual(a, b any) bool {
	aJSON, errA := json.Marshal(a)
	bJSON, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(aJSON) == string(bJSON)
}
