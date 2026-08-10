package flowstore

import (
	"errors"
	"strings"
	"testing"

	"goflow/pkg/model"
	"goflow/pkg/piece"
)

// emptyRegistry is a buildRegistry that returns an empty *piece.Registry —
// enough for CODE-only flows (which need no registered pieces to validate or
// run) and enough to make a PIECE-referencing flow fail validation.
func emptyRegistry() (*piece.Registry, error) {
	return piece.NewRegistry(), nil
}

// failingRegistry is a buildRegistry that always errors, to exercise Run's
// "buildRegistry failed" path — the one Run surfaces as its third return
// value (err), distinct from a validation failure.
func failingRegistry() (*piece.Registry, error) {
	return nil, errors.New("simulated registry build failure")
}

// TestRun_BuildRegistryFails_PropagatesErr is the "our problem, not the
// caller's flow" path: a buildRegistry failure is returned as err (not as
// validationErrs), and both state and validationErrs are nil.
func TestRun_BuildRegistryFails_PropagatesErr(t *testing.T) {
	fv := validCodeFlow()
	state, vErrs, err := Run(&fv, failingRegistry, nil, false)
	if err == nil {
		t.Fatalf("err = nil, want a buildRegistry failure")
	}
	if !strings.Contains(err.Error(), "flowstore: building registry") {
		t.Fatalf("err = %q, want it wrapped with 'flowstore: building registry'", err.Error())
	}
	if !strings.Contains(err.Error(), "simulated registry build failure") {
		t.Fatalf("err = %q, want the underlying failure preserved", err.Error())
	}
	if state != nil {
		t.Fatalf("state = %v, want nil on buildRegistry failure", state)
	}
	if vErrs != nil {
		t.Fatalf("validationErrs = %v, want nil on buildRegistry failure", vErrs)
	}
}

// TestRun_PieceReferencingFlowAgainstEmptyRegistry_ReturnsValidationErrs is
// the "flow is structurally broken" path: a PIECE action against an empty
// registry fails flowvalidate.Validate, which Run surfaces as validationErrs
// (not err), with state nil and err nil.
func TestRun_PieceReferencingFlowAgainstEmptyRegistry_ReturnsValidationErrs(t *testing.T) {
	fv := pieceReferencingFlow()
	state, vErrs, err := Run(&fv, emptyRegistry, nil, false)
	if err != nil {
		t.Fatalf("err = %v, want nil — a validation failure is not an err", err)
	}
	if state != nil {
		t.Fatalf("state = %v, want nil when validation fails", state)
	}
	if len(vErrs) == 0 {
		t.Fatal("validationErrs = empty, want the missing-piece action reported")
	}
	// The error must point at the PIECE action's missing registry entry.
	found := false
	for _, e := range vErrs {
		if strings.Contains(e.Message, "no_such_piece") {
			found = true
		}
	}
	if !found {
		t.Fatalf("validationErrs = %v, want one mentioning no_such_piece", vErrs)
	}
}

// TestRun_ValidCodeFlow_ReturnsSucceededState is the happy path: a CODE-only
// flow validates against an empty registry and runs, yielding a state whose
// Verdict.Status is model.FlowRunSucceeded, with err and validationErrs nil.
func TestRun_ValidCodeFlow_ReturnsSucceededState(t *testing.T) {
	fv := validCodeFlow()
	state, vErrs, err := Run(&fv, emptyRegistry, nil, false)
	if err != nil {
		t.Fatalf("err = %v, want nil for a valid flow", err)
	}
	if vErrs != nil {
		t.Fatalf("validationErrs = %v, want nil for a valid flow", vErrs)
	}
	if state == nil {
		t.Fatal("state = nil, want a non-nil ExecutionState")
	}
	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("Verdict.Status = %v, want %s", state.Verdict.Status, model.FlowRunSucceeded)
	}
	// The CODE step recorded its doubled output (n=21 -> 42).
	double, ok := state.Steps["double"]
	if !ok {
		t.Fatalf("Steps = %v, want a 'double' step", state.Steps)
	}
	out, ok := double.Output.(map[string]any)
	if !ok {
		t.Fatalf("double.Output = %T, want a map", double.Output)
	}
	// goja's Export() yields an integer type for a whole number (int on
	// 32-bit, int64 on 64-bit — no JSON roundtrip here, unlike the HTTP
	// tests, which always get float64), so accept any integer kind.
	switch d := out["doubled"].(type) {
	case int:
		if d != 42 {
			t.Fatalf("doubled = %v, want 42", d)
		}
	case int64:
		if d != 42 {
			t.Fatalf("doubled = %v, want 42", d)
		}
	case float64:
		if d != 42 {
			t.Fatalf("doubled = %v, want 42", d)
		}
	default:
		t.Fatalf("doubled = %v (%T), want 42", out["doubled"], out["doubled"])
	}
}
