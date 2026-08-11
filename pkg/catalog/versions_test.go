package catalog_test

import (
	"strings"
	"testing"

	"goflow/pkg/catalog"
)

// versionStoreContract exercises the VersionStore interface's contract
// generically — run against both MemoryVersionStore and FileVersionStore
// below so the two implementations are held to the exact same behavior,
// mirroring storeContract's own pattern.
func versionStoreContract(t *testing.T, vs catalog.VersionStore) {
	t.Helper()

	if _, ok, err := vs.Get("nope"); err != nil || ok {
		t.Fatalf("Get(nope) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	id1, err := vs.Save(catalog.VersionRecord{PieceName: "piece-a", Definition: catalog.Definition{Name: "piece-a", DisplayName: "v1"}})
	if err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	id2, err := vs.Save(catalog.VersionRecord{PieceName: "piece-a", Definition: catalog.Definition{Name: "piece-a", DisplayName: "v2"}})
	if err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("id1 == id2 == %q, want distinct ids", id1)
	}
	if _, err := vs.Save(catalog.VersionRecord{PieceName: "piece-b", Definition: catalog.Definition{Name: "piece-b"}}); err != nil {
		t.Fatalf("Save (other piece): %v", err)
	}

	rec, ok, err := vs.Get(id2)
	if err != nil || !ok {
		t.Fatalf("Get(id2): ok=%v err=%v", ok, err)
	}
	if rec.Definition.DisplayName != "v2" || rec.PieceName != "piece-a" {
		t.Fatalf("Get(id2) = %+v, want DisplayName=v2 PieceName=piece-a", rec)
	}

	list, err := vs.ListForPiece("piece-a")
	if err != nil {
		t.Fatalf("ListForPiece(piece-a): %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListForPiece(piece-a) = %+v, want exactly 2 (piece-b's version excluded)", list)
	}

	emptyList, err := vs.ListForPiece("never-saved")
	if err != nil {
		t.Fatalf("ListForPiece(never-saved): %v", err)
	}
	if len(emptyList) != 0 {
		t.Fatalf("ListForPiece(never-saved) = %+v, want empty", emptyList)
	}
}

func TestMemoryVersionStore_Contract(t *testing.T) {
	versionStoreContract(t, catalog.NewMemoryVersionStore())
}

func TestFileVersionStore_Contract(t *testing.T) {
	vs, err := catalog.NewFileVersionStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileVersionStore: %v", err)
	}
	versionStoreContract(t, vs)
}

func TestFileVersionStore_SurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	vs1, err := catalog.NewFileVersionStore(dir)
	if err != nil {
		t.Fatalf("NewFileVersionStore: %v", err)
	}
	id, err := vs1.Save(catalog.VersionRecord{PieceName: "persisted", Definition: catalog.Definition{Name: "persisted", DisplayName: "v1"}})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	vs2, err := catalog.NewFileVersionStore(dir)
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

func gatedStoreWithVersions(t *testing.T) (*catalog.GatedStore, catalog.VersionStore) {
	t.Helper()
	versions := catalog.NewMemoryVersionStore()
	gs := &catalog.GatedStore{Underlying: catalog.NewMemoryStore(), Versions: versions}
	return gs, versions
}

