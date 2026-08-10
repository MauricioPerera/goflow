// Package flowvalidate checks a *model.FlowVersion for problems that would
// otherwise only surface mid-run — some as a normal FAILED step (a missing
// piece action), some far worse (a cycle in the NextAction chain hangs
// Engine.ExecuteBegin's executeChain loop forever, with no recovery).
// Closes the last of the four gaps identified while planning goflow's
// AI-first direction: a flow built from Phase 1's JSON data, potentially
// entirely agent-authored, was never checked for structural soundness
// before its first real run — which matters when that run has real
// side effects (money, files, outbound requests).
//
// Deliberately structural + syntactic, not semantic: this does NOT try to
// prove a {{ }} expression will resolve correctly at runtime (that would
// mean simulating execution, including piece outputs it can't know ahead
// of time) — only that every expression PARSES as valid JS via
// goja.Compile, which never executes anything. Whether
// "{{ step_1.output.foo }}" actually resolves depends on step_1's real
// Output shape once it runs; a bad reference still surfaces as a normal,
// clear runtime error (expr.Eval already wraps goja's ReferenceError
// cleanly) — duplicating that check here would mean either re-guessing at
// runtime values or producing false positives on expressions this package
// genuinely cannot evaluate ahead of time.
package flowvalidate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dop251/goja"

	"goflow/pkg/model"
	"goflow/pkg/piece"
)

// ValidationError is one problem Validate found — same {Path, Message}
// shape as piece.ValidationError and catalog.ValidationError, deliberately,
// for a consistent error style across every validator in this project.
type ValidationError struct {
	Path    string
	Message string
}

var anyTemplate = regexp.MustCompile(`\{\{(.*?)\}\}`)

// Validate walks every reachable step in fv (trigger, then every action
// reachable via NextAction, ROUTER Children, and LOOP_ON_ITEMS
// FirstLoopAction) and reports every problem found — it does not stop at
// the first one. registry is used to confirm every referenced piece
// action/trigger actually exists; pass nil to skip those specific checks
// (useful for validating a flow's shape before a registry is even
// assembled).
func Validate(fv *model.FlowVersion, registry *piece.Registry) []ValidationError {
	var errs []ValidationError

	if fv.Trigger == nil {
		return []ValidationError{{Path: "trigger", Message: "flow has no trigger"}}
	}

	if fv.Trigger.Type == model.TriggerPiece && registry != nil {
		if _, ok := registry.GetTrigger(fv.Trigger.PieceName, fv.Trigger.TriggerName); !ok {
			errs = append(errs, ValidationError{
				Path:    "trigger",
				Message: fmt.Sprintf("piece trigger %q.%q not found in registry", fv.Trigger.PieceName, fv.Trigger.TriggerName),
			})
		}
	}
	errs = append(errs, checkTemplatesInMap("trigger.input", fv.Trigger.Input)...)

	names := map[string]int{fv.Trigger.Name: 1}
	w := &walker{registry: registry, names: names}
	w.walkChain("trigger", fv.Trigger.NextAction)
	errs = append(errs, w.errs...)

	for name, count := range names {
		if count > 1 {
			errs = append(errs, ValidationError{
				Path:    fmt.Sprintf("steps[%s]", name),
				Message: fmt.Sprintf("step name %q used %d times — duplicate step names silently overwrite each other's recorded output in ExecutionState.Steps", name, count),
			})
		}
	}

	return errs
}

type walker struct {
	registry *piece.Registry
	names    map[string]int
	errs     []ValidationError
}

// walkChain follows one independent NextAction chain — the trigger's, one
// ROUTER branch's, or one LOOP_ON_ITEMS body's. Each gets its own fresh
// `visited` set: a cycle only actually hangs the engine if it loops back
// WITHIN the same chain (Engine.executeChain's `for action != nil` loop is
// scoped exactly this way — see pkg/engine/engine.go), so chains don't
// share cycle-detection state with each other.
func (w *walker) walkChain(path string, action *model.FlowAction) {
	visited := map[*model.FlowAction]bool{}
	for action != nil {
		if visited[action] {
			w.errs = append(w.errs, ValidationError{
				Path:    path,
				Message: fmt.Sprintf("cycle detected: step %q's NextAction chain loops back on itself — this would hang the engine forever", action.Name),
			})
			return
		}
		visited[action] = true
		w.names[action.Name]++

		actionPath := fmt.Sprintf("%s > %s", path, action.Name)
		w.checkAction(actionPath, action)

		action = action.NextAction
	}
}

