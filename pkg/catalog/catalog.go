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

	// Dropdowns declares this action's dynamically-loaded properties, keyed
	// by property name — the persisted analogue of piece.Action.Dropdowns
	// (and jspiece.ActionSource.Dropdowns). A dropdown belongs to a specific
	// action, not the piece, so it hangs off ActionDefinition rather than
	// Definition — same ownership as piece.Action.Dropdowns. Each is built
	// from its Source via jspiece.NewDropdown in ToPiece. Optional; most
	// actions have none.
	Dropdowns map[string]DropdownDefinition
}

// DropdownDefinition is one JS-authored dynamically-loaded property on an
// ActionDefinition, persisted — the symmetric counterpart of
// ActionDefinition for piece.DropdownProperty instead of piece.Action. Source
// is the dropdown's JS source — see jspiece.DropdownSource.Source for the
// exact contract. A dropdown is validated exactly like an action: at least one
// DropdownExample is required, and Validate actually runs each against the
// real (JS-backed) dropdown before GatedStore.Save will persist it.
type DropdownDefinition struct {
	// Refreshers names the sibling input properties this dropdown depends on
	// — purely documentation, same as piece.DropdownProperty.Refreshers and
	// jspiece.DropdownSource.Refreshers; nothing enforces it, LoadOptions
	// decides what it actually reads from propsValue.
	Refreshers []string

	// Source is the dropdown's JS source — see jspiece.DropdownSource.Source
	// for the exact contract.
	Source string

	// Examples are worked PropsValue/SearchValue cases Validate actually runs
	// against this dropdown before GatedStore.Save will persist it — see
	// validate.go. At least one is required by Validate; same reasoning as
	// ActionDefinition.Examples (no engine-enforced way to check a JS
	// dropdown "works" other than running it against cases its own author
	// provides).
	Examples []DropdownExample
}

// TriggerDefinition is one JS-authored trigger, persisted — the symmetric
// counterpart of ActionDefinition for piece.Trigger instead of
// piece.Action. Deliberately has no InputSchema field: a trigger has no
// action-style Input schema in the same sense (piece.TriggerContext.Input
// is untyped map[string]any just like ActionContext.Input, but a trigger's
// "input" is whatever the flow author wired to it, and there's no engine-
// enforced shape to derive a real schema from — the same reason
// ActionDefinition.InputSchema is free text, and a trigger author has even
// less to gain from one). Source is the trigger's JS source — see
// jspiece.TriggerSource.Source for the exact contract.
type TriggerDefinition struct {
	Name        string
	DisplayName string

	// Description is plain-language text describing what this trigger does
	// — same reason ActionDefinition.Description exists: an agent deciding
	// whether to reuse this trigger instead of writing a new one needs
	// something to read.
	Description string

	// Source is the trigger's JS source — see jspiece.TriggerSource.Source
	// for the exact contract.
	Source string

	// Examples are worked Payload/Items cases Validate actually runs
	// against this trigger before GatedStore.Save will persist it — see
	// validate.go. At least one is required by Validate; same reasoning as
	// ActionDefinition.Examples (no engine-enforced way to check a JS
	// trigger "works" other than running it against cases its own author
	// provides).
	Examples []TriggerExample
}

// Definition is a serializable description of one JS-authored piece —
// what a Store persists.
type Definition struct {
	Name        string
	DisplayName string
	Description string
	Actions     []ActionDefinition
	Triggers    []TriggerDefinition
}

// ToPiece converts a Definition into a real piece.Piece, exactly the same
// shape jspiece.New/jspiece.NewTrigger produce directly — a piece loaded
// from a Store is indistinguishable from one built fresh in code.
func (d Definition) ToPiece() piece.Piece {
	sources := make([]jspiece.ActionSource, len(d.Actions))
	for i, a := range d.Actions {
		sources[i] = jspiece.ActionSource{Name: a.Name, DisplayName: a.DisplayName, Source: a.Source}
	}
	p := jspiece.New(d.Name, d.DisplayName, sources)
	// jspiece.New leaves Triggers as a nil map (it only builds Actions);
	// allocate it here so a Definition with Triggers produces a piece whose
	// Triggers map is populated, not one that panics on the first insert.
	if len(d.Triggers) > 0 {
		p.Triggers = make(map[string]piece.Trigger, len(d.Triggers))
		for _, tr := range d.Triggers {
			p.Triggers[tr.Name] = jspiece.NewTrigger(jspiece.TriggerSource{
				Name: tr.Name, DisplayName: tr.DisplayName, Source: tr.Source,
			})
		}
	}
	// jspiece.New builds each piece.Action with Dropdowns=nil — the
	// ActionSource slice above carries no Dropdowns (a Definition's dropdowns
	// hang off each ActionDefinition, not the top-level sources). Attach them
	// here: for every action with a non-empty Dropdowns map, rebuild that
	// action's entry in p.Actions with its dropdowns built via
	// jspiece.NewDropdown. piece.Action values live inside a map, so the
	// entry can't be mutated in place — take the value out, set Dropdowns,
	// put it back, the standard Go pattern for structs stored in maps.
	for _, a := range d.Actions {
		if len(a.Dropdowns) == 0 {
			continue
		}
		act := p.Actions[a.Name]
		act.Dropdowns = make(map[string]piece.DropdownProperty, len(a.Dropdowns))
		for name, dd := range a.Dropdowns {
			act.Dropdowns[name] = jspiece.NewDropdown(jspiece.DropdownSource{
				Refreshers: dd.Refreshers,
				Source:     dd.Source,
			})
		}
		p.Actions[a.Name] = act
	}
	return p
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
