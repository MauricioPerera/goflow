package catalog

import (
	"goflow/pkg/piece"
	"goflow/pkg/pieces"
)

// BuildRegistry assembles a fresh *piece.Registry from every built-in Go
// piece (pieces.All()) plus every Definition store currently holds —
// exactly what pkg/httpapi's Server.buildRegistry already did inline before
// this, extracted here so pkg/scheduler (which needs to validate/run a
// schedule-triggered flow against the exact same registry every other
// transport uses) doesn't have to duplicate the same two-loop assembly —
// the same "one shared path" discipline pkg/flowstore's Run/
// RunWithCredentials already apply across HTTP and MCP.
//
// Called fresh every time, never cached: a piece saved between two calls
// must be visible to the next one without a process restart, same reasoning
// Server.buildRegistry's own doc comment already gave.
func BuildRegistry(store Store) (*piece.Registry, error) {
	reg := piece.NewRegistry()
	for _, p := range pieces.All() {
		reg.Register(p)
	}
	defs, err := store.List()
	if err != nil {
		return nil, err
	}
	for _, def := range defs {
		reg.Register(def.ToPiece())
	}
	return reg, nil
}
