package httpapi_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/features/chatqueue"
)

// gatedModel holds every chat reply until release is closed (utility calls without tools answer at once) and
// records the user messages the replies saw, in order.
type gatedModel struct {
	*llmtest.Model
	release chan struct{}
	mu      sync.Mutex
	seen    []string
}

func newGatedModel() *gatedModel {
	g := &gatedModel{release: make(chan struct{})}
	g.Model = llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		var last string
		for _, m := range c.Messages {
			if m.Role == ai.RoleUser {
				last = m.Content
			}
		}
		g.mu.Lock()
		g.seen = append(g.seen, last)
		g.mu.Unlock()
		<-g.release
		return llmtest.Turn{Chunks: llmtest.Text("answer to " + last)}
	})
	return g
}

func (g *gatedModel) replies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.seen...)
}

func (r *rig) queue(user, thread string) []map[string]any {
	r.t.Helper()
	resp := r.do("GET", "/api/threads/"+thread+"/queue", user, "")
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	r.t.Fatalf("timed out waiting for %s", what)
}

func TestQueuedMessagesAreSentInOrderWhenTheReplyEnds(t *testing.T) {
	// Arrange: a reply that stays running while three messages are queued, one of them removed again
	model := newGatedModel()
	r := newRig(t, rigOpts{model: model.Model})
	id := r.newThread("alice")
	r.startRun("alice", id, "first")
	r.waitFor("the first reply to start", func() bool { return len(model.replies()) == 1 })

	// Act
	second := r.json("POST", "/api/threads/"+id+"/queue", "alice", `{"message":"second"}`, 201)
	third := r.json("POST", "/api/threads/"+id+"/queue", "alice", `{"message":"third"}`, 201)
	r.json("POST", "/api/threads/"+id+"/queue", "alice", `{"message":"fourth"}`, 201)
	if second["run"] != nil || third["run"] != nil {
		t.Fatalf("a message queued behind a running reply started a run: %v %v", second, third)
	}
	if resp := r.do("DELETE", "/api/threads/"+id+"/queue/"+third["item"].(map[string]any)["id"].(string), "alice", ""); resp.StatusCode != 204 {
		t.Fatalf("remove: status %d", resp.StatusCode)
	}
	if resp := r.do("DELETE", "/api/threads/"+id+"/queue/nope", "alice", ""); resp.StatusCode != 404 {
		t.Fatalf("removing an unknown message: status %d, want 404", resp.StatusCode)
	}
	q := r.queue("alice", id)

	// Assert: the queue is visible, in order, and nobody else sees it
	if len(q) != 2 || q[0]["text"] != "second" || q[1]["text"] != "fourth" {
		t.Fatalf("queue = %v", q)
	}
	if resp := r.do("GET", "/api/threads/"+id+"/queue", "mallory", ""); resp.StatusCode != 404 {
		t.Fatalf("another user's view of the queue: status %d, want 404", resp.StatusCode)
	}

	// Act: the first reply ends, the queue drains by itself, one run at a time
	close(model.release)
	r.waitFor("the queue to drain", func() bool { return len(model.replies()) == 3 && len(r.queue("alice", id)) == 0 })
	r.waitIdle(id, "alice")

	// Assert
	if got := strings.Join(model.replies(), ","); got != "first,second,fourth" {
		t.Fatalf("the replies answered %q, want first,second,fourth", got)
	}
	th := r.json("GET", "/api/threads/"+id, "alice", "", 200)
	var users []string
	for _, m := range th["messages"].([]any) {
		if m := m.(map[string]any); m["role"] == "user" {
			users = append(users, m["content"].(string))
		}
	}
	if strings.Join(users, ",") != "first,second,fourth" {
		t.Fatalf("user messages = %v", users)
	}
}

