package license

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testKeypair returns a fresh, throwaway Ed25519 keypair — every test
// below signs and verifies against ITS OWN pair, never the embedded
// production PublicKey, so these tests exercise real logic without
// depending on (or being able to forge) the actual production key.
func testKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

func TestSignVerify_RoundTrips(t *testing.T) {
	pub, priv := testKeypair(t)
	claims := Claims{Licensee: "acme", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	lic, err := Sign(priv, claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := Verify(lic, pub)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Licensee != "acme" {
		t.Fatalf("Licensee = %q, want acme", got.Licensee)
	}
}

func TestVerify_TamperedClaims_Rejected(t *testing.T) {
	pub, priv := testKeypair(t)
	claims := Claims{Licensee: "acme", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	lic, err := Sign(priv, claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	var tampered Claims
	if err := json.Unmarshal(lic.Claims, &tampered); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	tampered.Licensee = "not-acme"
	raw, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	lic.Claims = raw
	if _, err := Verify(lic, pub); err == nil {
		t.Fatalf("Verify succeeded on tampered claims, want error")
	}
}

func TestVerify_WrongPublicKey_Rejected(t *testing.T) {
	_, priv := testKeypair(t)
	otherPub, _ := testKeypair(t)
	claims := Claims{Licensee: "acme", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	lic, err := Sign(priv, claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := Verify(lic, otherPub); err == nil {
		t.Fatalf("Verify succeeded against the wrong public key, want error")
	}
}

func TestVerify_MissingFields_Rejected(t *testing.T) {
	pub, _ := testKeypair(t)
	if _, err := Verify(License{}, pub); err == nil {
		t.Fatalf("Verify succeeded on an empty License, want error")
	}
}

func TestCheckExpiry_WithinWindow_OK(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	claims := Claims{
		IssuedAt:  now.Add(-24 * time.Hour),
		ExpiresAt: now.Add(24 * time.Hour),
	}
	if err := CheckExpiry(claims, now); err != nil {
		t.Fatalf("CheckExpiry = %v, want nil", err)
	}
}

func TestCheckExpiry_Expired_Rejected(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	claims := Claims{
		IssuedAt:  now.Add(-48 * time.Hour),
		ExpiresAt: now.Add(-1 * time.Hour),
	}
	if err := CheckExpiry(claims, now); err == nil {
		t.Fatalf("CheckExpiry succeeded on an expired license, want error")
	}
}

func TestCheckExpiry_NotYetValid_Rejected(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	claims := Claims{
		IssuedAt:  now.Add(1 * time.Hour),
		ExpiresAt: now.Add(48 * time.Hour),
	}
	if err := CheckExpiry(claims, now); err == nil {
		t.Fatalf("CheckExpiry succeeded on a not-yet-valid license, want error")
	}
}

func TestCheckExpiry_ExactlyAtExpiry_Rejected(t *testing.T) {
	// now == ExpiresAt is treated as expired (the window is a strict
	// upper bound) — !now.Before(ExpiresAt) is true when they're equal.
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	claims := Claims{IssuedAt: now.Add(-1 * time.Hour), ExpiresAt: now}
	if err := CheckExpiry(claims, now); err == nil {
		t.Fatalf("CheckExpiry succeeded exactly at ExpiresAt, want error (strict upper bound)")
	}
}

func TestSaveLoad_RoundTrips(t *testing.T) {
	_, priv := testKeypair(t)
	claims := Claims{Licensee: "acme", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	lic, err := Sign(priv, claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	path := filepath.Join(t.TempDir(), "license.json")
	if err := Save(path, lic); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(loaded.Claims) != string(lic.Claims) {
		t.Fatalf("loaded Claims = %s, want %s", loaded.Claims, lic.Claims)
	}
}

func TestLoad_MissingFile_Errors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "never-written.json")); err == nil {
		t.Fatalf("Load succeeded on a missing file, want error")
	}
}

func TestLoad_MalformedJSON_Errors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "license.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatalf("Load succeeded on malformed JSON, want error")
	}
}

func TestLoadAndVerify_ValidLicense_ReturnsClaims(t *testing.T) {
	// Signs with the REAL embedded PublicKey's matching private key —
	// impossible without holding that private key, so this generates a
	// fresh keypair and monkey-patches PublicKey for the duration of the
	// test, restoring it afterward so no other test observes the swap.
	pub, priv := testKeypair(t)
	orig := PublicKey
	PublicKey = pub
	t.Cleanup(func() { PublicKey = orig })

	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	claims := Claims{Licensee: "acme", IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	lic, err := Sign(priv, claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	path := filepath.Join(t.TempDir(), "license.json")
	if err := Save(path, lic); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadAndVerify(path, now)
	if err != nil {
		t.Fatalf("LoadAndVerify: %v", err)
	}
	if got.Licensee != "acme" {
		t.Fatalf("Licensee = %q, want acme", got.Licensee)
	}
}

func TestLoadAndVerify_ExpiredLicense_Errors(t *testing.T) {
	pub, priv := testKeypair(t)
	orig := PublicKey
	PublicKey = pub
	t.Cleanup(func() { PublicKey = orig })

	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	claims := Claims{Licensee: "acme", IssuedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	lic, err := Sign(priv, claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	path := filepath.Join(t.TempDir(), "license.json")
	if err := Save(path, lic); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := LoadAndVerify(path, now); err == nil {
		t.Fatalf("LoadAndVerify succeeded on an expired license, want error")
	}
}

func TestLoadAndVerify_WrongSigningKey_Errors(t *testing.T) {
	// Signed with a DIFFERENT private key than the one PublicKey (even
	// swapped to a throwaway) corresponds to — the forged-license case.
	pub, _ := testKeypair(t)
	_, forgerPriv := testKeypair(t)
	orig := PublicKey
	PublicKey = pub
	t.Cleanup(func() { PublicKey = orig })

	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	claims := Claims{Licensee: "acme", IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	lic, err := Sign(forgerPriv, claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	path := filepath.Join(t.TempDir(), "license.json")
	if err := Save(path, lic); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := LoadAndVerify(path, now); err == nil {
		t.Fatalf("LoadAndVerify succeeded on a forged license, want error")
	}
}
