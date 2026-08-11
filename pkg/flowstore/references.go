package flowstore

import (
	"sort"

	"goflow/pkg/model"
)

// FindFlowsReferencingCredential returns the names of every saved flow
// that references credentialName — via FlowDefinition.WebhookSecretCredential,
// or a {"$credential": credentialName} marker anywhere in a CODE/PIECE/
// CALL_FLOW action's Input, or a PIECE_TRIGGER's own Input. Sorted by name.
//
// Read-only and proactive, NOT a gate: nothing here blocks deleting
// credentialName — same "delete needs no gate" philosophy every other
// Delete in this project already documents (a removal can't fail a
// quality check the way a save can). This exists so a caller can check
// what depends on a credential BEFORE deciding to delete it, the same
// "give context, don't force a workflow" reasoning
// catalog.ActionDefinition.RequiresAuth already applies elsewhere.
//
// Deliberately more thorough than ResolveCredentials' own walk: that one
// never visits a CALL_FLOW action's Input at all (it becomes the SUB-flow's
// own trigger payload — never itself scanned for markers, on any run, by
// design), so a $credential marker placed there is currently a latent,
// unresolved reference at runtime — a real, separate gap this function
// does not fix, only surfaces: it still counts as a reference here, since
// "would this credential's removal affect this flow" is the actual
// question being answered, independent of whether resolution works today.
func FindFlowsReferencingCredential(store Store, credentialName string) ([]string, error) {
	defs, err := store.List()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, def := range defs {
		if def.WebhookSecretCredential == credentialName {
			names = append(names, def.Name)
			continue
		}
		if flowReferencesCredential(def.Flow, credentialName) {
			names = append(names, def.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func flowReferencesCredential(fv model.FlowVersion, credentialName string) bool {
	if fv.Trigger == nil {
		return false
	}
	if fv.Trigger.Type == model.TriggerPiece && anyValueReferencesCredential(fv.Trigger.Input, credentialName) {
		return true
	}
	found := false
	walkActions(fv.Trigger.NextAction, func(a *model.FlowAction) {
		if found {
			return
		}
		var input map[string]any
		switch a.Type {
		case model.ActionCode:
			if a.Code != nil {
				input = a.Code.Input
			}
		case model.ActionPiece:
			if a.Piece != nil {
				input = a.Piece.Input
			}
		case model.ActionCallFlow:
			if a.CallFlow != nil {
				input = a.CallFlow.Input
			}
		}
		if anyValueReferencesCredential(input, credentialName) {
			found = true
		}
	})
	return found
}

// FindFlowsReferencingPiece returns the names of every saved flow that
// references pieceName — a PIECE action's PieceName, or a PIECE_TRIGGER's
// own PieceName. Sorted by name. Same read-only, proactive, non-blocking
// reasoning as FindFlowsReferencingCredential — see its own doc comment.
func FindFlowsReferencingPiece(store Store, pieceName string) ([]string, error) {
	defs, err := store.List()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, def := range defs {
		if flowReferencesPiece(def.Flow, pieceName) {
			names = append(names, def.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func flowReferencesPiece(fv model.FlowVersion, pieceName string) bool {
	if fv.Trigger == nil {
		return false
	}
	if fv.Trigger.Type == model.TriggerPiece && fv.Trigger.PieceName == pieceName {
		return true
	}
	found := false
	walkActions(fv.Trigger.NextAction, func(a *model.FlowAction) {
		if found {
			return
		}
		if a.Type == model.ActionPiece && a.Piece != nil && a.Piece.PieceName == pieceName {
			found = true
		}
	})
	return found
}

// walkActions calls visit for every action reachable from start — via
// NextAction, Router.Children, and Loop.FirstLoopAction, the same three
// edges flowvalidate.walkChain and credentials.go's copyAndResolveAction
// both already recurse through. CALL_FLOW is a leaf here too (mirroring
// flowvalidate's own choice): it targets a SEPARATE saved flow, not an
// inline subtree, so there's nothing further to walk INTO from it — but
// its own Input is still visited via visit like any other action's.
// Cycle-safe via a visited set, same protection the other two walkers use.
func walkActions(start *model.FlowAction, visit func(*model.FlowAction)) {
	visited := map[*model.FlowAction]bool{}
	var walk func(*model.FlowAction)
	walk = func(a *model.FlowAction) {
		for a != nil {
			if visited[a] {
				return
			}
			visited[a] = true
			visit(a)
			if a.Type == model.ActionRouter && a.Router != nil {
				for _, child := range a.Router.Children {
					walk(child)
				}
			}
			if a.Type == model.ActionLoopOnItems && a.Loop != nil {
				walk(a.Loop.FirstLoopAction)
			}
			a = a.NextAction
		}
	}
	walk(start)
}

// referencesCredential reports whether m is exactly a
// {"$credential": credentialName} marker — the same shape
// credentials.go's own credentialMarker helper detects, duplicated here
// (four lines, no state) rather than exported cross-file for something
// this small, matching this project's own precedent for tiny shared
// helpers (e.g. goCatalogMap duplicated between pkg/httpapi/pkg/mcpapi).
func referencesCredential(m map[string]any, credentialName string) bool {
	if len(m) != 1 {
		return false
	}
	v, ok := m["$credential"]
	if !ok {
		return false
	}
	s, ok := v.(string)
	return ok && s == credentialName
}

// anyValueReferencesCredential walks an arbitrary Input value — a
// map[string]any/[]any tree, the same untyped shape every action's Input
// already is — at ANY nesting depth, looking for a
// {"$credential": credentialName} marker. Deliberately deeper than
// ResolveCredentials' own top-level-only scan: a false negative here
// (missing a real reference) is worse than the extra thoroughness costs,
// since the whole point is answering "would removing this credential
// affect this flow."
func anyValueReferencesCredential(v any, credentialName string) bool {
	switch val := v.(type) {
	case map[string]any:
		if referencesCredential(val, credentialName) {
			return true
		}
		for _, inner := range val {
			if anyValueReferencesCredential(inner, credentialName) {
				return true
			}
		}
	case []any:
		for _, inner := range val {
			if anyValueReferencesCredential(inner, credentialName) {
				return true
			}
		}
	}
	return false
}
