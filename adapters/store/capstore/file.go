package capstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/7ar-chat/harness/core/capabilities"
)

// fileVersion is the schema of the persisted file; Open refuses any other.
const fileVersion = 1

// File is a Mem persisted to a JSON file: every change rewrites the file atomically before it is applied,
// so memory never gets ahead of what a restart would restore. One process per file.
type File struct {
	*Mem
	path string
}

var _ capabilities.Store = (*File)(nil)

// fileInstall is Install on disk: Install hides Secrets from JSON, the file must keep them (sealed).
type fileInstall struct {
	capabilities.Install
	Secrets []byte `json:"secrets,omitempty"`
}

type fileData struct {
	Version  int                  `json:"version"`
	Catalog  []capabilities.Entry `json:"catalog"`
	Installs []fileInstall        `json:"installs"`
}

// Open returns a store persisted to path, loading the file if it exists. A missing file is an empty store;
// a file that cannot be read or parsed is an error naming the path and is left untouched, because starting
// empty over it would silently drop the installs, and their keys, of every project. The parent directory
// is created (0700).
func Open(path string, now func() time.Time) (*File, error) {
	f := &File{Mem: NewMem(now), path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("capstore: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("capstore: read %s: %w", path, err)
	default:
		if err := f.load(b); err != nil {
			return nil, fmt.Errorf("capstore: load %s: %w", path, err)
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
	for _, e := range d.Catalog {
		if e.ID == "" {
			return errors.New("catalog entry without an id")
		}
		f.st.catalog[e.ID] = e
	}
	names := map[string]bool{}
	for _, fi := range d.Installs {
		in := fi.Install
		in.Secrets = fi.Secrets
		if in.ID == "" {
			return errors.New("install without an id")
		}
		if in.Scope != capabilities.ScopeProject && in.Scope != capabilities.ScopeUser {
			return fmt.Errorf("install %q: unknown scope %q", in.ID, in.Scope)
		}
		key := string(in.Scope) + "\x00" + in.ScopeID + "\x00" + in.Name
		if _, dup := f.st.installs[in.ID]; dup || names[key] {
			return fmt.Errorf("install %q (%q) is listed twice", in.ID, in.Name)
		}
		names[key] = true
		f.st.installs[in.ID] = cloneInstall(in)
	}
	return nil
}

// write replaces the file atomically: a 0600 temp file in the same directory, fsynced, renamed over the
// target, then the directory fsynced so the rename itself survives a power cut.
func (f *File) write(st state) error {
	d := fileData{Version: fileVersion, Catalog: make([]capabilities.Entry, 0, len(st.catalog)), Installs: make([]fileInstall, 0, len(st.installs))}
	for _, e := range st.catalog {
		d.Catalog = append(d.Catalog, e)
	}
	sort.Slice(d.Catalog, func(a, b int) bool { return d.Catalog[a].ID < d.Catalog[b].ID })
	for _, in := range st.installs {
		d.Installs = append(d.Installs, fileInstall{Install: in, Secrets: in.Secrets})
	}
	sort.Slice(d.Installs, func(a, b int) bool { return d.Installs[a].ID < d.Installs[b].ID })
	b, err := json.Marshal(&d)
	if err != nil {
		return fmt.Errorf("capstore: encode %s: %w", f.path, err)
	}
	tmp := f.path + ".tmp"
	// A leftover from a crash may have another mode; start from nothing.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("capstore: remove stale %s: %w", tmp, err)
	}
	t, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("capstore: write %s: %w", tmp, err)
	}
	if _, err = t.Write(b); err == nil {
		err = t.Sync()
	}
	if cerr := t.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("capstore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("capstore: replace %s: %w", f.path, err)
	}
	if err := syncDir(filepath.Dir(f.path)); err != nil {
		return fmt.Errorf("capstore: sync directory of %s: %w", f.path, err)
	}
	return nil
}

// syncDir fsyncs a directory; filesystems that cannot (EINVAL) are not an error.
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
