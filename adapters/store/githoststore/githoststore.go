// Package githoststore stores the code hosts an administrator registered (githost.Registry): in memory, in a JSON
// file, and in Postgres (migration 0800).
package githoststore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/githost"
)

// Mem is a Registry in memory. With a save function (File sets one) every change is written before it is applied.
type Mem struct {
	mu   sync.Mutex
	m    map[string]githost.Entry
	now  func() time.Time
	save func(map[string]githost.Entry) error
}

var _ githost.Registry = (*Mem)(nil)

// NewMem returns an empty registry.
func NewMem(now func() time.Time) *Mem {
	if now == nil {
		now = time.Now
	}
	return &Mem{m: map[string]githost.Entry{}, now: now}
}

// List implements githost.Registry.
func (s *Mem) List(context.Context) ([]githost.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]githost.Entry, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Host.Name < out[b].Host.Name })
	return out, nil
}

// Get implements githost.Registry.
func (s *Mem) Get(_ context.Context, name string) (githost.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[name]; ok {
		return e, nil
	}
	return githost.Entry{}, githost.ErrUnknownHost
}

// Put implements githost.Registry.
func (s *Mem) Put(_ context.Context, e githost.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.UpdatedAt = s.now()
	return s.apply(func(m map[string]githost.Entry) { m[e.Host.Name] = e })
}

// Delete implements githost.Registry.
func (s *Mem) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apply(func(m map[string]githost.Entry) { delete(m, name) })
}

// apply runs f on a copy, persists the copy, and only then swaps it in.
func (s *Mem) apply(f func(map[string]githost.Entry)) error {
	next := make(map[string]githost.Entry, len(s.m)+1)
	for k, v := range s.m {
		next[k] = v
	}
	f(next)
	if s.save != nil {
		if err := s.save(next); err != nil {
			return err
		}
	}
	s.m = next
	return nil
}

// File is a Mem persisted to a JSON file (0600, replaced atomically). One process per file.
type File struct {
	*Mem
	path string
}

var _ githost.Registry = (*File)(nil)

type fileData struct {
	Version int             `json:"version"`
	Hosts   []githost.Entry `json:"hosts"`
}

// Open loads path (a missing file is an empty registry).
func Open(path string, now func() time.Time) (*File, error) {
	f := &File{Mem: NewMem(now), path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("githoststore: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("githoststore: read %s: %w", path, err)
	default:
		var d fileData
		if err := json.Unmarshal(b, &d); err != nil || d.Version != 1 {
			return nil, fmt.Errorf("githoststore: %s is not a version 1 registry", path)
		}
		for _, e := range d.Hosts {
			f.m[e.Host.Name] = e
		}
	}
	f.Mem.save = f.write
	return f, nil
}

func (f *File) write(m map[string]githost.Entry) error {
	d := fileData{Version: 1, Hosts: make([]githost.Entry, 0, len(m))}
	for _, e := range m {
		d.Hosts = append(d.Hosts, e)
	}
	sort.Slice(d.Hosts, func(a, b int) bool { return d.Hosts[a].Host.Name < d.Hosts[b].Host.Name })
	b, err := json.Marshal(&d)
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("githoststore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("githoststore: replace %s: %w", f.path, err)
	}
	return nil
}

// PG is a Registry on Postgres (table of migration 0800, applied by pgstore).
type PG struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

var _ githost.Registry = (*PG)(nil)

// NewPG returns a registry on pool, whose search_path names the schema the migrations ran in.
func NewPG(pool *pgxpool.Pool, now func() time.Time) *PG {
	if now == nil {
		now = time.Now
	}
	return &PG{pool: pool, now: now}
}

func scanEntry(row pgx.Row) (githost.Entry, error) {
	var e githost.Entry
	var doc []byte
	if err := row.Scan(&doc, &e.SealedToken, &e.UpdatedAt); err != nil {
		return e, err
	}
	return e, json.Unmarshal(doc, &e.Host)
}

// List implements githost.Registry.
func (p *PG) List(ctx context.Context) ([]githost.Entry, error) {
	rows, err := p.pool.Query(ctx, `SELECT host, token, updated_at FROM work_git_hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []githost.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get implements githost.Registry.
func (p *PG) Get(ctx context.Context, name string) (githost.Entry, error) {
	e, err := scanEntry(p.pool.QueryRow(ctx, `SELECT host, token, updated_at FROM work_git_hosts WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return githost.Entry{}, githost.ErrUnknownHost
	}
	return e, err
}

// Put implements githost.Registry.
func (p *PG) Put(ctx context.Context, e githost.Entry) error {
	doc, err := json.Marshal(e.Host)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO work_git_hosts (name, host, token, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET host = EXCLUDED.host, token = EXCLUDED.token, updated_at = EXCLUDED.updated_at`,
		e.Host.Name, doc, e.SealedToken, p.now())
	return err
}

// Delete implements githost.Registry.
func (p *PG) Delete(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM work_git_hosts WHERE name = $1`, name)
	return err
}
