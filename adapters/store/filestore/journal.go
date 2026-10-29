package filestore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// record is one journal line: {"t":type,"k":key,"v":value}. Records are
// idempotent upserts ("the last record of a (t,k) wins"), except "events" and
// "msg" which add rows; replay never depends on the order of unrelated
// records and a crash can only lose a suffix.
type record struct {
	T string          `json:"t"`
	K string          `json:"k,omitempty"`
	V json.RawMessage `json:"v"`
}

// journal is the append-only file. A batch is written with a single write(2)
// on an O_APPEND descriptor, so a crash tears at most the last line.
type journal struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	always bool
	dirty  bool
	size   int64
}

func openJournal(path string, always bool) (*journal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &journal{f: f, path: path, always: always, size: st.Size()}, nil
}

func (j *journal) write(buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return ErrClosed
	}
	n, err := j.f.Write(buf)
	j.size += int64(n)
	if err != nil {
		return fmt.Errorf("filestore: journal write: %w", err)
	}
	j.dirty = true
	if j.always {
		return j.syncLocked()
	}
	return nil
}

func (j *journal) syncLocked() error {
	if !j.dirty || j.f == nil {
		return nil
	}
	j.dirty = false
	return j.f.Sync()
}

func (j *journal) sync() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.syncLocked()
}

func (j *journal) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.syncLocked()
	if cerr := j.f.Close(); err == nil {
		err = cerr
	}
	j.f = nil
	return err
}

// ErrCorrupt means the journal is damaged somewhere other than its tail.
var ErrCorrupt = errors.New("filestore: journal corrupt (not a torn tail)")

// scan reads a journal file, calling fn for every valid record. A torn tail
// (a final line without newline, or an invalid final line) is cut off by
// truncating the file; an invalid line followed by valid ones is ErrCorrupt.
func scan(path string, fn func(record) error) (records int, truncated bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var good, off int64
	badAt := int64(-1)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			complete := line[len(line)-1] == '\n'
			var rec record
			if complete && json.Unmarshal(bytes.TrimSpace(line), &rec) == nil && rec.T != "" {
				if badAt >= 0 {
					return records, false, fmt.Errorf("%w: invalid line at offset %d", ErrCorrupt, badAt)
				}
				if err := fn(rec); err != nil {
					return records, false, err
				}
				records++
				good = off + int64(len(line))
			} else if badAt < 0 {
				badAt = off
			}
			off += int64(len(line))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return records, false, rerr
		}
	}
	if badAt >= 0 {
		if err := f.Truncate(good); err != nil {
			return records, false, err
		}
		if err := f.Sync(); err != nil {
			return records, false, err
		}
		truncated = true
	}
	return records, truncated, nil
}
