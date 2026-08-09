package catalog_test

import (
	"testing"

	"goflow/pkg/catalog"
)

func sampleDefinition(name string) catalog.Definition {
	return catalog.Definition{
		Name: name, DisplayName: "Sample", Description: "does sample things",
		Actions: []catalog.ActionDefinition{
			{
				Name: "run", DisplayName: "Run",
				Description: "runs the sample action",
				InputSchema: "x (number, required)",
				Source:      `(ctx) => ({ doubled: Number(ctx.input.x) * 2 })`,
			},
		},
	}
}

// storeContract exercises the Store interface's contract generically —
// run against both MemoryStore and FileStore below so the two
// implementations are held to the exact same behavior.
func storeContract(t *testing.T, store catalog.Store) {
	t.Helper()

	if _, ok, err := store.Get("nope"); err != nil || ok {
		t.Fatalf("Get(nope) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	defs, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("List() on empty store = %+v, want empty", defs)
	}

	def := sampleDefinition("risk_score")
	if err := store.Save(def); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, ok, err := store.Get("risk_score")
	if err != nil || !ok {
		t.Fatalf("Get(risk_score) = ok=%v err=%v, want ok=true", ok, err)
	}
	if got.DisplayName != "Sample" || len(got.Actions) != 1 || got.Actions[0].Source != def.Actions[0].Source {
		t.Fatalf("Get() = %+v, want it to round-trip exactly", got)
	}

	defs, err = store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "risk_score" {
		t.Fatalf("List() = %+v, want exactly [risk_score]", defs)
	}

	// Save again with the same Name overwrites, not duplicates.
	updated := def
	updated.Description = "an updated description"
	if err := store.Save(updated); err != nil {
		t.Fatalf("Save() (update) error = %v", err)
	}
	defs, err = store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("List() after overwrite = %+v, want still exactly 1 entry", defs)
	}
	got, _, _ = store.Get("risk_score")
	if got.Description != "an updated description" {
		t.Fatalf("Get() after overwrite: Description = %q", got.Description)
	}
}

func TestMemoryStore_ImplementsStoreContract(t *testing.T) {
	storeContract(t, catalog.NewMemoryStore())
}

func TestFileStore_ImplementsStoreContract(t *testing.T) {
	store, err := catalog.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	storeContract(t, store)
}

func TestFileStore_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()

	store1, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store1.Save(sampleDefinition("persisted")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A brand new FileStore instance pointed at the SAME directory — this
	// is the actual point of FileStore over MemoryStore: it must see data
	// saved by a previous instance/process, not just its own in-memory
	// state.
	store2, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore (second instance): %v", err)
	}
	got, ok, err := store2.Get("persisted")
	if err != nil || !ok {
		t.Fatalf("Get(persisted) on a fresh instance = ok=%v err=%v, want ok=true", ok, err)
	}
	if got.DisplayName != "Sample" {
		t.Fatalf("got = %+v", got)
	}
}

func TestFileStore_RejectsPathTraversalNames(t *testing.T) {
	store, err := catalog.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	cases := []string{"../escape", "..\\escape", "a/b", "a\\b", ".", "..", ""}
	for _, name := range cases {
		def := sampleDefinition(name)
		if err := store.Save(def); err == nil {
			t.Fatalf("Save(name=%q) error = nil, want a rejection", name)
		}
	}
}

func TestDefinition_ToPiece(t *testing.T) {
	def := sampleDefinition("risk_score")
	p := def.ToPiece()

	if p.Name != "risk_score" || p.DisplayName != "Sample" {
		t.Fatalf("p = %+v", p)
	}
	act, ok := p.Actions["run"]
	if !ok {
		t.Fatal("action \"run\" not present after ToPiece()")
	}
	_ = act // exercised end-to-end in catalog_integration_test.go
}
