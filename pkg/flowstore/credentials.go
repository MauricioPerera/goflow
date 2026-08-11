package flowstore

import (
	"fmt"

	"goflow/pkg/credentials"
	"goflow/pkg/engine"
	"goflow/pkg/flowvalidate"
	"goflow/pkg/model"
	"goflow/pkg/piece"
)

// credentialRefKey is the single JSON key that marks an Input value as a
// reference to a stored credential rather than a literal. A value is a
// credential reference iff, as decoded JSON, it is a map[string]any with
// EXACTLY this one key whose value is a non-empty string — the credential's
// name. Any other shape (more keys, non-string value, empty string) is left
// completely untouched, so a flow's Input never has to be aware of this
// convention unless it opts in.
const credentialRefKey = "$credential"

// CredentialRef records one place in a flow where a stored credential was
// substituted in for its $credential marker — the StepName whose Input held
// the marker, the InputKey within that step's Input map, and the
// CredentialName that was resolved there. RunWithCredentials collects these
// while resolving, then hands them to RedactCredentials after the run so each
// substituted value can be replaced with a placeholder before the
// ExecutionState leaves the process over HTTP/MCP.
type CredentialRef struct {
	StepName       string
	InputKey       string
	CredentialName string
}

// ResolveCredentials returns a deep-enough COPY of fv in which every
// $credential marker has been replaced by the credential's real decrypted
// value (so the piece actually receives the secret at run time), plus the
// list of places it substituted. The fv passed in is NEVER mutated — neither
// its Trigger nor any FlowAction nor any Input map — because a flow handed to
// this function may be a saved definition or an HTTP request body that other
// code still holds a reference to. Only the tree nodes and their Input maps
// are copied; non-map values inside an Input map are shared, which is fine
// since the only mutation is replacing one map entry.
//
// The tree is walked with the same shape flowvalidate.walker uses — the
// trigger's Input (when Type==TriggerPiece), then every action reachable via
// NextAction, with Router.Children and Loop.FirstLoopAction descending
// recursively — so no branch is missed.
func ResolveCredentials(fv *model.FlowVersion, store credentials.Store) (resolved *model.FlowVersion, refs []CredentialRef, err error) {
	if fv == nil {
		return nil, nil, fmt.Errorf("flowstore: flow is nil")
	}

	out := *fv // shallow copy of the FlowVersion struct itself

	if fv.Trigger != nil {
		tCopy := *fv.Trigger
		// Only a PIECE_TRIGGER's Input is consumed by the engine (ExecuteBegin
		// reads fv.Trigger.Input solely in the ExecuteTrigger && TriggerPiece
		// branch); an EMPTY trigger's Input is never used, so leave it alone.
		if fv.Trigger.Type == model.TriggerPiece && len(fv.Trigger.Input) > 0 {
			resolvedInput, rRefs, rErr := resolveInputMap(fv.Trigger.Name, fv.Trigger.Input, store)
			if rErr != nil {
				return nil, nil, rErr
			}
			tCopy.Input = resolvedInput
			refs = append(refs, rRefs...)
		}
		out.Trigger = &tCopy

		visited := map[*model.FlowAction]*model.FlowAction{}
		copied, aRefs, aErr := copyAndResolveAction(fv.Trigger.NextAction, visited, store)
		if aErr != nil {
			return nil, nil, aErr
		}
		out.Trigger.NextAction = copied
		refs = append(refs, aRefs...)
	}

	return &out, refs, nil
}

