// Package store persists rbridge's own state behind a small interface, so the
// JSON files used in v0.1 can later be replaced by SQLite.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Store is a set of named collections of JSON documents.
type Store interface {
	// Get decodes the document into v. It reports false when it does not exist.
	Get(ctx context.Context, collection, key string, v any) (bool, error)
	Put(ctx context.Context, collection, key string, v any) error
	Delete(ctx context.Context, collection, key string) error
	// List returns the keys in a collection in sorted order.
	List(ctx context.Context, collection string) ([]string, error)
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func checkName(kind, s string) error {
	if !namePattern.MatchString(s) || strings.Contains(s, "..") {
		return fmt.Errorf("store: invalid %s %q", kind, s)
	}
	return nil
}

// JSONStore keeps each document in its own file: <root>/<collection>/<key>.json.
type JSONStore struct {
	root string
	mu   sync.Mutex
}

// NewJSONStore returns a store rooted at dir. The directory is created on first write.
func NewJSONStore(dir string) (*JSONStore, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("store: root must be absolute: %q", dir)
	}
	return &JSONStore{root: dir}, nil
}

func (s *JSONStore) path(collection, key string) (string, error) {
	if err := checkName("collection", collection); err != nil {
		return "", err
	}
	if err := checkName("key", key); err != nil {
		return "", err
	}
	return filepath.Join(s.root, collection, key+".json"), nil
}

func (s *JSONStore) Get(_ context.Context, collection, key string, v any) (bool, error) {
	p, err := s.path(collection, key)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("store: decode %s/%s: %w", collection, key, err)
	}
	return true, nil
}

// Put writes to a temporary file and renames it, so readers never see a partial document.
func (s *JSONStore) Put(_ context.Context, collection, key string, v any) error {
	p, err := s.path(collection, key)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+key+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *JSONStore) Delete(_ context.Context, collection, key string) error {
	p, err := s.path(collection, key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *JSONStore) List(_ context.Context, collection string) ([]string, error) {
	if err := checkName("collection", collection); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.root, collection))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		keys = append(keys, strings.TrimSuffix(name, ".json"))
	}
	slices.Sort(keys)
	return keys, nil
}

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu   sync.Mutex
	data map[string]map[string][]byte
}

func NewMemStore() *MemStore { return &MemStore{data: map[string]map[string][]byte{}} }

func (m *MemStore) Get(_ context.Context, c, k string, v any) (bool, error) {
	m.mu.Lock()
	b, ok := m.data[c][k]
	m.mu.Unlock()
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(b, v)
}

func (m *MemStore) Put(_ context.Context, c, k string, v any) error {
	if err := checkName("collection", c); err != nil {
		return err
	}
	if err := checkName("key", k); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data[c] == nil {
		m.data[c] = map[string][]byte{}
	}
	m.data[c][k] = b
	return nil
}

func (m *MemStore) Delete(_ context.Context, c, k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data[c], k)
	return nil
}

func (m *MemStore) List(_ context.Context, c string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.data[c] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, nil
}
