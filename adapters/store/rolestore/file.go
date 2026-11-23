package rolestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/7ar-chat/harness/core/roles"
)

// fileVersion is the schema of the persisted file; Open refuses any other.
const fileVersion = 1

// File is a Mem persisted to a JSON file: every change rewrites the file atomically before it is applied, so
// memory never gets ahead of what a restart would restore. One process per file.
type File struct {
	*Mem
	path string
}

var _ roles.Store = (*File)(nil)

type fileRole struct {
	roles.Role
	History []roles.Version `json:"history,omitempty"`
}

type fileData struct {
	Version int        `json:"version"`
	Roles   []fileRole `json:"roles"`
}

// Open returns a store persisted to path, loading the file if it exists. A missing file is an empty store; a
// file that cannot be read or parsed is an error naming the path and is left untouched. The parent directory is
// created (0700).
func Open(path string, now func() time.Time) (*File, error) {
	f := &File{Mem: NewMem(now), path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("rolestore: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("rolestore: read %s: %w", path, err)
	default:
		if err := f.load(b); err != nil {
			return nil, fmt.Errorf("rolestore: load %s: %w", path, err)
		}
	}
	f.Mem.save = f.write
	return f, nil
}

func (f *File) load(b []byte) error {
	var d fileData
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	if d.Version != fileVersion {
		return fmt.Errorf("unsupported version %d (want %d)", d.Version, fileVersion)
	}
	for _, fr := range d.Roles {
		r := fr.Role
		if r.Scope != roles.ScopeProject && r.Scope != roles.ScopeUser {
			return fmt.Errorf("role %q: unknown scope %q", r.Name, r.Scope)
		}
		k := key(r.Scope, r.ScopeID, r.Name)
		if _, dup := f.st.roles[k]; dup {
			return fmt.Errorf("role %q is listed twice", r.Name)
		}
		f.st.roles[k] = r
		if len(fr.History) > 0 {
			f.st.history[k] = fr.History
		}
	}
	return nil
}

// write replaces the file atomically: a 0600 temp file in the same directory, fsynced, renamed over the target,
// then the directory fsynced.
func (f *File) write(st state) error {
	d := fileData{Version: fileVersion, Roles: make([]fileRole, 0, len(st.roles))}
	for k, r := range st.roles {
		d.Roles = append(d.Roles, fileRole{Role: r, History: st.history[k]})
	}
	sort.Slice(d.Roles, func(a, b int) bool {
		return key(d.Roles[a].Scope, d.Roles[a].ScopeID, d.Roles[a].Name) < key(d.Roles[b].Scope, d.Roles[b].ScopeID, d.Roles[b].Name)
	})
	b, err := json.Marshal(&d)
	if err != nil {
		return fmt.Errorf("rolestore: encode %s: %w", f.path, err)
	}
	tmp := f.path + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rolestore: remove stale %s: %w", tmp, err)
	}
	t, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("rolestore: write %s: %w", tmp, err)
	}
	if _, err = t.Write(b); err == nil {
		err = t.Sync()
	}
	if cerr := t.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rolestore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rolestore: replace %s: %w", f.path, err)
	}
	if err := syncDir(filepath.Dir(f.path)); err != nil {
		return fmt.Errorf("rolestore: sync directory of %s: %w", f.path, err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.EINVAL) {
		return nil
	}
	return err
}
