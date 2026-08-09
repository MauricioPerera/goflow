package text_test

import (
	"testing"

	"goflow/pkg/piece"
	textpiece "goflow/pkg/pieces/text"
)

func run(t *testing.T, action string, input map[string]any) (map[string]any, error) {
	t.Helper()
	p := textpiece.New()
	act, ok := p.Actions[action]
	if !ok {
		t.Fatalf("action %q not registered", action)
	}
	out, err := act.Run(piece.ActionContext{Input: input})
	if err != nil {
		return nil, err
	}
	return out.(map[string]any), nil
}

func TestText_Split(t *testing.T) {
	out, err := run(t, "split", map[string]any{"text": "a,b,c", "separator": ","})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	parts := out["parts"].([]string)
	if len(parts) != 3 || parts[0] != "a" || parts[2] != "c" {
		t.Fatalf("parts = %#v", parts)
	}
}

func TestText_Split_EmptySeparatorSplitsIntoRunes(t *testing.T) {
	out, err := run(t, "split", map[string]any{"text": "abc", "separator": ""})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	parts := out["parts"].([]string)
	if len(parts) != 3 || parts[0] != "a" || parts[1] != "b" || parts[2] != "c" {
		t.Fatalf("parts = %#v, want individual runes", parts)
	}
}

func TestText_Join(t *testing.T) {
	out, err := run(t, "join", map[string]any{
		"parts":     []any{"a", "b", "c"},
		"separator": "-",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["text"] != "a-b-c" {
		t.Fatalf("text = %q", out["text"])
	}
}

func TestText_Join_CoercesNonStringElements(t *testing.T) {
	out, err := run(t, "join", map[string]any{
		"parts":     []any{"a", int64(2), true},
		"separator": ",",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["text"] != "a,2,true" {
		t.Fatalf("text = %q", out["text"])
	}
}

func TestText_Replace_AllByDefault(t *testing.T) {
	out, err := run(t, "replace", map[string]any{"text": "a-a-a", "old": "a", "new": "b"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["text"] != "b-b-b" {
		t.Fatalf("text = %q", out["text"])
	}
}

func TestText_Replace_FirstOccurrenceOnly(t *testing.T) {
	out, err := run(t, "replace", map[string]any{"text": "a-a-a", "old": "a", "new": "b", "all": false})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["text"] != "b-a-a" {
		t.Fatalf("text = %q, want only the first occurrence replaced", out["text"])
	}
}

func TestText_Trim_DefaultWhitespace(t *testing.T) {
	out, err := run(t, "trim", map[string]any{"text": "  hi  "})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["text"] != "hi" {
		t.Fatalf("text = %q", out["text"])
	}
}

func TestText_Trim_ExplicitCutset(t *testing.T) {
	out, err := run(t, "trim", map[string]any{"text": "**hi**", "cutset": "*"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["text"] != "hi" {
		t.Fatalf("text = %q", out["text"])
	}
}

func TestText_Case(t *testing.T) {
	cases := []struct{ mode, text, want string }{
		{"upper", "Hello World", "HELLO WORLD"},
		{"lower", "Hello World", "hello world"},
		{"title", "hello world", "Hello World"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			out, err := run(t, "case", map[string]any{"text": c.text, "mode": c.mode})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if out["text"] != c.want {
				t.Fatalf("text = %q, want %q", out["text"], c.want)
			}
		})
	}
}

func TestText_Case_InvalidModeFailsClearly(t *testing.T) {
	_, err := run(t, "case", map[string]any{"text": "hi", "mode": "snake"})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection for an unknown mode")
	}
}

func TestText_MissingRequiredInputs(t *testing.T) {
	cases := []struct {
		action string
		input  map[string]any
	}{
		{"split", map[string]any{"separator": ","}},
		{"split", map[string]any{"text": "a"}},
		{"join", map[string]any{"separator": ","}},
		{"join", map[string]any{"parts": []any{"a"}}},
		{"replace", map[string]any{"old": "a", "new": "b"}},
		{"replace", map[string]any{"text": "a", "new": "b"}},
		{"replace", map[string]any{"text": "a", "old": "a"}},
		{"trim", map[string]any{}},
		{"case", map[string]any{"mode": "upper"}},
		{"case", map[string]any{"text": "a"}},
	}
	for i, c := range cases {
		_, err := run(t, c.action, c.input)
		if err == nil {
			t.Fatalf("case %d (%s): Run() error = nil, want a missing-input error", i, c.action)
		}
	}
}

func TestText_FormatDropdownReturnsExactOptions(t *testing.T) {
	p := textpiece.New()
	dd := p.Actions["case"].Dropdowns["mode"]

	state, err := dd.LoadOptions(map[string]any{}, piece.PropertyContext{})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if len(state.Options) != 3 {
		t.Fatalf("options = %+v, want exactly 3", state.Options)
	}
}
