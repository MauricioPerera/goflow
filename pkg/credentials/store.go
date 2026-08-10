// Package credentials is a separate, encrypted-at-rest store for secrets —
// distinct from pkg/catalog on purpose. The catalog persists piece
// definitions in plain JSON; that's fine for code, but credentials must never
// travel or rest in plaintext. Here each secret lives in its own file, sealed
// with AES-256-GCM: the only plaintext form is the in-memory value handed back
// to trusted Go callers via Get. HTTP never exposes the decrypted value.
//
// The store uses nothing but the standard library (crypto/aes, crypto/cipher,
// crypto/rand, encoding/base64, encoding/json, os, path/filepath, sort) — no
// new dependency, matching this project's stance.
package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNotFound is returned by Delete when no credential file exists for the
// given name. Callers (the HTTP layer) map it to 404 without inspecting the
// raw os error. Get does NOT use it: a missing name is the (nil, false, nil)
// case, matching catalog.FileStore.Get.
var ErrNotFound = errors.New("credentials: not found")

// Store is the surface a credential vault exposes. Save encrypts and
// persists; Get decrypts into an any (Go callers only — never HTTP); List
// returns names only; Delete removes. None of these round-trip a secret in
// plaintext except Get, which is intentionally not wired to any HTTP route.
type Store interface {
	Save(name string, value any) error
	Get(name string) (value any, ok bool, err error)
	List() ([]string, error)
	Delete(name string) error
}

// FileStore is a Store backed by one encrypted file per credential, in a
// directory on disk. dir is where the .enc files live; key is the 32-byte
// AES-256-GCM key shared by every seal/open. The key is never written to disk.
type FileStore struct {
	dir string
	key []byte
}

// NewFileStore returns a FileStore rooted at dir (created if missing, 0o755,
// same as catalog.FileStore) and sealed with key. key MUST be exactly 32
// bytes for AES-256-GCM — a wrong length is a hard error: never truncate or
// pad silently, since that would silently weaken the cipher.
func NewFileStore(dir string, key []byte) (*FileStore, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("credentials: key must be exactly 32 bytes for AES-256-GCM, got %d", len(key))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("credentials: creating store directory: %w", err)
	}
	// Copy the key so a caller mutating their slice after construction can't
	// corrupt the cipher. Length is already validated to 32.
	k := make([]byte, 32)
	copy(k, key)
	return &FileStore{dir: dir, key: k}, nil
}

// Dir returns the on-disk directory backing this store. It is intended for
// tests that need to inspect raw .enc files directly (e.g. to assert a
// plaintext secret never touches disk). Production code has no use for it.
func (s *FileStore) Dir() string { return s.dir }

// path validates name and returns the on-disk file it maps to. A credential
// name may be agent-authored — never trust it as a raw path segment. Same
// rejection criteria as catalog.FileStore.path: reject "", any "/" or "\",
// "." and "..", rather than trying to "clean" a traversal attempt. The
// validation happens before any filesystem touch.
func (s *FileStore) path(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("credentials: name is empty")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("credentials: invalid name %q", name)
	}
	return filepath.Join(s.dir, name+".enc"), nil
}

// envelope is the on-disk JSON shape: a fresh random nonce plus the GCM
// ciphertext, both base64 (StdEncoding) so the file is pure printable JSON.
type envelope struct {
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// Save serializes value to JSON, seals it with AES-256-GCM under a fresh
// random nonce, and writes the envelope atomically (CreateTemp in the same
// dir + Rename, same pattern as catalog.FileStore.Save, with cleanup at every
// failure point and 0o644 before the rename). The nonce is regenerated per
// Save — GCM nonces must never repeat under the same key.
func (s *FileStore) Save(name string, value any) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	plaintext, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("credentials: encoding %q: %w", name, err)
	}

	block, err := aes.NewCipher(s.key)
	if err != nil {
		return fmt.Errorf("credentials: building cipher for %q: %w", name, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("credentials: building GCM for %q: %w", name, err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("credentials: generating nonce for %q: %w", name, err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	env := envelope{
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials: encoding envelope for %q: %w", name, err)
	}

	// Atomic write: same pattern as catalog.FileStore.Save. A reader (or a
	// crash) only ever sees the previous version or the fully-written new one.
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("credentials: creating temp file for %q: %w", name, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("credentials: writing %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("credentials: closing temp file for %q: %w", name, err)
	}
	// os.CreateTemp uses 0o600; match 0o644 like catalog.FileStore does, since
	// os.Rename carries the source's mode over.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		cleanup()
		return fmt.Errorf("credentials: setting mode for %q: %w", name, err)
	}
	if err := os.Rename(tmpPath, p); err != nil {
		cleanup()
		return fmt.Errorf("credentials: renaming %q into place: %w", name, err)
	}
	return nil
}

// Get returns the decrypted value for name. If the file does not exist it
// returns (nil, false, nil) — not an error — matching catalog.FileStore.Get.
// If the file exists but GCM.Open fails (wrong key, corruption, tampering),
// the error is returned wrapped in context; never partial data, never a
// silenced auth failure. On success the plaintext is json.Unmarshal'd into an
// any and returned (json into any yields map[string]interface{} for objects,
// which is what callers compare against).
func (s *FileStore) Get(name string) (value any, ok bool, err error) {
	p, err := s.path(name)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("credentials: reading %q: %w", name, err)
	}

	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, false, fmt.Errorf("credentials: decoding envelope %q: %w", name, err)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, false, fmt.Errorf("credentials: decoding nonce %q: %w", name, err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, false, fmt.Errorf("credentials: decoding ciphertext %q: %w", name, err)
	}

	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, false, fmt.Errorf("credentials: building cipher for %q: %w", name, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, false, fmt.Errorf("credentials: building GCM for %q: %w", name, err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Wrong key, corruption, or tampering — propagate verbatim, wrapped.
		return nil, false, fmt.Errorf("credentials: decrypting %q: %w", name, err)
	}

	var v any
	if err := json.Unmarshal(plaintext, &v); err != nil {
		return nil, false, fmt.Errorf("credentials: unmarshaling plaintext %q: %w", name, err)
	}
	return v, true, nil
}

// List returns the credential names (without the ".enc" suffix), sorted
// alphabetically. It reads only directory entry names — it never opens or
// decrypts any credential file to build this list.
func (s *FileStore) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("credentials: listing store directory: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".enc") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".enc"))
	}
	sort.Strings(names)
	return names, nil
}

// Delete removes the credential file for name. If no such file exists it
// returns ErrNotFound (not the raw os.IsNotExist) so the HTTP layer can map
// it to 404 without inspecting the underlying error.
func (s *FileStore) Delete(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("credentials: deleting %q: %w", name, err)
	}
	return nil
}
