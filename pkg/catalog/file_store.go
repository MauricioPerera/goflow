package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileStore is a Store backed by one JSON file per piece, in a directory
// on disk — real persistence across process restarts, using nothing but
// encoding/json and os (no new dependency, matching this project's
// zero-external-dependency-beyond-goja stance).
type FileStore struct {
	dir string
}

// NewFileStore returns a FileStore rooted at dir, creating it if it
// doesn't already exist.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("catalog: creating store directory: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

// path validates name and returns the on-disk file it maps to. A
// Definition's Name may be agent-authored — never trust it as a raw path
// segment. Rejects anything that isn't a plain identifier-like string
// rather than trying to "clean" a path-traversal attempt.
func (s *FileStore) path(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("catalog: piece name is empty")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("catalog: invalid piece name %q", name)
	}
	return filepath.Join(s.dir, name+".json"), nil
}

func (s *FileStore) Save(def Definition) error {
	p, err := s.path(def.Name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(def, "", "  ")
	if err != nil {
		return fmt.Errorf("catalog: encoding %q: %w", def.Name, err)
	}

	// Atomic write: encode into a temp file in the SAME directory as the
	// final file, then os.Rename it into place. os.Rename is atomic within
	// a single filesystem on both POSIX and Windows, so a reader (or a
	// process crash) never observes a half-written final file — only the
	// previous version or the fully-written new one. Any failure below
	// removes the temp file so no ".tmp-*" orphan is left behind.
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("catalog: creating temp file for %q: %w", def.Name, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("catalog: writing %q: %w", def.Name, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("catalog: closing temp file for %q: %w", def.Name, err)
	}
	// os.CreateTemp uses 0o600; match the 0o644 mode the final file used
	// to have, since os.Rename carries the source's mode over.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		cleanup()
		return fmt.Errorf("catalog: setting mode for %q: %w", def.Name, err)
	}
	if err := os.Rename(tmpPath, p); err != nil {
		cleanup()
		return fmt.Errorf("catalog: renaming %q into place: %w", def.Name, err)
	}
	return nil
}

func (s *FileStore) Get(name string) (Definition, bool, error) {
	p, err := s.path(name)
	if err != nil {
		return Definition{}, false, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Definition{}, false, nil
		}
		return Definition{}, false, fmt.Errorf("catalog: reading %q: %w", name, err)
	}
	var def Definition
	if err := json.Unmarshal(data, &def); err != nil {
		return Definition{}, false, fmt.Errorf("catalog: decoding %q: %w", name, err)
	}
	return def, true, nil
}

func (s *FileStore) List() ([]Definition, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing store directory: %w", err)
	}
	defs := make([]Definition, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("catalog: reading %q: %w", e.Name(), err)
		}
		var def Definition
		if err := json.Unmarshal(data, &def); err != nil {
			return nil, fmt.Errorf("catalog: decoding %q: %w", e.Name(), err)
		}
		defs = append(defs, def)
	}
	return defs, nil
}
