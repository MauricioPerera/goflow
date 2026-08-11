// Package runstore persists a record of every flow run — closing the gap
// README.md documented: a Verdict/Steps map used to exist only for the
// duration of one flowstore.Run call and was handed back to the caller,
// never stored anywhere. flowstore.RunWithHistory (used by HTTP's
// POST /flows/run and POST /flows/{name}/run, and by MCP's tools/call — the
// same three transports pkg/flowstore's own doc comment already says share
// one execution path so they can't drift apart) records one Record here
// after every completed run, when a Store is configured.
//
// The on-disk shape mirrors pkg/catalog, pkg/credentials, and pkg/flowstore
// on purpose: one JSON file per record, atomic write (CreateTemp in the same
// dir + Rename). Unlike those three, a Record has no caller-chosen name —
// there is nothing for a human or agent to name about one specific run — so
// Save assigns a fresh random ID and hands it back, the way a database
// insert returns a generated primary key, rather than taking one as input
// the way catalog.Store.Save/credentials.Store.Save/flowstore.Store.Save do.
//
// Recording is opt-in at every call site that threads a Store through: it is
// taken as possibly nil (matching credentials.Store's existing
// nil-means-off convention in this project), so code that doesn't want run
// history persisted pays nothing for it. pkg/httpapi's production wiring
// always supplies a real Store, the same as it does for credentials and
// flows.
//
// The State recorded is exactly what the caller already produced — for a
// run that went through flowstore.RunWithCredentials first (which
// RunWithHistory always does), that means any $credential markers are
// already resolved-then-redacted (see flowstore.RedactCredentials) before
// Save ever sees the state. This package has no credential-awareness of its
// own and trusts its caller not to hand it an unredacted secret; it is not
// itself a place secrets should ever end up.
//
// Everything here is stdlib only — crypto/rand, encoding/hex,
// encoding/json, os, path/filepath, sort, strings, sync, time — matching the
// rest of this project's zero-new-dependency stance.
package runstore

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

	"goflow/pkg/model"
)

// Record is one completed flow run.
type Record struct {
	ID string
	// FlowName is the persisted flow's name for a named run
	// (POST /flows/{name}/run or an MCP tools/call) — empty for an ad-hoc run
	// (POST /flows/run with an inline FlowVersion body), which has no name to
	// record.
	FlowName   string
	Trigger    any
	State      *model.ExecutionState
	StartedAt  time.Time
	FinishedAt time.Time
	// ExecuteTrigger is the flag this run actually used — whether a
	// PIECE_TRIGGER's Run hook was really invoked, or Trigger was passed
	// straight through as the trigger step's output. Recorded so
	// flowstore.ReplayRun can faithfully reproduce a past run's mode
	// instead of guessing false (this run's own trigger step already
	// tells you WHAT happened; this field is the only record of HOW).
	ExecuteTrigger bool
	// ReplayOfRunID, if set, is the id of the past run this one replayed
	// — flowstore.ReplayRun's own trace of provenance, so a caller
	// browsing history later can tell a replay apart from an organic
	// run, rather than the two looking identical. Empty for every
	// organic run, including a CALL_FLOW's own sub-run (a replay only
	// ever marks its own top-level record — a sub-flow triggered from
	// inside one is a normal run, not itself a replay).
	ReplayOfRunID string
	// ResumeOfRunID, if set, is the id of the PAUSED run this one resumed
	// — flowstore.ResumeRun's own trace of provenance, mirroring
	// ReplayOfRunID's own reasoning above. Empty for every run that
	// wasn't produced by resuming a pause, including the resumed run's
	// own eventual completion history entries beyond the first (only the
	// record ResumeRun directly produces carries it).
	ResumeOfRunID string
	// ResumeToken is a fresh 128-bit random secret Save assigns whenever
	// (and only whenever) rec.State.Verdict.Status is PAUSED — any value
	// the caller already set on rec is ignored and overwritten, the same
	// "never trust caller input for something meant to be unforgeable"
	// treatment ID itself already gets. It exists so a PAUSED run can be
	// resumed by someone who holds only this token — e.g. clicking an
	// approval link in an email — without the full GOFLOW_API_TOKEN; see
	// pkg/httpapi's POST /public/runs/{id}/resume. Empty for every run
	// that never paused. Never included in Summary (List's own
	// projection) — deliberately: a caller with only enough access to
	// LIST runs shouldn't also bulk-harvest every paused run's resume
	// token in one call; a full Get (which already requires the same
	// GOFLOW_API_TOKEN this token exists to let someone bypass) is the
	// only way to read it back. No expiry and no revocation once issued
	// — this project's runstore.Record is append-only/immutable like
	// every other Record here, so there is no "mark it consumed"
	// operation; the same lack-of-guard flowstore.ResumeRun's own doc
	// comment already discloses for resuming a run id twice applies
	// doubly here, since this is now reachable by anyone who has the
	// token, not just an authenticated caller.
	ResumeToken string
}

