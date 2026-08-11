package flowstore

import (
	"strings"
	"testing"

	"goflow/pkg/model"
	"goflow/pkg/piece"
)

// versionStoreContract exercises the VersionStore interface's contract
// generically — run against both MemoryVersionStore and FileVersionStore
// below so the two implementations are held to the exact same behavior,
// mirroring storeContract's own pattern in catalog_test.go/store_test.go.
func versionStoreContract(t *testing.T, vs VersionStore) {
	t.Helper()

	if _, ok, err := vs.Get("nope"); err != nil || ok {
		t.Fatalf("Get(nope) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	id1, err := vs.Save(VersionRecord{FlowName: "flow-a", Definition: FlowDefinition{Name: "flow-a", DisplayName: "v1"}})
	if err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	id2, err := vs.Save(VersionRecord{FlowName: "flow-a", Definition: FlowDefinition{Name: "flow-a", DisplayName: "v2"}})
	if err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("id1 == id2 == %q, want distinct ids", id1)
	}
	if _, err := vs.Save(VersionRecord{FlowName: "flow-b", Definition: FlowDefinition{Name: "flow-b"}}); err != nil {
		t.Fatalf("Save (other flow): %v", err)
	}

	rec, ok, err := vs.Get(id2)
	if err != nil || !ok {
		t.Fatalf("Get(id2): ok=%v err=%v", ok, err)
	}
	if rec.Definition.DisplayName != "v2" || rec.FlowName != "flow-a" {
		t.Fatalf("Get(id2) = %+v, want DisplayName=v2 FlowName=flow-a", rec)
	}

	list, err := vs.ListForFlow("flow-a")
	if err != nil {
		t.Fatalf("ListForFlow(flow-a): %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListForFlow(flow-a) = %+v, want exactly 2 (flow-b's version excluded)", list)
	}

	emptyList, err := vs.ListForFlow("never-saved")
	if err != nil {
		t.Fatalf("ListForFlow(never-saved): %v", err)
	}
	if len(emptyList) != 0 {
		t.Fatalf("ListForFlow(never-saved) = %+v, want empty", emptyList)
	}
}

func TestMemoryVersionStore_Contract(t *testing.T) {
	versionStoreContract(t, NewMemoryVersionStore())
}

func TestFileVersionStore_Contract(t *testing.T) {
	vs, err := NewFileVersionStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileVersionStore: %v", err)
	}
	versionStoreContract(t, vs)
}

func TestFileVersionStore_SurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	vs1, err := NewFileVersionStore(dir)
	if err != nil {
		t.Fatalf("NewFileVersionStore: %v", err)
	}
	id, err := vs1.Save(VersionRecord{FlowName: "persisted", Definition: FlowDefinition{Name: "persisted", DisplayName: "v1"}})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	vs2, err := NewFileVersionStore(dir)
	if err != nil {
		t.Fatalf("NewFileVersionStore (reopen): %v", err)
	}
	rec, ok, err := vs2.Get(id)
	if err != nil || !ok {
		t.Fatalf("Get after reopen: ok=%v err=%v", ok, err)
	}
	if rec.Definition.DisplayName != "v1" {
		t.Fatalf("rec = %+v, want DisplayName=v1", rec)
	}
}

// --- GatedStore.Save recording + Rollback -----------------------------

func gatedStoreWithVersions(t *testing.T) (*GatedStore, VersionStore) {
	t.Helper()
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	versions := NewMemoryVersionStore()
	gs := &GatedStore{
		Underlying:    fs,
		BuildRegistry: func() (*piece.Registry, error) { return piece.NewRegistry(), nil },
		Versions:      versions,
	}
	return gs, versions
}

