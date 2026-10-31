package product

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/memory"
)

func TestTmMemoryProjectScopeRequiresTeamMode(t *testing.T) {
	r := newRig(t, rigOpts{})
	for _, m := range []string{"GET", "POST", "PATCH", "DELETE"} {
		path := "/api/memory?project=p1"
		if m == "PATCH" || m == "DELETE" {
			path = "/api/memory/mem_x?project=p1"
		}
		if got := r.do(m, path, "alice", `{"content":"x"}`).StatusCode; got != 404 {
			t.Errorf("%s with ?project in solo mode = %d, want 404", m, got)
		}
	}
	// exports and the AGENTS.md import are project features
	for _, rt := range [][2]string{{"GET", "/api/projects/p1/memory/export"}, {"POST", "/api/projects/p1/memory/import-agents"}} {
		if got := r.do(rt[0], rt[1], "alice", "{}").StatusCode; got != 501 {
			t.Errorf("%s %s = %d, want 501", rt[0], rt[1], got)
		}
	}
}

func TestTmMemoryProjectAccessByRole(t *testing.T) {
	// Arrange: alice owner, bob member, carol viewer, dave outsider
	r := newTmFaultRig(t)
	q := "?project=" + r.project
	entry := r.json("POST", "/api/memory"+q, "bob", `{"title":"Rule","content":"Use tabs."}`, 201)["id"].(string)

	type row struct{ owner, member, viewer, outsider int }
	cases := map[string]struct {
		method, path, body string
		want               row
	}{
		"list":   {"GET", "/api/memory" + q, "", row{200, 200, 200, 404}},
		"stats":  {"GET", "/api/memory/stats" + q, "", row{200, 200, 200, 404}},
		"create": {"POST", "/api/memory" + q, `{"content":"A new fact."}`, row{201, 201, 403, 404}},
		"patch":  {"PATCH", "/api/memory/" + entry + q, `{"title":"Rule 2"}`, row{200, 200, 403, 404}},
		"export": {"GET", "/api/projects/" + r.project + "/memory/export", "", row{200, 200, 200, 404}},
	}
	for name, c := range cases {
		for i, u := range []string{"alice", "bob", "carol", "dave"} {
			want := []int{c.want.owner, c.want.member, c.want.viewer, c.want.outsider}[i]
			if got := r.do(c.method, c.path, u, c.body).StatusCode; got != want {
				t.Errorf("%s as %s = %d, want %d", name, u, got, want)
			}
		}
	}
	// a viewer cannot delete, confirm or reject either; the entry survives
	for _, rt := range [][2]string{{"DELETE", "/api/memory/" + entry + q}, {"POST", "/api/memory/" + entry + "/confirm" + q}, {"POST", "/api/memory/" + entry + "/reject" + q}} {
		if got := r.do(rt[0], rt[1], "carol", "").StatusCode; got != 403 {
			t.Errorf("viewer %s %s = %d, want 403", rt[0], rt[1], got)
		}
		if got := r.do(rt[0], rt[1], "dave", "").StatusCode; got != 404 {
			t.Errorf("outsider %s %s = %d, want 404", rt[0], rt[1], got)
		}
	}
	if !has(entriesOf(r.rig, "carol", q), "Use tabs") {
		t.Fatal("a refused request changed the project memory")
	}
	// project writes are audited with the actor, personal ones are not project business
	r.json("POST", "/api/memory", "bob", `{"content":"Bob's own note."}`, 201)
	body := rawBody(r.do("GET", "/api/projects/"+r.project+"/audit", "alice", "").Body)
	if !strings.Contains(body, "memory.create") || !strings.Contains(body, "memory.update") || strings.Contains(body, "Bob's own note") {
		t.Fatalf("audit = %s", body)
	}
}