func TestSteerNowTakesAQueuedMessageIntoTheRunningReply(t *testing.T) {
	model := newGatedModel()
	r := newRig(t, rigOpts{model: model.Model})
	id := r.newThread("alice")
	r.startRun("alice", id, "first")
	r.waitFor("the first reply to start", func() bool { return len(model.replies()) == 1 })
	it := r.json("POST", "/api/threads/"+id+"/queue", "alice", `{"message":"actually, be brief"}`, 201)["item"].(map[string]any)

	res := r.json("POST", "/api/threads/"+id+"/queue/"+it["id"].(string)+"/send", "alice", "", 202)

	if res["steered"] != true {
		t.Fatalf("send now: %v, want steered", res)
	}
	if q := r.queue("alice", id); len(q) != 0 {
		t.Fatalf("the steered message is still queued: %v", q)
	}
	if resp := r.do("POST", "/api/threads/"+id+"/queue/"+it["id"].(string)+"/send", "alice", ""); resp.StatusCode != 404 {
		t.Fatalf("sending twice: status %d, want 404", resp.StatusCode)
	}
	close(model.release)
	r.waitIdle(id, "alice")
}

func TestAMessageQueuedOnAnIdleThreadIsSentAtOnce(t *testing.T) {
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")

	ack := r.json("POST", "/api/threads/"+id+"/queue", "alice", `{"message":"hello"}`, 201)

	run, _ := ack["run"].(map[string]any)
	if run == nil || run["id"] == "" {
		t.Fatalf("no run for a message queued on an idle thread: %v", ack)
	}
	finished(t, r.stream(run["id"].(string), "alice"))
	if q := r.queue("alice", id); len(q) != 0 {
		t.Fatalf("queue = %v, want empty", q)
	}
}

func TestDrainingTheQueueFromSeveralReplicasSendsEachMessageOnce(t *testing.T) {
	model := newGatedModel()
	close(model.release)
	r := newRig(t, rigOpts{model: model.Model})
	id := r.newThread("alice")
	if _, err := r.h.Queued.Add(r.ctx, id, "only once", chatqueue.Options{Model: "gpt-test"}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.h.DrainQueue(r.ctx, "alice", id) }()
	}
	wg.Wait()
	r.waitFor("the reply", func() bool { return len(model.replies()) >= 1 })
	r.waitIdle(id, "alice")

	if got := model.replies(); len(got) != 1 {
		t.Fatalf("the message was sent %d times: %v", len(got), got)
	}
	if q := r.queue("alice", id); len(q) != 0 {
		t.Fatalf("queue = %v", q)
	}
}

func TestEnvironmentsAreListedChosenPerThreadAndFrozenOnceTheThreadHasAWorkspace(t *testing.T) {
	r := newRig(t, rigOpts{env: map[string]string{"SANDBOX_IMAGES": "work=base-image,coding=dev-image"}})

	list := r.json("GET", "/api/environments", "alice", "", 200)
	envs := list["environments"].([]any)
	if len(envs) != 2 || list["selectable"] != true || list["default"] != "work" {
		t.Fatalf("environments = %v", list)
	}
	first := envs[0].(map[string]any)
	if first["name"] != "work" || first["kind"] == "" || first["egress"] == "" || first["description"] == "" {
		t.Fatalf("environment = %v", first)
	}

	// chosen at creation
	id := r.json("POST", "/api/threads", "alice", `{"environment":"coding"}`, 201)["id"].(string)
	if got := r.json("GET", "/api/threads/"+id+"/environment", "alice", "", 200); got["environment"] != "coding" || got["frozen"] != false {
		t.Fatalf("thread environment = %v", got)
	}
	// unknown names are refused, at creation and later
	if resp := r.do("POST", "/api/threads", "alice", `{"environment":"nope"}`); resp.StatusCode != 400 {
		t.Fatalf("unknown environment at creation: status %d", resp.StatusCode)
	}
	if resp := r.do("PUT", "/api/threads/"+id+"/environment", "alice", `{"environment":"nope"}`); resp.StatusCode != 400 {
		t.Fatalf("unknown environment: status %d", resp.StatusCode)
	}
	// changed while the thread has no workspace
	r.json("PUT", "/api/threads/"+id+"/environment", "alice", `{"environment":"work"}`, 200)
	r.json("PUT", "/api/threads/"+id+"/environment", "alice", `{"environment":"coding"}`, 200)

	// the sandbox is created in that environment, and then the choice is frozen
	if _, err := r.h.Sandboxes.Acquire(r.ctx, id, "alice"); err != nil {
		t.Fatal(err)
	}
	opts := r.backend.CreateOptions()
	if len(opts) != 1 || opts[0].Profile != "coding" {
		t.Fatalf("sandbox create options = %+v, want the coding profile", opts)
	}
	if resp := r.do("PUT", "/api/threads/"+id+"/environment", "alice", `{"environment":"work"}`); resp.StatusCode != 409 {
		t.Fatalf("changing a frozen environment: status %d, want 409", resp.StatusCode)
	}
	// saying the same thing again is harmless
	if got := r.json("PUT", "/api/threads/"+id+"/environment", "alice", `{"environment":"coding"}`, 200); got["frozen"] != true {
		t.Fatalf("thread environment = %v", got)
	}
	if resp := r.do("GET", "/api/threads/"+id+"/environment", "mallory", ""); resp.StatusCode != 404 {
		t.Fatalf("another user: status %d, want 404", resp.StatusCode)
	}
}

