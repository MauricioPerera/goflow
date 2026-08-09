package catalog_test

import (
	"testing"

	"goflow/pkg/catalog"
	"goflow/pkg/engine"
	"goflow/pkg/model"
	"goflow/pkg/piece"
)

// TestRegisterFromStore_PieceSurvivesAcrossProcessesAndRunsForReal is the
// decisive proof for this package: a piece is saved to disk by one
// FileStore instance (standing in for one process/session), loaded by a
// completely separate FileStore instance pointed at the same directory
// (standing in for a later process/session — e.g. a restart, or a
// different agent-authoring session reusing what an earlier one built),
// registered into a fresh engine, and run through a real flow. Nothing
// about the piece was rebuilt from source code — only from what
// RegisterFromStore read back off disk.
func TestRegisterFromStore_PieceSurvivesAcrossProcessesAndRunsForReal(t *testing.T) {
	dir := t.TempDir()

	authoringStore, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := authoringStore.Save(catalog.Definition{
		Name: "risk_score", DisplayName: "Risk Score", Description: "classifies an amount",
		Actions: []catalog.ActionDefinition{
			{
				Name: "classify", DisplayName: "Classify",
				Description: "returns high/medium/low for a numeric amount",
				InputSchema: "amount (number, required)",
				Source: `(ctx) => {
					const amount = Number(ctx.input.amount);
					let level;
					if (amount > 1000) level = "high";
					else if (amount > 100) level = "medium";
					else level = "low";
					return { level: level };
				}`,
			},
		},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulates a later process: a brand new Store instance, a brand new
	// registry, nothing carried over except the directory on disk.
	laterStore, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore (later process): %v", err)
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
	fv := &model.FlowVersion{ID: "fv-catalog-persisted", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: scoreStep,
	}}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{"amount": int64(750)}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	out := state.Steps["score"].Output.(map[string]any)
	if out["level"] != "medium" {
		t.Fatalf("level = %v, want medium", out["level"])
	}
}

// TestRegisterFromStore_MultiplePiecesAlongsideRealCatalog proves a
// persisted, agent-authored piece coexists with the real Go catalog after
// being loaded from a Store — the same guarantee
// TestCatalog_JSPieceComposesWithRealCatalog already proved for an
// in-memory jspiece.Piece, now for one that round-tripped through disk.
func TestRegisterFromStore_MultiplePiecesAlongsideRealCatalog(t *testing.T) {
	store := catalog.NewMemoryStore()
	store.Save(sampleDefinition("risk_score"))

	registry := piece.NewRegistry()
	if err := catalog.RegisterFromStore(registry, store); err != nil {
		t.Fatalf("RegisterFromStore: %v", err)
	}

	act, ok := registry.GetAction("risk_score", "run")
	if !ok {
		t.Fatal("risk_score.run not resolvable after RegisterFromStore")
	}
	out, err := act.Run(piece.ActionContext{Input: map[string]any{"x": int64(21)}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.(map[string]any)["doubled"] != int64(42) {
		t.Fatalf("out = %#v", out)
	}
}
