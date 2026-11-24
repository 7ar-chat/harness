package recipestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/7ar-chat/harness/core/recipes"
)

// fileVersion is the schema of the persisted file; Open refuses any other.
const fileVersion = 1

// File is a Mem persisted to a JSON file: every change rewrites the file atomically before it is applied, so
// memory never gets ahead of what a restart would restore. One process per file.
type File struct {
	*Mem
	path string
}

var _ recipes.Store = (*File)(nil)

type fileRecipe struct {
	recipes.Recipe
	History []recipes.Version `json:"history,omitempty"`
}

type fileData struct {
	Version int            `json:"version"`
	Recipes []fileRecipe   `json:"recipes"`
	Uses    map[string]int `json:"uses,omitempty"`
}

// Open returns a store persisted to path, loading the file if it exists. A missing file is an empty store; a file
// that cannot be read or parsed is an error naming the path and is left untouched. The parent directory is created.
func Open(path string, now func() time.Time) (*File, error) {
	f := &File{Mem: NewMem(now), path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("recipestore: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("recipestore: read %s: %w", path, err)
	default:
		if err := f.load(b); err != nil {
			return nil, fmt.Errorf("recipestore: load %s: %w", path, err)
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
	for _, fr := range d.Recipes {
		r := fr.Recipe
		switch r.Scope {
		case recipes.ScopeInstance, recipes.ScopeProject, recipes.ScopePersonal:
		default:
			return fmt.Errorf("recipe %q: unknown scope %q", r.ID, r.Scope)
		}
		if _, dup := f.st.recipes[r.ID]; dup || r.ID == "" {
			return fmt.Errorf("recipe id %q is empty or listed twice", r.ID)
		}
		f.st.recipes[r.ID] = r
		if len(fr.History) > 0 {
			f.st.history[r.ID] = fr.History
		}
	}
	for k, n := range d.Uses {
		f.st.uses[k] = n
	}
	return nil
}

// write replaces the file atomically: a 0600 temp file in the same directory, fsynced, renamed over the target, then
// the directory fsynced.
func (f *File) write(st state) error {
	d := fileData{Version: fileVersion, Recipes: make([]fileRecipe, 0, len(st.recipes)), Uses: st.uses}
	for id, r := range st.recipes {
		d.Recipes = append(d.Recipes, fileRecipe{Recipe: r, History: st.history[id]})
	}
	sort.Slice(d.Recipes, func(a, b int) bool { return d.Recipes[a].ID < d.Recipes[b].ID })
	b, err := json.Marshal(&d)
	if err != nil {
		return fmt.Errorf("recipestore: encode %s: %w", f.path, err)
	}
	tmp := f.path + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("recipestore: remove stale %s: %w", tmp, err)
	}
	t, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("recipestore: write %s: %w", tmp, err)
	}
	if _, err = t.Write(b); err == nil {
		err = t.Sync()
	}
	if cerr := t.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("recipestore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("recipestore: replace %s: %w", f.path, err)
	}
	if err := syncDir(filepath.Dir(f.path)); err != nil {
		return fmt.Errorf("recipestore: sync directory of %s: %w", f.path, err)
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