// Summary is the metadata-only projection List returns — mirrors
// flowSummary in pkg/httpapi: a run's full State (every step's Input/Output)
// can be large, so listing stays cheap and a caller fetches one run's full
// Record via Get only when it actually wants the detail.
type Summary struct {
	ID         string
	FlowName   string
	Status     model.FlowRunStatus
	StartedAt  time.Time
	FinishedAt time.Time
	// ReplayOfRunID mirrors Record's own field — included here (unlike
	// State, the one field List always omits) since it's a single small
	// string, cheap enough that hiding it would only cost a caller an
	// extra Get to tell a replay apart from an organic run while browsing.
	ReplayOfRunID string
	// ResumeOfRunID mirrors Record's own field, same reasoning as
	// ReplayOfRunID above.
	ResumeOfRunID string
}

// Store is the surface a run-history store exposes. Save assigns rec a
// fresh ID (any ID rec already carries is ignored) and returns it. Get
// fetches one full Record by ID — a missing id is (zero, false, nil), not an
// error, matching every other Store in this project. List returns every
// recorded run, metadata only, newest first.
type Store interface {
	Save(rec Record) (id string, err error)
	Get(id string) (Record, bool, error)
	List() ([]Summary, error)
}

// newID returns a fresh 128-bit random hex id — always safe to use as a
// filename verbatim (no path-traversal characters possible from a fixed
// hex alphabet), unlike a caller-supplied name elsewhere in this project.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("runstore: generating id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// newResumeToken generates a fresh 128-bit random hex secret — same
// shape as newID, kept as its own function (not newID reused) since the
// two mean different things: an id is not sensitive and appears freely
// in URLs/logs (GET /runs/{id}), a resume token IS the credential a
// caller without GOFLOW_API_TOKEN presents — see Record.ResumeToken's
// own doc comment.
func newResumeToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("runstore: generating resume token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// assignResumeToken sets rec.ResumeToken to a fresh token whenever rec's
// State says the run is PAUSED, and clears it (empty) otherwise —
// called by both Save implementations below so a caller-supplied
// ResumeToken is never trusted, the same treatment rec.ID already gets.
func assignResumeToken(rec *Record) error {
	if rec.State == nil || rec.State.Verdict.Status != model.FlowRunPaused {
		rec.ResumeToken = ""
		return nil
	}
	token, err := newResumeToken()
	if err != nil {
		return err
	}
	rec.ResumeToken = token
	return nil
}

// validateID rejects an id that can't safely become a filename. Save's own
// ids are always safe hex, but Get's id may come straight from an HTTP path
// segment (GET /runs/{id}) — caller-supplied, so it gets the same
// reject-outright treatment as catalog.FileStore.path/
// credentials.FileStore.path/flowstore.FileStore.path give a caller-supplied
// name, rather than trying to "clean" a traversal attempt.
func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("runstore: id is empty")
	}
	if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return fmt.Errorf("runstore: invalid id %q", id)
	}
	return nil
}

