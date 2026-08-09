package piece_test

import (
	"strings"
	"testing"

	"goflow/pkg/piece"
)

func validAction() piece.Action {
	return piece.Action{
		Name: "do_thing", DisplayName: "Do Thing",
		Run: func(ctx piece.ActionContext) (any, error) { return nil, nil },
	}
}

func validTrigger() piece.Trigger {
	return piece.Trigger{
		Name: "on_thing", DisplayName: "On Thing",
		Run: func(ctx piece.TriggerContext) ([]any, error) { return nil, nil },
	}
}

func TestValidate_ValidPiecePassesWithNoErrors(t *testing.T) {
	p := piece.Piece{
		Name: "demo", DisplayName: "Demo",
		Actions:  map[string]piece.Action{"do_thing": validAction()},
		Triggers: map[string]piece.Trigger{"on_thing": validTrigger()},
	}
	if errs := piece.Validate(p); len(errs) != 0 {
		t.Fatalf("errs = %+v, want none", errs)
	}
}

func TestValidate_CatchesEachKindOfMistake(t *testing.T) {
	cases := []struct {
		name    string
		build   func() piece.Piece
		wantHit string // substring expected somewhere in the joined error messages
	}{
		{
			name: "empty piece name",
			build: func() piece.Piece {
				return piece.Piece{DisplayName: "Demo"}
			},
			wantHit: "Name is empty",
		},
		{
			name: "empty piece display name",
			build: func() piece.Piece {
				return piece.Piece{Name: "demo"}
			},
			wantHit: "DisplayName is empty",
		},
		{
			name: "action Name does not match its registry key",
			build: func() piece.Piece {
				a := validAction()
				a.Name = "wrong_name"
				return piece.Piece{Name: "demo", DisplayName: "Demo", Actions: map[string]piece.Action{"do_thing": a}}
			},
			wantHit: `does not match its registry key "do_thing"`,
		},
		{
			name: "action with nil Run",
			build: func() piece.Piece {
				a := validAction()
				a.Run = nil
				return piece.Piece{Name: "demo", DisplayName: "Demo", Actions: map[string]piece.Action{"do_thing": a}}
			},
			wantHit: "Run is nil",
		},
		{
			name: "dropdown with nil LoadOptions",
			build: func() piece.Piece {
				a := validAction()
				a.Dropdowns = map[string]piece.DropdownProperty{"channel": {Refreshers: []string{}}}
				return piece.Piece{Name: "demo", DisplayName: "Demo", Actions: map[string]piece.Action{"do_thing": a}}
			},
			wantHit: `Dropdowns["channel"]`,
		},
		{
			name: "trigger Name does not match its registry key",
			build: func() piece.Piece {
				tr := validTrigger()
				tr.Name = "wrong_name"
				return piece.Piece{Name: "demo", DisplayName: "Demo", Triggers: map[string]piece.Trigger{"on_thing": tr}}
			},
			wantHit: `does not match its registry key "on_thing"`,
		},
		{
			name: "trigger with nil Run",
			build: func() piece.Piece {
				tr := validTrigger()
				tr.Run = nil
				return piece.Piece{Name: "demo", DisplayName: "Demo", Triggers: map[string]piece.Trigger{"on_thing": tr}}
			},
			wantHit: "Run is nil",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := piece.Validate(c.build())
			if len(errs) == 0 {
				t.Fatal("errs is empty, want at least one issue")
			}
			joined := ""
			for _, e := range errs {
				joined += e.Error() + "\n"
			}
			if !strings.Contains(joined, c.wantHit) {
				t.Fatalf("errors = %q, want a message containing %q", joined, c.wantHit)
			}
		})
	}
}

func TestMustValidate_PanicsOnInvalidPiece(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustValidate did not panic on an invalid piece")
		}
	}()
	piece.MustValidate(piece.Piece{}) // missing Name and DisplayName
}

func TestMustValidate_DoesNotPanicOnValidPiece(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("MustValidate panicked on a valid piece: %v", r)
		}
	}()
	piece.MustValidate(piece.Piece{
		Name: "demo", DisplayName: "Demo",
		Actions: map[string]piece.Action{"do_thing": validAction()},
	})
}

func TestRegistry_RegisterValidated(t *testing.T) {
	t.Run("valid piece registers and becomes resolvable", func(t *testing.T) {
		r := piece.NewRegistry()
		p := piece.Piece{Name: "demo", DisplayName: "Demo", Actions: map[string]piece.Action{"do_thing": validAction()}}
		if err := r.RegisterValidated(p); err != nil {
			t.Fatalf("RegisterValidated: %v", err)
		}
		if _, ok := r.GetAction("demo", "do_thing"); !ok {
			t.Fatal("action not resolvable after RegisterValidated")
		}
	})

	t.Run("invalid piece is rejected, never registered", func(t *testing.T) {
		r := piece.NewRegistry()
		bad := validAction()
		bad.Run = nil
		p := piece.Piece{Name: "demo", DisplayName: "Demo", Actions: map[string]piece.Action{"do_thing": bad}}

		err := r.RegisterValidated(p)
		if err == nil {
			t.Fatal("RegisterValidated: err = nil, want a validation error")
		}
		if _, ok := r.GetAction("demo", "do_thing"); ok {
			t.Fatal("action IS resolvable — RegisterValidated must not register an invalid piece")
		}
	})
}
