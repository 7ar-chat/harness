package product

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
)

// supModel plays the supervisor: it calls one tool once (the one named by next()), then says done.
// Any other agent (without the supervisor tools) just answers.
func supModel(next func() (tool, args string)) *llmtest.Model {
	var mu sync.Mutex
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		has := false
		for _, tl := range c.Tools {
			if n := toolName(tl); n == "list_sessions" {
				has = true
			}
		}
		if len(c.Tools) == 0 || !has {
			return llmtest.Turn{Chunks: llmtest.Text("ok")}
		}
		if last := c.Messages[len(c.Messages)-1]; last.Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("done")}
		}
		mu.Lock()
		tool, args := next()
		mu.Unlock()
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", tool, args), "tool_calls")}}
	})
}

func toolName(t any) string {
	b, _ := json.Marshal(t)
	var m struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Name
}

func supRig(t *testing.T, next func() (string, string)) (*teamRig, *teamstore.Mem) {
	t.Helper()
	tm := teamstore.NewMem()
	ctx := context.Background()
	for _, l := range []string{"alice", "bob", "carol"} {
		tm.UpsertUser(ctx, store.User{Login: l, Name: strings.ToUpper(l[:1]) + l[1:]})
	}
	r := newRig(t, rigOpts{prod: Options{Team: tm}, team: true, model: supModel(next), sup: NewSupervisor(tm)})
	p := r.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	r.json("PUT", "/api/projects/"+p+"/members/bob", "alice", `{"role":"member"}`, 204)
	return &teamRig{rig: r, team: tm, project: p}, tm
}

func supervisorThread(r *rig, user string) string {
	return r.json("GET", "/api/supervisor", user, "", 200)["id"].(string)
}

func lastToolResult(t *testing.T, r *rig, thread, user string) string {
	t.Helper()
	msgs := r.json("GET", "/api/threads/"+thread, user, "", 200)["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i].(map[string]any); m["role"] == "tool" {
			return m["content"].(string)
		}
	}
	t.Fatalf("no tool result in %v", msgs)
	return ""
}

func TestSupervisorThreadIsPersonalAndStable(t *testing.T) {
	r, _ := supRig(t, func() (string, string) { return "list_sessions", `{}` })
	a1, a2, b1 := supervisorThread(r.rig, "alice"), supervisorThread(r.rig, "alice"), supervisorThread(r.rig, "bob")
	if a1 != a2 || a1 == b1 {
		t.Fatalf("supervisor threads alice=%s,%s bob=%s", a1, a2, b1)
	}
	if got := r.json("GET", "/api/threads/"+a1, "alice", "", 200)["kind"]; got != "supervisor" {
		t.Fatalf("kind = %v", got)
	}
	r.json("GET", "/api/threads/"+a1, "bob", "", 404)
	// It does not appear among the personal sessions.
	if body := rawBody(r.do("GET", "/api/threads", "alice", "").Body); strings.Contains(body, a1) {
		t.Fatalf("supervisor thread listed as a session: %s", body)
	}
}

func TestSupervisorListsOnlyWhatTheUserCanSee(t *testing.T) {
	r, _ := supRig(t, func() (string, string) { return "list_sessions", `{}` })
	r.json("POST", "/api/threads", "alice", `{"projectId":"`+r.project+`","title":"Fix login bug"}`, 201)
	r.json("POST", "/api/threads", "carol", `{"title":"Carol private"}`, 201)

	for user, want := range map[string]string{"bob": "Fix login bug", "alice": "Fix login bug"} {
		th := supervisorThread(r.rig, user)
		finished(t, r.stream(r.startRun(user, th, "what is running?"), user))
		if res := lastToolResult(t, r.rig, th, user); !strings.Contains(res, want) || strings.Contains(res, "Carol private") {
			t.Fatalf("%s sees %s", user, res)
		}
	}
	th := supervisorThread(r.rig, "carol")
	finished(t, r.stream(r.startRun("carol", th, "what is running?"), "carol"))
	if res := lastToolResult(t, r.rig, th, "carol"); strings.Contains(res, "Fix login bug") || !strings.Contains(res, "Carol private") {
		t.Fatalf("carol sees %s", res)
	}
}

func TestSupervisorCannotReadSessionsOutsideTheUsersProjects(t *testing.T) {
	var target string
	r, _ := supRig(t, func() (string, string) { return "read_session", `{"thread_id":"` + target + `"}` })
	target = r.json("POST", "/api/threads", "alice", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)
	th := supervisorThread(r.rig, "carol")
	finished(t, r.stream(r.startRun("carol", th, "read it"), "carol"))
	if res := lastToolResult(t, r.rig, th, "carol"); !strings.Contains(strings.ToLower(res), "not found") {
		t.Fatalf("carol read a foreign session: %s", res)
	}
}

func TestSupervisorSendNeedsApprovalAndIsAttributed(t *testing.T) {
	var target string
	r, _ := supRig(t, func() (string, string) {
		return "session_send", `{"thread_id":"` + target + `","message":"please also add tests"}`
	})
	target = r.json("POST", "/api/threads", "alice", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)
	th := supervisorThread(r.rig, "bob")
	run := r.startRun("bob", th, "tell it to add tests")
	out := finished(t, r.stream(run, "bob"))
	if o, _ := out["outcome"].(map[string]any); o["type"] != "interrupt" {
		t.Fatalf("session_send ran without approval: %v\n%v", out, r.json("GET", "/api/threads/"+th, "bob", "", 200)["messages"])
	}
	pending := r.json("GET", "/api/threads/"+th, "bob", "", 200)["pendingApproval"].(map[string]any)
	if pending["toolName"] != "session_send" {
		t.Fatalf("pending = %v", pending)
	}
	resumed := r.json("POST", "/api/threads/"+th+"/resume", "bob", `{"interruptId":"`+pending["id"].(string)+`","decision":"approve"}`, 202)
	finished(t, r.stream(resumed["id"].(string), "bob"))
	r.waitIdle(target, "alice")

	msgs := r.json("GET", "/api/threads/"+target, "alice", "", 200)["messages"].([]any)
	var found bool
	for _, m := range msgs {
		m := m.(map[string]any)
		if m["role"] == "user" && strings.Contains(m["content"].(string), "also add tests") {
			a := m["author"].(map[string]any)
			found = a["kind"] == "supervisor" && a["onBehalfOf"] == "bob"
		}
	}
	if !found {
		t.Fatalf("no supervisor-authored message on behalf of bob: %v", msgs)
	}
	if body := rawBody(r.do("GET", "/api/projects/"+r.project+"/audit", "alice", "").Body); !strings.Contains(body, `"actor":"supervisor"`) || !strings.Contains(body, `"onBehalfOf":"bob"`) {
		t.Fatalf("audit = %s", body)
	}
}

func TestSupervisorSettings(t *testing.T) {
	r, tm := supRig(t, func() (string, string) { return "list_sessions", `{}` })
	if m := r.json("GET", "/api/supervisor/settings", "alice", "", 200); m["watch"] != false {
		t.Fatalf("default = %v", m)
	}
	r.json("PUT", "/api/supervisor/settings", "alice", `{"watch":true,"stuckMinutes":5}`, 200)
	m := r.json("GET", "/api/supervisor/settings", "alice", "", 200)
	if m["watch"] != true || m["stuckMinutes"] != float64(5) {
		t.Fatalf("settings = %v", m)
	}
	if p, _ := tm.UserPrefs(context.Background(), "bob"); len(p) != 0 {
		t.Fatalf("settings leaked to bob: %v", p)
	}
}
