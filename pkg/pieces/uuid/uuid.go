// Package uuid provides goflow's "UUID" catalog piece: generation of
// RFC 4122 version 4 (random) UUIDs using only the Go standard library
// (crypto/rand for the random bytes, encoding/hex for the hex encoding).
// No third-party UUID library is pulled in — the whole point of this
// piece is to show a small, self-contained catalog piece that depends on
// nothing beyond stdlib, same posture as pkg/pieces/hash.
package uuid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"goflow/pkg/piece"
)

const PieceName = "uuid"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "UUID",
		Actions: map[string]piece.Action{
			"generate": generateAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func generateAction() piece.Action {
	return piece.Action{
		Name: "generate", DisplayName: "Generate",
		Run: func(ctx piece.ActionContext) (any, error) {
			count := 1
			if raw, ok := ctx.Input["count"]; ok && raw != nil {
				c, ok := toInt(raw)
				if !ok {
					return nil, fmt.Errorf("invalid input: count (must be a number)")
				}
				count = c
			}
			if count < 1 {
				return nil, fmt.Errorf("invalid input: count (must be >= 1, got %d)", count)
			}

			if count == 1 {
				id, err := newV4()
				if err != nil {
					return nil, err
				}
				return map[string]any{"uuid": id}, nil
			}

			uuids := make([]string, 0, count)
			for i := 0; i < count; i++ {
				id, err := newV4()
				if err != nil {
					return nil, err
				}
				uuids = append(uuids, id)
			}
			return map[string]any{"uuids": uuids}, nil
		},
	}
}

// newV4 returns a freshly generated RFC 4122 version 4 UUID in its
// canonical 8-4-4-4-12 hyphenated text form. Version 4 means the 6 most
// significant bits of the time_hi_and_version field are set to 0b0100
// (the high nibble of byte 6 becomes 4); the variant is the RFC 4122
// variant (10xx in the high bits of byte 8, so the top two bits are 10,
// i.e. byte 8 has 0b1000_0000 | 0b0000 applied then its top two bits
// cleared and 0b10 set).
func newV4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating uuid: %w", err)
	}
	// Version 4: high nibble of byte 6 -> 0x4.
	b[6] = (b[6] & 0x0f) | 0x40
	// Variant: high two bits of byte 8 -> 0b10.
	b[8] = (b[8] & 0x3f) | 0x80

	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32], nil
}

// toInt coerces a JSON-decoded count value to an int. JSON numbers come
// through model.ParseFlowVersion as float64; a plain int is also accepted
// for callers that build Input maps directly in Go (the tests do this).
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		// Reject non-integer floats so count=1.5 fails clearly rather
		// than silently truncating to 1.
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}
