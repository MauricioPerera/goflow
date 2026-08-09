package datetime_test

import (
	"testing"
	"time"

	"goflow/pkg/piece"
	datetimepiece "goflow/pkg/pieces/datetime"
)

func run(t *testing.T, action string, input map[string]any) (map[string]any, error) {
	t.Helper()
	p := datetimepiece.New()
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

func TestDatetime_Now(t *testing.T) {
	before := time.Now()
	out, err := run(t, "now", map[string]any{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	iso, _ := out["iso"].(string)
	got, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Fatalf("iso = %q is not valid RFC3339: %v", iso, err)
	}
	if got.Before(before.Add(-5*time.Second)) || got.After(before.Add(5*time.Second)) {
		t.Fatalf("now() = %v, want within 5s of %v", got, before)
	}
}

func TestDatetime_ParseFormatRoundTrip(t *testing.T) {
	parsed, err := run(t, "parse", map[string]any{"text": "2024-03-15", "layout": "2006-01-02"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	iso, _ := parsed["iso"].(string)
	if iso == "" {
		t.Fatalf("parsed iso is empty: %+v", parsed)
	}
	if _, ok := parsed["unixMs"].(int64); !ok {
		t.Fatalf("unixMs = %#v, want int64", parsed["unixMs"])
	}

	formatted, err := run(t, "format", map[string]any{"iso": iso, "layout": "2006-01-02"})
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if formatted["text"] != "2024-03-15" {
		t.Fatalf("text = %q, want 2024-03-15", formatted["text"])
	}
}

func TestDatetime_Add_Positive(t *testing.T) {
	out, err := run(t, "add", map[string]any{"iso": "2024-01-01T00:00:00Z", "amountMs": int64(3600000)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["iso"] != "2024-01-01T01:00:00Z" {
		t.Fatalf("iso = %q", out["iso"])
	}
}

func TestDatetime_Add_Negative(t *testing.T) {
	out, err := run(t, "add", map[string]any{"iso": "2024-01-01T01:00:00Z", "amountMs": int64(-3600000)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out["iso"] != "2024-01-01T00:00:00Z" {
		t.Fatalf("iso = %q", out["iso"])
	}
}

func TestDatetime_Diff(t *testing.T) {
	cases := []struct {
		name, a, b string
		want       int64
	}{
		{"a after b", "2024-01-01T00:00:01Z", "2024-01-01T00:00:00Z", 1000},
		{"a before b", "2024-01-01T00:00:00Z", "2024-01-01T00:00:01Z", -1000},
		{"equal", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := run(t, "diff", map[string]any{"a": c.a, "b": c.b})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if out["diffMs"] != c.want {
				t.Fatalf("diffMs = %v, want %d", out["diffMs"], c.want)
			}
		})
	}
}

func TestDatetime_InvalidISOFailsClearly(t *testing.T) {
	_, err := run(t, "format", map[string]any{"iso": "not-a-timestamp", "layout": "2006-01-02"})
	if err == nil {
		t.Fatal("Run() error = nil, want a parse failure for an invalid RFC3339 string")
	}
}

func TestDatetime_ParseMismatchedLayoutFailsClearly(t *testing.T) {
	_, err := run(t, "parse", map[string]any{"text": "not a date", "layout": "2006-01-02"})
	if err == nil {
		t.Fatal("Run() error = nil, want a parse failure for a text/layout mismatch")
	}
}
