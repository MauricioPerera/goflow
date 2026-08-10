package catalog_test

import (
	"testing"

	"goflow/pkg/catalog"
	"goflow/pkg/engine"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/pieces"
)

// workingTriggerDefinition is the trigger analogue of workingDefinition: a
// Definition carrying one JS-authored trigger ("new_orders") that maps each
// payload item to {id, amount, source}, with two passing TriggerExamples —
// one checking the mapped items, one checking the empty-array case. This is
// the baseline "structurally sound AND every example behaves as asserted"
// case Validate must accept.
func workingTriggerDefinition(name string) catalog.Definition {
	return catalog.Definition{
		Name: name, DisplayName: "Order Trigger", Description: "maps incoming order payload items",
		Triggers: []catalog.TriggerDefinition{
			{
				Name: "new_orders", DisplayName: "New Orders",
				Description: "maps each payload item to {id, amount, source}",
				Source: `(ctx) => ctx.payload.map(item => ({
					id: item.id, amount: item.amount, source: "js-trigger"
				}))`,
				Examples: []catalog.TriggerExample{
					{
						Description: "two payload items both map through",
						Payload: []any{
							map[string]any{"id": int64(1), "amount": int64(100)},
							map[string]any{"id": int64(2), "amount": int64(250)},
						},
						CheckOutput: true,
						WantItems: []any{
							map[string]any{"id": int64(1), "amount": int64(100), "source": "js-trigger"},
							map[string]any{"id": int64(2), "amount": int64(250), "source": "js-trigger"},
						},
					},
					{
						Description: "empty payload yields empty items",
						Payload:     []any{},
						CheckOutput: true,
						WantItems:   []any{},
					},
				},
			},
		},
	}
}

func TestValidate_TriggerWithPassingExamplesSucceeds(t *testing.T) {
	if errs := catalog.Validate(workingTriggerDefinition("order_trigger")); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}
}

func TestValidate_TriggerWithoutExamplesFails(t *testing.T) {
	def := workingTriggerDefinition("order_trigger")
	def.Triggers[0].Examples = nil

	errs := catalog.Validate(def)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a rejection for a trigger with zero Examples")
	}
}

func TestValidate_TriggerExampleExpectedErrorButSucceededFails(t *testing.T) {
	def := workingTriggerDefinition("order_trigger")
	// The trigger succeeds for a non-empty payload, but this example claims
	// it must error — Validate must reject that mismatch.
	def.Triggers[0].Examples = []catalog.TriggerExample{
		{
			Payload:   []any{map[string]any{"id": int64(1)}},
			WantError: true,
		},
	}

	errs := catalog.Validate(def)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a rejection — the trigger succeeded but the example was marked WantError")
	}
}

func TestValidate_TriggerExampleOutputMismatchFails(t *testing.T) {
	def := workingTriggerDefinition("order_trigger")
	def.Triggers[0].Examples = []catalog.TriggerExample{
		{
			Payload: []any{map[string]any{"id": int64(1), "amount": int64(100)}},
			// WantItems deliberately wrong (source mismatch) so the real
			// trigger's output can't match.
			CheckOutput: true,
			WantItems: []any{
				map[string]any{"id": int64(1), "amount": int64(100), "source": "wrong-source"},
			},
		},
	}

	errs := catalog.Validate(def)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a rejection for a mismatched trigger output")
	}
}

func TestDefinition_ToPiece_BuildsInvocableTrigger(t *testing.T) {
	def := workingTriggerDefinition("order_trigger")
	p := def.ToPiece()

	if p.Name != "order_trigger" || p.DisplayName != "Order Trigger" {
		t.Fatalf("p = %+v", p)
	}
	trig, ok := p.Triggers["new_orders"]
	if !ok {
		t.Fatal("trigger \"new_orders\" not present after ToPiece()")
	}
	// Invokable for real, not a mock: run it with a payload and confirm it
	// returns the mapped items a hand-written Go trigger would.
	items, err := trig.Run(piece.TriggerContext{
		Payload: []any{map[string]any{"id": int64(7), "amount": int64(300)}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want exactly 1", items)
	}
	mapped, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] = %#v, want a map", items[0])
	}
	if mapped["id"] != int64(7) || mapped["source"] != "js-trigger" {
		t.Fatalf("mapped = %+v, want id=7 source=js-trigger", mapped)
	}
}