func TestASingleEnvironmentIsNotSelectableUnlessTheServerSaysSo(t *testing.T) {
	r := newRig(t, rigOpts{})
	if got := r.json("GET", "/api/environments", "alice", "", 200); got["selectable"] != false || len(got["environments"].([]any)) != 1 {
		t.Fatalf("environments = %v", got)
	}
	r2 := newRig(t, rigOpts{env: map[string]string{"ENVIRONMENT_SELECTOR": "true"}})
	if got := r2.json("GET", "/api/environments", "alice", "", 200); got["selectable"] != true {
		t.Fatalf("environments = %v", got)
	}
	r3 := newRig(t, rigOpts{noSandbox: true})
	if got := r3.json("GET", "/api/environments", "alice", "", 200); got["selectable"] != false || len(got["environments"].([]any)) != 0 {
		t.Fatalf("environments without a sandbox = %v", got)
	}
}

func TestPublishArtifactMarksAWorkspaceFileAsADeliverableOfTheAnswer(t *testing.T) {
	// Arrange: write a file, publish it, answer
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		tools := 0
		for _, m := range c.Messages {
			if m.Role == ai.RoleTool {
				tools++
			}
		}
		call := func(id, name, args string) llmtest.Turn {
			return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, id, name, args), "tool_calls")}}
		}
		switch tools {
		case 0:
			return call("w1", "write_file", `{"path":"/workspace/outputs/report.html","content":"<h1>Report</h1>"}`)
		case 1:
			return call("p1", "publish_artifact", `{"path":"outputs/report.html","title":"The report"}`)
		case 2:
			return call("p2", "publish_artifact", `{"path":"/workspace/missing.txt"}`)
		}
		return llmtest.Turn{Chunks: llmtest.Text("done")}
	})
	r := newRig(t, rigOpts{model: model})
	id := r.newThread("alice")

	// Act
	evs := r.stream(r.startRun("alice", id, "make a report"), "alice")
	r.waitIdle(id, "alice")

	// Assert: a published event follows the tool result, the thread lists the artifact, a missing file is refused
	var event map[string]any
	for _, e := range evs {
		var m map[string]any
		_ = json.Unmarshal([]byte(e.Data), &m)
		if m["type"] == "CUSTOM" && m["name"] == "work.artifact_published" {
			event = m["value"].(map[string]any)
		}
	}
	if event == nil || event["path"] != "/workspace/outputs/report.html" || event["title"] != "The report" || event["toolCallId"] != "p1" {
		t.Fatalf("published event = %v", event)
	}
	resp := r.do("GET", "/api/threads/"+id+"/published", "alice", "")
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0]["name"] != "report.html" || list[0]["title"] != "The report" || list[0]["mimeType"] == "" || list[0]["artifactId"] == "" {
		t.Fatalf("published = %v", list)
	}
	th := r.json("GET", "/api/threads/"+id, "alice", "", 200)
	var failed bool
	for _, m := range th["messages"].([]any) {
		if m := m.(map[string]any); m["toolCallId"] == "p2" && strings.Contains(m["content"].(string), "does not exist") {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("publishing a missing file was not refused: %v", th["messages"])
	}
	if resp := r.do("GET", "/api/threads/"+id+"/published", "mallory", ""); resp.StatusCode != 404 {
		t.Fatalf("another user: status %d, want 404", resp.StatusCode)
	}
}
