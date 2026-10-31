package memorytools_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/memory"
	"github.com/GhaziBenDahmane/harness/features/memory/memorytools"
)

func provider(t *testing.T) (*memstore.Store, map[string]ai.InvokableTool) {
	t.Helper()
	st := memstore.New()
	p := memorytools.New(memory.New(st, memory.Options{}))
	list, _, err := p.Tools(context.Background(), tools.RunContext{Login: "alice", ThreadID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ai.InvokableTool{}
	for _, tl := range list {
		info, _ := tl.Info(context.Background())
		out[info.Name] = tl.(ai.InvokableTool)
	}
	return st, out
}

func call(t *testing.T, tl ai.InvokableTool, args string) (map[string]any, bool) {
	t.Helper()
	out, err := tl.InvokableRun(context.Background(), args)
	text := out
	if err != nil {
		var ue *tools.UserError
		if !errorsAs(err, &ue) {
			t.Fatalf("not a user error: %v", err)
		}
		text = ue.Msg
	}
	var m map[string]any
	if jerr := json.Unmarshal([]byte(text), &m); jerr != nil {
		t.Fatalf("result %q: %v", text, jerr)
	}
	return m, err != nil
}

func errorsAs(err error, target **tools.UserError) bool {
	ue, ok := err.(*tools.UserError)
	if ok {
		*target = ue
	}
	return ok
}

// workSchemas are the input schemas of Work's codex-memory-proxy (_tool_schemas).
var workSchemas = map[string]string{
	"memory_search": `{"type":"object","properties":{"query":{"type":"string"},"scope":{"type":"string","enum":["personal","repository","workspace"]},
		"repository":{"type":"string"},"top_k":{"type":"integer","minimum":1,"maximum":50},"filters":{"type":"object"}},"required":["query"]}`,
	"memory_get": `{"type":"object","properties":{"id":{"type":"string"},"path":{"type":"string"},"include_metadata":{"type":"boolean"}}}`,
	"memory_create": `{"type":"object","properties":{"content":{"type":"string"},"metadata":{"type":"object"},"tags":{"type":"array","items":{"type":"string"}},
		"scope":{"type":"string","enum":["personal","repository","workspace"]},"repository":{"type":"string"},"path":{"type":"string"},
		"audience":{"type":"string","enum":["project","me"],"description":"In a project: project (default) is shared by the team, me is the current user's own memory inside this project."}},"required":["content"]}`,
	"memory_update": `{"type":"object","properties":{"id":{"type":"string"},"path":{"type":"string"},"content":{"type":"string"},"metadata":{"type":"object"},
		"tags":{"type":"array","items":{"type":"string"}}}}`,
	"memory_delete": `{"type":"object","properties":{"id":{"type":"string"},"path":{"type":"string"},"metadata":{"type":"object"}}}`,
	"memory_create_batch": `{"type":"object","properties":{"entries":{"type":"array","items":{"type":"object","properties":{"content":{"type":"string"},
		"metadata":{"type":"object"},"tags":{"type":"array","items":{"type":"string"}},"scope":{"type":"string"},"repository":{"type":"string"},
		"path":{"type":"string"}},"required":["content"]}}},"required":["entries"]}`,
	// memory_delete_batch differs from Work on purpose: ids or paths (A-26).
	"memory_delete_batch": `{"type":"object","properties":{"ids":{"type":"array","items":{"type":"string"}},"paths":{"type":"array","items":{"type":"string"}}}}`,
	"memory_capabilities": `{"type":"object","properties":{}}`,
}

func TestToolsHaveWorksNamesAndInputSchemas(t *testing.T) {
	// The eight tools carry exactly the names and input schemas of Work's memory MCP server.
	// Arrange
	_, all := provider(t)

	// Act / Assert
	if len(all) != len(memorytools.Names) {
		t.Fatalf("tools %d", len(all))
	}
	for _, name := range memorytools.Names {
		tl := all[name]
		if tl == nil {
			t.Fatalf("missing %s", name)
		}
		info, _ := tl.Info(context.Background())
		b, _ := json.Marshal(info.Parameters)
		var got, want any
		_ = json.Unmarshal(b, &got)
		_ = json.Unmarshal([]byte(workSchemas[name]), &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s schema\n got %s\nwant %s", name, b, workSchemas[name])
		}
		if info.Desc == "" {
			t.Errorf("%s has no description", name)
		}
	}
}

func TestToolsAreOnlyOfferedWhenMemoryIsEnabled(t *testing.T) {
	// A user who turned memory off gets no memory tool.
	// Arrange
	st := memstore.New()
	_ = st.SetMemoryEnabled(context.Background(), "alice", false)

	// Act
	list, _, err := memorytools.New(memory.New(st, memory.Options{})).Tools(context.Background(), tools.RunContext{Login: "alice"})

	// Assert
	if err != nil || len(list) != 0 {
		t.Fatalf("tools %d %v", len(list), err)
	}
}

func TestToolsValidateArgumentsLikeWork(t *testing.T) {
	// Invalid arguments are tool errors {"error": {code: invalid_arguments, message, details: {tool}}} with Work's messages.
	// Arrange
	_, all := provider(t)
	cases := []struct{ tool, args, msg string }{
		{"memory_search", `{}`, "query is required"},
		{"memory_search", `{"query":"  "}`, "query is required"},
		{"memory_search", `{"query":"x","top_k":true}`, "top_k must be an integer"},
		{"memory_search", `{"query":"x","top_k":2.5}`, "top_k must be an integer"},
		{"memory_search", `{"query":"x","top_k":51}`, "top_k must be between 1 and 50"},
		{"memory_search", `{"query":"x","filters":[]}`, "filters must be an object"},
		{"memory_get", `{}`, "id or path is required"},
		{"memory_get", `{"id":"m","include_metadata":"yes"}`, "include_metadata must be a boolean"},
		{"memory_create", `{"content":1}`, "content must be a string"},
		{"memory_create", `{"content":"x","tags":["a",2]}`, "tags must be an array of strings"},
		{"memory_update", `{"content":"x"}`, "id or path is required"},
		{"memory_delete", `{"id":"m","metadata":"x"}`, "metadata must be an object"},
		{"memory_create_batch", `{"entries":{}}`, "entries must be an array"},
		{"memory_create_batch", `{"entries":[{"content":"a","path":3}]}`, "batch entry path must be a string"},
		{"memory_create_batch", `{"entries":[` + strings.TrimSuffix(strings.Repeat(`{"content":"a"},`, 101), ",") + `]}`, "entries must contain at most 100 items"},
		{"memory_delete_batch", `{"paths":[]}`, "ids or paths is required"},
		{"memory_search", `[1]`, "Memory tool arguments must be an object"},
	}
	for _, c := range cases {
		t.Run(c.tool+" "+c.msg, func(t *testing.T) {
			// Act
			res, isErr := call(t, all[c.tool], c.args)

			// Assert
			e, _ := res["error"].(map[string]any)
			if !isErr || e["code"] != "invalid_arguments" || e["message"] != c.msg || e["details"].(map[string]any)["tool"] != c.tool {
				t.Fatalf("got %v", res)
			}
		})
	}
}

func TestToolResultsHaveWorksShapes(t *testing.T) {
	// create → {memory, policy}; get → {memory, trace}; search → {results, trace, provenance}; update → {memory, policy, changes};
	// delete → {deleted, id, path, mode, policy}; batches → counts; capabilities → {capabilities}; errors → {error}.
	// Arrange
	_, all := provider(t)
	keys := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		return out
	}
	has := func(m map[string]any, want ...string) bool {
		for _, k := range want {
			if _, ok := m[k]; !ok {
				return false
			}
		}
		return true
	}

	// Act
	created, _ := call(t, all["memory_create"], `{"content":"remember this: the CRO reads the risk report","tags":["fact"]}`)
	id := created["memory"].(map[string]any)["id"].(string)
	got, _ := call(t, all["memory_get"], `{"id":"`+id+`"}`)
	found, _ := call(t, all["memory_search"], `{"query":"risk report"}`)
	updated, _ := call(t, all["memory_update"], `{"id":"`+id+`","tags":["fact","report"]}`)
	batch, _ := call(t, all["memory_create_batch"], `{"entries":[{"content":"a"},{"content":"b"}]}`)
	deleted, _ := call(t, all["memory_delete"], `{"id":"`+id+`","metadata":{"reason":"outdated"}}`)
	dbatch, _ := call(t, all["memory_delete_batch"], `{"ids":["mem_nope"]}`)
	caps, _ := call(t, all["memory_capabilities"], `{}`)
	missing, isErr := call(t, all["memory_get"], `{"id":"mem_nope"}`)

	// Assert
	if !has(created, "memory", "policy") || !has(created["memory"].(map[string]any), "id", "path", "scope", "repository", "tags", "metadata",
		"created_at", "updated_at", "source", "embedding_updated_at") {
		t.Fatalf("create %v", keys(created))
	}
	if !has(got, "memory", "trace") || !has(got["memory"].(map[string]any), "id", "path", "content", "source", "timestamp", "metadata", "tags", "scope", "repository") {
		t.Fatalf("get %v", got)
	}
	if !has(found, "results", "trace", "provenance") || len(found["results"].([]any)) != 1 {
		t.Fatalf("search %v", found)
	}
	if !has(updated, "memory", "policy", "changes") || updated["changes"].(map[string]any)["tags_changed"] != true {
		t.Fatalf("update %v", updated)
	}
	if batch["created_count"] != float64(2) || batch["error_count"] != float64(0) {
		t.Fatalf("batch %v", batch)
	}
	if deleted["deleted"] != true || deleted["id"] != id || deleted["mode"] != "soft" || !has(deleted, "path", "policy") {
		t.Fatalf("delete %v", deleted)
	}
	if dbatch["deleted_count"] != float64(0) || dbatch["error_count"] != float64(1) {
		t.Fatalf("delete batch %v", dbatch)
	}
	if !has(caps["capabilities"].(map[string]any), "storage", "search", "write_mode", "delete_mode") {
		t.Fatalf("caps %v", caps)
	}
	if !isErr || missing["error"].(map[string]any)["code"] != "memory_not_found" {
		t.Fatalf("missing %v", missing)
	}
}

func TestEventValueOfAProposedUpdateShowsTheProposal(t *testing.T) {
	// work.memory proposed for an update carries the proposed title, text and content hash (what Keep confirms), the entry staying active.
	// Arrange
	st := memstore.New()
	svc := memory.New(st, memory.Options{})
	ctx := context.Background()
	created := svc.Create(ctx, memory.Writer{Login: "alice"}, memory.CreateInput{Content: "Prefers tea"})
	content := "Prefers coffee"

	// Act
	r := svc.Update(ctx, memory.Writer{Login: "alice", Untrusted: true},
		memory.UpdateInput{ID: created.Changes[0].Entry.ID, Content: &content})
	v := memorytools.EventValue(r.Changes[0])
	pending := memorytools.EventValue(svc.Create(ctx, memory.Writer{Login: "alice", Untrusted: true},
		memory.CreateInput{Content: "Reports to Bob"}).Changes[0])

	// Assert
	p := r.Changes[0].Entry.Proposal
	if v["action"] != "proposed" || v["proposal"] != "update" || v["status"] != "active" || v["text"] != "Prefers coffee" ||
		v["title"] != "Prefers coffee" || v["contentHash"] != p.ContentHash {
		t.Fatalf("event %v", v)
	}
	if pending["proposal"] != "create" || pending["status"] != "pending" || pending["contentHash"] == "" {
		t.Fatalf("pending event %v", pending)
	}
}

func TestEventValueOfAnUntrustedDeleteIsAProposedSoftDelete(t *testing.T) {
	// work.memory deleted after untrusted content carries mode soft and proposed (the chat offers Undo).
	// Arrange
	st := memstore.New()
	svc := memory.New(st, memory.Options{DeleteMode: "hard"})
	ctx := context.Background()
	created := svc.Create(ctx, memory.Writer{Login: "alice"}, memory.CreateInput{Content: "Prefers tea"})

	// Act
	r := svc.Delete(ctx, memory.Writer{Login: "alice", Untrusted: true}, created.Changes[0].Entry.ID, "", nil, "")
	v := memorytools.EventValue(r.Changes[0])

	// Assert
	if v["action"] != "deleted" || v["mode"] != "soft" || v["proposed"] != true || v["status"] != "deleted" {
		t.Fatalf("event %v", v)
	}
}

func TestDeleteBatchAcceptsPathsAlone(t *testing.T) {
	// memory_delete_batch takes ids, paths or both, as its schema says: paths alone delete the entries they name.
	// Arrange
	_, all := provider(t)
	created, _ := call(t, all["memory_create"], `{"content":"Prefers French answers","path":"memories/semantic/preferences/lang.md"}`)
	path := created["memory"].(map[string]any)["path"].(string)

	// Act
	res, isErr := call(t, all["memory_delete_batch"], `{"paths":["`+path+`"]}`)

	// Assert
	info, _ := all["memory_delete_batch"].Info(context.Background())
	required, _ := info.Parameters["required"].([]any)
	if isErr || res["deleted_count"] != float64(1) || len(required) != 0 {
		t.Fatalf("result %v required %v", res, required)
	}
}

func TestToolDescriptionsDoNotMentionTransientTrees(t *testing.T) {
	// The model-facing descriptions say nothing about runs/ or handoff/ (Work-internal trees the agent never sees).
	// Arrange
	_, all := provider(t)

	// Act / Assert
	for name, tl := range all {
		info, _ := tl.Info(context.Background())
		if strings.Contains(info.Desc, "runs/") || strings.Contains(info.Desc, "handoff") {
			t.Errorf("%s: %q", name, info.Desc)
		}
	}
}
