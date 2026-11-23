package usagestore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/7ar-chat/harness/core/usage"
)

// File is a Mem persisted under a directory: the ledger as an append-only JSON-lines file (usage.jsonl), the caps as
// a small JSON file (caps.json) rewritten atomically. One process per directory.
type File struct {
	*Mem
	dir string
	led *os.File
}

type fileCap struct {
	Scope string  `json:"scope"`
	ID    string  `json:"id"`
	USD   float64 `json:"usd"`
}

var _ usage.Store = (*File)(nil)

// Open returns a store persisted under dir (created, 0700), loading what is there. A line of the ledger that cannot be
// read is an error naming the file: starting empty over it would drop the history the caps are judged by.
func Open(dir string) (*File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("usagestore: create %s: %w", dir, err)
	}
	f := &File{Mem: NewMem(), dir: dir}
	ledger := filepath.Join(dir, "usage.jsonl")
	if err := f.load(ledger); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "caps.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("usagestore: read caps: %w", err)
	default:
		var caps []fileCap
		if err := json.Unmarshal(b, &caps); err != nil {
			return nil, fmt.Errorf("usagestore: parse %s: %w", filepath.Join(dir, "caps.json"), err)
		}
		for _, c := range caps {
			f.caps[capKey(c.Scope, c.ID)] = c.USD
		}
	}
	led, err := os.OpenFile(ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("usagestore: open %s: %w", ledger, err)
	}
	f.led = led
	f.onAppend, f.onCaps = f.appendEntries, f.writeCaps
	return f, nil
}

func (f *File) load(path string) error {
	fh, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("usagestore: open %s: %w", path, err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e usage.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return fmt.Errorf("usagestore: %s line %d: %w", path, n, err)
		}
		f.entries = append(f.entries, e)
		f.runs[e.RunID] = true
	}
	return sc.Err()
}

func (f *File) appendEntries(entries []usage.Entry) error {
	var b []byte
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		b = append(append(b, line...), '\n')
	}
	if _, err := f.led.Write(b); err != nil {
		return fmt.Errorf("usagestore: append: %w", err)
	}
	return f.led.Sync()
}

func (f *File) writeCaps(caps map[string]float64) error {
	list := make([]fileCap, 0, len(caps))
	for k, v := range caps {
		scope, id, _ := strings.Cut(k, "\x00")
		list = append(list, fileCap{Scope: scope, ID: id, USD: v})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Scope+"\x00"+list[i].ID < list[j].Scope+"\x00"+list[j].ID
	})
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	path := filepath.Join(f.dir, "caps.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("usagestore: write caps: %w", err)
	}
	return os.Rename(tmp, path)
}

// Close releases the ledger file.
func (f *File) Close() error { return f.led.Close() }