func (w *walker) checkAction(path string, action *model.FlowAction) {
	switch action.Type {
	case model.ActionCode:
		if action.Code == nil {
			w.errs = append(w.errs, ValidationError{Path: path, Message: "type is CODE but Code settings is nil"})
			return
		}
		w.checkJSSyntax(path+".code.source", "("+action.Code.Source+")")
		w.errs = append(w.errs, checkTemplatesInMap(path+".code.input", action.Code.Input)...)

	case model.ActionPiece:
		if action.Piece == nil {
			w.errs = append(w.errs, ValidationError{Path: path, Message: "type is PIECE but Piece settings is nil"})
			return
		}
		if w.registry != nil {
			if _, ok := w.registry.GetAction(action.Piece.PieceName, action.Piece.ActionName); !ok {
				w.errs = append(w.errs, ValidationError{
					Path:    path,
					Message: fmt.Sprintf("piece action %q.%q not found in registry", action.Piece.PieceName, action.Piece.ActionName),
				})
			}
		}
		w.errs = append(w.errs, checkTemplatesInMap(path+".piece.input", action.Piece.Input)...)

	case model.ActionRouter:
		if action.Router == nil {
			w.errs = append(w.errs, ValidationError{Path: path, Message: "type is ROUTER but Router settings is nil"})
			return
		}
		if len(action.Router.Children) != len(action.Router.Branches) {
			w.errs = append(w.errs, ValidationError{
				Path: path,
				Message: fmt.Sprintf("Router.Children has %d entries but Router.Branches has %d — they must be index-aligned",
					len(action.Router.Children), len(action.Router.Branches)),
			})
		}
		for i, branch := range action.Router.Branches {
			if branch.Type == model.BranchCondition && len(branch.Conditions) == 0 {
				w.errs = append(w.errs, ValidationError{
					Path:    fmt.Sprintf("%s.router.branches[%d]", path, i),
					Message: fmt.Sprintf("branch %q is type CONDITION but has no Conditions — an empty condition group never matches in the engine (evaluateConditionGroups returns false for no groups), so this branch can never run; use type FALLBACK for a catch-all, or add at least one condition group", branch.Name),
				})
			}
			for g, group := range branch.Conditions {
				for c, cond := range group {
					condPath := fmt.Sprintf("%s.router.branches[%d].conditions[%d][%d]", path, i, g, c)
					w.checkTemplateString(condPath+".firstValue", cond.FirstValue)
					w.checkTemplateString(condPath+".secondValue", cond.SecondValue)
				}
			}
		}
		for i, child := range action.Router.Children {
			w.walkChain(fmt.Sprintf("%s.router.children[%d]", path, i), child)
		}

	case model.ActionLoopOnItems:
		if action.Loop == nil {
			w.errs = append(w.errs, ValidationError{Path: path, Message: "type is LOOP_ON_ITEMS but Loop settings is nil"})
			return
		}
		// Items is a raw expression (not string-templated — see the
		// goflow-piece-authoring skill's own note on this), trimmed the
		// same way engine.go's trimTemplate does before compiling it.
		w.checkJSSyntax(path+".loop.items", trimTemplate(action.Loop.Items))
		w.walkChain(path+".loop.firstLoopAction", action.Loop.FirstLoopAction)

	default:
		w.errs = append(w.errs, ValidationError{Path: path, Message: fmt.Sprintf("unknown action type %q", action.Type)})
	}
}

// checkJSSyntax confirms src parses as valid JS via goja.Compile, which
// creates an internal representation WITHOUT running any of it — no
// side effects, no risk of hanging on adversarial input, unlike actually
// evaluating the expression would be.
func (w *walker) checkJSSyntax(path, src string) {
	if strings.TrimSpace(src) == "" {
		return
	}
	if _, err := goja.Compile("", src, false); err != nil {
		w.errs = append(w.errs, ValidationError{Path: path, Message: fmt.Sprintf("invalid JS syntax: %v", err)})
	}
}

func (w *walker) checkTemplateString(path, s string) {
	for _, match := range anyTemplate.FindAllStringSubmatch(s, -1) {
		w.checkJSSyntax(path, strings.TrimSpace(match[1]))
	}
}

func checkTemplatesInMap(path string, input map[string]any) []ValidationError {
	w := &walker{}
	w.checkTemplatesInValue(path, input)
	return w.errs
}

func (w *walker) checkTemplatesInValue(path string, v any) {
	switch val := v.(type) {
	case string:
		w.checkTemplateString(path, val)
	case map[string]any:
		for k, vv := range val {
			w.checkTemplatesInValue(path+"."+k, vv)
		}
	case []any:
		for i, vv := range val {
			w.checkTemplatesInValue(fmt.Sprintf("%s[%d]", path, i), vv)
		}
	}
}

func trimTemplate(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{{")
	s = strings.TrimSuffix(s, "}}")
	return strings.TrimSpace(s)
}
