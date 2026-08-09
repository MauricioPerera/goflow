package catalog

import "fmt"

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
}

func (s *GatedStore) Save(def Definition) error {
	if errs := Validate(def); len(errs) > 0 {
		return fmt.Errorf("catalog: definition %q failed validation: %s", def.Name, FormatValidationErrors(errs))
	}
	return s.Underlying.Save(def)
}

func (s *GatedStore) Get(name string) (Definition, bool, error) {
	return s.Underlying.Get(name)
}

func (s *GatedStore) List() ([]Definition, error) {
	return s.Underlying.List()
}
