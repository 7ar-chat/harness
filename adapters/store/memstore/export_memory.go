package memstore

import "github.com/GhaziBenDahmane/harness/core/store"

// ExportMemory returns every memory entry (clones) and the users whose memory
// is disabled; a persistent store (store/filestore) journals them.
func (s *Store) ExportMemory() (entries []*store.MemoryEntry, disabled []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memInit()
	for _, e := range s.mem.entries {
		entries = append(entries, e.Clone())
	}
	for login, off := range s.mem.disabled {
		if off {
			disabled = append(disabled, login)
		}
	}
	return entries, disabled
}

// ImportMemory restores what ExportMemory returned (replacing the memory).
func (s *Store) ImportMemory(entries []*store.MemoryEntry, disabled []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memInit()
	s.mem.entries = nil
	for _, e := range entries {
		s.mem.entries = append(s.mem.entries, e.Clone())
	}
	s.mem.disabled = map[string]bool{}
	for _, l := range disabled {
		s.mem.disabled[l] = true
	}
}
