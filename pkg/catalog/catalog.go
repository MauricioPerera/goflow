// Package catalog persists JS-authored pieces (pkg/jspiece) and makes them
// discoverable, closing a real gap Phase 2 left open: jspiece.New builds a
// piece.Piece purely in memory — it dies with the process, and nothing lets
// an agent check "does a piece for this already exist" before generating a
// near-duplicate one. That defeats the whole point of a growing, reused
// catalog instead of a piece re-created every time it's needed.
//
// Also covers a quality gate (validate.go, GatedStore): a Definition must
// carry at least one worked Example per action, and Validate actually RUNS
// them against the piece before GatedStore.Save persists it — catching a
// structurally-valid-but-broken piece before it ever reaches the shared
// catalog. Deliberately still narrow beyond that: nothing here validates a
// *flow* that references a cataloged piece ahead of running it — a real,
// separate gap, intentionally left for later.
package catalog

import (
	"fmt"

	"goflow/pkg/jspiece"
	"goflow/pkg/piece"
)

// ActionDefinition is one JS-authored action, persisted.
type ActionDefinition struct {
	Name        string
	DisplayName string

	// Description is plain-language text describing what this action
	// does — the whole reason this package exists: an agent deciding
	// whether to reuse this action instead of writing a new one needs
	// something to read.
	Description string

	// InputSchema is free-text describing the action's expected Input
	// fields (e.g. "url (string, required), method (string, optional,
	// default GET)") — deliberately not a formal JSON Schema for v1.
	// piece.ActionContext.Input is untyped (map[string]any) with no
	// engine-enforced shape to derive a real schema from, so a
	// hand-written description is what's actually available; a formal
	// schema would need the piece author to also author (and keep in
	// sync with) a separate machine-checked contract, which is real
	// added scope this package doesn't take on.
	InputSchema string

	// Source is the action's JS source — see jspiece.ActionSource.Source
	// for the exact contract.
	Source string

	// Examples are worked Input/Output cases Validate actually runs
	// against this action before GatedStore.Save will persist it — see
	// validate.go. At least one is required by Validate; there is no
	// engine-enforced way to check a JS action "works" other than running
	// it against cases its own author (agent or human) provides.
	Examples []Example
}

// Definition is a serializable description of one JS-authored piece —
// what a Store persists.
type Definition struct {
	Name        string
	DisplayName string
	Description string
	Actions     []ActionDefinition
}

// ToPiece converts a Definition into a real piece.Piece, exactly the same
// shape jspiece.New produces directly — a piece loaded from a Store is
// indistinguishable from one built fresh in code.
func (d Definition) ToPiece() piece.Piece {
	sources := make([]jspiece.ActionSource, len(d.Actions))
	for i, a := range d.Actions {
		sources[i] = jspiece.ActionSource{Name: a.Name, DisplayName: a.DisplayName, Source: a.Source}
	}
	return jspiece.New(d.Name, d.DisplayName, sources)
}

// Store persists and retrieves piece Definitions. Save overwrites any
// existing Definition with the same Name — a re-saved piece replaces its
// prior version, there is no history/versioning in v1.
type Store interface {
	Save(def Definition) error
	Get(name string) (Definition, bool, error)
	List() ([]Definition, error)
}

// RegisterFromStore loads every Definition in store, converts each to a
// piece.Piece, and registers it into r via RegisterValidated — the
// mechanism that turns "pieces an agent saved in a prior session" into
// pieces a running Engine can actually invoke. Stops at the first
// failure, matching pieces.RegisterAll's own error-handling convention.
func RegisterFromStore(r *piece.Registry, store Store) error {
	defs, err := store.List()
	if err != nil {
		return fmt.Errorf("listing catalog store: %w", err)
	}
	for _, def := range defs {
		if err := r.RegisterValidated(def.ToPiece()); err != nil {
			return fmt.Errorf("registering piece %q from catalog: %w", def.Name, err)
		}
	}
	return nil
}
