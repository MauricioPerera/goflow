package flowstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// VersionRecord is one immutable snapshot of a named flow, taken at the
// moment it was saved. Every successful GatedStore.Save records one here
// — closing the gap left after FlowExample shipped: that gate stops a
// BROKEN save, but does nothing for one that passes every check and
// still turns out to break something the examples didn't cover. Without
// this, the only way back to a working flow was remembering (or
// re-authoring) its previous definition by hand.
//
// Mirrors pkg/runstore's own "append-only immutable snapshot" shape
// closely on purpose: one JSON file per version, atomic write, a
// caller-opaque random ID Save assigns and hands back — the same
// nil-means-off convention every optional Store in this project already
// uses, so a deployment that doesn't configure one pays nothing for it
// and behaves exactly as it did before this file existed.
//
// One deliberate divergence from runstore.Store.List: VersionStore.
// ListForFlow takes a flowName and returns only that flow's versions,
// server-side — unlike runs (where a global timeline across every flow
// is a real, legitimate view GET /runs already serves), browsing
// versions is almost always "show me THIS flow's history," so filtering
// happens here rather than leaving every caller to do it themselves
// against a global list.
//
// Rollback is deliberately NOT a method on this store: it lives on
// GatedStore instead, as "fetch an old VersionRecord.Definition and Save
// it again" — reusing the exact same validation/example gate a live edit
// already goes through (a version whose target piece no longer exists
// fails to roll back, rather than silently restoring something broken
// right now), and recording ITS OWN new version entry, so the history is
// a strictly-growing log that's never rewritten, the same immutability
// runstore's own records already have.
//
// No pruning/TTL here, matching the same accepted, disclosed tradeoff
// runstore already has — real added scope this doesn't take on.
type VersionRecord struct {
	ID         string
	FlowName   string
	Definition FlowDefinition
	SavedAt    time.Time
}

// VersionSummary is the metadata-only projection ListForFlow returns — a
// version's full Definition (the whole trigger/action graph) can be large,
// so listing stays cheap and a caller fetches one version's full
// VersionRecord via Get only when it actually wants to inspect or roll
// back to it.
type VersionSummary struct {
	ID       string
	FlowName string
	SavedAt  time.Time
}

// VersionStore is the surface a flow-version store exposes. Save assigns
// rec a fresh ID (any ID rec already carries is ignored) and returns it.
// Get fetches one full VersionRecord by ID — a missing id is (zero,
// false, nil), not an error, matching every other Store in this project.
// ListForFlow returns every version recorded for flowName, newest first.
type VersionStore interface {
	Save(rec VersionRecord) (id string, err error)
	Get(id string) (VersionRecord, bool, error)
	ListForFlow(flowName string) ([]VersionSummary, error)
}

func newVersionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("flowstore: generating version id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func validateVersionID(id string) error {
	if id == "" {
		return fmt.Errorf("flowstore: version id is empty")
	}
	if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return fmt.Errorf("flowstore: invalid version id %q", id)
	}
	return nil
}

func summarizeVersion(rec VersionRecord) VersionSummary {
	return VersionSummary{ID: rec.ID, FlowName: rec.FlowName, SavedAt: rec.SavedAt}
}

func sortVersionsNewestFirst(s []VersionSummary) {
	sort.Slice(s, func(i, j int) bool { return s[i].SavedAt.After(s[j].SavedAt) })
}

// MemoryVersionStore is the default VersionStore: a plain, mutex-guarded
// map — dies with the process, mainly for tests, same convention as
// runstore.MemoryStore.
type MemoryVersionStore struct {
	mu       sync.Mutex
	versions map[string]VersionRecord
}

func NewMemoryVersionStore() *MemoryVersionStore {
	return &MemoryVersionStore{versions: map[string]VersionRecord{}}
}

func (s *MemoryVersionStore) Save(rec VersionRecord) (string, error) {
	id, err := newVersionID()
	if err != nil {
		return "", err
	}
	rec.ID = id
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versions[id] = rec
	return id, nil
}

func (s *MemoryVersionStore) Get(id string) (VersionRecord, bool, error) {
	if err := validateVersionID(id); err != nil {
		return VersionRecord{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.versions[id]
	return rec, ok, nil
}

func (s *MemoryVersionStore) ListForFlow(flowName string) ([]VersionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]VersionSummary, 0)
	for _, rec := range s.versions {
		if rec.FlowName == flowName {
			out = append(out, summarizeVersion(rec))
		}
	}
	sortVersionsNewestFirst(out)
	return out, nil
}

// FileVersionStore is a VersionStore backed by one JSON file per version,
// in a directory on disk — real persistence across process restarts,
// same pattern as runstore.FileStore.
type FileVersionStore struct {
	dir string
}

// NewFileVersionStore returns a FileVersionStore rooted at dir, creating
// it if it doesn't already exist (0o755, same as every other FileStore in
// this project).
func NewFileVersionStore(dir string) (*FileVersionStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("flowstore: creating version store directory: %w", err)
	}
	return &FileVersionStore{dir: dir}, nil
}

func (s *FileVersionStore) path(id string) (string, error) {
	if err := validateVersionID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, id+".json"), nil
}

// Save encodes rec (after assigning it a fresh id) and writes it
// atomically — CreateTemp in the same dir + Rename, with cleanup at every
// failure point, the same pattern every other FileStore in this project
// uses. A reader (or a crash) only ever sees a fully-written record,
// never a partial one.
func (s *FileVersionStore) Save(rec VersionRecord) (string, error) {
	id, err := newVersionID()
	if err != nil {
		return "", err
	}
	rec.ID = id
	p, err := s.path(id)
	if err != nil {
		return "", err
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("flowstore: encoding version %q: %w", id, err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("flowstore: creating temp file for version %q: %w", id, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return "", fmt.Errorf("flowstore: writing version %q: %w", id, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("flowstore: closing temp file for version %q: %w", id, err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		cleanup()
		return "", fmt.Errorf("flowstore: setting mode for version %q: %w", id, err)
	}
	if err := os.Rename(tmpPath, p); err != nil {
		cleanup()
		return "", fmt.Errorf("flowstore: renaming version %q into place: %w", id, err)
	}
	return id, nil
}

// Get returns the VersionRecord for id. If the file does not exist it
// returns (zero, false, nil) — not an error — matching every other
// FileStore.Get in this project.
func (s *FileVersionStore) Get(id string) (VersionRecord, bool, error) {
	p, err := s.path(id)
	if err != nil {
		return VersionRecord{}, false, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return VersionRecord{}, false, nil
		}
		return VersionRecord{}, false, fmt.Errorf("flowstore: reading version %q: %w", id, err)
	}
	var rec VersionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return VersionRecord{}, false, fmt.Errorf("flowstore: decoding version %q: %w", id, err)
	}
	return rec, true, nil
}

// ListForFlow returns every version recorded for flowName, metadata only,
// newest first — reading each .json file's full VersionRecord and
// filtering/projecting client-side (there is no separate lightweight
// index file; this project's other FileStores work the same way and
// haven't needed one at their current scale).
func (s *FileVersionStore) ListForFlow(flowName string) ([]VersionSummary, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("flowstore: listing version store directory: %w", err)
	}
	out := make([]VersionSummary, 0)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("flowstore: reading %q: %w", e.Name(), err)
		}
		var rec VersionRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("flowstore: decoding %q: %w", e.Name(), err)
		}
		if rec.FlowName == flowName {
			out = append(out, summarizeVersion(rec))
		}
	}
	sortVersionsNewestFirst(out)
	return out, nil
}
