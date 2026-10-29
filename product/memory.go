package product

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/features/memory"
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

// UserMemoryPrefix starts the key of a user's private layer inside a project ("pu:<project>:<login>").
const UserMemoryPrefix = "pu:"

// MemoryShared is harness.Options.MemoryShared for team mode: a project's memory is shared.
func MemoryShared(key string) bool { return strings.HasPrefix(key, ProjectMemoryPrefix) }

// MemoryUserKey is harness.Options.MemoryUserKey for team mode: in a project thread, the user who
// asked (the author of the thread's last message that is not a steer) has a private layer.
func MemoryUserKey(team store.TeamStore, messages func(ctx context.Context, threadID string) ([]*store.Message, error)) func(ctx context.Context, login, threadID string) string {
	return func(ctx context.Context, login, threadID string) string {
		meta, err := team.GetThreadMeta(ctx, threadID)
		if err != nil || meta.ProjectID == "" {
			return ""
		}
		msgs, err := messages(ctx, threadID)
		if err != nil {
			return ""
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if m.Role != store.RoleUser || m.IsSteer() {
				continue
			}
			who := m.AuthorLogin
			if m.AuthorKind == store.AuthorSupervisor || m.AuthorKind == store.AuthorSystem {
				who = m.OnBehalfOf
			}
			if who == "" {
				return ""
			}
			return UserMemoryPrefix + meta.ProjectID + ":" + who
		}
		return ""
	}
}

func (s *server) routeMemory() {
	s.Route("GET /memory/stats", s.memoryStats)
	s.Route("GET /projects/{pid}/memory/export", s.exportMemory)
	s.Route("POST /projects/{pid}/memory/import-agents", s.importAgentsMD)
	s.Route("GET /memory", s.listMemory)
	s.Route("POST /memory", s.createMemory)
	s.Route("PATCH /memory/{mid}", s.patchMemory)
	s.Route("DELETE /memory/{mid}", s.deleteMemory)
	s.Route("POST /memory/{mid}/confirm", s.confirmMemory)
	s.Route("POST /memory/{mid}/reject", s.rejectMemory)
}

// memoryKey resolves the memory the request is about and checks access.
func (s *server) memoryKey(w http.ResponseWriter, r *http.Request, c Caller, write bool) (string, bool) {
	if s.H.Memory == nil {
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
		s.FailAccess(w, err)
		return "", false
	}
	if r.URL.Query().Get("mine") == "1" { // the user's own layer inside the project: the key comes from the session, never the request
		return UserMemoryPrefix + pid + ":" + c.Actor, true
	}
	return ProjectMemoryPrefix + pid, true
}