func TestGatedStore_Save_RecordsVersionWhenConfigured(t *testing.T) {
	gs, versions := gatedStoreWithVersions(t)
	def := FlowDefinition{Name: "tracked", DisplayName: "v1", Flow: validCodeFlow()}
	if err := gs.Save(def); err != nil {
		t.Fatalf("Save: %v", err)
	}
	list, err := versions.ListForFlow("tracked")
	if err != nil {
		t.Fatalf("ListForFlow: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("versions = %+v, want exactly 1 after one Save", list)
	}
}

func TestGatedStore_Save_NilVersions_NoOpNoError(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	gs := &GatedStore{Underlying: fs, BuildRegistry: func() (*piece.Registry, error) { return piece.NewRegistry(), nil }}
	def := FlowDefinition{Name: "untracked", Flow: validCodeFlow()}
	if err := gs.Save(def); err != nil {
		t.Fatalf("Save with nil Versions: %v", err)
	}
	if _, ok, _ := fs.Get("untracked"); !ok {
		t.Fatal("flow not persisted despite nil Versions")
	}
}

func TestGatedStore_MultipleEdits_EachRecordedSeparately(t *testing.T) {
	gs, versions := gatedStoreWithVersions(t)
	def := FlowDefinition{Name: "iterated", DisplayName: "v1", Flow: validCodeFlow()}
	if err := gs.Save(def); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	def.DisplayName = "v2"
	if err := gs.Save(def); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	def.DisplayName = "v3"
	if err := gs.Save(def); err != nil {
		t.Fatalf("Save 3: %v", err)
	}

	list, err := versions.ListForFlow("iterated")
	if err != nil {
		t.Fatalf("ListForFlow: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("versions = %+v, want exactly 3 (one per edit)", list)
	}
	// Newest first.
	newest, _, err := versions.Get(list[0].ID)
	if err != nil {
		t.Fatalf("Get newest: %v", err)
	}
	if newest.Definition.DisplayName != "v3" {
		t.Fatalf("newest version DisplayName = %q, want v3 — ListForFlow must return newest first", newest.Definition.DisplayName)
	}
}

func TestGatedStore_ExampleFailure_DoesNotRecordVersion(t *testing.T) {
	gs, versions := gatedStoreWithVersions(t)
	def := FlowDefinition{
		Name: "rejected", Flow: validCodeFlow(),
		Examples: []FlowExample{{WantError: true}}, // validCodeFlow succeeds, so this fails the gate
	}
	if err := gs.Save(def); err == nil {
		t.Fatal("Save with a failing example succeeded, want rejection")
	}
	list, err := versions.ListForFlow("rejected")
	if err != nil {
		t.Fatalf("ListForFlow: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("versions = %+v, want none — a gate-rejected save must never be versioned", list)
	}
}

func TestGatedStore_Rollback_RestoresPastVersionAsNewVersion(t *testing.T) {
	gs, versions := gatedStoreWithVersions(t)
	original := FlowDefinition{Name: "flow", DisplayName: "original", Flow: validCodeFlow()}
	if err := gs.Save(original); err != nil {
		t.Fatalf("Save original: %v", err)
	}
	list, _ := versions.ListForFlow("flow")
	originalVersionID := list[0].ID

	broken := original
	broken.DisplayName = "broken edit"
	if err := gs.Save(broken); err != nil {
		t.Fatalf("Save broken edit: %v", err)
	}
	current, ok, err := gs.Get("flow")
	if err != nil || !ok || current.DisplayName != "broken edit" {
		t.Fatalf("current flow = %+v ok=%v err=%v, want DisplayName=broken edit", current, ok, err)
	}

	if err := gs.Rollback("flow", originalVersionID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	restored, ok, err := gs.Get("flow")
	if err != nil || !ok || restored.DisplayName != "original" {
		t.Fatalf("after rollback = %+v ok=%v err=%v, want DisplayName=original", restored, ok, err)
	}

	// Rollback goes through Save, so it recorded its OWN new version —
	// history never gets rewritten, only extended.
	list, err = versions.ListForFlow("flow")
	if err != nil {
		t.Fatalf("ListForFlow after rollback: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("versions = %+v, want exactly 3 (original, broken edit, rollback) — rollback must not rewrite history", list)
	}
}

func TestGatedStore_Rollback_UnknownVersionID_Fails(t *testing.T) {
	gs, _ := gatedStoreWithVersions(t)
	if err := gs.Save(FlowDefinition{Name: "flow", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	err := gs.Rollback("flow", "never-saved")
	if err == nil || !strings.Contains(err.Error(), "no version") {
		t.Fatalf("err = %v, want a \"no version\" rejection", err)
	}
}

func TestGatedStore_Rollback_VersionBelongsToDifferentFlow_Rejected(t *testing.T) {
	gs, versions := gatedStoreWithVersions(t)
	if err := gs.Save(FlowDefinition{Name: "flow-a", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save flow-a: %v", err)
	}
	listA, _ := versions.ListForFlow("flow-a")

	if err := gs.Save(FlowDefinition{Name: "flow-b", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save flow-b: %v", err)
	}

	err := gs.Rollback("flow-b", listA[0].ID)
	if err == nil || !strings.Contains(err.Error(), "belongs to flow") {
		t.Fatalf("err = %v, want a rejection naming the mismatched flow", err)
	}
}

func TestGatedStore_Rollback_NoVersionStoreConfigured_Fails(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	gs := &GatedStore{Underlying: fs, BuildRegistry: func() (*piece.Registry, error) { return piece.NewRegistry(), nil }}
	err = gs.Rollback("flow", "some-id")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v, want a \"not configured\" rejection", err)
	}
}

// TestGatedStore_Rollback_TargetPieceRemoved_FailsCleanly proves rollback
// goes through the SAME validation gate a live save does: a version
// referencing a piece that no longer exists in the CURRENT registry fails
// to roll back, rather than silently restoring something broken.
func TestGatedStore_Rollback_TargetPieceRemoved_FailsCleanly(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	versions := NewMemoryVersionStore()
	pieceStillRegistered := true
	gs := &GatedStore{
		Underlying: fs,
		BuildRegistry: func() (*piece.Registry, error) {
			r := piece.NewRegistry()
			if pieceStillRegistered {
				r.Register(piece.Piece{
					Name: "test_piece", DisplayName: "Test Piece",
					Actions: map[string]piece.Action{
						"do": {Name: "do", DisplayName: "Do", Run: func(piece.ActionContext) (any, error) { return map[string]any{"ok": true}, nil }},
					},
				})
			}
			return r, nil
		},
		Versions: versions,
	}
	def := FlowDefinition{Name: "flow", Flow: pieceUsingFlow()}
	if err := gs.Save(def); err != nil {
		t.Fatalf("Save while the piece is registered: %v", err)
	}
	list, err := versions.ListForFlow("flow")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListForFlow: %+v err=%v, want exactly 1", list, err)
	}
	versionID := list[0].ID

	pieceStillRegistered = false
	err = gs.Rollback("flow", versionID)
	if err == nil || !strings.Contains(err.Error(), "not found in registry") {
		t.Fatalf("err = %v, want a rejection naming the missing piece — rollback must not restore a now-broken flow", err)
	}
}

// pieceUsingFlow references "test_piece.do" — used by
// TestGatedStore_Rollback_TargetPieceRemoved_FailsCleanly to prove a
// rollback re-validates against the CURRENT registry.
func pieceUsingFlow() model.FlowVersion {
	return model.FlowVersion{
		ID: "fv-piece-using",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "call_piece", DisplayName: "Call Piece", Type: model.ActionPiece,
				Piece: &model.PieceSettings{PieceName: "test_piece", ActionName: "do"},
			},
		},
	}
}