// copyAndResolveAction returns a deep-enough copy of the action subtree rooted
// at a (NextAction chain, Router.Children, Loop.FirstLoopAction all copied
// recursively) with every $credential marker in each visited Input map
// resolved. visited maps each ORIGINAL action pointer to its COPY so a shared
// or cyclic chain is traversed once rather than infinitely — flowvalidate
// would reject a cycle before Run, but ResolveCredentials runs before
// flowvalidate in RunWithCredentials, so this guard keeps a malformed
// (cyclic) flow from hanging resolution.
func copyAndResolveAction(a *model.FlowAction, visited map[*model.FlowAction]*model.FlowAction, store credentials.Store) (*model.FlowAction, []CredentialRef, error) {
	if a == nil {
		return nil, nil, nil
	}
	if cp, ok := visited[a]; ok {
		return cp, nil, nil
	}

	node := *a // shallow copy of the FlowAction struct
	cp := &node
	visited[a] = cp
	var refs []CredentialRef

	switch a.Type {
	case model.ActionCode:
		if a.Code != nil {
			codeCopy := *a.Code
			if len(a.Code.Input) > 0 {
				resolvedInput, rRefs, rErr := resolveInputMap(a.Name, a.Code.Input, store)
				if rErr != nil {
					return nil, nil, rErr
				}
				codeCopy.Input = resolvedInput
				refs = append(refs, rRefs...)
			}
			cp.Code = &codeCopy
		}

	case model.ActionPiece:
		if a.Piece != nil {
			pieceCopy := *a.Piece
			if len(a.Piece.Input) > 0 {
				resolvedInput, rRefs, rErr := resolveInputMap(a.Name, a.Piece.Input, store)
				if rErr != nil {
					return nil, nil, rErr
				}
				pieceCopy.Input = resolvedInput
				refs = append(refs, rRefs...)
			}
			cp.Piece = &pieceCopy
		}

	case model.ActionRouter:
		if a.Router != nil {
			routerCopy := *a.Router
			childrenCopy := make([]*model.FlowAction, len(a.Router.Children))
			for i, child := range a.Router.Children {
				c, cRefs, cErr := copyAndResolveAction(child, visited, store)
				if cErr != nil {
					return nil, nil, cErr
				}
				childrenCopy[i] = c
				refs = append(refs, cRefs...)
			}
			routerCopy.Children = childrenCopy
			cp.Router = &routerCopy
		}

	case model.ActionLoopOnItems:
		if a.Loop != nil {
			loopCopy := *a.Loop
			c, cRefs, cErr := copyAndResolveAction(a.Loop.FirstLoopAction, visited, store)
			if cErr != nil {
				return nil, nil, cErr
			}
			loopCopy.FirstLoopAction = c
			refs = append(refs, cRefs...)
			cp.Loop = &loopCopy
		}
	}

	next, nRefs, nErr := copyAndResolveAction(a.NextAction, visited, store)
	if nErr != nil {
		return nil, nil, nErr
	}
	cp.NextAction = next
	refs = append(refs, nRefs...)

	return cp, refs, nil
}

// resolveInputMap returns a copy of in with every $credential marker replaced
// by the credential's real value. The original in is not touched. A marker
// only matches a top-level value of the Input map (mirroring how the engine
// consumes Input); nested maps inside a value are not inspected, so anything
// that isn't an exact one-key {$credential: name} marker is left intact.
func resolveInputMap(stepName string, in map[string]any, store credentials.Store) (map[string]any, []CredentialRef, error) {
	out := make(map[string]any, len(in))
	var refs []CredentialRef
	for k, v := range in {
		if name, ok := credentialMarker(v); ok {
			if store == nil {
				return nil, nil, fmt.Errorf(
					"flowstore: flow references credential %q (step %q, input key %q) but no credential store is configured",
					name, stepName, k)
			}
			val, ok, err := store.Get(name)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"flowstore: resolving credential %q (step %q, input key %q): %w",
					name, stepName, k, err)
			}
			if !ok {
				return nil, nil, fmt.Errorf(
					"flowstore: step %q references credential %q in input key %q but it is not stored",
					stepName, name, k)
			}
			out[k] = val
			refs = append(refs, CredentialRef{StepName: stepName, InputKey: k, CredentialName: name})
			continue
		}
		out[k] = v
	}
	return out, refs, nil
}

