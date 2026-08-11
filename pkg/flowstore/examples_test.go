package flowstore

import (
	"strings"
	"testing"

	"goflow/pkg/piece"
)

func gatedStoreForExamples(t *testing.T) (*GatedStore, Store) {
	t.Helper()
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	gs := &GatedStore{
		Underlying:    fs,
		BuildRegistry: func() (*piece.Registry, error) { return piece.NewRegistry(), nil },
	}
	return gs, fs
}

func TestGatedStore_NoExamples_UnchangedBehavior(t *testing.T) {
	gs, fs := gatedStoreForExamples(t)
	def := FlowDefinition{Name: "no-examples", DisplayName: "No Examples", Flow: validCodeFlow()}
	if err := gs.Save(def); err != nil {
		t.Fatalf("Save with zero Examples: %v", err)
	}
	if _, ok, err := fs.Get("no-examples"); err != nil || !ok {
		t.Fatalf("Get after Save: ok=%v err=%v", ok, err)
	}
}

func TestGatedStore_ExamplePasses_Saved(t *testing.T) {
	gs, fs := gatedStoreForExamples(t)
	def := FlowDefinition{
		Name: "double-it", DisplayName: "Double It", Flow: validCodeFlow(),
		Examples: []FlowExample{{
			Description: "doubles 21", CheckOutputs: true,
			WantStepOutputs: map[string]any{"double": map[string]any{"doubled": 42}},
		}},
	}
	if err := gs.Save(def); err != nil {
		t.Fatalf("Save with a passing example: %v", err)
	}
	if _, ok, err := fs.Get("double-it"); err != nil || !ok {
		t.Fatalf("Get after Save: ok=%v err=%v", ok, err)
	}
}

func TestGatedStore_ExampleCheckOutputsMismatch_RejectedNotSaved(t *testing.T) {
	gs, fs := gatedStoreForExamples(t)
	def := FlowDefinition{
		Name: "double-it-wrong", DisplayName: "Double It Wrong", Flow: validCodeFlow(),
		Examples: []FlowExample{{
			Description: "wrong expectation", CheckOutputs: true,
			WantStepOutputs: map[string]any{"double": map[string]any{"doubled": 999}},
		}},
	}
	err := gs.Save(def)
	if err == nil {
		t.Fatal("Save with a failing example succeeded, want rejection")
	}
	if !strings.Contains(err.Error(), "double") || !strings.Contains(err.Error(), "999") {
		t.Fatalf("error = %v, want it to mention the mismatched step and expected value", err)
	}
	if _, ok, _ := fs.Get("double-it-wrong"); ok {
		t.Fatal("rejected flow was persisted")
	}
}

func TestGatedStore_ExampleMissingStep_Rejected(t *testing.T) {
	gs, _ := gatedStoreForExamples(t)
	def := FlowDefinition{
		Name: "missing-step", DisplayName: "Missing Step", Flow: validCodeFlow(),
		Examples: []FlowExample{{
			CheckOutputs:    true,
			WantStepOutputs: map[string]any{"never_ran": map[string]any{"x": 1}},
		}},
	}
	err := gs.Save(def)
	if err == nil || !strings.Contains(err.Error(), "never_ran") {
		t.Fatalf("err = %v, want it to name the missing step", err)
	}
}

func TestGatedStore_ExampleWantErrorSatisfied_Saved(t *testing.T) {
	gs, fs := gatedStoreForExamples(t)
	def := FlowDefinition{
		Name: "throws-example", DisplayName: "Throws Example", Flow: failingCodeFlow(),
		Examples: []FlowExample{{Description: "proves it fails", WantError: true}},
	}
	if err := gs.Save(def); err != nil {
		t.Fatalf("Save with a satisfied WantError example: %v", err)
	}
	if _, ok, err := fs.Get("throws-example"); err != nil || !ok {
		t.Fatalf("Get after Save: ok=%v err=%v", ok, err)
	}
}

func TestGatedStore_ExampleWantErrorNotSatisfied_Rejected(t *testing.T) {
	gs, _ := gatedStoreForExamples(t)
	def := FlowDefinition{
		Name: "should-throw-but-doesnt", DisplayName: "Should Throw", Flow: validCodeFlow(),
		Examples: []FlowExample{{Description: "expects a failure that never happens", WantError: true}},
	}
	err := gs.Save(def)
	if err == nil || !strings.Contains(err.Error(), "expected the run to fail") {
		t.Fatalf("err = %v, want it to say the run was expected to fail", err)
	}
}

func TestGatedStore_ExampleExpectedSuccessButFailed_Rejected(t *testing.T) {
	gs, _ := gatedStoreForExamples(t)
	def := FlowDefinition{
		Name: "unexpectedly-throws", DisplayName: "Unexpectedly Throws", Flow: failingCodeFlow(),
		Examples: []FlowExample{{Description: "should succeed but the flow throws"}},
	}
	err := gs.Save(def)
	if err == nil || !strings.Contains(err.Error(), "expected the run to succeed") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want it to say the run unexpectedly failed and mention the real error", err)
	}
}

func TestGatedStore_MultipleExamples_AllRunReportsAllFailures(t *testing.T) {
	gs, _ := gatedStoreForExamples(t)
	def := FlowDefinition{
		Name: "two-bad-examples", DisplayName: "Two Bad Examples", Flow: validCodeFlow(),
		Examples: []FlowExample{
			{Description: "first bad one", WantError: true},
			{Description: "second bad one", CheckOutputs: true, WantStepOutputs: map[string]any{"double": map[string]any{"doubled": 111}}},
		},
	}
	err := gs.Save(def)
	if err == nil {
		t.Fatal("Save with two failing examples succeeded, want rejection")
	}
	if !strings.Contains(err.Error(), "first bad one") || !strings.Contains(err.Error(), "second bad one") {
		t.Fatalf("err = %v, want BOTH examples' failures reported, not just the first", err)
	}
}

func TestGatedStore_StructurallyInvalidFlow_ExamplesNeverRun(t *testing.T) {
	gs, _ := gatedStoreForExamples(t)
	def := FlowDefinition{
		Name: "bad-piece-ref", DisplayName: "Bad Piece Ref", Flow: pieceReferencingFlow(),
		Examples: []FlowExample{{Description: "would never even get to run"}},
	}
	err := gs.Save(def)
	if err == nil {
		t.Fatal("Save of a structurally invalid flow succeeded, want rejection")
	}
	// The rejection is the STRUCTURAL error (piece not found), not
	// anything about the example — proving the example was never
	// attempted against a flow that can't even be validated.
	if !strings.Contains(err.Error(), "not found in registry") {
		t.Fatalf("err = %v, want the structural \"piece not found\" error, not an example-run error", err)
	}
	if strings.Contains(err.Error(), "would never even get to run") {
		t.Fatalf("err = %v, want the example's own Description to NOT appear — it was never run", err)
	}
}
