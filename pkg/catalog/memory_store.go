package catalog

import "sync"

// MemoryStore is a Store backed by a plain, mutex-guarded map — mirrors
// piece.MemoryStore/piece.MemoryFileWriter's "simple default, real
// persistence is the caller's choice" convention. Dies with the process,
// same as any in-memory store elsewhere in this project; use FileStore
// (or your own Store) when a Definition needs to survive a restart.
type MemoryStore struct {
	mu   sync.Mutex
	defs map[string]Definition
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{defs: map[string]Definition{}}
}

func (s *MemoryStore) Save(def Definition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defs[def.Name] = def
	return nil
}

func (s *MemoryStore) Get(name string) (Definition, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	def, ok := s.defs[name]
	return def, ok, nil
}

func (s *MemoryStore) List() ([]Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Definition, 0, len(s.defs))
	for _, def := range s.defs {
		out = append(out, def)
	}
	return out, nil
}