// credentialMarker reports whether v is a $credential reference and, if so,
// returns the credential name. v is a reference iff it is a map[string]any
// with exactly one key, credentialRefKey, whose value is a non-empty string.
func credentialMarker(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	if len(m) != 1 {
		return "", false
	}
	name, ok := m[credentialRefKey].(string)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// RedactCredentials replaces, in place on state, every Input value that
// ResolveCredentials substituted, with the placeholder string
// "<credential:NAME>". This is the second half of the two-step design: the
// engine records each step's already-resolved Input (including the real
// secret) in ExecutionState.Steps, and that is exactly what the HTTP and MCP
// layers serialize back — so without redaction the secret would leak in the
// response. Redacting after the run, on the real result, is the only thing
// that closes that gap without touching pkg/engine.
//
// The same StepName can appear many times: an action inside a LOOP_ON_ITEMS
// is recorded once per iteration, in that iteration's own Steps map, and
// iterations can nest further loops. So every level is walked — the top-level
// Steps map, then recursively every step's Iterations — and every appearance
// of each ref is redacted.
func RedactCredentials(state *model.ExecutionState, refs []CredentialRef) {
	if state == nil || len(refs) == 0 {
		return
	}
	redactSteps(state.Steps, refs)
}

// redactSteps redacts the matching Input entries in one Steps map, then
// recurses into every step's Iterations (regardless of which ref matched — a
// loop body action is not present at the level that holds the loop step, so
// the recursion must not be gated on the ref's StepName being found here).
func redactSteps(steps map[string]*model.StepOutput, refs []CredentialRef) {
	for _, ref := range refs {
		step, ok := steps[ref.StepName]
		if !ok {
			continue
		}
		if in, ok := step.Input.(map[string]any); ok {
			if _, exists := in[ref.InputKey]; exists {
				in[ref.InputKey] = "<credential:" + ref.CredentialName + ">"
			}
		}
	}
	for _, step := range steps {
		for _, iter := range step.Iterations {
			redactSteps(iter, refs)
		}
	}
}

// RunWithCredentials is the credential-aware replacement for Run: it resolves
// every $credential marker in fv against credStore BEFORE running (so the
// pieces receive the real secrets), runs the flow through the exact same
// validate-then-execute path Run uses (Run itself is unchanged), and
// redacts every substituted value on the returned state BEFORE returning it
// (so the secret never reaches the HTTP/MCP response). The three return
// values keep Run's contract:
//   - err != nil: a buildRegistry failure (server fault), propagated verbatim.
//   - validationErrs non-empty: the flow can't run as configured — including
//     a missing/unresolvable credential, which is wrapped as a single
//     ValidationError{Path:"credentials"} here. This is NOT an err: a flow
//     that references a credential that isn't stored is a caller-supplied
//     configuration problem, the same category as referencing a piece that
//     doesn't exist, not a server fault.
//   - state non-nil: the flow ran, with every credential redacted on it.
//
// callFlow is passed straight through to Run unchanged — see that
// function's own doc comment for why RunWithCredentials itself builds no
// actual sub-flow lookup/execution logic (RunWithHistory does).
func RunWithCredentials(fv *model.FlowVersion, buildRegistry func() (*piece.Registry, error), credStore credentials.Store, callFlow engine.CallFlowFunc, trigger any, executeTrigger bool) (state *model.ExecutionState, validationErrs []flowvalidate.ValidationError, err error) {
	resolved, refs, err := ResolveCredentials(fv, credStore)
	if err != nil {
		return nil, []flowvalidate.ValidationError{{Path: "credentials", Message: err.Error()}}, nil
	}
	state, validationErrs, err = Run(resolved, buildRegistry, callFlow, trigger, executeTrigger)
	if err != nil {
		return nil, validationErrs, err
	}
	if state != nil {
		RedactCredentials(state, refs)
	}
	return state, validationErrs, nil
}
