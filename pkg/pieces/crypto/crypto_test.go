package crypto_test

import (
	"testing"

	"goflow/pkg/piece"
	cryptopiece "goflow/pkg/pieces/crypto"
)

func TestCrypto_EncryptDecryptRoundTrip(t *testing.T) {
	p := cryptopiece.New()
	encrypt, decrypt := p.Actions["encrypt"], p.Actions["decrypt"]

	key := []byte("0123456789abcdef") // AES-128
	out, err := encrypt.Run(piece.ActionContext{
		Input: map[string]any{"plaintext": "the launch codes are 12345"},
		Auth:  key,
	})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ciphertext, _ := out.(map[string]any)["ciphertext"].(string)
	if ciphertext == "" || ciphertext == "the launch codes are 12345" {
		t.Fatalf("ciphertext = %q, want a real (non-empty, non-plaintext) encrypted value", ciphertext)
	}

	out, err = decrypt.Run(piece.ActionContext{
		Input: map[string]any{"ciphertext": ciphertext},
		Auth:  key,
	})
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got := out.(map[string]any)["plaintext"]; got != "the launch codes are 12345" {
		t.Fatalf("plaintext = %q", got)
	}
}

func TestCrypto_StringAuthKeyWorksLikeBytes(t *testing.T) {
	p := cryptopiece.New()
	encrypt, decrypt := p.Actions["encrypt"], p.Actions["decrypt"]

	const key = "0123456789abcdef" // AES-128, passed as a string, not []byte
	out, err := encrypt.Run(piece.ActionContext{
		Input: map[string]any{"plaintext": "secret"}, Auth: key,
	})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ciphertext := out.(map[string]any)["ciphertext"].(string)

	out, err = decrypt.Run(piece.ActionContext{
		Input: map[string]any{"ciphertext": ciphertext}, Auth: key,
	})
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got := out.(map[string]any)["plaintext"]; got != "secret" {
		t.Fatalf("plaintext = %q", got)
	}
}

func TestCrypto_DifferentKeyLengths(t *testing.T) {
	p := cryptopiece.New()
	encrypt, decrypt := p.Actions["encrypt"], p.Actions["decrypt"]

	cases := []struct {
		name string
		key  []byte
	}{
		{"AES-128", []byte("0123456789abcdef")},
		{"AES-192", []byte("0123456789abcdef01234567")},
		{"AES-256", []byte("0123456789abcdef0123456789abcdef")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := encrypt.Run(piece.ActionContext{
				Input: map[string]any{"plaintext": "secret"}, Auth: c.key,
			})
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}
			ciphertext := out.(map[string]any)["ciphertext"].(string)
			out, err = decrypt.Run(piece.ActionContext{
				Input: map[string]any{"ciphertext": ciphertext}, Auth: c.key,
			})
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if got := out.(map[string]any)["plaintext"]; got != "secret" {
				t.Fatalf("plaintext = %q", got)
			}
		})
	}
}

func TestCrypto_InvalidKeyLengthFailsClearly(t *testing.T) {
	p := cryptopiece.New()
	act := p.Actions["encrypt"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"plaintext": "secret"},
		Auth:  []byte("too-short"),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection — 9 bytes is not a valid AES key length")
	}
}

func TestCrypto_Encrypt_MissingKeyFailsClearly(t *testing.T) {
	p := cryptopiece.New()
	act := p.Actions["encrypt"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"plaintext": "secret"}})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-key error")
	}
}

func TestCrypto_Encrypt_MissingPlaintextFailsClearly(t *testing.T) {
	p := cryptopiece.New()
	act := p.Actions["encrypt"]

	_, err := act.Run(piece.ActionContext{Auth: []byte("0123456789abcdef")})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-plaintext error")
	}
}

func TestCrypto_Decrypt_MissingKeyFailsClearly(t *testing.T) {
	p := cryptopiece.New()
	act := p.Actions["decrypt"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"ciphertext": "irrelevant"}})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-key error")
	}
}

func TestCrypto_Decrypt_MissingCiphertextFailsClearly(t *testing.T) {
	p := cryptopiece.New()
	act := p.Actions["decrypt"]

	_, err := act.Run(piece.ActionContext{Auth: []byte("0123456789abcdef")})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-ciphertext error")
	}
}

func TestCrypto_Decrypt_InvalidBase64FailsClearly(t *testing.T) {
	p := cryptopiece.New()
	act := p.Actions["decrypt"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"ciphertext": "not valid base64!!"},
		Auth:  []byte("0123456789abcdef"),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a base64-decoding error")
	}
}

func TestCrypto_Decrypt_TruncatedCiphertextFailsClearly(t *testing.T) {
	p := cryptopiece.New()
	act := p.Actions["decrypt"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"ciphertext": "AA=="}, // decodes to 1 byte, shorter than any GCM nonce
		Auth:  []byte("0123456789abcdef"),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a too-short-ciphertext error")
	}
}

func TestCrypto_Decrypt_WrongKeyFailsClearly(t *testing.T) {
	p := cryptopiece.New()
	encrypt, decrypt := p.Actions["encrypt"], p.Actions["decrypt"]

	rightKey := []byte("0123456789abcdef")
	wrongKey := []byte("fedcba9876543210")

	out, err := encrypt.Run(piece.ActionContext{
		Input: map[string]any{"plaintext": "top secret"}, Auth: rightKey,
	})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ciphertext := out.(map[string]any)["ciphertext"].(string)

	_, err = decrypt.Run(piece.ActionContext{
		Input: map[string]any{"ciphertext": ciphertext}, Auth: wrongKey,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a decryption failure — GCM authentication must reject a wrong key")
	}
}

func TestCrypto_Decrypt_TamperedCiphertextFailsClearly(t *testing.T) {
	p := cryptopiece.New()
	encrypt, decrypt := p.Actions["encrypt"], p.Actions["decrypt"]

	key := []byte("0123456789abcdef")
	out, err := encrypt.Run(piece.ActionContext{
		Input: map[string]any{"plaintext": "top secret"}, Auth: key,
	})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ciphertext := []byte(out.(map[string]any)["ciphertext"].(string))
	// Flip the last character to corrupt the sealed bytes/auth tag without
	// touching the base64 alphabet in a way that breaks decoding itself.
	last := ciphertext[len(ciphertext)-1]
	if last == 'A' {
		ciphertext[len(ciphertext)-1] = 'B'
	} else {
		ciphertext[len(ciphertext)-1] = 'A'
	}

	_, err = decrypt.Run(piece.ActionContext{
		Input: map[string]any{"ciphertext": string(ciphertext)}, Auth: key,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a decryption failure — GCM must reject tampered ciphertext")
	}
}
