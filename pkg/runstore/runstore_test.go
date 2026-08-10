package runstore_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"goflow/pkg/model"
	"goflow/pkg/runstore"
)

func writeStrayFile(dir string) error {
	return os.WriteFile(filepath.Join(dir, "not-a-record.txt"), []byte("hello"), 0o644)
}

func sampleRecord(flowName string, status model.FlowRunStatus) runstore.Record {
	now := time.Now().UTC().Truncate(time.Millisecond) // JSON round-trip loses sub-millisecond precision on some platforms
	return runstore.Record{
		FlowName: flowName,
		Trigger:  map[string]any{"n": float64(7)},
		State: &model.ExecutionState{
			Steps:   map[string]*model.StepOutput{"double_it": {Status: model.StepSucceeded, Output: map[string]any{"n": float64(14)}}},
			Verdict: model.Verdict{Status: status},
		},
		StartedAt:  now,
		FinishedAt: now.Add(time.Millisecond),
	}
}

// storeContract exercises the Store interface's contract generically — run
// against both MemoryStore and FileStore below so the two implementations
// are held to the exact same behavior. Unlike catalog.Store/
// credentials.Store/flowstore.Store, Save assigns its own ID rather than
// taking a caller-chosen name, so the contract shape differs slightly from
// theirs.
func storeContract(t *testing.T, store runstore.Store) {
	t.Helper()

	if _, ok, err := store.Get("deadbeefdeadbeefdeadbeefdeadbeef"); err != nil || ok {
		t.Fatalf("Get(never-saved) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	summaries, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(summaries) != 0 {
		t.Fatalf("List() on empty store = %+v, want empty", summaries)
	}

	rec := sampleRecord("my-flow", model.FlowRunSucceeded)
	id, err := store.Save(rec)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if id == "" {
		t.Fatal("Save() returned an empty id")
	}

	got, ok, err := store.Get(id)
	if err != nil || !ok {
		t.Fatalf("Get(%q) = ok=%v err=%v, want ok=true", id, ok, err)
	}
	if got.ID != id {
		t.Fatalf("Get(%q).ID = %q, want it to match the id Save returned", id, got.ID)
	}
	if got.FlowName != "my-flow" {
		t.Fatalf("Get().FlowName = %q, want %q", got.FlowName, "my-flow")
	}
	if got.State == nil || got.State.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("Get().State = %+v, want it to round-trip the Verdict", got.State)
	}
	if !got.StartedAt.Equal(rec.StartedAt) {
		t.Fatalf("Get().StartedAt = %v, want %v", got.StartedAt, rec.StartedAt)
	}

	// A second Save gets a DIFFERENT id — unlike catalog/credentials/
	// flowstore, there is no caller-chosen key to overwrite by.
	id2, err := store.Save(sampleRecord("other-flow", model.FlowRunFailed))
	if err != nil {
		t.Fatalf("Save() (second record) error = %v", err)
	}
	if id2 == id {
		t.Fatalf("second Save() returned the same id %q as the first — ids must be unique per run", id)
	}

	summaries, err = store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("List() = %+v, want exactly 2 summaries", summaries)
	}
	byID := map[string]runstore.Summary{}
	for _, s := range summaries {
		byID[s.ID] = s
	}
	if s, ok := byID[id]; !ok || s.FlowName != "my-flow" || s.Status != model.FlowRunSucceeded {
		t.Fatalf("List() summary for %q = %+v, want FlowName=my-flow Status=SUCCEEDED", id, s)
	}
	if s, ok := byID[id2]; !ok || s.FlowName != "other-flow" || s.Status != model.FlowRunFailed {
		t.Fatalf("List() summary for %q = %+v, want FlowName=other-flow Status=FAILED", id2, s)
	}
}

func TestMemoryStore_ImplementsStoreContract(t *testing.T) {
	storeContract(t, runstore.NewMemoryStore())
}

func TestFileStore_ImplementsStoreContract(t *testing.T) {
	store, err := runstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	storeContract(t, store)
}

func TestFileStore_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()

	store1, err := runstore.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	id, err := store1.Save(sampleRecord("persisted-flow", model.FlowRunSucceeded))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A brand new FileStore instance pointed at the SAME directory — this is
	// the actual point of FileStore over MemoryStore: it must see data saved
	// by a previous instance/process, not just its own in-memory state.
	store2, err := runstore.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore (second instance): %v", err)
	}
	got, ok, err := store2.Get(id)
	if err != nil || !ok {
		t.Fatalf("Get(%q) on a fresh instance = ok=%v err=%v, want ok=true", id, ok, err)
	}
	if got.FlowName != "persisted-flow" {
		t.Fatalf("got = %+v", got)
	}
}

func TestFileStore_RejectsPathTraversalIDs(t *testing.T) {
	store, err := runstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	cases := []string{"../escape", "..\\escape", "a/b", "a\\b", ".", "..", ""}
	for _, id := range cases {
		if _, ok, err := store.Get(id); err == nil {
			t.Fatalf("Get(id=%q) = ok=%v err=nil, want a rejection", id, ok)
		}
	}
}

func TestFileStore_ListSkipsUnrelatedFiles(t *testing.T) {
	// Every other FileStore in this project only reads *.json in its
	// directory; a stray non-JSON file (e.g. a leftover .tmp-* from an
	// interrupted Save, or an unrelated file) must not break List.
	dir := t.TempDir()
	store, err := runstore.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if _, err := store.Save(sampleRecord("f", model.FlowRunSucceeded)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := writeStrayFile(dir); err != nil {
		t.Fatalf("writing stray file: %v", err)
	}

	summaries, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("List() = %+v, want exactly 1 (the stray file must be skipped)", summaries)
	}
}
