package httpserver

import (
	"context"
	"net/http"
	"strings"

	"github.com/GhaziBenDahmane/harness/features/memory"
	"github.com/GhaziBenDahmane/harness/store"
)

// Memory routes. Without ?project the memory is the user's own global one;
// with ?project=ID (team mode) it is the project's, shared by its members
// (viewers read, members write). Agents running in a project thread read and
// write the project memory; in personal threads, the user's.
//
//	GET    /api/memory[?project=]                    → {entries:[…], enabled}
//	POST   /api/memory[?project=] {title,content,tags,path}  → 201 entry
//	PATCH  /api/memory/{mid}[?project=] {title,content,tags}
//	DELETE /api/memory/{mid}[?project=]
//	POST   /api/memory/{mid}/confirm|reject[?project=]       keep or drop what the agent proposed

// ProjectMemoryPrefix starts the memory key of a project.
const ProjectMemoryPrefix = "project:"

// MemoryKey is harness.Options.MemoryKey for team mode: project threads use the
// project's memory.
func MemoryKey(team store.TeamStore) func(ctx context.Context, login, threadID string) string {
	return func(ctx context.Context, login, threadID string) string {
		if meta, err := team.GetThreadMeta(ctx, threadID); err == nil && meta.ProjectID != "" {
			return ProjectMemoryPrefix + meta.ProjectID
		}
		return login
	}
}

func (s *server) routeMemory() {
	s.route("GET /memory", s.listMemory)
	s.route("POST /memory", s.createMemory)
	s.route("PATCH /memory/{mid}", s.patchMemory)
	s.route("DELETE /memory/{mid}", s.deleteMemory)
	s.route("POST /memory/{mid}/confirm", s.confirmMemory)
	s.route("POST /memory/{mid}/reject", s.rejectMemory)
}

// memoryKey resolves the memory the request is about and checks access.
func (s *server) memoryKey(w http.ResponseWriter, r *http.Request, c Caller, write bool) (string, bool) {
	if s.h.Memory == nil {
		writeError(w, http.StatusNotImplemented, "not_supported", "This server has no memory feature.")
		return "", false
	}
	pid := r.URL.Query().Get("project")
	if pid == "" {
		return c.Actor, true
	}
	if s.team == nil {
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
		return "", false
	}
	need := store.RoleViewer
	if write {
		need = store.RoleMember
	}
	if _, err := s.requireProject(r.Context(), pid, c.Actor, need); err != nil {
		s.failAccess(w, err)
		return "", false
	}
	return ProjectMemoryPrefix + pid, true
}

func entryView(e *store.MemoryEntry) map[string]any {
	m := memory.PublicEntry(e)
	m["content"] = e.Content
	if e.Proposal != nil {
		m["proposal"] = e.Proposal
	}
	return m
}

func memoryFail(w http.ResponseWriter, e *memory.Error) {
	status := http.StatusBadRequest
	switch e.Code {
	case "memory_not_found":
		status = http.StatusNotFound
	case "memory_conflict", "memory_not_pending", "memory_path_exists", "memory_limit_reached":
		status = http.StatusConflict
	case "memory_store_failed":
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, map[string]any{"error": e.Payload()})
}

func (s *server) listMemory(w http.ResponseWriter, r *http.Request, c Caller) {
	key, ok := s.memoryKey(w, r, c, false)
	if !ok {
		return
	}
	entries, err := s.h.Store.ListMemoryEntries(r.Context(), key, store.MemoryActive, store.MemoryPending)
	if err != nil {
		fail(w, err)
		return
	}
	enabled, _ := s.h.Store.MemoryEnabled(r.Context(), key)
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryView(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "enabled": enabled})
}

