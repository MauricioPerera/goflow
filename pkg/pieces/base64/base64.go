// Package base64piece provides goflow's "Base64" catalog piece: encode
// arbitrary text to base64 and decode it back, using only the Go standard
// library (encoding/base64). The package is named base64piece rather than
// base64 so it does not collide with the stdlib package "encoding/base64"
// that this file imports and references unqualified as base64 — same
// dodge pkg/pieces/hash uses (it lives in package hash but imports stdlib
// "hash" as stdhash). Here the dodge is cleaner: by giving the Go package
// a different name than its folder, the stdlib import needs no alias at
// all, and callers refer to the constructor via an aliased import (see
// pkg/pieces/pieces.go, which imports this as base64piece).
package base64piece

import (
	"encoding/base64"
	"fmt"

	"goflow/pkg/piece"
)

const PieceName = "base64"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Base64",
		Actions: map[string]piece.Action{
			"encode": encodeAction(),
			"decode": decodeAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func encodeAction() piece.Action {
	return piece.Action{
		Name: "encode", DisplayName: "Encode",
		Run: func(ctx piece.ActionContext) (any, error) {
			text, ok := ctx.Input["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: text (string)")
			}
			// urlSafe selects the base64 variant. Default (false) is
			// StdEncoding: A–Z, a–z, 0–9, '+' and '/' with '=' padding —
			// the variant for opaque payloads. urlSafe=true switches to
			// URLEncoding, which replaces '+' with '-' and '/' with '_'
			// so the result can be embedded in a URL path/query without
			// escaping. The same flag on decode must match whatever
			// variant produced the input.
			urlSafe, _ := ctx.Input["urlSafe"].(bool)
			enc := base64.StdEncoding
			if urlSafe {
				enc = base64.URLEncoding
			}
			return map[string]any{"encoded": enc.EncodeToString([]byte(text))}, nil
		},
	}
}

func decodeAction() piece.Action {
	return piece.Action{
		Name: "decode", DisplayName: "Decode",
		Run: func(ctx piece.ActionContext) (any, error) {
			text, ok := ctx.Input["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing required input: text (string)")
			}
			// urlSafe must match the variant used to encode the input;
			// see encodeAction for the rationale. A StdEncoding input
			// decoded with URLEncoding (or vice versa) fails at the
			// characters that differ between the two ('+'/'/' vs '-'/'_').
			urlSafe, _ := ctx.Input["urlSafe"].(bool)
			enc := base64.StdEncoding
			if urlSafe {
				enc = base64.URLEncoding
			}
			decoded, err := enc.DecodeString(text)
			if err != nil {
				return nil, fmt.Errorf("decoding base64: %w", err)
			}
			return map[string]any{"decoded": string(decoded)}, nil
		},
	}
}
