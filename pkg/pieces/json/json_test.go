package json_test

import (
	"fmt"
	"testing"

	"goflow/pkg/piece"
	jsonpiece "goflow/pkg/pieces/json"
)

func TestJSON_Parse(t *testing.T) {
	p := jsonpiece.New()
	act, ok := p.Actions["parse"]
	if !ok {
		t.Fatal("parse action not registered")
	}

	out, err := act.Run(piece.ActionContext{Input: map[string]any{"text": `{"name":"ok","count":3}`}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	data, ok := out.(map[string]any)["data"].(map[string]any)
	if !ok {
		t.Fatalf("out = %#v, want data to be a map", out)
	}
	if data["name"] != "ok" || data["count"] != float64(3) {
		t.Fatalf("data = %+v", data)
	}
}

func TestJSON_Parse_InvalidJSONFailsClearly(t *testing.T) {
	p := jsonpiece.New()
	act := p.Actions["parse"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"text": `{not valid`}})
	if err == nil {
		t.Fatal("Run() error = nil, want a clear parse failure")
	}
}

func TestJSON_Parse_MissingTextFailsClearly(t *testing.T) {
	p := jsonpiece.New()
	act := p.Actions["parse"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{}})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-input error")
	}
}

func TestJSON_Stringify(t *testing.T) {
	p := jsonpiece.New()
	act, ok := p.Actions["stringify"]
	if !ok {
		t.Fatal("stringify action not registered")
	}

	out, err := act.Run(piece.ActionContext{Input: map[string]any{
		"data": map[string]any{"name": "ok"},
	}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	text, _ := out.(map[string]any)["text"].(string)
	if text != `{"name":"ok"}` {
		t.Fatalf("text = %q", text)
	}
}

func TestJSON_Stringify_Pretty(t *testing.T) {
	p := jsonpiece.New()
	act := p.Actions["stringify"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{
		"data":   map[string]any{"name": "ok"},
		"pretty": true,
	}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	text, _ := out.(map[string]any)["text"].(string)
	want := "{\n  \"name\": \"ok\"\n}"
	if text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

func TestJSON_Parse_EmptyTextFailsClearly(t *testing.T) {
	p := jsonpiece.New()
	act := p.Actions["parse"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"text": ""}})
	if err == nil {
		t.Fatal("Run() error = nil, want a parse failure for empty input")
	}
}

func TestJSON_Parse_NonObjectTopLevelValues(t *testing.T) {
	p := jsonpiece.New()
	act := p.Actions["parse"]

	cases := []struct {
		name string
		text string
		want any
	}{
		{"array", `[1,2,3]`, []any{float64(1), float64(2), float64(3)}},
		{"string", `"hello"`, "hello"},
		{"number", `42`, float64(42)},
		{"bool", `true`, true},
		{"null", `null`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := act.Run(piece.ActionContext{Input: map[string]any{"text": c.text}})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			data := out.(map[string]any)["data"]
			if fmt.Sprint(data) != fmt.Sprint(c.want) {
				t.Fatalf("data = %#v, want %#v", data, c.want)
			}
		})
	}
}

func TestJSON_Stringify_UnsupportedValueFailsClearly(t *testing.T) {
	p := jsonpiece.New()
	act := p.Actions["stringify"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{
		"data": map[string]any{"fn": func() {}},
	}})
	if err == nil {
		t.Fatal("Run() error = nil, want an encoding failure for a func value")
	}
}

func TestJSON_Stringify_NilDataValueIsValid(t *testing.T) {
	p := jsonpiece.New()
	act := p.Actions["stringify"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{"data": nil}})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — the key is present, its value is just nil", err)
	}
	text, _ := out.(map[string]any)["text"].(string)
	if text != "null" {
		t.Fatalf("text = %q, want %q", text, "null")
	}
}

func TestJSON_Stringify_MissingDataKeyFailsClearly(t *testing.T) {
	p := jsonpiece.New()
	act := p.Actions["stringify"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{}})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-input error — no \"data\" key at all")
	}
}

func TestJSON_RoundTrip(t *testing.T) {
	p := jsonpiece.New()
	stringify, parse := p.Actions["stringify"], p.Actions["parse"]

	original := map[string]any{"items": []any{"a", "b"}, "count": float64(2)}
	stringified, err := stringify.Run(piece.ActionContext{Input: map[string]any{"data": original}})
	if err != nil {
		t.Fatalf("stringify: %v", err)
	}
	parsed, err := parse.Run(piece.ActionContext{Input: map[string]any{"text": stringified.(map[string]any)["text"]}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	back := parsed.(map[string]any)["data"].(map[string]any)
	if back["count"] != float64(2) {
		t.Fatalf("round-tripped count = %v, want 2", back["count"])
	}
}
