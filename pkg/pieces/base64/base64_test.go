package base64piece_test

import (
	"strings"
	"testing"

	"goflow/pkg/piece"
	base64piece "goflow/pkg/pieces/base64"
)

// "hello" -> base64 std with padding, computed by hand:
// h e l l o = 0x68 0x65 0x6c 0x6c 0x6f (5 bytes -> 8 base64 chars, one '=' pad)
// 01101000 01100101 01101100 01101100 01101111
// -> aGVsbG8=
const helloStdBase64 = "aGVsbG8="

func TestBase64_Encode_SimpleStringMatchesKnownValue(t *testing.T) {
	p := base64piece.New()
	act := p.Actions["encode"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{"text": "hello"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("output is %T, want map[string]any", out)
	}
	encoded, ok := m["encoded"].(string)
	if !ok {
		t.Fatalf("output[\"encoded\"] is %T, want string", m["encoded"])
	}
	if encoded != helloStdBase64 {
		t.Fatalf("encoded = %q, want %q (known base64 of \"hello\")", encoded, helloStdBase64)
	}
}

func TestBase64_Decode_RoundTripsToOriginal(t *testing.T) {
	p := base64piece.New()
	act := p.Actions["decode"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{"text": helloStdBase64}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("output is %T, want map[string]any", out)
	}
	decoded, ok := m["decoded"].(string)
	if !ok {
		t.Fatalf("output[\"decoded\"] is %T, want string", m["decoded"])
	}
	if decoded != "hello" {
		t.Fatalf("decoded = %q, want \"hello\" (round-trip of %q)", decoded, helloStdBase64)
	}
}

func TestBase64_Decode_InvalidBase64FailsClearly(t *testing.T) {
	p := base64piece.New()
	act := p.Actions["decode"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"text": "!!!not base64"}})
	if err == nil {
		t.Fatal("Run() error = nil, want a clear error for invalid base64 input")
	}
	if !strings.Contains(err.Error(), "decoding base64") {
		t.Fatalf("error = %q, want it to mention \"decoding base64\"", err.Error())
	}
}

// "\xff\xff\xff" encodes under StdEncoding to "////" (every 6-bit group is
// 63 -> '/'); under URLEncoding 63 maps to '_', so the urlSafe result is
// "____". The two differ exactly at the characters that distinguish the
// two base64 variants, which is what this test pins.
func TestBase64_Encode_URLSafeDiffersFromDefaultForSlashInput(t *testing.T) {
	p := base64piece.New()
	act := p.Actions["encode"]
	input := "\xff\xff\xff"

	stdOut, err := act.Run(piece.ActionContext{Input: map[string]any{"text": input}})
	if err != nil {
		t.Fatalf("Run() (default) error = %v", err)
	}
	urlOut, err := act.Run(piece.ActionContext{Input: map[string]any{"text": input, "urlSafe": true}})
	if err != nil {
		t.Fatalf("Run() (urlSafe) error = %v", err)
	}
	stdEncoded := stdOut.(map[string]any)["encoded"].(string)
	urlEncoded := urlOut.(map[string]any)["encoded"].(string)

	if stdEncoded != "////" {
		t.Fatalf("default encoded = %q, want \"////\"", stdEncoded)
	}
	if urlEncoded != "____" {
		t.Fatalf("urlSafe encoded = %q, want \"____\"", urlEncoded)
	}
	if stdEncoded == urlEncoded {
		t.Fatalf("default and urlSafe encodings are identical (%q); want them to differ for input %q", stdEncoded, input)
	}
}

func TestBase64_Encode_MissingTextFailsClearly(t *testing.T) {
	p := base64piece.New()
	act := p.Actions["encode"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{}})
	if err == nil {
		t.Fatal("Run() error = nil, want a clear error for missing text input")
	}
	if !strings.Contains(err.Error(), "missing required input: text") {
		t.Fatalf("error = %q, want it to mention \"missing required input: text\"", err.Error())
	}
}

func TestBase64_Decode_MissingTextFailsClearly(t *testing.T) {
	p := base64piece.New()
	act := p.Actions["decode"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{}})
	if err == nil {
		t.Fatal("Run() error = nil, want a clear error for missing text input")
	}
	if !strings.Contains(err.Error(), "missing required input: text") {
		t.Fatalf("error = %q, want it to mention \"missing required input: text\"", err.Error())
	}
}