func TestTmMemoryCRUDAndItsErrors(t *testing.T) {
	r := newRig(t, rigOpts{})

	// create: content is required; the entry comes back complete with who created it
	r.json("POST", "/api/memory", "alice", `{"title":"No content"}`, 400)
	r.json("POST", "/api/memory", "alice", `{"content":"  "}`, 400)
	r.json("POST", "/api/memory", "alice", `{`, 400)
	e := r.json("POST", "/api/memory", "alice", `{"title":"Style","content":"Terse answers.","tags":["style","Voice"],"path":"memories/semantic/preferences/style.md"}`, 201)
	id := e["id"].(string)
	if e["title"] != "Style" || e["path"] != "memories/semantic/preferences/style.md" || e["status"] != "active" || e["source"] != "user" ||
		e["metadata"].(map[string]any)["createdBy"] != "alice" {
		t.Fatalf("created = %v", e)
	}
	// a taken path is a conflict, a transient path is refused
	r.json("POST", "/api/memory", "alice", `{"content":"Other.","path":"memories/semantic/preferences/style.md"}`, 409)
	r.json("POST", "/api/memory", "alice", `{"content":"Other.","path":"../escape.md"}`, 400)
	r.json("POST", "/api/memory", "alice", `{"content":"x","scope":"nonsense"}`, 201) // unknown JSON fields are ignored

	// patch: only the named fields change; unknown ids are 404; empty content is refused
	got := r.json("PATCH", "/api/memory/"+id, "alice", `{"content":"Terse and kind answers."}`, 200)
	if got["content"] != "Terse and kind answers." || got["title"] != "Style" {
		t.Fatalf("patched = %v", got)
	}
	got = r.json("PATCH", "/api/memory/"+id, "alice", `{"tags":["only"],"pinned":true}`, 200)
	if tags := got["tags"].([]any); len(tags) != 1 || tags[0] != "only" || got["metadata"].(map[string]any)["pinned"] != true {
		t.Fatalf("patched tags = %v", got)
	}
	r.json("PATCH", "/api/memory/nope", "alice", `{"title":"x"}`, 404)
	r.json("PATCH", "/api/memory/"+id, "alice", `{"content":""}`, 400)
	r.json("PATCH", "/api/memory/"+id, "alice", `{`, 400)

	// the memory is the user's own: another user sees neither the entry nor can change it
	r.json("PATCH", "/api/memory/"+id, "bob", `{"title":"hijack"}`, 404)
	r.json("DELETE", "/api/memory/"+id, "bob", "", 404)
	if has(entriesOf(r, "bob", ""), "Terse") {
		t.Fatal("bob sees alice's memory")
	}

	// delete: gone from the list; deleting again is 404
	if got := r.do("DELETE", "/api/memory/"+id, "alice", "").StatusCode; got != 204 {
		t.Fatalf("delete = %d", got)
	}
	if has(entriesOf(r, "alice", ""), "Terse") {
		t.Fatal("deleted entry still listed")
	}
	r.json("DELETE", "/api/memory/"+id, "alice", "", 404)
	r.json("PATCH", "/api/memory/"+id, "alice", `{"title":"back"}`, 404)
}

// tmProposal puts a pending entry (a new memory the agent proposed) and an active entry with a proposed change
// in alice's memory, the way an agent running after untrusted content leaves them.
func tmProposals(t *testing.T, r *rig, key string) (pending, changed string) {
	t.Helper()
	ctx := context.Background()
	agent := memory.Writer{Login: key, Source: store.MemorySourceProxy, Untrusted: true}
	res := r.h.Memory.Create(ctx, agent, memory.CreateInput{Title: &[]string{"Proposed"}[0], Content: "The agent wants this remembered."})
	if res.IsError || len(res.Changes) == 0 || res.Changes[0].Action != "proposed" {
		t.Fatalf("proposal = %+v", res)
	}
	pending = res.Changes[0].Entry.ID
	user := memory.Writer{Login: key, Source: store.MemorySourceUser}
	base := r.h.Memory.Create(ctx, user, memory.CreateInput{Title: &[]string{"Confirmed"}[0], Content: "Original text."})
	changed = base.Changes[0].Entry.ID
	newText := "Text changed by the agent."
	if up := r.h.Memory.Update(ctx, agent, memory.UpdateInput{ID: changed, Content: &newText}); up.IsError {
		t.Fatalf("proposed change = %+v", up)
	}
	return pending, changed
}

