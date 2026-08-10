package catalog_test

import (
	"os"
	"strings"
	"testing"

	"goflow/pkg/catalog"
)

// newFileStore is a small helper shared by the atomicity tests below.
func newFileStore(t *testing.T) (*catalog.FileStore, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return store, dir
}

// assertNoOrphanTemps fails the test if dir contains any leftover temp
// files from Save's atomic-rename dance — only the final .json should be
// there.
func assertNoOrphanTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".tmp-") {
			t.Fatalf("orphan temp file left in store dir: %q", name)
		}
	}
}

// TestFileStore_SaveRoundTrip keeps the existing behavior intact: a piece
// saved and then read back via Get must round-trip exactly.
func TestFileStore_SaveRoundTrip(t *testing.T) {
	store, _ := newFileStore(t)

	def := sampleDefinition("round_trip")
	if err := store.Save(def); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := store.Get("round_trip")
	if err != nil || !ok {
		t.Fatalf("Get(round_trip) = ok=%v err=%v, want ok=true", ok, err)
	}
	if got.DisplayName != def.DisplayName || len(got.Actions) != 1 ||
		got.Actions[0].Source != def.Actions[0].Source {
		t.Fatalf("Get() = %+v, want exact round-trip of %+v", got, def)
	}
}

// TestFileStore_SaveLeavesNoTempFiles confirms a successful Save leaves
// only the final .json — no ".tmp-*" orphan from the atomic rename.
func TestFileStore_SaveLeavesNoTempFiles(t *testing.T) {
	store, dir := newFileStore(t)

	if err := store.Save(sampleDefinition("solo")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertNoOrphanTemps(t, dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var jsons []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			jsons = append(jsons, e.Name())
		}
	}
	if len(jsons) != 1 || jsons[0] != "solo.json" {
		t.Fatalf("dir json files = %v, want exactly [solo.json]", jsons)
	}
}

// TestFileStore_SaveOverwritesWithoutOrphans runs several Saves over the
// same piece name (overwriting) and checks each round leaves no temp
// orphan and the latest content is what Get returns.
func TestFileStore_SaveOverwritesWithoutOrphans(t *testing.T) {
	store, dir := newFileStore(t)

	for i := 0; i < 5; i++ {
		def := sampleDefinition("overwritten")
		def.Description = "revision"
		if err := store.Save(def); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
		assertNoOrphanTemps(t, dir)

		got, ok, err := store.Get("overwritten")
		if err != nil || !ok {
			t.Fatalf("Get #%d = ok=%v err=%v, want ok=true", i, ok, err)
		}
		if got.Description != "revision" {
			t.Fatalf("Get #%d Description = %q, want %q", i, got.Description, "revision")
		}
	}

	// After all overwrites, exactly one .json for this piece remains.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.Name() == "overwritten.json" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("found %d overwritten.json, want 1", count)
	}
}
