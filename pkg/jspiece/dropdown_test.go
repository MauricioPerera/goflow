package jspiece_test

import (
	"testing"

	"goflow/pkg/jspiece"
	"goflow/pkg/piece"
)

func TestJSDropdown_BasicOptions(t *testing.T) {
	dd := jspiece.NewDropdown(jspiece.DropdownSource{
		Source: `(propsValue, ctx) => ({
			options: [
				{ label: "Text", value: "text" },
				{ label: "Base64", value: "base64" },
			]
		})`,
	})
	state, err := dd.LoadOptions(map[string]any{}, piece.PropertyContext{})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if len(state.Options) != 2 || state.Options[0].Label != "Text" || state.Options[0].Value != "text" {
		t.Fatalf("state.Options = %+v", state.Options)
	}
	if state.Options[1].Label != "Base64" || state.Options[1].Value != "base64" {
		t.Fatalf("state.Options = %+v", state.Options)
	}
}

func TestJSDropdown_DisabledAndPlaceholder(t *testing.T) {
	dd := jspiece.NewDropdown(jspiece.DropdownSource{
		Source: `(propsValue, ctx) => ({
			disabled: true,
			placeholder: "connect an account first",
			options: []
		})`,
	})
	state, err := dd.LoadOptions(map[string]any{}, piece.PropertyContext{})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if !state.Disabled || state.Placeholder != "connect an account first" {
		t.Fatalf("state = %+v", state)
	}
	if len(state.Options) != 0 {
		t.Fatalf("state.Options = %+v, want empty", state.Options)
	}
}

func TestJSDropdown_PropsValueIsPassedThrough(t *testing.T) {
	dd := jspiece.NewDropdown(jspiece.DropdownSource{
		Refreshers: []string{"workspace"},
		Source: `(propsValue, ctx) => ({
			options: [{ label: propsValue.workspace, value: propsValue.workspace }]
		})`,
	})
	state, err := dd.LoadOptions(map[string]any{"workspace": "engineering"}, piece.PropertyContext{})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if len(state.Options) != 1 || state.Options[0].Label != "engineering" {
		t.Fatalf("state.Options = %+v", state.Options)
	}
}

func TestJSDropdown_SearchValueIsPassedThrough(t *testing.T) {
	dd := jspiece.NewDropdown(jspiece.DropdownSource{
		Source: `(propsValue, ctx) => ({
			options: [{ label: "match: " + ctx.searchValue, value: ctx.searchValue }]
		})`,
	})
	state, err := dd.LoadOptions(map[string]any{}, piece.PropertyContext{SearchValue: "abc"})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if len(state.Options) != 1 || state.Options[0].Label != "match: abc" {
		t.Fatalf("state.Options = %+v", state.Options)
	}
}

func TestJSDropdown_NonObjectReturnFailsClearly(t *testing.T) {
	dd := jspiece.NewDropdown(jspiece.DropdownSource{
		Source: `(propsValue, ctx) => "not an object"`,
	})
	_, err := dd.LoadOptions(map[string]any{}, piece.PropertyContext{})
	if err == nil {
		t.Fatal("LoadOptions() error = nil, want a rejection — a string isn't a valid DropdownState")
	}
}

func TestJSDropdown_NonObjectOptionFailsClearly(t *testing.T) {
	dd := jspiece.NewDropdown(jspiece.DropdownSource{
		Source: `(propsValue, ctx) => ({ options: ["not an object"] })`,
	})
	_, err := dd.LoadOptions(map[string]any{}, piece.PropertyContext{})
	if err == nil {
		t.Fatal("LoadOptions() error = nil, want a rejection — an option must be an object")
	}
}

func TestJSDropdown_ThrownExceptionBecomesError(t *testing.T) {
	dd := jspiece.NewDropdown(jspiece.DropdownSource{
		Source: `(propsValue, ctx) => { throw new Error("boom"); }`,
	})
	_, err := dd.LoadOptions(map[string]any{}, piece.PropertyContext{})
	if err == nil {
		t.Fatal("LoadOptions() error = nil, want the thrown error surfaced")
	}
}

func TestJSDropdown_MissingOptionsIsValidEmptyResult(t *testing.T) {
	dd := jspiece.NewDropdown(jspiece.DropdownSource{
		Source: `(propsValue, ctx) => ({})`,
	})
	state, err := dd.LoadOptions(map[string]any{}, piece.PropertyContext{})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if len(state.Options) != 0 {
		t.Fatalf("state.Options = %+v, want empty", state.Options)
	}
}

func TestJSAction_WithDropdownsBuildsAValidatablePiece(t *testing.T) {
	p := jspiece.New("storage_like", "Storage Like", []jspiece.ActionSource{
		{
			Name: "write", DisplayName: "Write",
			Source: `(ctx) => ({ ok: true })`,
			Dropdowns: map[string]jspiece.DropdownSource{
				"format": {
					Source: `(propsValue, ctx) => ({
						options: [
							{ label: "Text", value: "text" },
							{ label: "Base64", value: "base64" },
						]
					})`,
				},
			},
		},
	})
	if errs := piece.Validate(p); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}

	dd := p.Actions["write"].Dropdowns["format"]
	state, err := dd.LoadOptions(map[string]any{}, piece.PropertyContext{})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if len(state.Options) != 2 {
		t.Fatalf("state.Options = %+v, want 2", state.Options)
	}
}