func TestTmMemoryConfirmAndRejectProposals(t *testing.T) {
	r := newRig(t, rigOpts{})
	pending, changed := tmProposals(t, r, "alice")

	// the list and the stats tell the user what waits
	list := entriesOf(r, "alice", "")
	var seenPending, seenProposal bool
	for _, e := range list {
		seenPending = seenPending || (e["id"] == pending && e["status"] == "pending")
		seenProposal = seenProposal || (e["id"] == changed && e["proposal"] != nil)
	}
	if !seenPending || !seenProposal {
		t.Fatalf("list = %v", list)
	}
	if st := r.json("GET", "/api/memory/stats", "alice", "", 200); st["pending"] != float64(2) || st["active"] != float64(1) {
		t.Fatalf("stats = %v", st)
	}

	// another user cannot decide on alice's proposals
	r.json("POST", "/api/memory/"+pending+"/confirm", "bob", "", 404)
	r.json("POST", "/api/memory/"+changed+"/reject", "bob", "", 404)

	// confirm: the pending entry becomes active; the proposal of an active entry is applied
	got := r.json("POST", "/api/memory/"+pending+"/confirm", "alice", "", 200)
	if got["status"] != "active" {
		t.Fatalf("confirmed = %v", got)
	}
	got = r.json("POST", "/api/memory/"+changed+"/confirm", "alice", "", 200)
	if got["content"] != "Text changed by the agent." || got["proposal"] != nil {
		t.Fatalf("applied = %v", got)
	}
	if st := r.json("GET", "/api/memory/stats", "alice", "", 200); st["pending"] != float64(0) || st["active"] != float64(2) {
		t.Fatalf("stats after confirming = %v", st)
	}

	// reject: a new proposal is deleted outright, a proposed change is dropped and the entry keeps its text
	pending2, changed2 := tmProposals(t, r, "alice")
	if got := r.do("POST", "/api/memory/"+pending2+"/reject", "alice", "").StatusCode; got != 204 {
		t.Fatalf("reject pending = %d", got)
	}
	kept := r.json("POST", "/api/memory/"+changed2+"/reject", "alice", "", 200)
	if kept["content"] != "Original text." || kept["proposal"] != nil {
		t.Fatalf("after rejecting the change = %v", kept)
	}
	if has(entriesOf(r, "alice", ""), "The agent wants this remembered") && len(entriesOf(r, "alice", "")) != 3 {
		t.Fatalf("rejected proposal is still around: %v", entriesOf(r, "alice", ""))
	}

	// nothing waits any more: rejecting is a conflict, confirming an unchanged entry is a no-op, unknown ids are 404
	r.json("POST", "/api/memory/"+changed2+"/reject", "alice", "", 409)
	r.json("POST", "/api/memory/"+pending2+"/reject", "alice", "", 404)
	r.json("POST", "/api/memory/"+changed2+"/confirm", "alice", "", 200)
	r.json("POST", "/api/memory/nope/confirm", "alice", "", 404)
	r.json("POST", "/api/memory/nope/reject", "alice", "", 404)
}

func TestTmMemoryFailureCodesMapToStatuses(t *testing.T) {
	cases := map[string]int{
		"memory_not_found":    http.StatusNotFound,
		"memory_deleted":      http.StatusNotFound,
		"memory_conflict":     http.StatusConflict,
		"memory_not_pending":  http.StatusConflict,
		"path_exists":         http.StatusConflict,
		"memory_limit":        http.StatusConflict,
		"memory_store_failed": http.StatusInternalServerError,
		"invalid_content":     http.StatusBadRequest,
		"invalid_scope":       http.StatusBadRequest,
		"anything else":       http.StatusBadRequest,
	}
	for code, want := range cases {
		w := &tmRecorder{header: http.Header{}}
		memoryFail(w, &memory.Error{Code: code, Message: "m"})
		var doc struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(w.body.String()), &doc); err != nil || w.status != want || doc.Error.Code != code || doc.Error.Message != "m" {
			t.Errorf("%s: status %d body %s (%v), want %d with error.code/error.message at the top level", code, w.status, w.body.String(), err, want)
		}
	}
}

func TestTmMemoryLimitsAnswerConflict(t *testing.T) {
	r := newRig(t, rigOpts{env: map[string]string{"HARNESS_MEMORY_MAX_ENTRIES": "2"}})
	r.json("POST", "/api/memory", "alice", `{"content":"one"}`, 201)
	r.json("POST", "/api/memory", "alice", `{"content":"two"}`, 201)

	resp := r.do("POST", "/api/memory", "alice", `{"content":"three"}`)

	if body := rawBody(resp.Body); resp.StatusCode != 409 || !strings.Contains(body, "memory_limit") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	// another user has their own budget
	r.json("POST", "/api/memory", "bob", `{"content":"mine"}`, 201)
}