func TestGatedStore_RejectsTriggerWithoutExamples(t *testing.T) {
	underlying := catalog.NewMemoryStore()
	gated := &catalog.GatedStore{Underlying: underlying}

	broken := workingTriggerDefinition("order_trigger")
	broken.Triggers[0].Examples = nil
	if err := gated.Save(broken); err == nil {
		t.Fatal("Save() error = nil, want a rejection — the trigger has no Examples")
	}

	// A rejected Save must never reach the underlying store.
	defs, err := underlying.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("underlying store = %+v, want empty — Save must not have delegated on validation failure", defs)
	}
}

func TestGatedStore_AcceptsValidTriggerDefinition(t *testing.T) {
	underlying := catalog.NewMemoryStore()
	gated := &catalog.GatedStore{Underlying: underlying}

	if err := gated.Save(workingTriggerDefinition("order_trigger")); err != nil {
		t.Fatalf("Save() error = %v, want nil — this trigger definition has passing Examples", err)
	}

	got, ok, err := gated.Get("order_trigger")
	if err != nil || !ok {
		t.Fatalf("Get() = ok=%v err=%v, want ok=true", ok, err)
	}
	if got.Name != "order_trigger" || len(got.Triggers) != 1 || got.Triggers[0].Name != "new_orders" {
		t.Fatalf("got = %+v", got)
	}
}

// TestTrigger_PersistedAcrossProcessesAndFiresThroughRealEngine is the
// decisive end-to-end proof for trigger persistence: a Definition with a
// valid trigger is saved to disk by one FileStore instance wrapped in a
// GatedStore (standing in for one authoring process), loaded by a
// completely separate FileStore instance pointed at the same directory
// (a later process), registered into a fresh *piece.Registry via
// RegisterFromStore, and run through a real engine.ExecuteBegin with a
// PIECE_TRIGGER pointing at the persisted piece — confirming the real
// engine fires the trigger loaded off disk and uses its first returned
// item, exactly like pkg/pieces' own TestCatalog_JSTriggerComposesWithRealCatalog
// does for an in-memory jspiece trigger.
func TestTrigger_PersistedAcrossProcessesAndFiresThroughRealEngine(t *testing.T) {
	dir := t.TempDir()

	authoringStore, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	gated := &catalog.GatedStore{Underlying: authoringStore}
	if err := gated.Save(workingTriggerDefinition("order_trigger")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A later process: a brand new Store instance, a brand new registry,
	// nothing carried over except the directory on disk.
	laterStore, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore (later process): %v", err)
	}
	registry := piece.NewRegistry()
	// RegisterAll supplies the real Go catalog (the json piece the report
	// step below uses) — same call TestCatalog_JSTriggerComposesWithRealCatalog
	// makes, so the persisted JS trigger piece coexists with the real catalog.
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if err := catalog.RegisterFromStore(registry, laterStore); err != nil {
		t.Fatalf("RegisterFromStore: %v", err)
	}

	reportStep := &model.FlowAction{
		Name: "report", DisplayName: "Report", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "stringify", Input: map[string]any{
			"data": map[string]any{
				"id":     "{{ trigger_1.output.id }}",
				"source": "{{ trigger_1.output.source }}",
			},
		}},
	}
	fv := &model.FlowVersion{ID: "fv-catalog-persisted-trigger", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "New Orders", Type: model.TriggerPiece,
		PieceName: "order_trigger", TriggerName: "new_orders", Input: map[string]any{},
		NextAction: reportStep,
	}}

	payload := []any{
		map[string]any{"id": int64(1), "amount": int64(100)},
		map[string]any{"id": int64(2), "amount": int64(250)},
	}
	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{
		TriggerPayload: payload,
		ExecuteTrigger: true,
	})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}

	// ExecuteBegin only ever uses items[0] of what a PIECE trigger returns
	// — confirmed by reading engine.go before relying on it, not assumed.
	// The second payload item is never seen by this flow run.
	triggerOut := state.Steps["trigger_1"].Output.(map[string]any)
	if triggerOut["id"] != int64(1) || triggerOut["source"] != "js-trigger" {
		t.Fatalf("trigger_1 output = %+v, want the FIRST mapped item (id=1, source=js-trigger)", triggerOut)
	}

	reportOut := state.Steps["report"].Output.(map[string]any)
	reportText, _ := reportOut["text"].(string)
	if reportText != `{"id":1,"source":"js-trigger"}` {
		t.Fatalf("report text = %q", reportText)
	}
}
