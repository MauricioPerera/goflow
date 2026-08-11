package flowstore

import (
	"fmt"
	"log"
	"strings"
	"time"

	"goflow/pkg/flowvalidate"
	"goflow/pkg/piece"
)

// FormatValidationErrors joins errs into one human-readable message — one
// error per line as "<path>: <message>". This is the flowvalidate equivalent
// of catalog.FormatValidationErrors (which joins with "; "): flowvalidate
// paths can be long ("trigger > double > nextAction > ...") and a flow can
// accumulate many errors, so newline-separated reads better in an HTTP error
// body. Same spirit: a single readable string GatedStore.Save wraps its
// rejection error with.
func FormatValidationErrors(errs []flowvalidate.ValidationError) string {
	lines := make([]string, len(errs))
	for i, e := range errs {
		lines[i] = fmt.Sprintf("%s: %s", e.Path, e.Message)
	}
	return strings.Join(lines, "\n")
}

// GatedStore wraps another Store and runs flowvalidate.Validate before
// delegating Save — the enforced quality gate for persisted flows, same
// "wrap the raw operation, offer the safe path as a decorator" shape as
// catalog.GatedStore wrapping a catalog.Store. A flow that fails Validate is
// never handed to Underlying at all; the raw Store interface is still there
// for a caller that wants to bypass the gate on purpose.
//
// BuildRegistry is called fresh on EVERY Save (never cached): a flow saved
// now must validate against the piece registry AS IT IS now, including JS
// pieces added to the catalog after this process started. That's why it's a
// func field rather than a *piece.Registry captured at construction — the
// registry is rebuilt per save, same as httpapi.Server.buildRegistry rebuilds
// it per request.
type GatedStore struct {
	Underlying    Store
	BuildRegistry func() (*piece.Registry, error)
	// Versions records an immutable snapshot of every successful Save,
	// when configured — nil (the default) disables version history
	// entirely, the same nil-means-off convention every optional Store
	// in this project already uses. See VersionRecord's own doc comment
	// for the full rationale and Rollback below for how a past version
	// gets restored.
	Versions VersionStore
}

// Save builds a fresh registry, runs ValidateExamples against def (which
// itself runs flowvalidate.Validate first, then actually EXECUTES every
// def.Examples case if that passes — see ValidateExamples's own doc
// comment for exactly what that does and doesn't cover), and — if it
// passes — delegates to Underlying.Save. A BuildRegistry failure is
// propagated wrapped in context (never reaches validation or Underlying).
// A validation or example failure is rejected without calling
// Underlying.Save, with an error whose message is the formatted list of
// every problem found — a flow with zero Examples behaves exactly as
// before this field existed.
//
// On a successful Underlying.Save, if Versions is configured, def is
// also recorded as a new VersionRecord — best-effort: a failure to WRITE
// that record is logged and otherwise swallowed, the same
// "already-completed operation must not become an error over an
// audit-log hiccup" reasoning TriggerOnFailure's own history recording
// already uses. The flow itself is already saved by that point regardless.
func (s *GatedStore) Save(def FlowDefinition) error {
	registry, err := s.BuildRegistry()
	if err != nil {
		return fmt.Errorf("flowstore: building registry for %q: %w", def.Name, err)
	}
	if errs := ValidateExamples(def, registry); len(errs) > 0 {
		return fmt.Errorf("flowstore: flow %q failed validation: %s", def.Name, FormatValidationErrors(errs))
	}
	if err := s.Underlying.Save(def); err != nil {
		return err
	}
	if s.Versions != nil {
		if _, err := s.Versions.Save(VersionRecord{FlowName: def.Name, Definition: def, SavedAt: time.Now()}); err != nil {
			log.Printf("flowstore: recording version of %q: %v", def.Name, err)
		}
	}
	return nil
}

// Rollback restores flowName to a past version: fetches versionID from
// Versions and calls Save with its Definition — reusing the EXACT same
// validation/example gate a live edit already goes through (a version
// whose target piece no longer exists in the CURRENT registry fails to
// roll back, rather than silently restoring something broken right now),
// and — since it goes through Save — recording its own new version entry
// too, so rolling back never rewrites history, only extends it.
func (s *GatedStore) Rollback(flowName, versionID string) error {
	if s.Versions == nil {
		return fmt.Errorf("flowstore: version history is not configured on this store")
	}
	rec, ok, err := s.Versions.Get(versionID)
	if err != nil {
		return fmt.Errorf("flowstore: resolving version %q: %w", versionID, err)
	}
	if !ok {
		return fmt.Errorf("flowstore: no version %q", versionID)
	}
	if rec.FlowName != flowName {
		return fmt.Errorf("flowstore: version %q belongs to flow %q, not %q", versionID, rec.FlowName, flowName)
	}
	return s.Save(rec.Definition)
}

// Get/List/Delete are pass-through to Underlying, without re-validating —
// same criterion as catalog.GatedStore: a flow already on disk was valid when
// it was saved; re-validating on read would also reject a flow that
// references a piece since removed from the catalog, which is a real runtime
// concern (a stale flow) better surfaced at run time than hidden at read time.
func (s *GatedStore) Get(name string) (FlowDefinition, bool, error) {
	return s.Underlying.Get(name)
}

func (s *GatedStore) List() ([]FlowDefinition, error) {
	return s.Underlying.List()
}

func (s *GatedStore) Delete(name string) error {
	return s.Underlying.Delete(name)
}
