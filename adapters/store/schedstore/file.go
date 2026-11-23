package schedstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/7ar-chat/harness/core/schedule"
)

// File is a Mem persisted to a JSON file, rewritten atomically after every change. One process per file.
type File struct {
	*Mem
	path string
}

var _ schedule.Store = (*File)(nil)

// Open returns a store persisted to path, loading the file if it exists. A file that cannot be parsed is an error
// naming the path and is left untouched: starting empty over it would drop every schedule.
func Open(path string) (*File, error) {
	f := &File{Mem: NewMem(), path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("schedstore: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("schedstore: read %s: %w", path, err)
	default:
		var st state
		if err := json.Unmarshal(b, &st); err != nil {
			return nil, fmt.Errorf("schedstore: parse %s: %w", path, err)
		}
		for i := range st.Schedules {
			s := st.Schedules[i]
			f.schedules[s.ID] = &s
		}
		for _, r := range st.Runs {
			f.runs[r.ScheduleID] = append(f.runs[r.ScheduleID], r)
		}
	}
	f.persist = f.write
	return f, nil
}

func (f *File) write(st state) error {
	b, err := json.Marshal(&st)
	if err != nil {
		return fmt.Errorf("schedstore: encode: %w", err)
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("schedstore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("schedstore: replace %s: %w", f.path, err)
	}
	return nil
}
