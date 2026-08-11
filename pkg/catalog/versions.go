package catalog

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
	"sync/atomic"
	"time"
)

// VersionRecord is one immutable snapshot of a named piece, taken at the
// moment it was saved. Every successful GatedStore.Save records one here
// — the piece-level analogue of flowstore.VersionRecord, closing the same
// gap for pieces that flow versioning already closed for flows: a piece
// edit can pass every worked Example and still turn out to break
// something the examples didn't cover, and without this the only way
// back to a working piece was remembering (or re-authoring) its previous
// Definition by hand.
//
// Mirrors flowstore.VersionRecord (itself mirroring pkg/runstore's own
// "append-only immutable snapshot" shape) closely on purpose: one JSON
// file per version, atomic write, a caller-opaque random ID Save assigns
// and hands back — the same nil-means-off convention every optional
// Store in this project already uses, so a deployment that doesn't
// configure one pays nothing for it and behaves exactly as it did before
// this file existed.
//
// Rollback is deliberately NOT a method on this store: it lives on
// GatedStore instead, as "fetch an old VersionRecord.Definition and Save
// it again" — reusing the exact same Validate gate (which actually RUNS
// every action/trigger/dropdown's Examples) a live edit already goes
// through, and recording ITS OWN new version entry, so the history is a
// strictly-growing log that's never rewritten.
type VersionRecord struct {
	ID         string
	PieceName  string
	Definition Definition
	SavedAt    time.Time
	// Seq is a monotonically increasing counter Save assigns (never
	// caller-set), used ONLY to break a SavedAt tie in
	// sortVersionsNewestFirst: two Save calls close enough together can
	// land on the identical time.Time value on a coarse system clock
	// (observed on Windows), and a map's iteration order (MemoryVersionStore
	// builds ListForPiece's slice by ranging over one) is randomized —
	// without this, "newest first" would be silently wrong whenever a tie
	// happens, not just cosmetically flaky. SavedAt still dominates
	// ordering across a process restart (a fresh process's counter starts
	// over at a low value, but its saves also have a genuinely LATER
	// SavedAt than anything from a previous run, so the tie-break case
	// only ever matters within one process's short window, exactly where
	// Seq is meaningful).
	Seq int64
}

// VersionSummary is the metadata-only projection ListForPiece returns —
// a version's full Definition (every action/trigger's real source code)
// can be large, so listing stays cheap and a caller fetches one
// version's full VersionRecord via Get only when it actually wants to
// inspect or roll back to it.
type VersionSummary struct {
	ID        string
	PieceName string
	SavedAt   time.Time
	Seq       int64
}

// VersionStore is the surface a piece-version store exposes. Save assigns
// rec a fresh ID (any ID rec already carries is ignored) and returns it.
// Get fetches one full VersionRecord by ID — a missing id is (zero,
// false, nil), not an error, matching every other Store in this project.
// ListForPiece returns every version recorded for pieceName, newest first
// — filtered server-side, same reasoning flowstore.VersionStore.
// ListForFlow already documents: browsing versions is almost always
// "show me THIS piece's history," not a global timeline.
type VersionStore interface {
	Save(rec VersionRecord) (id string, err error)
	Get(id string) (VersionRecord, bool, error)
	ListForPiece(pieceName string) ([]VersionSummary, error)
}

func newVersionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("catalog: generating version id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// versionSeq is the process-wide monotonic counter backing
// VersionRecord.Seq — see that field's own doc comment for why it exists.
var versionSeq int64

func nextVersionSeq() int64 {
	return atomic.AddInt64(&versionSeq, 1)
}

func validateVersionID(id string) error {
	if id == "" {
		return fmt.Errorf("catalog: version id is empty")
	}
	if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return fmt.Errorf("catalog: invalid version id %q", id)
	}
	return nil
}

func summarizeVersion(rec VersionRecord) VersionSummary {
	return VersionSummary{ID: rec.ID, PieceName: rec.PieceName, SavedAt: rec.SavedAt, Seq: rec.Seq}
}

// sortVersionsNewestFirst orders by SavedAt descending, breaking a tie
// (see VersionRecord.Seq's own doc comment for why one can happen) with
// Seq descending — the more recently-assigned counter value wins.
func sortVersionsNewestFirst(s []VersionSummary) {
	sort.Slice(s, func(i, j int) bool {
		if !s[i].SavedAt.Equal(s[j].SavedAt) {
			return s[i].SavedAt.After(s[j].SavedAt)
		}
		return s[i].Seq > s[j].Seq
	})
}

// MemoryVersionStore is the default VersionStore: a plain, mutex-guarded
// map — dies with the process, mainly for tests, same convention as
// runstore.MemoryStore/flowstore.MemoryVersionStore.
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
	rec.Seq = nextVersionSeq()
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

func (s *MemoryVersionStore) ListForPiece(pieceName string) ([]VersionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]VersionSummary, 0)
	for _, rec := range s.versions {
		if rec.PieceName == pieceName {
			out = append(out, summarizeVersion(rec))
		}
	}
	sortVersionsNewestFirst(out)
	return out, nil
}

// FileVersionStore is a VersionStore backed by one JSON file per version,
// in a directory on disk — real persistence across process restarts,
// same pattern as runstore.FileStore/flowstore.FileVersionStore.
type FileVersionStore struct {
	dir string
}

// NewFileVersionStore returns a FileVersionStore rooted at dir, creating
// it if it doesn't already exist (0o755, same as every other FileStore in
// this project).
func NewFileVersionStore(dir string) (*FileVersionStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("catalog: creating version store directory: %w", err)
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
	rec.Seq = nextVersionSeq()
	p, err := s.path(id)
	if err != nil {
		return "", err
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("catalog: encoding version %q: %w", id, err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("catalog: creating temp file for version %q: %w", id, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return "", fmt.Errorf("catalog: writing version %q: %w", id, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("catalog: closing temp file for version %q: %w", id, err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		cleanup()
		return "", fmt.Errorf("catalog: setting mode for version %q: %w", id, err)
	}
	if err := os.Rename(tmpPath, p); err != nil {
		cleanup()
		return "", fmt.Errorf("catalog: renaming version %q into place: %w", id, err)
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
		return VersionRecord{}, false, fmt.Errorf("catalog: reading version %q: %w", id, err)
	}
	var rec VersionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return VersionRecord{}, false, fmt.Errorf("catalog: decoding version %q: %w", id, err)
	}
	return rec, true, nil
}

// ListForPiece returns every version recorded for pieceName, metadata
// only, newest first — reading each .json file's full VersionRecord and
// filtering/projecting client-side (there is no separate lightweight
// index file; this project's other FileStores work the same way and
// haven't needed one at their current scale).
func (s *FileVersionStore) ListForPiece(pieceName string) ([]VersionSummary, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing version store directory: %w", err)
	}
	out := make([]VersionSummary, 0)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("catalog: reading %q: %w", e.Name(), err)
		}
		var rec VersionRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("catalog: decoding %q: %w", e.Name(), err)
		}
		if rec.PieceName == pieceName {
			out = append(out, summarizeVersion(rec))
		}
	}
	sortVersionsNewestFirst(out)
	return out, nil
}