func TestGatedStore_Save_RecordsVersionWhenConfigured(t *testing.T) {
	gs, versions := gatedStoreWithVersions(t)
	if err := gs.Save(workingDefinition("tracked")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	list, err := versions.ListForPiece("tracked")
	if err != nil {
		t.Fatalf("ListForPiece: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("versions = %+v, want exactly 1 after one Save", list)
	}
}

func TestGatedStore_Save_NilVersions_NoOpNoError(t *testing.T) {
	gs := &catalog.GatedStore{Underlying: catalog.NewMemoryStore()}
	if err := gs.Save(workingDefinition("untracked")); err != nil {
		t.Fatalf("Save with nil Versions: %v", err)
	}
	if _, ok, _ := gs.Get("untracked"); !ok {
		t.Fatal("piece not persisted despite nil Versions")
	}
}

func TestGatedStore_MultipleEdits_EachRecordedSeparately(t *testing.T) {
	gs, versions := gatedStoreWithVersions(t)
	def := workingDefinition("iterated")
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

	list, err := versions.ListForPiece("iterated")
	if err != nil {
		t.Fatalf("ListForPiece: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("versions = %+v, want exactly 3 (one per edit)", list)
	}
	newest, _, err := versions.Get(list[0].ID)
	if err != nil {
		t.Fatalf("Get newest: %v", err)
	}
	if newest.Definition.DisplayName != "v3" {
		t.Fatalf("newest version DisplayName = %q, want v3 — ListForPiece must return newest first", newest.Definition.DisplayName)
	}
}

func TestGatedStore_RejectedSave_DoesNotRecordVersion(t *testing.T) {
	gs, versions := gatedStoreWithVersions(t)
	broken := sampleDefinition("rejected") // no Examples -> fails Validate
	if err := gs.Save(broken); err == nil {
		t.Fatal("Save of a definition with no Examples succeeded, want rejection")
	}
	list, err := versions.ListForPiece("rejected")
	if err != nil {
		t.Fatalf("ListForPiece: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("versions = %+v, want none — a gate-rejected save must never be versioned", list)
	}
}

func TestGatedStore_Rollback_RestoresPastVersionAsNewVersion(t *testing.T) {
	gs, versions := gatedStoreWithVersions(t)
	original := workingDefinition("piece")
	original.DisplayName = "original"
	if err := gs.Save(original); err != nil {
		t.Fatalf("Save original: %v", err)
	}
	list, _ := versions.ListForPiece("piece")
	originalVersionID := list[0].ID

	broken := original
	broken.DisplayName = "broken edit"
	if err := gs.Save(broken); err != nil {
		t.Fatalf("Save broken edit: %v", err)
	}
	current, ok, err := gs.Get("piece")
	if err != nil || !ok || current.DisplayName != "broken edit" {
		t.Fatalf("current piece = %+v ok=%v err=%v, want DisplayName=broken edit", current, ok, err)
	}

	if err := gs.Rollback("piece", originalVersionID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	restored, ok, err := gs.Get("piece")
	if err != nil || !ok || restored.DisplayName != "original" {
		t.Fatalf("after rollback = %+v ok=%v err=%v, want DisplayName=original", restored, ok, err)
	}

	list, err = versions.ListForPiece("piece")
	if err != nil {
		t.Fatalf("ListForPiece after rollback: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("versions = %+v, want exactly 3 (original, broken edit, rollback) — rollback must not rewrite history", list)
	}
}

func TestGatedStore_Rollback_UnknownVersionID_Fails(t *testing.T) {
	gs, _ := gatedStoreWithVersions(t)
	if err := gs.Save(workingDefinition("piece")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	err := gs.Rollback("piece", "never-saved")
	if err == nil || !strings.Contains(err.Error(), "no version") {
		t.Fatalf("err = %v, want a \"no version\" rejection", err)
	}
}

func TestGatedStore_Rollback_VersionBelongsToDifferentPiece_Rejected(t *testing.T) {
	gs, versions := gatedStoreWithVersions(t)
	if err := gs.Save(workingDefinition("piece-a")); err != nil {
		t.Fatalf("Save piece-a: %v", err)
	}
	listA, _ := versions.ListForPiece("piece-a")

	pieceB := workingDefinition("piece-a")
	pieceB.Name = "piece-b"
	if err := gs.Save(pieceB); err != nil {
		t.Fatalf("Save piece-b: %v", err)
	}

	err := gs.Rollback("piece-b", listA[0].ID)
	if err == nil || !strings.Contains(err.Error(), "belongs to piece") {
		t.Fatalf("err = %v, want a rejection naming the mismatched piece", err)
	}
}

func TestGatedStore_Rollback_NoVersionStoreConfigured_Fails(t *testing.T) {
	gs := &catalog.GatedStore{Underlying: catalog.NewMemoryStore()}
	err := gs.Rollback("piece", "some-id")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v, want a \"not configured\" rejection", err)
	}
}
