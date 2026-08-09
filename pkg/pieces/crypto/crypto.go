// Package crypto provides goflow's "Crypto" catalog piece: AES-GCM
// encrypt/decrypt actions. This formalizes, as a real catalog piece, the
// exact mechanism already proven in pkg/engine/engine_test.go's
// TestFlow_EncryptDecryptRoundTrip (the hand-rolled "vault" piece there) —
// same key convention (a []byte under piece.AuthInputKey, surfaced as
// ctx.Auth), same AES-GCM construction, same nonce-prepended,
// base64-encoded ciphertext shape. Nothing new: that test already
// established there's no engine-level crypto surface needed (expr.Resolve's
// default case passes a []byte through {{ }} templating untouched, same as
// *piece.ApFile and *piece.OAuth2Auth) — this just makes the piece reusable
// instead of copy-pasted per test/flow.
//
// Key management (generating, storing, rotating the AES key) is not this
// piece's job, same "not this engine's job" boundary as OAuth2 token
// refresh — the caller supplies the key via ctx.Auth, however it manages
// that key's lifecycle.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	"goflow/pkg/piece"
)

const PieceName = "crypto"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Crypto",
		Actions: map[string]piece.Action{
			"encrypt": encryptAction(),
			"decrypt": decryptAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func encryptAction() piece.Action {
	return piece.Action{
		Name: "encrypt", DisplayName: "Encrypt",
		Run: func(ctx piece.ActionContext) (any, error) {
			key, ok := ctx.Auth.([]byte)
			if !ok || len(key) == 0 {
				return nil, fmt.Errorf("missing encryption key: expected a []byte under Input[%q]", piece.AuthInputKey)
			}
			plaintext, ok := ctx.Input["plaintext"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: plaintext (string)")
			}
			gcm, err := newGCM(key)
			if err != nil {
				return nil, err
			}
			nonce := make([]byte, gcm.NonceSize())
			if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
				return nil, fmt.Errorf("generating nonce: %w", err)
			}
			sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
			return map[string]any{"ciphertext": base64.StdEncoding.EncodeToString(sealed)}, nil
		},
	}
}

func decryptAction() piece.Action {
	return piece.Action{
		Name: "decrypt", DisplayName: "Decrypt",
		Run: func(ctx piece.ActionContext) (any, error) {
			key, ok := ctx.Auth.([]byte)
			if !ok || len(key) == 0 {
				return nil, fmt.Errorf("missing decryption key: expected a []byte under Input[%q]", piece.AuthInputKey)
			}
			ciphertextB64, ok := ctx.Input["ciphertext"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: ciphertext (string)")
			}
			raw, err := base64.StdEncoding.DecodeString(ciphertextB64)
			if err != nil {
				return nil, fmt.Errorf("invalid ciphertext encoding: %w", err)
			}
			gcm, err := newGCM(key)
			if err != nil {
				return nil, err
			}
			nonceSize := gcm.NonceSize()
			if len(raw) < nonceSize {
				return nil, fmt.Errorf("ciphertext too short")
			}
			nonce, sealed := raw[:nonceSize], raw[nonceSize:]
			plaintext, err := gcm.Open(nil, nonce, sealed, nil)
			if err != nil {
				return nil, fmt.Errorf("decryption failed: %w", err)
			}
			return map[string]any{"plaintext": string(plaintext)}, nil
		},
	}
}

// newGCM builds an AES-GCM cipher from key. aes.NewCipher requires exactly
// 16, 24, or 32 bytes (AES-128/192/256); any other length fails here with a
// clear error rather than silently truncating or padding.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("invalid encryption key: %w", err)
	}
	return cipher.NewGCM(block)
}
