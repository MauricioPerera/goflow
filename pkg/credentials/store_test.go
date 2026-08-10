package credentials

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testKey is a fixed 32-byte AES-256 key for tests — never any secret, just
// a constant so cross-store comparisons in the same test are deterministic.
var testKey = []byte("0123456789abcdef0123456789abcdef") // 32 bytes

func newStore(t *testing.T, key []byte) *FileStore {
	t.Helper()
	fs, err := NewFileStore(t.TempDir(), key)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return fs
}

func TestSaveGet_StringRoundtrips(t *testing.T) {
	s := newStore(t, testKey)
	if err := s.Save("alpha", "hello-world"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := s.Get("alpha")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got != "hello-world" {
		t.Fatalf("got = %v, want hello-world", got)
	}
}

func TestSaveGet_StructuredRoundtrips(t *testing.T) {
	s := newStore(t, testKey)
	in := map[string]any{"user": "x", "pass": "y", "port": float64(5432)}
	if err := s.Save("db", in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := s.Get("db")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	// json.Unmarshal into any yields map[string]interface{}; compare field by
	// field rather than reflect.DeepEqual on the original (types differ).
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("got = %T, want map[string]any", got)
	}
	if m["user"] != "x" || m["pass"] != "y" || m["port"] != float64(5432) {
		t.Fatalf("got = %#v, want user=x pass=y port=5432", m)
	}
}

func TestGet_MissingName_NotFoundNotError(t *testing.T) {
	s := newStore(t, testKey)
	got, ok, err := s.Get("never-saved")
	if err != nil {
		t.Fatalf("err = %v, want nil for missing name", err)
	}
	if ok {
		t.Fatalf("ok = true, want false for missing name; got=%#v", got)
	}
	if got != nil {
		t.Fatalf("got = %#v, want nil for missing name", got)
	}
}

func TestList_ReflectsSavedNames_SortedNoValues(t *testing.T) {
	s := newStore(t, testKey)
	for _, n := range []string{"charlie", "alpha", "bravo"} {
		if err := s.Save(n, "secret-"+n); err != nil {
			t.Fatalf("Save %q: %v", n, err)
		}
	}
	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"alpha", "bravo", "charlie"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("names = %v, want %v (sorted)", names, want)
		}
	}
}

func TestDelete_RemovesFile(t *testing.T) {
	s := newStore(t, testKey)
	if err := s.Save("goner", "v"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Delete("goner"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, ok, err := s.Get("goner"); ok || err != nil {
		t.Fatalf("Get after delete: ok=%v err=%v got=%#v", ok, err, got)
	}
	names, _ := s.List()
	for _, n := range names {
		if n == "goner" {
			t.Fatalf("deleted name still listed: %v", names)
		}
	}
}

func TestDelete_MissingName_ErrNotFound(t *testing.T) {
	s := newStore(t, testKey)
	err := s.Delete("nope")
	if err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestEncryption_SecretNotPresentInRawFile(t *testing.T) {
	s := newStore(t, testKey)
	secret := "el-secreto-super-unico-xyz123"
	if err := s.Save("vault", secret); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Read the on-disk file directly — NOT through the Store — and assert the
	// plaintext secret does not appear in any recognizable form.
	raw, err := os.ReadFile(filepath.Join(s.dir, "vault.enc"))
	if err != nil {
		t.Fatalf("read raw file: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("plaintext secret appears in raw file:\n%s", raw)
	}
	// The file must not be plain JSON of the value either: a sealed envelope
	// has nonce/ciphertext base64 fields, not the raw value. The decoded
	// ciphertext bytes would be gibberish, but base64-encoding of a short
	// string can coincidentally contain readable substrings — so also confirm
	// the envelope shape rather than a bare value.
	body := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(body, "{") || !strings.Contains(body, "nonce") || !strings.Contains(body, "ciphertext") {
		t.Fatalf("raw file is not a sealed envelope:\n%s", raw)
	}
}

func TestWrongKey_GetFails(t *testing.T) {
	dir := t.TempDir()
	keyA := []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") // 32 bytes
	keyB := []byte("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB") // 32 bytes
	storeA, err := NewFileStore(dir, keyA)
	if err != nil {
		t.Fatalf("NewFileStore A: %v", err)
	}
	storeB, err := NewFileStore(dir, keyB)
	if err != nil {
		t.Fatalf("NewFileStore B: %v", err)
	}
	if err := storeA.Save("shared", "top-secret"); err != nil {
		t.Fatalf("Save with key A: %v", err)
	}
	got, ok, err := storeB.Get("shared")
	if err == nil {
		t.Fatalf("Get with wrong key: err = nil, want error; ok=%v got=%#v", ok, got)
	}
	if ok {
		t.Fatalf("Get with wrong key returned ok=true with data: %#v", got)
	}
	if got != nil {
		t.Fatalf("Get with wrong key returned non-nil value: %#v", got)
	}
}

func TestNewFileStore_WrongKeyLength(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range [][]byte{nil, {}, []byte("too-short"), make([]byte, 16), make([]byte, 64)} {
		if _, err := NewFileStore(dir, bad); err == nil {
			t.Fatalf("NewFileStore with key len %d: err = nil, want error", len(bad))
		}
	}
}

func TestPathTraversal_Rejected(t *testing.T) {
	s := newStore(t, testKey)
	bad := []string{"", "../fuera", "a/b", "a\\b", ".", ".."}
	for _, name := range bad {
		if err := s.Save(name, "x"); err == nil {
			t.Fatalf("Save(%q): err = nil, want error", name)
		}
		if _, ok, err := s.Get(name); err == nil {
			t.Fatalf("Get(%q): err = nil, want error", name)
		} else if ok {
			t.Fatalf("Get(%q): ok = true, want false", name)
		}
		if err := s.Delete(name); err == nil {
			t.Fatalf("Delete(%q): err = nil, want error", name)
		}
	}
	// Nothing was written outside dir: the parent of dir must contain no .enc
	// file named "fuera.enc" (the ../fuera attempt would have landed there).
	parent := filepath.Dir(s.dir)
	if _, err := os.Stat(filepath.Join(parent, "fuera.enc")); err == nil {
		t.Fatalf("traversal wrote outside dir: %s/fuera.enc exists", parent)
	}
}
