package catalog

import (
	"fmt"
	"log"
	"time"
)

// GatedStore wraps another Store and runs Validate before delegating
// Save — the enforced version of the quality gate, same "wrap the raw
// operation, offer the safe path as a decorator" shape as
// piece.RegisterValidated wrapping Registry.Register and
// piece.ScopedStore wrapping a shared piece.Store. A definition that
// fails Validate is never handed to Underlying at all — the raw Store
// interface is still there for a caller that wants to bypass the gate on
// purpose (e.g. re-saving a Definition already known to have passed).
type GatedStore struct {
	Underlying Store
	// Versions records an immutable snapshot of every successful Save,
	// when configured — nil (the default) disables version history
	// entirely, the same nil-means-off convention every optional Store
	// in this project already uses. See VersionRecord's own doc comment
	// for the full rationale and Rollback below for how a past version
	// gets restored. Mirrors flowstore.GatedStore.Versions exactly.
	Versions VersionStore
}

// Save runs Validate (which actually RUNS every action/trigger/dropdown's
// Examples), and — if it passes — delegates to Underlying.Save. On a
// successful save, if Versions is configured, def is also recorded as a
// new VersionRecord — best-effort: a failure to WRITE that record is
// logged and otherwise swallowed, the same "already-completed operation
// must not become an error over an audit-log hiccup" reasoning
// flowstore.GatedStore.Save's own version recording already uses.
func (s *GatedStore) Save(def Definition) error {
	if errs := Validate(def); len(errs) > 0 {
		return fmt.Errorf("catalog: definition %q failed validation: %s", def.Name, FormatValidationErrors(errs))
	}
	if err := s.Underlying.Save(def); err != nil {
		return err
	}
	if s.Versions != nil {
		if _, err := s.Versions.Save(VersionRecord{PieceName: def.Name, Definition: def, SavedAt: time.Now()}); err != nil {
			log.Printf("catalog: recording version of %q: %v", def.Name, err)
		}
	}
	return nil
}

// Rollback restores pieceName to a past version: fetches versionID from
// Versions and calls Save with its Definition — reusing the EXACT same
// Validate gate a live edit already goes through (a version whose
// action/trigger/dropdown Examples no longer pass — e.g. because
// something else in the piece's own JS environment assumptions changed —
// fails to roll back, rather than silently restoring something broken),
// and — since it goes through Save — recording its own new version entry
// too, so rolling back never rewrites history, only extends it. Mirrors
// flowstore.GatedStore.Rollback exactly.
func (s *GatedStore) Rollback(pieceName, versionID string) error {
	if s.Versions == nil {
		return fmt.Errorf("catalog: version history is not configured on this store")
	}
	rec, ok, err := s.Versions.Get(versionID)
	if err != nil {
		return fmt.Errorf("catalog: resolving version %q: %w", versionID, err)
	}
	if !ok {
		return fmt.Errorf("catalog: no version %q", versionID)
	}
	if rec.PieceName != pieceName {
		return fmt.Errorf("catalog: version %q belongs to piece %q, not %q", versionID, rec.PieceName, pieceName)
	}
	return s.Save(rec.Definition)
}

func (s *GatedStore) Get(name string) (Definition, bool, error) {
	return s.Underlying.Get(name)
}

func (s *GatedStore) List() ([]Definition, error) {
	return s.Underlying.List()
}

// Delete is a pass-through to Underlying, without re-validating — same
// criterion flowstore.GatedStore.Delete already documents: a piece already
// on disk was valid when it was saved, and deleting it needs no quality
// check at all.
func (s *GatedStore) Delete(name string) error {
	return s.Underlying.Delete(name)
}
