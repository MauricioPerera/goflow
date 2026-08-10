package flowstore

import (
	"fmt"
	"strings"

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
}

// Save builds a fresh registry, runs flowvalidate.Validate against def.Flow,
// and — if it passes — delegates to Underlying.Save. A BuildRegistry failure
// is propagated wrapped in context (never reaches Validate or Underlying). A
// validation failure is rejected without calling Underlying.Save, with an
// error whose message is the formatted list of every problem found.
func (s *GatedStore) Save(def FlowDefinition) error {
	registry, err := s.BuildRegistry()
	if err != nil {
		return fmt.Errorf("flowstore: building registry for %q: %w", def.Name, err)
	}
	if errs := flowvalidate.Validate(&def.Flow, registry); len(errs) > 0 {
		return fmt.Errorf("flowstore: flow %q failed validation: %s", def.Name, FormatValidationErrors(errs))
	}
	return s.Underlying.Save(def)
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
