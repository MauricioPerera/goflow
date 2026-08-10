package flowstore

import (
	"errors"
	"os"
	"strings"
	"testing"

	"goflow/pkg/model"
	"goflow/pkg/piece"
)

// validCodeFlow is a FlowVersion that needs no piece at all — trigger EMPTY
// -> a single CODE action — so it validates even against an empty piece
// registry. Mirrors the style of examples/main.go and the /flows/run test
// flow (doubles a number).
func validCodeFlow() model.FlowVersion {
	return model.FlowVersion{
		ID: "fv-test",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "double", DisplayName: "Double", Type: model.ActionCode,
				Code: &model.CodeSettings{
					Input:  map[string]any{"n": 21},
					Source: `(params) => ({ doubled: params.n * 2 })`,
				},
			},
		},
	}
}

// pieceReferencingFlow references a PIECE action that doesn't exist in an
// empty registry — so flowvalidate.Validate must report it.
func pieceReferencingFlow() model.FlowVersion {
	return model.FlowVersion{
		ID: "fv-bad",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "badstep", DisplayName: "Bad", Type: model.ActionPiece,
				Piece: &model.PieceSettings{PieceName: "no_such_piece", ActionName: "no_such_action"},
			},
		},
	}
}

func TestFileStore_SaveGetRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	def := FlowDefinition{
		Name: "double-it", DisplayName: "Double It", Description: "doubles n",
		InputSchema: "n (number, required)",
		Flow:        validCodeFlow(),
	}
	if err := fs.Save(def); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := fs.Get("double-it")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get returned ok=false for a saved flow")
	}
	if got.Name != "double-it" || got.DisplayName != "Double It" || got.Description != "doubles n" {
		t.Fatalf("metadata mismatch: %#v", got)
	}
	if got.InputSchema != "n (number, required)" {
		t.Fatalf("InputSchema = %q, want %q", got.InputSchema, "n (number, required)")
	}
	if got.Flow.Trigger == nil || got.Flow.Trigger.NextAction == nil {
		t.Fatalf("Flow not persisted: %#v", got.Flow)
	}
	if got.Flow.Trigger.NextAction.Type != model.ActionCode {
		t.Fatalf("Flow action type = %v, want CODE", got.Flow.Trigger.NextAction.Type)
	}
}

func TestFileStore_GetMissing_OkFalseNoError(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	got, ok, err := fs.Get("never-saved")
	if err != nil {
		t.Fatalf("Get missing: err = %v, want nil", err)
	}
	if ok {
		t.Fatalf("Get missing: ok = true, want false; def=%#v", got)
	}
}

func TestFileStore_ListReflectsSavedNames(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		def := FlowDefinition{Name: name, DisplayName: name, Flow: validCodeFlow()}
		if err := fs.Save(def); err != nil {
			t.Fatalf("Save %q: %v", name, err)
		}
	}
	defs, err := fs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(defs) != 3 {
		t.Fatalf("len(defs) = %d, want 3", len(defs))
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, want := range []string{"alpha", "bravo", "charlie"} {
		if !names[want] {
			t.Fatalf("List missing %q; got %v", want, names)
		}
	}
}

func TestFileStore_Delete_ExistingThenGone(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Save(FlowDefinition{Name: "killme", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := fs.Delete("killme"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, err := fs.Get("killme"); err != nil || ok {
		t.Fatalf("Get after Delete: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestFileStore_DeleteMissing_ErrNotFound(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing: err = %v, want ErrNotFound", err)
	}
}

func TestFileStore_PathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	for _, name := range []string{"", "../x", "a/b", "a\\b", ".", ".."} {
		err := fs.Save(FlowDefinition{Name: name, Flow: validCodeFlow()})
		if err == nil {
			t.Fatalf("name=%q: Save succeeded, want rejection", name)
		}
	}
	// Nothing escaped the store dir: the "../x" and "a/b" attempts must not
	// have created files above or in subdirs of dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("unexpected subdirectory created in store dir: %s", e.Name())
		}
	}
}

func TestGatedStore_PieceReferencingFlow_RejectedUnderlyingNotCalled(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	gs := &GatedStore{
		Underlying:    fs,
		BuildRegistry: func() (*piece.Registry, error) { return piece.NewRegistry(), nil },
	}
	def := FlowDefinition{Name: "bad", DisplayName: "Bad", Flow: pieceReferencingFlow()}
	if err := gs.Save(def); err == nil {
		t.Fatal("Save of a piece-referencing flow against an empty registry succeeded, want rejection")
	} else if !strings.Contains(err.Error(), "failed validation") {
		t.Fatalf("error = %v, want a 'failed validation' rejection", err)
	}
	// Underlying.Save was never called: the flow is not on disk.
	if _, ok, err := fs.Get("bad"); err != nil || ok {
		t.Fatalf("rejected flow was persisted: ok=%v err=%v", ok, err)
	}
	defs, err := fs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("List = %d defs, want 0 (Underlying.Save never called)", len(defs))
	}
}

func TestGatedStore_CodeFlow_PassesAgainstEmptyRegistry(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	gs := &GatedStore{
		Underlying:    fs,
		BuildRegistry: func() (*piece.Registry, error) { return piece.NewRegistry(), nil },
	}
	def := FlowDefinition{Name: "ok", DisplayName: "OK", Flow: validCodeFlow()}
	if err := gs.Save(def); err != nil {
		t.Fatalf("Save of a no-piece flow against an empty registry: %v", err)
	}
	if _, ok, err := fs.Get("ok"); err != nil || !ok {
		t.Fatalf("Get after Save: ok=%v err=%v, want ok=true err=nil", ok, err)
	}
}

func TestGatedStore_BuildRegistryError_PropagatedUnderlyingNotCalled(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	boom := errors.New("registry boom")
	gs := &GatedStore{
		Underlying:    fs,
		BuildRegistry: func() (*piece.Registry, error) { return nil, boom },
	}
	err = gs.Save(FlowDefinition{Name: "whatever", Flow: validCodeFlow()})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap %v", err, boom)
	}
	// Underlying.Save was never called: nothing on disk.
	if defs, err := fs.List(); err != nil || len(defs) != 0 {
		t.Fatalf("List after failed BuildRegistry: defs=%d err=%v, want 0/nil", len(defs), err)
	}
}

func TestGatedStore_GetListDelete_Passthrough(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	gs := &GatedStore{
		Underlying:    fs,
		BuildRegistry: func() (*piece.Registry, error) { return piece.NewRegistry(), nil },
	}
	// Seed the underlying store directly (bypassing the gate on purpose) so
	// Get/List/Delete exercise pure pass-through.
	if err := fs.Save(FlowDefinition{Name: "seeded", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if _, ok, err := gs.Get("seeded"); err != nil || !ok {
		t.Fatalf("GatedStore.Get passthrough: ok=%v err=%v", ok, err)
	}
	if defs, err := gs.List(); err != nil || len(defs) != 1 {
		t.Fatalf("GatedStore.List passthrough: defs=%d err=%v", len(defs), err)
	}
	if err := gs.Delete("seeded"); err != nil {
		t.Fatalf("GatedStore.Delete passthrough: %v", err)
	}
	if err := gs.Delete("seeded"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GatedStore.Delete missing: err=%v, want ErrNotFound", err)
	}
}