func TestTmMemoryStatsCountKindsPinsAndLayers(t *testing.T) {
	r := newTmFaultRig(t)
	post := func(user, query, body string) {
		r.json("POST", "/api/memory"+query, user, body, 201)
	}
	q := "?project=" + r.project
	post("alice", q, `{"content":"Pinned rule.","pinned":true,"path":"memories/semantic/constraints/a.md"}`)
	post("alice", q, `{"content":"A fact.","path":"memories/semantic/facts/b.md"}`)
	post("alice", q, `{"content":"Another fact.","path":"memories/semantic/facts/c.md"}`)
	post("alice", q, `{"content":"Top level.","path":"memories/loose.md"}`)
	post("bob", q+"&mine=1", `{"content":"Private.","path":"memories/semantic/notes/d.md"}`)
	tmProposals(t, r.rig, "project:"+r.project)

	proj := r.json("GET", "/api/memory/stats"+q, "carol", "", 200)
	mine := r.json("GET", "/api/memory/stats"+q+"&mine=1", "bob", "", 200)
	personal := r.json("GET", "/api/memory/stats", "alice", "", 200)

	if proj["active"] != float64(5) || proj["pinned"] != float64(1) || proj["pending"] != float64(2) {
		t.Fatalf("project stats = %v", proj)
	}
	kinds := proj["byKind"].(map[string]any)
	if kinds["facts"] != float64(2) || kinds["constraints"] != float64(1) || len(kinds) < 2 {
		t.Fatalf("kinds = %v", kinds)
	}
	if mine["active"] != float64(1) || mine["byKind"].(map[string]any)["notes"] != float64(1) {
		t.Fatalf("private layer stats = %v", mine)
	}
	if personal["active"] != float64(0) || personal["pending"] != float64(0) {
		t.Fatalf("personal stats include the project's: %v", personal)
	}
	if proj["embeddings"].(map[string]any)["enabled"] != false {
		t.Fatalf("embeddings = %v", proj["embeddings"])
	}
	// A user's private layer is addressed by the session, never by the request: dave has none in this project.
	r.json("GET", "/api/memory/stats"+q+"&mine=1", "dave", "", 404)
}

func TestTmMemoryExportGroupsPinnedThenKinds(t *testing.T) {
	r := newTmFaultRig(t)
	q := "?project=" + r.project
	r.json("POST", "/api/memory"+q, "alice", `{"title":"Style","content":"Line one.\nLine two.","path":"memories/semantic/preferences/style.md"}`, 201)
	r.json("POST", "/api/memory"+q, "alice", `{"content":"Always sign commits.","pinned":true,"path":"memories/semantic/constraints/sign.md"}`, 201)
	r.json("POST", "/api/memory"+q, "alice", `{"title":"Stack","content":"Go and React.","path":"memories/semantic/facts/stack.md"}`, 201)
	r.json("POST", "/api/memory"+q+"&mine=1", "alice", `{"content":"Private to alice.","path":"memories/semantic/facts/private.md"}`, 201)
	tmProposals(t, r.rig, "project:"+r.project) // pending entries are not exported

	resp := r.do("GET", "/api/projects/"+r.project+"/memory/export", "carol", "")
	md := rawBody(resp.Body)

	if resp.Header.Get("Content-Type") != "text/markdown; charset=utf-8" || !strings.Contains(resp.Header.Get("Content-Disposition"), "MEMORY.md") {
		t.Fatalf("headers = %v", resp.Header)
	}
	if !strings.HasPrefix(md, "# api — project memory\n\n## Always apply\n\n- **Always sign commits.** — Always sign commits.") {
		t.Fatalf("export = %q", md)
	}
	if strings.Index(md, "## Facts") < strings.Index(md, "Always apply") || !strings.Contains(md, "- **Stack** — Go and React.") ||
		!strings.Contains(md, "- **Style** — Line one.\n  Line two.") || !strings.Contains(md, "## Preferences") {
		t.Fatalf("export = %q", md)
	}
	if strings.Contains(md, "Private to alice") || strings.Contains(md, "wants this remembered") {
		t.Fatalf("export leaks private or unconfirmed entries: %q", md)
	}
	if strings.Count(md, "\n- **Always sign commits.**") != 1 {
		t.Fatalf("a pinned entry is listed twice: %q", md)
	}
}