func (s *server) createMemory(w http.ResponseWriter, r *http.Request, c Caller) {
	key, ok := s.memoryKey(w, r, c, true)
	if !ok {
		return
	}
	var body struct {
		Title   *string  `json:"title"`
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
		Path    string   `json:"path"`
	}
	if !decode(w, r, &body) {
		return
	}
	meta := map[string]any{"createdBy": c.Actor}
	res := s.h.Memory.Create(r.Context(), memory.Writer{Login: key, Source: store.MemorySourceUser},
		memory.CreateInput{Content: body.Content, Title: body.Title, Tags: body.Tags, Path: body.Path, Metadata: meta})
	if res.IsError {
		memoryFail(w, &memory.Error{Code: errStr(res.Data, "code"), Message: errStr(res.Data, "message")})
		return
	}
	for _, ch := range res.Changes {
		if ch.Entry != nil {
			s.noteMemory(r, c, "memory.create", ch.Entry.ID)
			writeJSON(w, http.StatusCreated, entryView(ch.Entry))
			return
		}
	}
	writeJSON(w, http.StatusCreated, res.Data)
}

func (s *server) noteMemory(r *http.Request, c Caller, action, id string) {
	if pid := r.URL.Query().Get("project"); pid != "" {
		s.note(Caller{Actor: c.Actor, ProjectID: pid}, "", action, map[string]any{"memory": id}, FeedEvent{Type: "memory.changed"})
	}
}

func (s *server) patchMemory(w http.ResponseWriter, r *http.Request, c Caller) {
	key, ok := s.memoryKey(w, r, c, true)
	if !ok {
		return
	}
	var body struct {
		Title   *string   `json:"title"`
		Content *string   `json:"content"`
		Tags    *[]string `json:"tags"`
	}
	if !decode(w, r, &body) {
		return
	}
	res := s.h.Memory.Update(r.Context(), memory.Writer{Login: key, Source: store.MemorySourceUser},
		memory.UpdateInput{ID: r.PathValue("mid"), Title: body.Title, Content: body.Content, Tags: body.Tags})
	if res.IsError {
		memoryFail(w, &memory.Error{Code: errStr(res.Data, "code"), Message: errStr(res.Data, "message")})
		return
	}
	s.noteMemory(r, c, "memory.update", r.PathValue("mid"))
	e, err := s.h.Store.GetMemoryEntry(r.Context(), key, r.PathValue("mid"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entryView(e))
}

func (s *server) deleteMemory(w http.ResponseWriter, r *http.Request, c Caller) {
	key, ok := s.memoryKey(w, r, c, true)
	if !ok {
		return
	}
	res := s.h.Memory.Delete(r.Context(), memory.Writer{Login: key, Source: store.MemorySourceUser}, r.PathValue("mid"), "", nil, "")
	if res.IsError {
		memoryFail(w, &memory.Error{Code: errStr(res.Data, "code"), Message: errStr(res.Data, "message")})
		return
	}
	s.noteMemory(r, c, "memory.delete", r.PathValue("mid"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) confirmMemory(w http.ResponseWriter, r *http.Request, c Caller) {
	s.resolveMemory(w, r, c, true)
}

func (s *server) rejectMemory(w http.ResponseWriter, r *http.Request, c Caller) {
	s.resolveMemory(w, r, c, false)
}

func (s *server) resolveMemory(w http.ResponseWriter, r *http.Request, c Caller, keep bool) {
	key, ok := s.memoryKey(w, r, c, true)
	if !ok {
		return
	}
	var e *store.MemoryEntry
	var merr *memory.Error
	if keep {
		e, merr = s.h.Memory.Confirm(r.Context(), key, r.PathValue("mid"), memory.Expect{})
	} else {
		e, merr = s.h.Memory.Reject(r.Context(), key, r.PathValue("mid"), memory.Expect{})
	}
	if merr != nil {
		memoryFail(w, merr)
		return
	}
	action := "memory.reject"
	if keep {
		action = "memory.confirm"
	}
	s.noteMemory(r, c, action, r.PathValue("mid"))
	if e == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, entryView(e))
}

func errStr(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	if err, ok := m["error"].(map[string]any); ok {
		m = err
	}
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}