func entryView(e *store.MemoryEntry) map[string]any {
	m := memory.PublicEntry(e)
	m["content"] = e.Content
	if e.SourceThreadID != "" {
		m["sourceThreadId"] = e.SourceThreadID
	}
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
	entries, err := s.H.Store.ListMemoryEntries(r.Context(), key, store.MemoryActive, store.MemoryPending)
	if err != nil {
		fail(w, err)
		return
	}
	enabled, _ := s.H.Store.MemoryEnabled(r.Context(), key)
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
		Pinned  bool     `json:"pinned"`
	}
	if !decode(w, r, &body) {
		return
	}
	meta := map[string]any{"createdBy": c.Actor}
	if body.Pinned {
		meta["pinned"] = true
	}
	res := s.H.Memory.Create(r.Context(), memory.Writer{Login: key, Source: store.MemorySourceUser},
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
		Pinned  *bool     `json:"pinned"`
	}
	if !decode(w, r, &body) {
		return
	}
	in := memory.UpdateInput{ID: r.PathValue("mid"), Title: body.Title, Content: body.Content, Tags: body.Tags}
	if body.Pinned != nil {
		in.Metadata = map[string]any{"pinned": *body.Pinned}
	}
	res := s.H.Memory.Update(r.Context(), memory.Writer{Login: key, Source: store.MemorySourceUser}, in)
	if res.IsError {
		memoryFail(w, &memory.Error{Code: errStr(res.Data, "code"), Message: errStr(res.Data, "message")})
		return
	}
	s.noteMemory(r, c, "memory.update", r.PathValue("mid"))
	e, err := s.H.Store.GetMemoryEntry(r.Context(), key, r.PathValue("mid"))
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
	res := s.H.Memory.Delete(r.Context(), memory.Writer{Login: key, Source: store.MemorySourceUser}, r.PathValue("mid"), "", nil, "")
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
		e, merr = s.H.Memory.Confirm(r.Context(), key, r.PathValue("mid"), memory.Expect{})
	} else {
		e, merr = s.H.Memory.Reject(r.Context(), key, r.PathValue("mid"), memory.Expect{})
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

// memoryStats describes a memory and what the memory service did in this process.
//
//	GET /api/memory/stats[?project=ID[&mine=1]] → {active, pending, pinned, byKind, embeddings:{enabled,model,vectors}, service:{recall…, extraction…}}
func (s *server) memoryStats(w http.ResponseWriter, r *http.Request, c Caller) {
	key, ok := s.memoryKey(w, r, c, false)
	if !ok {
		return
	}
	entries, err := s.H.Store.ListMemoryEntries(r.Context(), key, store.MemoryActive, store.MemoryPending)
	if err != nil {
		fail(w, err)
		return
	}
	out := map[string]any{"active": 0, "pending": 0, "pinned": 0}
	kinds := map[string]int{}
	active, pending, pinned := 0, 0, 0
	for _, e := range entries {
		if e.Status == store.MemoryPending || e.Proposal != nil {
			pending++
		}
		if e.Status != store.MemoryActive {
			continue
		}
		active++
		if memory.IsPinned(e) {
			pinned++
		}
		parts := strings.Split(e.Path, "/")
		if len(parts) >= 3 && parts[0] == "memories" {
			kinds[parts[len(parts)-2]]++
		}
	}
	out["active"], out["pending"], out["pinned"], out["byKind"] = active, pending, pinned, kinds
	emb := map[string]any{"enabled": false}
	if o := s.H.Memory.Options(); o.Embedder != nil {
		emb["enabled"], emb["model"] = true, o.Embedder.Model()
		if vs, ok := s.H.Store.(store.MemoryEmbeddingStore); ok {
			if v, err := vs.GetMemoryEmbeddings(r.Context(), key); err == nil {
				emb["vectors"] = len(v)
			}
		}
	}
	out["embeddings"], out["service"] = emb, s.H.Memory.Stats()
	writeJSON(w, http.StatusOK, out)
}

// exportMemory renders the project's pinned and active memories as markdown, to paste into AGENTS.md.
func (s *server) exportMemory(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.H.Memory == nil || s.team == nil {
		writeError(w, http.StatusNotImplemented, "not_supported", "Not available.")
		return
	}
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	entries, err := s.H.Store.ListMemoryEntries(r.Context(), ProjectMemoryPrefix+p.ID, store.MemoryActive)
	if err != nil {
		fail(w, err)
		return
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — project memory\n\n", p.Name)
	section := func(title string, keep func(*store.MemoryEntry) bool) {
		first := true
		for _, e := range entries {
			if !keep(e) || e.Path == agentsMDPath {
				continue
			}
			if first {
				fmt.Fprintf(&b, "## %s\n\n", title)
				first = false
			}
			body := strings.TrimSpace(e.Content)
			fmt.Fprintf(&b, "- **%s** — %s\n", firstLine(e.Title, e.Path), strings.ReplaceAll(body, "\n", "\n  "))
		}
		if !first {
			b.WriteString("\n")
		}
	}
	section("Always apply", memory.IsPinned)
	for _, k := range memory.SemanticKinds {
		kind := k
		section(strings.ToUpper(kind[:1])+kind[1:], func(e *store.MemoryEntry) bool {
			return !memory.IsPinned(e) && strings.Contains(e.Path, "/"+kind+"/")
		})
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="MEMORY.md"`)
	_, _ = w.Write([]byte(b.String()))
}

func firstLine(title, path string) string {
	if strings.TrimSpace(title) != "" {
		return strings.TrimSpace(title)
	}
	return path
}

// agentsMDPath is where the repository's AGENTS.md lives in the project memory.
const agentsMDPath = "memories/core/agents-md.md"

// importAgentsMD reads the repository's AGENTS.md (else CLAUDE.md) through the GitHub App and keeps it
// as a pinned entry of the project memory, so the file the humans maintain is what every agent starts with.
//
//	POST /api/projects/{pid}/memory/import-agents → the entry
func (s *server) importAgentsMD(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.H.Memory == nil || s.team == nil {
		writeError(w, http.StatusNotImplemented, "not_supported", "Not available.")
		return
	}
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	e, err := s.syncAgentsMD(r.Context(), p, c.Actor)
	switch {
	case errors.Is(err, errNoRepoFile):
		writeError(w, http.StatusNotFound, "not_found", "The repository has no AGENTS.md or CLAUDE.md (or the GitHub App cannot read it).")
	case err != nil:
		writeError(w, http.StatusBadGateway, "github_error", "The file could not be read from GitHub.")
	default:
		s.noteMemory(r, c, "memory.import_agents", e.ID)
		writeJSON(w, http.StatusOK, entryView(e))
	}
}

var errNoRepoFile = errors.New("no AGENTS.md in the repository")

// syncAgentsMD creates or refreshes the pinned AGENTS.md entry of a project.
func (s *server) syncAgentsMD(ctx context.Context, p *store.Project, actor string) (*store.MemoryEntry, error) {
	owner, repo, ok := ParseRepo(p.RepoURL)
	if !ok || s.opt.GitHub == nil || p.GitHubInstallation == 0 {
		return nil, errNoRepoFile
	}
	var data []byte
	var file string
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		b, err := s.opt.GitHub.FileContent(ctx, p.GitHubInstallation, owner, repo, name, p.DefaultBranch)
		if errors.Is(err, errNoRepoFile) {
			continue
		}
		if err != nil {
			return nil, err
		}
		data, file = b, name
		break
	}
	if data == nil {
		return nil, errNoRepoFile
	}
	content := strings.TrimSpace(string(data))
	if len(content) > 20000 {
		content = content[:20000] + "\n\n(truncated)"
	}
	key := ProjectMemoryPrefix + p.ID
	w := memory.Writer{Login: key, Source: store.MemorySourceUser}
	meta := map[string]any{"pinned": true, "source": "repo", "file": file, "createdBy": actor}
	if cur, err := s.H.Store.GetMemoryEntryByPath(ctx, key, agentsMDPath); err == nil && cur.Status != store.MemoryDeleted {
		if cur.Content == content {
			return cur, nil
		}
		res := s.H.Memory.Update(ctx, w, memory.UpdateInput{ID: cur.ID, Content: &content, Metadata: meta})
		if res.IsError {
			return nil, fmt.Errorf("updating AGENTS.md: %v", res.Data)
		}
		return s.H.Store.GetMemoryEntry(ctx, key, cur.ID)
	}
	title := file + " (from the repository)"
	res := s.H.Memory.Create(ctx, w, memory.CreateInput{Content: content, Title: &title, Path: agentsMDPath, Metadata: meta, Tags: []string{"pinned"}})
	if res.IsError {
		return nil, fmt.Errorf("creating AGENTS.md: %v", res.Data)
	}
	for _, ch := range res.Changes {
		if ch.Entry != nil {
			return ch.Entry, nil
		}
	}
	return nil, errors.New("AGENTS.md not stored")
}
