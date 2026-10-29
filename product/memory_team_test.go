package product

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/adapters/store/teamstore"
	"github.com/GhaziBenDahmane/harness/core/store"
)

func entriesOf(r *rig, user, query string) []map[string]any {
	r.t.Helper()
	var out []map[string]any
	list, _ := r.json("GET", "/api/memory"+query, user, "", 200)["entries"].([]any)
	for _, e := range list {
		out = append(out, e.(map[string]any))
	}
	return out
}

func has(entries []map[string]any, needle string) bool {
	for _, e := range entries {
		if strings.Contains(e["content"].(string), needle) || strings.Contains(e["title"].(string), needle) {
			return true
		}
	}
	return false
}

func TestProjectMemoryLayersPinningStatsAndAgentsMD(t *testing.T) {
	r, gh, _ := githubRig(t)
	gh.agents = "# Rules\nAlways run `make check` before pushing."
	connectGitHub(t, r, "alice", "alice")
	p := r.json("POST", "/api/projects/import", "alice", `{"repo":"acme/web"}`, 201)["id"].(string)
	r.json("PUT", "/api/projects/"+p+"/members/bob", "alice", `{"role":"member"}`, 204)
	q := "?project=" + p

	// Importing the project pins the repository's AGENTS.md in the project memory.
	var agents map[string]any
	for i := 0; i < 100 && agents == nil; i++ {
		for _, e := range entriesOf(r, "bob", q) {
			if e["path"] == "memories/core/agents-md.md" {
				agents = e
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if agents == nil || !strings.Contains(agents["content"].(string), "make check") || agents["metadata"].(map[string]any)["pinned"] != true {
		t.Fatalf("AGENTS.md not pinned: %v", agents)
	}
	// It follows the file; the previous text stays in the history.
	gh.agents = "# Rules\nRun `make ci` before pushing."
	got := r.json("POST", "/api/projects/"+p+"/memory/import-agents", "bob", `{}`, 200)
	if !strings.Contains(got["content"].(string), "make ci") || len(got["metadata"].(map[string]any)["history"].([]any)) != 1 {
		t.Fatalf("refresh = %v", got)
	}
	gh.agents = ""
	r.json("POST", "/api/projects/"+p+"/memory/import-agents", "alice", `{}`, 404)

	// The user's own layer inside the project is private to them.
	r.json("POST", "/api/memory"+q+"&mine=1", "bob", `{"title":"Bob","content":"Bob likes release notes in French."}`, 201)
	r.json("POST", "/api/memory"+q, "alice", `{"title":"Team","content":"Tag releases from main.","pinned":true}`, 201)
	if !has(entriesOf(r, "bob", q+"&mine=1"), "French") || has(entriesOf(r, "alice", q+"&mine=1"), "French") || has(entriesOf(r, "bob", q), "French") {
		t.Fatal("a private layer must be visible to its owner only, and apart from the project's")
	}
	if !has(entriesOf(r, "bob", q), "Tag releases") {
		t.Fatal("the project memory is shared")
	}
	r.json("GET", "/api/memory"+q+"&mine=1", "mallory", "", 404)

	// Pinning can be toggled.
	team := entriesOf(r, "alice", q)
	var teamID string
	for _, e := range team {
		if e["title"] == "Team" {
			teamID = e["id"].(string)
		}
	}
	upd := r.json("PATCH", "/api/memory/"+teamID+q, "alice", `{"pinned":false}`, 200)
	if upd["metadata"].(map[string]any)["pinned"] != false {
		t.Fatalf("unpinned = %v", upd["metadata"])
	}
	r.json("PATCH", "/api/memory/"+teamID+q, "alice", `{"pinned":true}`, 200)

	st := r.json("GET", "/api/memory/stats"+q, "bob", "", 200)
	if st["active"] != float64(2) || st["pinned"] != float64(2) || st["pending"] != float64(0) {
		t.Fatalf("stats = %v", st)
	}
	if _, ok := st["service"].(map[string]any)["recallRuns"]; !ok {
		t.Fatalf("service stats missing: %v", st)
	}

	// Export renders the pinned entries first, ready for an AGENTS.md.
	md := rawBody(r.do("GET", "/api/projects/"+p+"/memory/export", "bob", "").Body)
	if !strings.Contains(md, "## Always apply") || !strings.Contains(md, "Tag releases from main.") || strings.Contains(md, "French") {
		t.Fatalf("export = %s", md)
	}
	r.json("GET", "/api/projects/"+p+"/memory/export", "mallory", "", 404)
}

func TestMemoryUserKeyFollowsWhoAskedInAProjectThread(t *testing.T) {
	tm := teamTestStore(t)
	msgs := []*store.Message{
		{Role: store.RoleUser, Content: "first", AuthorLogin: "alice"},
		{Role: store.RoleAssistant, Content: "ok"},
		{Role: store.RoleUser, Content: "then me", AuthorLogin: "bob"},
		{Role: store.RoleUser, Content: "steer", AuthorLogin: "alice", Metadata: map[string]any{"steer": true}},
	}
	key := MemoryUserKey(tm, func(context.Context, string) ([]*store.Message, error) { return msgs, nil })
	ctx := context.Background()
	if got := key(ctx, "alice", "t-project"); got != "pu:p1:bob" {
		t.Fatalf("key = %q: the layer is the user who asked, not the thread owner nor a steerer", got)
	}
	if got := key(ctx, "alice", "t-personal"); got != "" {
		t.Fatalf("personal thread key = %q", got)
	}
	msgs = []*store.Message{{Role: store.RoleUser, Content: "check", AuthorKind: store.AuthorSupervisor, OnBehalfOf: "carol"}}
	if got := key(ctx, "alice", "t-project"); got != "pu:p1:carol" {
		t.Fatalf("supervisor message key = %q", got)
	}
	msgs = []*store.Message{{Role: store.RoleUser, Content: "legacy, no author"}}
	if got := key(ctx, "alice", "t-project"); got != "" {
		t.Fatalf("unknown author key = %q", got)
	}
	if !MemoryShared("project:p1") || MemoryShared("pu:p1:bob") || MemoryShared("alice") {
		t.Fatal("only project keys are shared")
	}
}

func teamTestStore(t *testing.T) store.TeamStore {
	t.Helper()
	tm := teamstore.NewMem()
	ctx := context.Background()
	p, _ := tm.CreateProject(ctx, store.Project{ID: "p1", Name: "api"}, "alice")
	tm.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t-project", ProjectID: p.ID, Owner: "alice", CreatedBy: "alice"})
	tm.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t-personal", Owner: "alice", CreatedBy: "alice"})
	return tm
}
