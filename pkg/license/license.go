// Package license implements offline license enforcement for goflow
// binaries distributed outside this (private) repository: a signed,
// time-limited license file cmd/server refuses to start without — the
// same "hard fail at startup on bad config" pattern GOFLOW_API_TOKEN and
// GOFLOW_CREDENTIALS_KEY already use, not a new category of failure.
//
// The mechanism is deliberately NOT tamper-proof DRM: anyone with access
// to this repo's source can delete the check and rebuild. It only raises
// the bar for someone running a distributed BINARY as-is past its
// license's expiry — real enforcement rests on the repo staying private,
// not on this package. See cmd/licensegen's own doc comment for how a
// license file is produced.
//
// Ed25519 (crypto/ed25519, stdlib) rather than RSA: fixed-size keys, one
// signing scheme with no padding mode to get wrong, matching this
// project's stdlib-only stance the same way pkg/credentials' AES-GCM
// choice already does for a different concern.
package license

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// publicKeyHex is the Ed25519 public key every License is verified
// against — safe to embed and commit; it can only VERIFY signatures, not
// produce them. The matching private key is held offline by whoever
// issues licenses (see cmd/licensegen) and must never appear in this
// repo, committed or not.
const publicKeyHex = "8c48b49be11b5fe395afe12f45068731126a0ca0915d0cd7544c86c5771a556d"

// PublicKey is publicKeyHex decoded once at package init. A malformed
// constant is a programmer error (this file failed to be updated after
// key generation), so init panics rather than letting every caller
// re-check a decode error that can only ever be a build-time mistake.
var PublicKey ed25519.PublicKey

func init() {
	b, err := hex.DecodeString(publicKeyHex)
	if err != nil || len(b) != ed25519.PublicKeySize {
		panic(fmt.Sprintf("license: embedded public key is malformed (%d bytes, want %d): %v", len(b), ed25519.PublicKeySize, err))
	}
	PublicKey = ed25519.PublicKey(b)
}

// Claims is the payload a License signs — deliberately minimal: who it
// was issued to and when it stops being valid. No seat count, no feature
// flags: this is a binary allow/refuse gate, not an entitlements system:
// real added scope nothing has asked for yet.
type Claims struct {
	Licensee  string    `json:"licensee"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// License is the on-disk (and on-wire) shape: Claims verbatim as they
// were signed, plus the signature over exactly those bytes. Claims is
// json.RawMessage (not a decoded Claims) so re-marshaling this struct
// reproduces the EXACT bytes Sign originally signed — a re-encode that
// reordered fields or changed whitespace would otherwise silently break
// every existing license's signature.
type License struct {
	Claims    json.RawMessage `json:"claims"`
	Signature []byte          `json:"signature"`
}

// Sign encodes claims to JSON and signs those exact bytes with priv,
// returning a License ready to be written to disk (see Save). Used only
// by cmd/licensegen, which holds the private key — never linked into
// cmd/server.
func Sign(priv ed25519.PrivateKey, claims Claims) (License, error) {
	raw, err := json.Marshal(claims)
	if err != nil {
		return License{}, fmt.Errorf("license: encoding claims: %w", err)
	}
	sig := ed25519.Sign(priv, raw)
	return License{Claims: raw, Signature: sig}, nil
}

// Verify checks lic's signature against pub and, only if valid, decodes
// and returns its Claims. Callers needing the embedded production key
// pass PublicKey; tests pass a throwaway keypair's public half instead,
// which is why this takes pub as a parameter rather than reaching for
// the package-level PublicKey itself.
func Verify(lic License, pub ed25519.PublicKey) (Claims, error) {
	if len(lic.Signature) == 0 || len(lic.Claims) == 0 {
		return Claims{}, fmt.Errorf("license: missing claims or signature")
	}
	if !ed25519.Verify(pub, lic.Claims, lic.Signature) {
		return Claims{}, fmt.Errorf("license: signature verification failed")
	}
	var claims Claims
	if err := json.Unmarshal(lic.Claims, &claims); err != nil {
		return Claims{}, fmt.Errorf("license: decoding claims: %w", err)
	}
	return claims, nil
}

// CheckExpiry reports whether claims is valid at instant now — separate
// from Verify (a distinct question: WAS this signed by us, vs IS it
// still within its validity window) so a caller like cmd/server's
// startup check can report which one failed distinctly, and so tests
// can exercise expiry logic against a fixed now without waiting on the
// real clock or minting a new signature per case.
func CheckExpiry(claims Claims, now time.Time) error {
	if now.Before(claims.IssuedAt) {
		return fmt.Errorf("license: not yet valid (issued %s, now %s)", claims.IssuedAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if !now.Before(claims.ExpiresAt) {
		return fmt.Errorf("license: expired %s (now %s)", claims.ExpiresAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	return nil
}

// Save encodes lic as COMPACT JSON (deliberately not MarshalIndent) and
// writes it to path (0o644) — a plain os.WriteFile, not the atomic
// CreateTemp+Rename pattern every Store in this project otherwise uses:
// a license file is written once by cmd/licensegen on an operator's own
// machine, never concurrently read while being written by a running
// server, so that concern doesn't apply here the way it does for
// pkg/flowstore/pkg/catalog's own writes. MarshalIndent specifically
// would break Verify on the very file it just wrote: Go's JSON indenter
// re-formats EVERY nested object it finds, including the raw bytes
// inside License.Claims (a json.RawMessage embedded verbatim precisely
// so it round-trips byte-for-byte) — reindenting those bytes changes
// them, and Claims's signature was computed over the ORIGINAL compact
// bytes Sign produced, so a Load right after an indented Save would
// fail Verify against its own freshly-issued license.
func Save(path string, lic License) error {
	data, err := json.Marshal(lic)
	if err != nil {
		return fmt.Errorf("license: encoding: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("license: writing %q: %w", path, err)
	}
	return nil
}

// Load reads and decodes a License from path. Any failure (missing
// file, malformed JSON) is returned as-is rather than mapped to a
// sentinel — unlike pkg/flowstore/pkg/catalog's Get, there is no
// "not found is a valid outcome" case here: cmd/server always requires
// a license file to exist, so a missing one is exactly as fatal as a
// malformed one.
func Load(path string) (License, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return License{}, fmt.Errorf("license: reading %q: %w", path, err)
	}
	var lic License
	if err := json.Unmarshal(data, &lic); err != nil {
		return License{}, fmt.Errorf("license: decoding %q: %w", path, err)
	}
	return lic, nil
}

// LoadAndVerify is Load + Verify(_, PublicKey) + CheckExpiry(_, now)
// composed — the one call cmd/server actually needs at startup. Kept
// separate from its three parts (rather than the only entry point) so
// pkg/license's own tests can exercise signature verification and
// expiry checking independently, against throwaway keys and fixed
// clocks the embedded production key and real time.Now() can't provide.
func LoadAndVerify(path string, now time.Time) (Claims, error) {
	lic, err := Load(path)
	if err != nil {
		return Claims{}, err
	}
	claims, err := Verify(lic, PublicKey)
	if err != nil {
		return Claims{}, err
	}
	if err := CheckExpiry(claims, now); err != nil {
		return Claims{}, err
	}
	return claims, nil
}
