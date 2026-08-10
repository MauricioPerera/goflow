package pieces_test

import (
	"testing"

	"goflow/pkg/piece"
	"goflow/pkg/pieces"
)

func TestRegisterAll_EveryCatalogPieceBecomesResolvable(t *testing.T) {
	r := piece.NewRegistry()
	if err := pieces.RegisterAll(r); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	cases := []struct {
		pieceName, kind, name string
	}{
		{"http", "action", "request"},
		{"json", "action", "parse"},
		{"json", "action", "stringify"},
		{"delay", "action", "wait"},
		{"webhook", "trigger", "catch_hook"},
		{"schedule", "trigger", "interval"},
		{"crypto", "action", "encrypt"},
		{"crypto", "action", "decrypt"},
		{"storage", "action", "write"},
		{"approval", "action", "request"},
		{"webhook_reply", "action", "respond"},
		{"webhook_reply", "action", "stop"},
		{"text", "action", "split"},
		{"text", "action", "join"},
		{"text", "action", "replace"},
		{"text", "action", "trim"},
		{"text", "action", "case"},
		{"datetime", "action", "now"},
		{"datetime", "action", "parse"},
		{"datetime", "action", "format"},
		{"datetime", "action", "add"},
		{"datetime", "action", "diff"},
		{"hash", "action", "digest"},
		{"hash", "action", "hmac"},
		{"regex", "action", "match"},
		{"regex", "action", "find_all"},
		{"regex", "action", "replace"},
		{"regex", "action", "extract_groups"},
		{"csv", "action", "parse"},
		{"csv", "action", "stringify"},
		{"uuid", "action", "generate"},
		{"base64", "action", "encode"},
		{"base64", "action", "decode"},
		{"email", "action", "send"},
	}
	for _, c := range cases {
		switch c.kind {
		case "action":
			if _, ok := r.GetAction(c.pieceName, c.name); !ok {
				t.Fatalf("action %s.%s not resolvable after RegisterAll", c.pieceName, c.name)
			}
		case "trigger":
			if _, ok := r.GetTrigger(c.pieceName, c.name); !ok {
				t.Fatalf("trigger %s.%s not resolvable after RegisterAll", c.pieceName, c.name)
			}
		}
	}
}

func TestAll_ReturnsOneEntryPerCatalogPiece(t *testing.T) {
	all := pieces.All()
	seen := map[string]bool{}
	for _, p := range all {
		if seen[p.Name] {
			t.Fatalf("piece name %q returned more than once by All()", p.Name)
		}
		seen[p.Name] = true
	}
	for _, want := range []string{
		"http", "json", "delay", "webhook", "schedule", "crypto",
		"storage", "approval", "webhook_reply", "text", "datetime", "hash", "regex", "csv", "uuid", "base64", "email",
	} {
		if !seen[want] {
			t.Fatalf("All() did not include piece %q", want)
		}
	}
}
