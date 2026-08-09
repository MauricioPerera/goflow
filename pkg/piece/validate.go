package piece

import "fmt"

// ValidationError describes one problem found in a Piece definition.
type ValidationError struct {
	Path    string // e.g. `Actions["send_message"]` or `Actions["x"].Dropdowns["channel"]`
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Message)
}

// Validate checks a Piece definition for structural mistakes that would
// otherwise only surface as a runtime panic or a silent, confusing
// misbehavior — exactly the class of error found repeatedly while
// hand-authoring pieces throughout this project's own test suite (a nil Run
// panics the instant the engine calls it; an Action.Name that doesn't match
// its own map key doesn't break anything TODAY — GetAction looks up by map
// key, never by the Name field — but is precisely the kind of inconsistency
// an agent generating a piece on the fly, with no human reviewing the
// struct literal, is likely to introduce). Call this right after building a
// Piece and before Registry.Register — it never mutates anything and has no
// side effects, so it's cheap to call unconditionally.
//
// Deliberately narrow: this checks STRUCTURE (nil funcs, name/key
// consistency, empty required strings), not BEHAVIOR — it cannot tell you
// whether an Action's Run function is actually correct, only that calling
// it won't panic on nil. See the goflow-piece-authoring skill for the
// broader checklist of behavioral pitfalls (auth handling, templating
// pass-through, retry semantics) this validator does not and cannot check.
func Validate(p Piece) []ValidationError {
	var errs []ValidationError

	if p.Name == "" {
		errs = append(errs, ValidationError{Path: "Piece", Message: "Name is empty"})
	}
	if p.DisplayName == "" {
		errs = append(errs, ValidationError{Path: "Piece", Message: "DisplayName is empty"})
	}

	for key, action := range p.Actions {
		path := fmt.Sprintf("Actions[%q]", key)
		if action.Name == "" {
			errs = append(errs, ValidationError{Path: path, Message: "Action.Name is empty"})
		} else if action.Name != key {
			errs = append(errs, ValidationError{Path: path, Message: fmt.Sprintf(
				"Action.Name %q does not match its registry key %q — GetAction resolves by key, not Name, so this is confusing rather than broken, but it's exactly the kind of drift worth catching before it does matter",
				action.Name, key)})
		}
		if action.Run == nil {
			errs = append(errs, ValidationError{Path: path, Message: "Run is nil — calling this action will panic"})
		}
		for propName, dd := range action.Dropdowns {
			ddPath := fmt.Sprintf("%s.Dropdowns[%q]", path, propName)
			if dd.LoadOptions == nil {
				errs = append(errs, ValidationError{Path: ddPath, Message: "LoadOptions is nil — Engine.LoadOptions will panic calling this"})
			}
		}
	}

	for key, trig := range p.Triggers {
		path := fmt.Sprintf("Triggers[%q]", key)
		if trig.Name == "" {
			errs = append(errs, ValidationError{Path: path, Message: "Trigger.Name is empty"})
		} else if trig.Name != key {
			errs = append(errs, ValidationError{Path: path, Message: fmt.Sprintf(
				"Trigger.Name %q does not match its registry key %q — GetTrigger resolves by key, not Name",
				trig.Name, key)})
		}
		if trig.Run == nil {
			errs = append(errs, ValidationError{Path: path, Message: "Run is nil — calling this trigger will panic"})
		}
	}

	return errs
}

// MustValidate calls Validate and panics if any errors are found — a
// fail-fast convenience for call sites where a validation failure means the
// calling code itself is wrong (tests, init-time registration in code
// that's supposed to already be correct by the time it runs). Prefer
// Validate directly wherever you want to handle or report errors yourself
// instead of panicking — an agent registering pieces generated at runtime
// should almost always do that, not call MustValidate.
func MustValidate(p Piece) {
	if errs := Validate(p); len(errs) > 0 {
		msg := fmt.Sprintf("piece %q failed validation (%d issue(s)):", p.Name, len(errs))
		for _, e := range errs {
			msg += "\n  - " + e.Error()
		}
		panic(msg)
	}
}

// RegisterValidated validates p and, if it passes, registers it — a
// validate-then-register convenience that returns an error instead of
// requiring the caller to call Validate separately (or panicking, unlike
// MustValidate). This is the call an agentic workflow generating pieces at
// runtime should reach for: never registers a structurally broken piece,
// never panics doing it.
func (r *Registry) RegisterValidated(p Piece) error {
	if errs := Validate(p); len(errs) > 0 {
		msg := fmt.Sprintf("piece %q failed validation (%d issue(s)):", p.Name, len(errs))
		for _, e := range errs {
			msg += "\n  - " + e.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	r.Register(p)
	return nil
}