func summarize(rec Record) Summary {
	var status model.FlowRunStatus
	if rec.State != nil {
		status = rec.State.Verdict.Status
	}
	return Summary{ID: rec.ID, FlowName: rec.FlowName, Status: status, StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt, ReplayOfRunID: rec.ReplayOfRunID, ResumeOfRunID: rec.ResumeOfRunID}
}

// sortNewestFirst orders by StartedAt descending — most recent run first,
// the order a caller browsing history actually wants.
func sortNewestFirst(s []Summary) {
	sort.Slice(s, func(i, j int) bool { return s[i].StartedAt.After(s[j].StartedAt) })
}

// MemoryStore is the default Store: a plain, mutex-guarded map — dies with
// the process, mainly for tests, same convention as
// piece.MemoryStore/catalog.MemoryStore.
type MemoryStore struct {
	mu      sync.Mutex
	records map[string]Record
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: map[string]Record{}}
}

func (s *MemoryStore) Save(rec Record) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	rec.ID = id
	if err := assignResumeToken(&rec); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[id] = rec
	return id, nil
}

func (s *MemoryStore) Get(id string) (Record, bool, error) {
	if err := validateID(id); err != nil {
		return Record{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	return rec, ok, nil
}

func (s *MemoryStore) List() ([]Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Summary, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, summarize(rec))
	}
	sortNewestFirst(out)
	return out, nil
}

// FileStore is a Store backed by one JSON file per run, in a directory on
// disk — real persistence across process restarts, using nothing but
// encoding/json and os, same pattern as catalog.FileStore/
// credentials.FileStore/flowstore.FileStore.
type FileStore struct {
	dir string
}

// NewFileStore returns a FileStore rooted at dir, creating it if it doesn't
// already exist (0o755, same as the other three FileStores in this
// project).
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("runstore: creating store directory: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

func (s *FileStore) path(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, id+".json"), nil
}

// Save encodes rec (after assigning it a fresh id) and writes it atomically
// — CreateTemp in the same dir + Rename, with cleanup at every failure
// point and 0o644 before the rename, the same pattern every other FileStore
// in this project uses. A reader (or a crash) only ever sees a fully-written
// record, never a partial one.
func (s *FileStore) Save(rec Record) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	rec.ID = id
	if err := assignResumeToken(&rec); err != nil {
		return "", err
	}
	p, err := s.path(id)
	if err != nil {
		return "", err
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("runstore: encoding %q: %w", id, err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("runstore: creating temp file for %q: %w", id, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return "", fmt.Errorf("runstore: writing %q: %w", id, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("runstore: closing temp file for %q: %w", id, err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		cleanup()
		return "", fmt.Errorf("runstore: setting mode for %q: %w", id, err)
	}
	if err := os.Rename(tmpPath, p); err != nil {
		cleanup()
		return "", fmt.Errorf("runstore: renaming %q into place: %w", id, err)
	}
	return id, nil
}

// Get returns the Record for id. If the file does not exist it returns
// (zero, false, nil) — not an error — matching every other FileStore.Get in
// this project.
func (s *FileStore) Get(id string) (Record, bool, error) {
	p, err := s.path(id)
	if err != nil {
		return Record{}, false, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("runstore: reading %q: %w", id, err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, false, fmt.Errorf("runstore: decoding %q: %w", id, err)
	}
	return rec, true, nil
}

// List returns every recorded run, metadata only, newest first — reading
// each .json file's full Record and projecting a Summary from it (there is
// no separate lightweight index file; this project's other FileStores work
// the same way and haven't needed one at their current scale).
func (s *FileStore) List() ([]Summary, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("runstore: listing store directory: %w", err)
	}
	out := make([]Summary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("runstore: reading %q: %w", e.Name(), err)
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("runstore: decoding %q: %w", e.Name(), err)
		}
		out = append(out, summarize(rec))
	}
	sortNewestFirst(out)
	return out, nil
}
