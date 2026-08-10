package uuid_test

import (
	"regexp"
	"testing"

	"goflow/pkg/piece"
	uuidpiece "goflow/pkg/pieces/uuid"
)

// uuidV4 matches the canonical 8-4-4-4-12 form of an RFC 4122 UUID and
// pins the version (4) and variant (8, 9, a, or b) nibbles so a value
// that is merely the right length still fails when the version/variant
// bits are wrong.
var uuidV4 = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUUID_Generate_SingleHasValidShape(t *testing.T) {
	p := uuidpiece.New()
	act := p.Actions["generate"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("output is %T, want map[string]any", out)
	}
	id, ok := m["uuid"].(string)
	if !ok {
		t.Fatalf("output[\"uuid\"] is %T, want string", m["uuid"])
	}
	if !uuidV4.MatchString(id) {
		t.Fatalf("uuid = %q, want a valid RFC 4122 v4 string", id)
	}
}

func TestUUID_Generate_ExplicitCountOneMatchesDefaultShape(t *testing.T) {
	p := uuidpiece.New()
	act := p.Actions["generate"]

	withoutCount, err := act.Run(piece.ActionContext{Input: map[string]any{}})
	if err != nil {
		t.Fatalf("Run() (no count) error = %v", err)
	}
	withCountOne, err := act.Run(piece.ActionContext{Input: map[string]any{"count": 1}})
	if err != nil {
		t.Fatalf("Run() (count=1) error = %v", err)
	}

	withoutM := withoutCount.(map[string]any)
	withM := withCountOne.(map[string]any)

	// Same shape: both expose a single "uuid" string, neither exposes "uuids".
	if _, ok := withoutM["uuids"]; ok {
		t.Fatal("no-count output has \"uuids\", want only \"uuid\"")
	}
	if _, ok := withM["uuids"]; ok {
		t.Fatal("count=1 output has \"uuids\", want only \"uuid\"")
	}
	id, ok := withM["uuid"].(string)
	if !ok {
		t.Fatalf("count=1 output[\"uuid\"] is %T, want string", withM["uuid"])
	}
	if !uuidV4.MatchString(id) {
		t.Fatalf("count=1 uuid = %q, want a valid RFC 4122 v4 string", id)
	}
	if _, ok := withoutM["uuid"].(string); !ok {
		t.Fatalf("no-count output[\"uuid\"] is %T, want string", withoutM["uuid"])
	}
}

func TestUUID_Generate_CountFiveReturnsFiveDistinctUUIDs(t *testing.T) {
	p := uuidpiece.New()
	act := p.Actions["generate"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{"count": 5}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("output is %T, want map[string]any", out)
	}
	uuids, ok := m["uuids"].([]string)
	if !ok {
		t.Fatalf("output[\"uuids\"] is %T, want []string", m["uuids"])
	}
	if len(uuids) != 5 {
		t.Fatalf("len(uuids) = %d, want 5", len(uuids))
	}

	seen := map[string]bool{}
	for i, id := range uuids {
		if !uuidV4.MatchString(id) {
			t.Fatalf("uuids[%d] = %q, want a valid RFC 4122 v4 string", i, id)
		}
		if seen[id] {
			t.Fatalf("uuids[%d] = %q duplicates an earlier entry", i, id)
		}
		seen[id] = true
	}
}

func TestUUID_Generate_InvalidCountFailsClearly(t *testing.T) {
	p := uuidpiece.New()
	act := p.Actions["generate"]

	cases := []struct {
		name  string
		input map[string]any
	}{
		{"negative", map[string]any{"count": -1}},
		{"zero", map[string]any{"count": 0}},
		{"non-number string", map[string]any{"count": "five"}},
		{"non-integer float", map[string]any{"count": 1.5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := act.Run(piece.ActionContext{Input: c.input})
			if err == nil {
				t.Fatal("Run() error = nil, want a clear rejection for invalid count")
			}
		})
	}
}

func TestUUID_Generate_EmptyInputDefaultsToOne(t *testing.T) {
	p := uuidpiece.New()
	act := p.Actions["generate"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("output is %T, want map[string]any", out)
	}
	id, ok := m["uuid"].(string)
	if !ok {
		t.Fatalf("output[\"uuid\"] is %T, want string (default count=1)", m["uuid"])
	}
	if !uuidV4.MatchString(id) {
		t.Fatalf("uuid = %q, want a valid RFC 4122 v4 string", id)
	}
}
