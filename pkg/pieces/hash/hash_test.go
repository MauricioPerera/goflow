package hash_test

import (
	"bytes"
	"testing"

	"goflow/pkg/piece"
	hashpiece "goflow/pkg/pieces/hash"
)

func TestHash_Digest_KnownVectors(t *testing.T) {
	p := hashpiece.New()
	act := p.Actions["digest"]

	cases := []struct {
		algorithm, text, wantHex string
	}{
		{"md5", "abc", "900150983cd24fb0d6963f7d28e17f72"},
		{"sha1", "abc", "a9993e364706816aba3e25717850c26c9cd0d89d"},
		{"sha256", "", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"sha512", "abc", "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f"},
	}
	for _, c := range cases {
		t.Run(c.algorithm, func(t *testing.T) {
			out, err := act.Run(piece.ActionContext{Input: map[string]any{
				"text": c.text, "algorithm": c.algorithm,
			}})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if got := out.(map[string]any)["hex"]; got != c.wantHex {
				t.Fatalf("hex = %q, want %q", got, c.wantHex)
			}
		})
	}
}

func TestHash_Digest_UnsupportedAlgorithmFailsClearly(t *testing.T) {
	p := hashpiece.New()
	act := p.Actions["digest"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"text": "x", "algorithm": "crc32"}})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection for an unsupported algorithm")
	}
}

// RFC 4231 test case 1.
func TestHash_HMAC_KnownVector(t *testing.T) {
	p := hashpiece.New()
	act := p.Actions["hmac"]

	key := bytes.Repeat([]byte{0x0b}, 20)
	out, err := act.Run(piece.ActionContext{
		Input: map[string]any{"text": "Hi There", "algorithm": "sha256"},
		Auth:  key,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"
	if got := out.(map[string]any)["hex"]; got != want {
		t.Fatalf("hex = %q, want %q", got, want)
	}
}

func TestHash_HMAC_UnsupportedAlgorithmFailsClearly(t *testing.T) {
	p := hashpiece.New()
	act := p.Actions["hmac"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"text": "x", "algorithm": "md5"},
		Auth:  []byte("key"),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection — md5 is not offered for hmac")
	}
}

func TestHash_HMAC_MissingKeyFailsClearly(t *testing.T) {
	p := hashpiece.New()
	act := p.Actions["hmac"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"text": "x", "algorithm": "sha256"}})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-key error")
	}
}

func TestHash_Digest_MissingTextFailsClearly(t *testing.T) {
	p := hashpiece.New()
	act := p.Actions["digest"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"algorithm": "sha256"}})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-text error")
	}
}
