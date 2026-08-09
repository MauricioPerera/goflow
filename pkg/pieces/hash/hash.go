// Package hash provides goflow's "Hash" catalog piece: keyless digests
// (md5/sha1/sha256/sha512) and HMAC signatures. hmac's key travels via
// ctx.Auth as a []byte, same secret-through-Auth convention as
// pkg/pieces/crypto's encryption key — the real-world use case is verifying
// an incoming webhook's signature (e.g. a provider's HMAC-SHA256 header),
// tying directly into the webhook catalog piece.
package hash

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	stdhash "hash"

	"goflow/pkg/piece"
)

const PieceName = "hash"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Hash",
		Actions: map[string]piece.Action{
			"digest": digestAction(),
			"hmac":   hmacAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func digestAction() piece.Action {
	return piece.Action{
		Name: "digest", DisplayName: "Digest",
		Dropdowns: map[string]piece.DropdownProperty{
			"algorithm": {LoadOptions: digestAlgorithmOptions},
		},
		Run: func(ctx piece.ActionContext) (any, error) {
			text, ok := ctx.Input["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: text (string)")
			}
			algorithm, _ := ctx.Input["algorithm"].(string)
			var sum []byte
			switch algorithm {
			case "md5":
				s := md5.Sum([]byte(text))
				sum = s[:]
			case "sha1":
				s := sha1.Sum([]byte(text))
				sum = s[:]
			case "sha256":
				s := sha256.Sum256([]byte(text))
				sum = s[:]
			case "sha512":
				s := sha512.Sum512([]byte(text))
				sum = s[:]
			default:
				return nil, fmt.Errorf(`unsupported algorithm %q: must be one of "md5", "sha1", "sha256", "sha512"`, algorithm)
			}
			return map[string]any{"hex": hex.EncodeToString(sum)}, nil
		},
	}
}

func hmacAction() piece.Action {
	return piece.Action{
		Name: "hmac", DisplayName: "HMAC",
		Dropdowns: map[string]piece.DropdownProperty{
			"algorithm": {LoadOptions: hmacAlgorithmOptions},
		},
		Run: func(ctx piece.ActionContext) (any, error) {
			text, ok := ctx.Input["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: text (string)")
			}
			key, ok := ctx.Auth.([]byte)
			if !ok || len(key) == 0 {
				return nil, fmt.Errorf("missing HMAC key: expected a []byte under Input[%q]", piece.AuthInputKey)
			}
			algorithm, _ := ctx.Input["algorithm"].(string)
			var newHash func() stdhash.Hash
			switch algorithm {
			case "sha1":
				newHash = sha1.New
			case "sha256":
				newHash = sha256.New
			case "sha512":
				newHash = sha512.New
			default:
				return nil, fmt.Errorf(`unsupported algorithm %q: must be one of "sha1", "sha256", "sha512"`, algorithm)
			}
			mac := hmac.New(newHash, key)
			mac.Write([]byte(text))
			return map[string]any{"hex": hex.EncodeToString(mac.Sum(nil))}, nil
		},
	}
}

func digestAlgorithmOptions(propsValue map[string]any, ctx piece.PropertyContext) (piece.DropdownState, error) {
	return piece.DropdownState{Options: []piece.DropdownOption{
		{Label: "MD5", Value: "md5"},
		{Label: "SHA-1", Value: "sha1"},
		{Label: "SHA-256", Value: "sha256"},
		{Label: "SHA-512", Value: "sha512"},
	}}, nil
}

func hmacAlgorithmOptions(propsValue map[string]any, ctx piece.PropertyContext) (piece.DropdownState, error) {
	return piece.DropdownState{Options: []piece.DropdownOption{
		{Label: "SHA-1", Value: "sha1"},
		{Label: "SHA-256", Value: "sha256"},
		{Label: "SHA-512", Value: "sha512"},
	}}, nil
}
