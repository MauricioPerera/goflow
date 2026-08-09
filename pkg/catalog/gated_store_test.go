package catalog_test

import (
	"testing"

	"goflow/pkg/catalog"
	"goflow/pkg/engine"
	"goflow/pkg/model"
	"goflow/pkg/piece"
)

func TestGatedStore_RejectsInvalidDefinition(t *testing.T) {
	underlying := catalog.NewMemoryStore()
	gated := &catalog.GatedStore{Underlying: underlying}

	broken := sampleDefinition("broken") // sampleDefinition has no Examples
	if err := gated.Save(broken); err == nil {
		t.Fatal("Save() error = nil, want a rejection — the definition has no Examples")
	}

	// The whole point: a rejected Save must never reach the underlying
	// store.
	defs, err := underlying.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("underlying store = %+v, want empty — Save must not have delegated on validation failure", defs)
	}
}

func TestGatedStore_AcceptsValidDefinition(t *testing.T) {
	underlying := catalog.NewMemoryStore()
	gated := &catalog.GatedStore{Underlying: underlying}

	if err := gated.Save(workingDefinition("risk_score")); err != nil {
		t.Fatalf("Save() error = %v, want nil — this definition has passing Examples", err)
	}

	got, ok, err := gated.Get("risk_score")
	if err != nil || !ok {
		t.Fatalf("Get() = ok=%v err=%v, want ok=true", ok, err)
	}
	if got.Name != "risk_score" {
		t.Fatalf("got = %+v", got)
	}
}

func TestGatedStore_ThenRegisterFromStoreRunsThroughRealEngine(t *testing.T) {
	dir := t.TempDir()
	fileStore, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	gated := &catalog.GatedStore{Underlying: fileStore}

	if err := gated.Save(workingDefinition("risk_score")); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// A later process/session: fresh Store instance, fresh registry,
	// nothing but the directory on disk.
	laterStore, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore (later): %v", err)
	}
	registry := piece.NewRegistry()
	if err := catalog.RegisterFromStore(registry, laterStore); err != nil {
		t.Fatalf("RegisterFromStore: %v", err)
	}

	scoreStep := &model.FlowAction{
		Name: "score", DisplayName: "Score", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "risk_score", ActionName: "classify", Input: map[string]any{
			"amount": "{{ trigger_1.output.amount }}",
		}},
	}
	fv := &model.FlowVersion{ID: "fv-gated-catalog", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: scoreStep,
	}}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{"amount": int64(200)}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	out := state.Steps["score"].Output.(map[string]any)
	if out["level"] != "medium" {
		t.Fatalf("level = %v, want medium", out["level"])
	}
}
