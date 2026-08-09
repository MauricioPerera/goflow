package csv_test

import (
	"testing"

	"goflow/pkg/piece"
	csvpiece "goflow/pkg/pieces/csv"
)

func run(t *testing.T, action string, input map[string]any) (map[string]any, error) {
	t.Helper()
	p := csvpiece.New()
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

func TestCSV_Parse_NoHeader(t *testing.T) {
	out, err := run(t, "parse", map[string]any{"text": "a,b\nc,d\n"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	rows := out["rows"].([][]string)
	if len(rows) != 2 || rows[0][0] != "a" || rows[1][1] != "d" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestCSV_Parse_WithHeader(t *testing.T) {
	out, err := run(t, "parse", map[string]any{
		"text": "name,age\nalice,30\nbob,25\n", "hasHeader": true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	rows := out["rows"].([]map[string]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %#v, want 2", rows)
	}
	if rows[0]["name"] != "alice" || rows[0]["age"] != "30" {
		t.Fatalf("rows[0] = %+v", rows[0])
	}
	if rows[1]["name"] != "bob" || rows[1]["age"] != "25" {
		t.Fatalf("rows[1] = %+v", rows[1])
	}
}

func TestCSV_Parse_CustomDelimiter(t *testing.T) {
	out, err := run(t, "parse", map[string]any{"text": "a;b\nc;d\n", "delimiter": ";"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	rows := out["rows"].([][]string)
	if rows[0][0] != "a" || rows[0][1] != "b" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestCSV_Parse_MalformedFailsClearly(t *testing.T) {
	_, err := run(t, "parse", map[string]any{"text": "a,\"unterminated\nb,c"})
	if err == nil {
		t.Fatal("Run() error = nil, want a parse failure for malformed CSV")
	}
}

func TestCSV_Parse_EmptyTextIsNotAnError(t *testing.T) {
	out, err := run(t, "parse", map[string]any{"text": ""})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	rows := out["rows"].([][]string)
	if len(rows) != 0 {
		t.Fatalf("rows = %#v, want empty", rows)
	}

	out, err = run(t, "parse", map[string]any{"text": "", "hasHeader": true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	rowsH := out["rows"].([]map[string]any)
	if len(rowsH) != 0 {
		t.Fatalf("rows = %#v, want empty", rowsH)
	}
}

func TestCSV_DelimiterLengthValidation(t *testing.T) {
	_, err := run(t, "parse", map[string]any{"text": "a,b", "delimiter": "::"})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection for a multi-character delimiter")
	}
}

func TestCSV_Stringify_NoHeader_RoundTripsThroughParse(t *testing.T) {
	out, err := run(t, "stringify", map[string]any{
		"rows": []any{
			[]any{"a", "b"},
			[]any{"c", "d"},
		},
	})
	if err != nil {
		t.Fatalf("stringify: %v", err)
	}
	text := out["text"].(string)

	parsed, err := run(t, "parse", map[string]any{"text": text})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rows := parsed["rows"].([][]string)
	if len(rows) != 2 || rows[0][0] != "a" || rows[1][1] != "d" {
		t.Fatalf("round-tripped rows = %#v", rows)
	}
}

func TestCSV_Stringify_WithHeaders_RoundTripsThroughParse(t *testing.T) {
	for i := 0; i < 5; i++ {
		out, err := run(t, "stringify", map[string]any{
			"headers": []any{"name", "age"},
			"rows": []any{
				map[string]any{"name": "alice", "age": "30"},
				map[string]any{"name": "bob", "age": "25"},
			},
		})
		if err != nil {
			t.Fatalf("stringify (iteration %d): %v", i, err)
		}
		text := out["text"].(string)

		parsed, err := run(t, "parse", map[string]any{"text": text, "hasHeader": true})
		if err != nil {
			t.Fatalf("parse (iteration %d): %v", i, err)
		}
		rows := parsed["rows"].([]map[string]any)
		if len(rows) != 2 || rows[0]["name"] != "alice" || rows[1]["age"] != "25" {
			t.Fatalf("iteration %d: round-tripped rows = %#v, want deterministic column order every time", i, rows)
		}
	}
}

func TestCSV_Stringify_CustomDelimiter(t *testing.T) {
	out, err := run(t, "stringify", map[string]any{
		"rows":      []any{[]any{"a", "b"}},
		"delimiter": ";",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["text"] != "a;b\n" {
		t.Fatalf("text = %q", out["text"])
	}
}

func TestCSV_MissingRowsFailsClearly(t *testing.T) {
	_, err := run(t, "stringify", map[string]any{})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-rows error")
	}
}
