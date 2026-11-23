package threadstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/7ar-chat/harness/core/store"
)

// File is a Mem persisted to a JSON file: every change rewrites the file atomically before it is applied. One
// process per file.
type File struct {
	*Mem
	path string
}

var _ store.ThreadStateStore = (*File)(nil)

// OpenFile returns a store persisted to path (written on the first change). A file that cannot be parsed is an
// error naming the path and is left untouched.
func OpenFile(path string) (*File, error) {
	f := &File{Mem: NewMem(), path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("threadstate: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("threadstate: read %s: %w", path, err)
	default:
		if err := json.Unmarshal(b, &f.Mem.docs); err != nil {
			return nil, fmt.Errorf("threadstate: load %s: %w", path, err)
		}
		if f.Mem.docs == nil {
			f.Mem.docs = map[string]map[string]json.RawMessage{}
		}
	}
	f.Mem.save = f.write
	return f, nil
}

func (f *File) write(docs map[string]map[string]json.RawMessage) error {
	b, err := json.Marshal(docs)
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("threadstate: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return fmt.Errorf("threadstate: replace %s: %w", f.path, err)
	}
	return nil
}