func TestTmImportAgentsMDReportsWhyItCannotRead(t *testing.T) {
	r, gh, _ := githubRig(t)
	connectGitHub(t, r, "alice", "alice")
	gh.agents = "# Rules\nBe nice."
	p := r.json("POST", "/api/projects/import", "alice", `{"repo":"acme/web"}`, 201)["id"].(string)
	r.json("PUT", "/api/projects/"+p+"/members/bob", "alice", `{"role":"viewer"}`, 204)
	// importing a project already pulls its AGENTS.md in the background; let that finish before importing by hand
	for i := 0; !has(entriesOf(r, "alice", "?project="+p), "Be nice."); i++ {
		if i > 400 {
			t.Fatal("the project import never stored AGENTS.md")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// viewers can read the memory but not refresh it from the repository
	r.json("POST", "/api/projects/"+p+"/memory/import-agents", "bob", `{}`, 403)
	r.json("POST", "/api/projects/"+p+"/memory/import-agents", "carol", `{}`, 404)
	e := r.json("POST", "/api/projects/"+p+"/memory/import-agents", "alice", `{}`, 200)
	if e["path"] != "memories/core/agents-md.md" || e["metadata"].(map[string]any)["pinned"] != true || e["metadata"].(map[string]any)["file"] != "AGENTS.md" {
		t.Fatalf("imported = %v", e)
	}

	// importing unchanged text again keeps the same entry and its history
	again := r.json("POST", "/api/projects/"+p+"/memory/import-agents", "alice", `{}`, 200)
	if again["id"] != e["id"] || again["updated_at"] != e["updated_at"] {
		t.Fatalf("an unchanged file rewrote the entry: %v vs %v", again, e)
	}

	// a very large file is stored truncated
	gh.agents = strings.Repeat("x", 25000)
	big := r.json("POST", "/api/projects/"+p+"/memory/import-agents", "alice", `{}`, 200)
	if c := big["content"].(string); !strings.HasSuffix(c, "(truncated)") || len(c) > 20100 {
		t.Fatalf("content length %d, tail %q", len(c), c[len(c)-20:])
	}

	// a project without a connected repository cannot import
	bare := r.json("POST", "/api/projects", "alice", `{"name":"bare"}`, 201)["id"].(string)
	r.json("POST", "/api/projects/"+bare+"/memory/import-agents", "alice", `{}`, 404)
}

type tmRecorder struct {
	header http.Header
	status int
	body   strings.Builder
}

func (w *tmRecorder) Header() http.Header         { return w.header }
func (w *tmRecorder) WriteHeader(s int)           { w.status = s }
func (w *tmRecorder) Write(b []byte) (int, error) { return w.body.WriteString(string(b)) }

func TestTmMemoryKeyIgnoresUnknownThreadsAndFailingLookups(t *testing.T) {
	r := newTmFaultRig(t)
	ctx := context.Background()
	key := MemoryKey(r.fault)
	th := r.projectThread("alice", "t")
	personal := r.json("POST", "/api/threads", "alice", `{}`, 201)["id"].(string)

	if got := key(ctx, "alice", th); got != ProjectMemoryPrefix+r.project {
		t.Fatalf("project thread key = %q", got)
	}
	if got := key(ctx, "alice", personal); got != "alice" {
		t.Fatalf("personal thread key = %q", got)
	}
	if got := key(ctx, "alice", "unknown"); got != "alice" {
		t.Fatalf("unknown thread key = %q", got)
	}
	// a failing lookup falls back to the user's own memory rather than writing into a project's
	r.fault.arm("GetThreadMeta")
	if got := key(ctx, "alice", th); got != "alice" {
		t.Fatalf("key during a store failure = %q", got)
	}
	// the private layer needs the message history; without it there is none
	userKey := MemoryUserKey(r.fault, func(context.Context, string) ([]*store.Message, error) { return nil, context.Canceled })
	r.fault.disarm("GetThreadMeta")
	if got := userKey(ctx, "alice", th); got != "" {
		t.Fatalf("user key without history = %q", got)
	}
	r.fault.arm("GetThreadMeta")
	if got := userKey(ctx, "alice", th); got != "" {
		t.Fatalf("user key during a store failure = %q", got)
	}
}
