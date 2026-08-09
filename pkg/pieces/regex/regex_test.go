package regex_test

import (
	"testing"

	"goflow/pkg/piece"
	regexpiece "goflow/pkg/pieces/regex"
)

func run(t *testing.T, action string, input map[string]any) (map[string]any, error) {
	t.Helper()
	p := regexpiece.New()
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

func TestRegex_Match_Found(t *testing.T) {
	out, err := run(t, "match", map[string]any{"text": "order-42", "pattern": `\d+`})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["matched"] != true || out["match"] != "42" {
		t.Fatalf("out = %+v", out)
	}
}

func TestRegex_Match_NotFoundIsNotAnError(t *testing.T) {
	out, err := run(t, "match", map[string]any{"text": "no digits here", "pattern": `\d+`})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — no match is a normal outcome", err)
	}
	if out["matched"] != false || out["match"] != "" {
		t.Fatalf("out = %+v", out)
	}
}

func TestRegex_FindAll(t *testing.T) {
	out, err := run(t, "find_all", map[string]any{"text": "a1 b2 c3", "pattern": `\d`})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	matches := out["matches"].([]string)
	if len(matches) != 3 || matches[0] != "1" || matches[2] != "3" {
		t.Fatalf("matches = %#v", matches)
	}
}

func TestRegex_FindAll_NoMatchesReturnsEmptySlice(t *testing.T) {
	out, err := run(t, "find_all", map[string]any{"text": "abc", "pattern": `\d`})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	matches := out["matches"].([]string)
	if len(matches) != 0 {
		t.Fatalf("matches = %#v, want empty", matches)
	}
}

func TestRegex_Replace_WithBackreference(t *testing.T) {
	out, err := run(t, "replace", map[string]any{
		"text": "2024-03-15", "pattern": `(\d+)-(\d+)-(\d+)`, "replacement": "$3/$2/$1",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["text"] != "15/03/2024" {
		t.Fatalf("text = %q", out["text"])
	}
}

func TestRegex_ExtractGroups_Present(t *testing.T) {
	out, err := run(t, "extract_groups", map[string]any{
		"text": "2024-03-15", "pattern": `(\d+)-(\d+)-(\d+)`,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["fullMatch"] != "2024-03-15" {
		t.Fatalf("fullMatch = %q", out["fullMatch"])
	}
	groups := out["groups"].([]string)
	if len(groups) != 3 || groups[0] != "2024" || groups[2] != "15" {
		t.Fatalf("groups = %#v", groups)
	}
}

func TestRegex_ExtractGroups_NoMatch(t *testing.T) {
	out, err := run(t, "extract_groups", map[string]any{"text": "abc", "pattern": `(\d+)`})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["fullMatch"] != "" {
		t.Fatalf("fullMatch = %q, want empty", out["fullMatch"])
	}
	if len(out["groups"].([]string)) != 0 {
		t.Fatalf("groups = %#v, want empty", out["groups"])
	}
}

func TestRegex_InvalidPatternFailsClearly(t *testing.T) {
	actions := []string{"match", "find_all", "replace", "extract_groups"}
	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			input := map[string]any{"text": "x", "pattern": "(unbalanced"}
			if action == "replace" {
				input["replacement"] = "y"
			}
			_, err := run(t, action, input)
			if err == nil {
				t.Fatalf("Run() error = nil, want a compile failure for an invalid pattern")
			}
		})
	}
}
