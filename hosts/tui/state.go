package tui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// stateFile is the small JSON file of UI state that outlives a session
// (tui-state.json in Options.StateDir): each feature owns a top-level key
// ("favorites", "recents", "theme", ...) and writes it by read-modify-write, so
// the keys of the others stay as they are. A nil stateFile keeps nothing.
type stateFile struct {
	path string
	mu   sync.Mutex
}

// stateFileName is the file inside Options.StateDir.
const stateFileName = "tui-state.json"

// newStateFile returns the state file of dir; "" means in-memory only (nil).
func newStateFile(dir string) *stateFile {
	if dir == "" {
		return nil
	}
	return &stateFile{path: filepath.Join(dir, stateFileName)}
}

// readAll returns the raw top-level keys; a missing or unreadable file is empty.
func (s *stateFile) readAll() map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if data, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(data, &out)
	}
	return out
}

// get decodes the value of key into v; a missing key leaves v alone.
func (s *stateFile) get(key string, v any) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.readAll()[key]
	if !ok {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// set replaces the value of key and keeps the other keys, writing the file atomically.
func (s *stateFile) set(key string, v any) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readAll()
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	all[key] = raw
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tui-state-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
