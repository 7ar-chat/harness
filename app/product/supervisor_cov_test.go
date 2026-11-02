package product

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
)

// tmSup is a team rig with a bound supervisor, whose tools the tests call directly (the model-driven path is
// covered by supervisor_test.go). alice owns "api", bob is a member, carol a viewer, dave is outside.
type tmSup struct {
	*rig
	tm      *teamstore.Mem
	sv      *Supervisor
	project string
	skew    *atomic.Int64 // nanoseconds the store's clock runs behind
	ctx     context.Context
}

// tmSupModel plays the agents: the assistant (it has list_sessions) acknowledges; a session answers by what its
// users wrote: "block" never ends, "ask" asks a question, "exec" runs a command (which needs approval), "boom" fails.
func tmSupModel() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		assistant := false
		for _, tl := range c.Tools {
			if toolName(tl) == "list_sessions" {
				assistant = true
			}
		}
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		if assistant {
			return llmtest.Turn{Chunks: llmtest.Text("ack")}
		}
		if last := c.Messages[len(c.Messages)-1]; last.Role == "tool" {
			return llmtest.Turn{Chunks: llmtest.Text("done")}
		}
		said := ""
		for _, m := range c.Messages {
			if m.Role == "user" {
				said += m.Content + "\n"
			}
		}
		switch {
		case strings.Contains(said, "block"):
			return llmtest.Turn{Chunks: llmtest.Text("working"), Block: true}
		case strings.Contains(said, "ask"):
			return llmtest.Turn{Chunks: []*aiMsg{llmtest.Finish(llmtest.ToolCall(0, "q1", "ask_user", `{"question":"Which colour?"}`), "tool_calls")}}
		case strings.Contains(said, "exec"):
			return llmtest.Turn{Chunks: []*aiMsg{llmtest.Finish(llmtest.ToolCall(0, "e1", "execute_command", `{"command":"echo hi"}`), "tool_calls")}}
		case strings.Contains(said, "boom"):
			return llmtest.Turn{Err: errors.New("400 bad request")}
		}
		return llmtest.Turn{Chunks: llmtest.Text("done")}
	})
}

func newTmSup(t *testing.T) *tmSup {
	t.Helper()
	tm := teamstore.NewMem()
	ctx := context.Background()
	for _, l := range []string{"alice", "bob", "carol", "dave"} {
		if _, err := tm.UpsertUser(ctx, store.User{Login: l, Name: strings.ToUpper(l[:1]) + l[1:], Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	skew := new(atomic.Int64)
	st := memstore.New()
	st.Now = func() time.Time { return time.Now().Add(-time.Duration(skew.Load())) }
	sv := NewSupervisor(tm)
	r := newRig(t, rigOpts{team: true, store: st, model: tmSupModel(), sup: sv, prod: Options{Team: tm}})
	p := r.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	r.json("PUT", "/api/projects/"+p+"/members/bob", "alice", `{"role":"member"}`, 204)
	r.json("PUT", "/api/projects/"+p+"/members/carol", "alice", `{"role":"viewer"}`, 204)
	return &tmSup{rig: r, tm: tm, sv: sv, project: p, skew: skew, ctx: ctx}
}

// thread creates a project thread as user.
func (r *tmSup) thread(user, title string) string {
	r.t.Helper()
	return r.json("POST", "/api/threads", user, `{"projectId":"`+r.project+`","title":`+jsonQuote(title)+`}`, 201)["id"].(string)
}

// run starts a run in thread as user and returns its id; interactive runs pause on risky tools.
func (r *tmSup) run(user, thread, msg string, interactive bool) string {
	r.t.Helper()
	body := `{"message":` + jsonQuote(msg) + `,"interactive":` + map[bool]string{true: "true", false: "false"}[interactive] + `}`
	return r.json("POST", "/api/threads/"+thread+"/runs", user, body, 202)["id"].(string)
}

// settle runs a message to the end of its stream.
func (r *tmSup) settle(user, thread, msg string, interactive bool) {
	r.t.Helper()
	finished(r.t, r.stream(r.run(user, thread, msg, interactive), user))
	r.waitIdle(thread, user)
}

// asJSON round-trips a tool result into generic JSON, as the model sees it.
func asJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (r *tmSup) sessions(user string, in listArgs) []map[string]any {
	r.t.Helper()
	res, err := r.sv.listSessions(r.ctx, user, in)
	if err != nil {
		r.t.Fatalf("listSessions(%s): %v", user, err)
	}
	var out []map[string]any
	list, _ := asJSON(r.t, res)["sessions"].([]any) // null when there are none
	for _, s := range list {
		out = append(out, s.(map[string]any))
	}
	return out
}

func TestTmListSessionsRanksWhatNeedsTheUserFirst(t *testing.T) {
	// Arrange: one session per state
	r := newTmSup(t)
	idle := r.thread("alice", "idle one")
	r.settle("alice", idle, "hello", false)
	failed := r.thread("alice", "failed one")
	r.run("alice", failed, "boom", false)
	r.waitIdle(failed, "alice")
	asking := r.thread("alice", "asking one")
	finished(t, r.stream(r.run("alice", asking, "ask me", true), "alice"))
	r.waitIdle(asking, "alice")
	waiting := r.thread("alice", "approval one")
	finished(t, r.stream(r.run("alice", waiting, "exec it", true), "alice"))
	r.waitIdle(waiting, "alice")
	running := r.thread("alice", "running one")
	r.run("alice", running, "block", false)

	// Act
	rows := r.sessions("bob", listArgs{})

	// Assert: approvals/questions, then failures, then running, then the rest; each says what it waits for
	var order []string
	for _, row := range rows {
		order = append(order, row["title"].(string))
	}
	if len(order) != 5 {
		t.Fatalf("sessions = %v", order)
	}
	if !(strings.HasPrefix(order[0], "a") && strings.HasPrefix(order[1], "a")) { // "approval one" / "asking one"
		t.Fatalf("attention first: %v", order)
	}
	if order[2] != "failed one" || order[3] != "running one" || order[4] != "idle one" {
		t.Fatalf("order = %v", order)
	}
	byTitle := map[string]map[string]any{}
	for _, row := range rows {
		byTitle[row["title"].(string)] = row
	}
	if w := byTitle["approval one"]["waiting_for"]; w != "approval for tool execute_command" {
		t.Fatalf("approval waits for %v", w)
	}
	if w := byTitle["asking one"]["waiting_for"]; w != "answer to: Which colour?" {
		t.Fatalf("question waits for %v", w)
	}
	if byTitle["idle one"]["project"] != "api" || byTitle["idle one"]["created_by"] != "alice" || byTitle["idle one"]["status"] != "idle" {
		t.Fatalf("idle row = %v", byTitle["idle one"])
	}
	// filters
	if only := r.sessions("bob", listArgs{Status: "failed"}); len(only) != 1 || only[0]["title"] != "failed one" {
		t.Fatalf("status filter = %v", only)
	}
	if got := r.sessions("bob", listArgs{ProjectID: r.project, Status: "waiting_input"}); len(got) != 1 || got[0]["title"] != "asking one" {
		t.Fatalf("project+status filter = %v", got)
	}
	// the user's assistant thread is never one of their sessions
	if _, err := r.sv.s.supervisorThread(r.ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if got := r.sessions("bob", listArgs{}); len(got) != 5 {
		t.Fatalf("assistant thread listed: %v", got)
	}
	// a project the user is not in is "not found", a viewer may list
	if _, err := r.sv.listSessions(r.ctx, "dave", listArgs{ProjectID: r.project}); !errors.Is(err, errNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	if got := r.sessions("carol", listArgs{ProjectID: r.project}); len(got) != 5 {
		t.Fatalf("viewer sees %d sessions", len(got))
	}
	if got := r.sessions("dave", listArgs{}); len(got) != 0 {
		t.Fatalf("dave sees %v", got)
	}
}

func TestTmAttentionRank(t *testing.T) {
	order := []string{"waiting_approval", "waiting_input", "failed", "running", "queued", "idle", "whatever"}
	want := []int{0, 0, 1, 2, 2, 3, 3}
	for i, s := range order {
		if got := attentionRank(s); got != want[i] {
			t.Errorf("attentionRank(%q) = %d, want %d", s, got, want[i])
		}
	}
}

func TestTmReadSessionShowsAuthorsPendingStateAndClipsLongText(t *testing.T) {
	// Arrange
	r := newTmSup(t)
	th := r.thread("alice", "work")
	r.settle("alice", th, "first from alice", false)
	r.settle("bob", th, "second from bob", false)
	if _, err := r.sv.sendToSession(r.ctx, "carol", sendArgs{ThreadID: th, Message: "x"}); !errors.Is(err, errForbidden) {
		t.Fatalf("viewer send: %v", err)
	}
	sent, err := r.sv.sendToSession(r.ctx, "bob", sendArgs{ThreadID: th, Message: "from the assistant"})
	if err != nil {
		t.Fatal(err)
	}
	finished(t, r.stream(asJSON(t, sent)["run_id"].(string), "bob"))
	r.waitIdle(th, "bob")

	// Act
	res, err := r.sv.readSession(r.ctx, "carol", readArgs{ThreadID: th}) // a viewer may read
	if err != nil {
		t.Fatal(err)
	}
	last1, _ := r.sv.readSession(r.ctx, "carol", readArgs{ThreadID: th, Last: 1})

	// Assert: authors by display name, the supervisor named with who it acts for
	doc := asJSON(t, res)
	var users []string
	for _, m := range doc["messages"].([]any) {
		if m := m.(map[string]any); m["role"] == "user" {
			users = append(users, m["author"].(string)+": "+m["content"].(string))
		}
	}
	want := []string{"Alice: first from alice", "Bob: second from bob", "supervisor (for bob): from the assistant"}
	if strings.Join(users, "|") != strings.Join(want, "|") {
		t.Fatalf("users = %v", users)
	}
	if doc["title"] != "work" || doc["thread_id"] != th {
		t.Fatalf("doc = %v", doc)
	}
	if msgs := asJSON(t, last1)["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("last_messages=1 gave %d", len(msgs))
	}
	if _, err := r.sv.readSession(r.ctx, "dave", readArgs{ThreadID: th}); !errors.Is(err, errNotFound) {
		t.Fatalf("outsider read: %v", err)
	}
	if _, err := r.sv.readSession(r.ctx, "alice", readArgs{ThreadID: "no-such-thread"}); err == nil {
		t.Fatal("reading an unknown thread succeeded")
	}

	// pending approval and question are reported with their ids
	ap := r.thread("alice", "approval")
	finished(t, r.stream(r.run("alice", ap, "exec", true), "alice"))
	r.waitIdle(ap, "alice")
	q := r.thread("alice", "question")
	finished(t, r.stream(r.run("alice", q, "ask", true), "alice"))
	r.waitIdle(q, "alice")
	apDoc, _ := r.sv.readSession(r.ctx, "bob", readArgs{ThreadID: ap})
	qDoc, _ := r.sv.readSession(r.ctx, "bob", readArgs{ThreadID: q})
	if pa, _ := asJSON(t, apDoc)["pending_approval"].(map[string]any); pa["toolName"] != "execute_command" || pa["id"] == "" {
		t.Fatalf("pending approval = %v", asJSON(t, apDoc))
	}
	if pq, _ := asJSON(t, qDoc)["pending_question"].(map[string]any); pq["question"] != "Which colour?" || pq["interruptId"] == "" {
		t.Fatalf("pending question = %v", asJSON(t, qDoc))
	}
	run := r.thread("alice", "running")
	id := r.run("alice", run, "block", false)
	rDoc, _ := r.sv.readSession(r.ctx, "bob", readArgs{ThreadID: run})
	if ar, _ := asJSON(t, rDoc)["active_run"].(map[string]any); ar["id"] != id {
		t.Fatalf("active run = %v", asJSON(t, rDoc))
	}
}

func TestTmClipTruncatesByBytes(t *testing.T) {
	if clip("short", 10) != "short" || clip("exactly10!", 10) != "exactly10!" {
		t.Fatal("text within the limit must be returned as is")
	}
	if got := clip(strings.Repeat("a", 20), 5); got != "aaaaa…" {
		t.Fatalf("clip = %q", got)
	}
}

func TestTmSendToSession(t *testing.T) {
	r := newTmSup(t)
	th := r.thread("alice", "work")

	// validation and authorization come before anything is touched
	if _, err := r.sv.sendToSession(r.ctx, "bob", sendArgs{ThreadID: th, Message: "  "}); err == nil || !strings.Contains(err.Error(), "message is required") {
		t.Fatalf("blank: %v", err)
	}
	if _, err := r.sv.sendToSession(r.ctx, "carol", sendArgs{ThreadID: th, Message: "hi"}); !errors.Is(err, errForbidden) {
		t.Fatalf("viewer: %v", err)
	}
	if _, err := r.sv.sendToSession(r.ctx, "dave", sendArgs{ThreadID: th, Message: "hi"}); !errors.Is(err, errNotFound) {
		t.Fatalf("outsider: %v", err)
	}

	// an idle session gets a new run; the audit log names the supervisor and who it acts for
	res, err := r.sv.sendToSession(r.ctx, "bob", sendArgs{ThreadID: th, Message: "please start"})
	if err != nil || asJSON(t, res)["delivered_as"] != "new_run" {
		t.Fatalf("idle send = %v, %v", res, err)
	}
	finished(t, r.stream(asJSON(t, res)["run_id"].(string), "bob"))
	r.waitIdle(th, "bob")
	log, _ := r.tm.ListAudit(r.ctx, r.project, 20)
	var seen bool
	for _, e := range log {
		seen = seen || (e.Action == "supervisor.send" && e.Actor == "supervisor" && e.OnBehalfOf == "bob" && e.ThreadID == th)
	}
	if !seen {
		t.Fatalf("audit = %+v", log)
	}

	// a running session is steered, not restarted
	r.run("alice", th, "block", false)
	res, err = r.sv.sendToSession(r.ctx, "bob", sendArgs{ThreadID: th, Message: "use tabs"})
	if err != nil || asJSON(t, res)["delivered_as"] != "steer" {
		t.Fatalf("running send = %v, %v", res, err)
	}

	// a session waiting for a decision must be answered through the right tool
	wait := r.thread("alice", "waiting")
	finished(t, r.stream(r.run("alice", wait, "exec", true), "alice"))
	r.waitIdle(wait, "alice")
	if _, err := r.sv.sendToSession(r.ctx, "bob", sendArgs{ThreadID: wait, Message: "go on"}); err == nil || !strings.Contains(err.Error(), "session_approve") {
		t.Fatalf("send to a waiting session: %v", err)
	}
}

func TestTmApproveAnswerAndCancel(t *testing.T) {
	r := newTmSup(t)

	// approve: argument and state checks
	wait := r.thread("alice", "waiting")
	idle := r.thread("alice", "idle")
	r.settle("alice", idle, "hello", false)
	if _, err := r.sv.approveSession(r.ctx, "bob", approveArgs{ThreadID: wait, Decision: "maybe"}); err == nil || !strings.Contains(err.Error(), "approve or deny") {
		t.Fatalf("bad decision: %v", err)
	}
	if _, err := r.sv.approveSession(r.ctx, "bob", approveArgs{ThreadID: idle, Decision: "approve"}); err == nil || !strings.Contains(err.Error(), "no pending approval") {
		t.Fatalf("nothing pending: %v", err)
	}
	finished(t, r.stream(r.run("alice", wait, "exec", true), "alice"))
	r.waitIdle(wait, "alice")
	if _, err := r.sv.approveSession(r.ctx, "carol", approveArgs{ThreadID: wait, Decision: "approve"}); !errors.Is(err, errForbidden) {
		t.Fatalf("viewer approving: %v", err)
	}
	if _, err := r.sv.approveSession(r.ctx, "dave", approveArgs{ThreadID: wait, Decision: "approve"}); !errors.Is(err, errNotFound) {
		t.Fatalf("outsider approving: %v", err)
	}
	// a denial resumes the run without executing, and names the tool
	res, err := r.sv.approveSession(r.ctx, "bob", approveArgs{ThreadID: wait, Decision: "deny"})
	if err != nil {
		t.Fatal(err)
	}
	doc := asJSON(t, res)
	if doc["tool"] != "execute_command" || doc["decision"] != "deny" {
		t.Fatalf("approval = %v", doc)
	}
	finished(t, r.stream(doc["run_id"].(string), "bob"))
	r.waitIdle(wait, "bob")
	log, _ := r.tm.ListAudit(r.ctx, r.project, 50)
	var audited bool
	for _, e := range log {
		audited = audited || (e.Action == "supervisor.approval" && e.OnBehalfOf == "bob" && e.Detail["decision"] == "deny" && e.Detail["tool"] == "execute_command")
	}
	if !audited {
		t.Fatalf("audit = %+v", log)
	}
	if _, err := r.sv.approveSession(r.ctx, "bob", approveArgs{ThreadID: wait, Decision: "approve"}); err == nil {
		t.Fatal("approving twice succeeded")
	}

	// answer
	q := r.thread("alice", "question")
	if _, err := r.sv.answerSession(r.ctx, "bob", answerArgs{ThreadID: q, Text: "red"}); err == nil || !strings.Contains(err.Error(), "no pending question") {
		t.Fatalf("nothing asked: %v", err)
	}
	finished(t, r.stream(r.run("alice", q, "ask me", true), "alice"))
	r.waitIdle(q, "alice")
	if _, err := r.sv.answerSession(r.ctx, "carol", answerArgs{ThreadID: q, Text: "red"}); !errors.Is(err, errForbidden) {
		t.Fatalf("viewer answering: %v", err)
	}
	res, err = r.sv.answerSession(r.ctx, "bob", answerArgs{ThreadID: q, Text: "red"})
	if err != nil {
		t.Fatal(err)
	}
	finished(t, r.stream(asJSON(t, res)["run_id"].(string), "bob"))
	r.waitIdle(q, "bob")
	if th := r.json("GET", "/api/threads/"+q, "alice", "", 200); th["pendingQuestion"] != nil {
		t.Fatalf("question still pending: %v", th["pendingQuestion"])
	}

	// cancel
	run := r.thread("alice", "running")
	if _, err := r.sv.cancelSession(r.ctx, "bob", cancelArgs{ThreadID: run}); err == nil || !strings.Contains(err.Error(), "no running run") {
		t.Fatalf("nothing running: %v", err)
	}
	id := r.run("alice", run, "block", false)
	if _, err := r.sv.cancelSession(r.ctx, "carol", cancelArgs{ThreadID: run}); !errors.Is(err, errForbidden) {
		t.Fatalf("viewer cancelling: %v", err)
	}
	if _, err := r.sv.cancelSession(r.ctx, "bob", cancelArgs{ThreadID: run}); err != nil {
		t.Fatal(err)
	}
	evs := r.stream(id, "alice")
	if out := finished(t, evs)["outcome"].(map[string]any); out["type"] != "cancelled" {
		t.Fatalf("outcome = %v", out)
	}
	log, _ = r.tm.ListAudit(r.ctx, r.project, 50)
	audited = false
	for _, e := range log {
		audited = audited || (e.Action == "supervisor.cancel" && e.OnBehalfOf == "bob")
	}
	if !audited {
		t.Fatalf("cancel not audited: %+v", log)
	}
}

func TestTmSpawnSession(t *testing.T) {
	r := newTmSup(t)
	if _, err := r.sv.spawnSession(r.ctx, "bob", spawnArgs{ProjectID: r.project, Task: " "}); err == nil || !strings.Contains(err.Error(), "task is required") {
		t.Fatalf("blank task: %v", err)
	}
	if _, err := r.sv.spawnSession(r.ctx, "carol", spawnArgs{ProjectID: r.project, Task: "x"}); !errors.Is(err, errForbidden) {
		t.Fatalf("viewer: %v", err)
	}
	if _, err := r.sv.spawnSession(r.ctx, "dave", spawnArgs{ProjectID: r.project, Task: "x"}); !errors.Is(err, errNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	if _, err := r.sv.spawnSession(r.ctx, "bob", spawnArgs{ProjectID: "nope", Task: "x"}); !errors.Is(err, errNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	if ths, _ := r.h.Store.ListThreads(r.ctx, "carol", 10, nil); len(ths) != 0 {
		t.Fatalf("a refused spawn created threads: %v", ths)
	}

	res, err := r.sv.spawnSession(r.ctx, "bob", spawnArgs{ProjectID: r.project, Task: "do the thing", Title: "Spawned"})
	if err != nil {
		t.Fatal(err)
	}
	doc := asJSON(t, res)
	id := doc["thread_id"].(string)
	finished(t, r.stream(doc["run_id"].(string), "bob"))
	r.waitIdle(id, "bob")
	th := r.json("GET", "/api/threads/"+id, "alice", "", 200) // visible to the whole project
	if th["title"] != "Spawned" || th["createdBy"] != "bob" || th["projectId"] != r.project {
		t.Fatalf("spawned = %v", th)
	}
	first := th["messages"].([]any)[0].(map[string]any)
	if a := first["author"].(map[string]any); first["content"] != "do the thing" || a["kind"] != "supervisor" || a["onBehalfOf"] != "bob" {
		t.Fatalf("first message = %v", first)
	}
}

func TestTmSupervisorSettingsRoutes(t *testing.T) {
	r := newTmSup(t)
	ctx := context.Background()

	// defaults come from the supervisor; bad minutes fall back to them, valid ones are kept
	if got := r.json("GET", "/api/supervisor/settings", "bob", "", 200); got["watch"] != false || got["stuckMinutes"] != float64(10) {
		t.Fatalf("defaults = %v", got)
	}
	for body, want := range map[string]float64{
		`{"watch":true,"stuckMinutes":0}`:    10,
		`{"watch":true,"stuckMinutes":-5}`:   10,
		`{"watch":true,"stuckMinutes":1441}`: 10,
		`{"watch":true,"stuckMinutes":1440}`: 1440,
		`{"watch":false,"stuckMinutes":1}`:   1,
	} {
		if got := r.json("PUT", "/api/supervisor/settings", "bob", body, 200); got["stuckMinutes"] != want {
			t.Errorf("%s: stuckMinutes = %v, want %v", body, got["stuckMinutes"], want)
		}
	}
	r.json("PUT", "/api/supervisor/settings", "bob", `{`, 400)
	r.json("PUT", "/api/supervisor/settings", "bob", `{"watch":true,"stuckMinutes":7}`, 200)
	// stored per user, next to the user's other preferences
	if err := r.tm.SetUserPrefs(ctx, "carol", map[string]any{"theme": "dark"}); err != nil {
		t.Fatal(err)
	}
	r.json("PUT", "/api/supervisor/settings", "carol", `{"watch":true,"stuckMinutes":3}`, 200)
	prefs, _ := r.tm.UserPrefs(ctx, "carol")
	if prefs["theme"] != "dark" || prefs["supervisor"] == nil {
		t.Fatalf("carol's prefs = %v", prefs)
	}
	if got := r.json("GET", "/api/supervisor/settings", "bob", "", 200); got["watch"] != true || got["stuckMinutes"] != float64(7) {
		t.Fatalf("bob = %v", got)
	}
	if got := r.json("GET", "/api/supervisor/settings", "alice", "", 200); got["watch"] != false {
		t.Fatalf("alice inherited %v", got)
	}

	// the settings reader accepts ints (in-memory) as well as floats (JSON round trip) and ignores nonsense
	for name, tc := range map[string]struct {
		prefs map[string]any
		want  supervisorSettings
	}{
		"int":            {map[string]any{"supervisor": map[string]any{"watch": true, "stuckMinutes": 4}}, supervisorSettings{true, 4}},
		"float":          {map[string]any{"supervisor": map[string]any{"watch": true, "stuckMinutes": float64(6)}}, supervisorSettings{true, 6}},
		"zero minutes":   {map[string]any{"supervisor": map[string]any{"stuckMinutes": 0}}, supervisorSettings{false, 10}},
		"wrong type":     {map[string]any{"supervisor": map[string]any{"watch": "yes", "stuckMinutes": "9"}}, supervisorSettings{false, 10}},
		"not an object":  {map[string]any{"supervisor": "on"}, supervisorSettings{false, 10}},
		"no preferences": {nil, supervisorSettings{false, 10}},
	} {
		if err := r.tm.SetUserPrefs(ctx, "dave", tc.prefs); err != nil {
			t.Fatal(err)
		}
		if got := r.sv.settings(ctx, "dave"); got != tc.want {
			t.Errorf("%s: settings = %+v, want %+v", name, got, tc.want)
		}
	}
}

func TestTmSupervisorThreadIsRepairedAndRenamed(t *testing.T) {
	r := newTmSup(t)
	ctx := context.Background()
	id := supervisorThread(r.rig, "bob")

	// an assistant created under its former name is renamed on next use
	old := "Global agent"
	if _, err := r.h.Store.UpdateThread(ctx, "bob", id, store.ThreadPatch{Title: &old}); err != nil {
		t.Fatal(err)
	}
	if again := supervisorThread(r.rig, "bob"); again != id {
		t.Fatalf("thread changed: %s -> %s", id, again)
	}
	if got := r.json("GET", "/api/threads/"+id, "bob", "", 200)["title"]; got != assistantTitle {
		t.Fatalf("title = %v", got)
	}

	// if the thread itself vanished (the link survives), a fresh one is created and linked
	if _, err := r.h.Store.DeleteThread(ctx, "bob", id); err != nil {
		t.Fatal(err)
	}
	fresh := supervisorThread(r.rig, "bob")
	if fresh == id {
		t.Fatal("the assistant thread was not recreated")
	}
	if m, err := r.tm.SupervisorThread(ctx, "bob"); err != nil || m.ThreadID != fresh {
		t.Fatalf("link = %+v, %v", m, err)
	}
	if !r.sv.isSupervisorThread(ctx, fresh) || r.sv.isSupervisorThread(ctx, "unknown") || (&Supervisor{}).isSupervisorThread(ctx, fresh) {
		t.Fatal("isSupervisorThread")
	}
}

func TestTmSupervisorToolsAreOfferedOnSupervisorThreadsOnly(t *testing.T) {
	r := newTmSup(t)
	sup := supervisorThread(r.rig, "bob")
	plain := r.thread("bob", "plain")
	list := func(thread, login string) []string {
		tl, cleanup, err := r.sv.Tools().Tools(r.ctx, tmRunContext(thread, login))
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		var names []string
		for _, x := range tl {
			info, err := x.Info(r.ctx)
			if err != nil {
				t.Fatal(err)
			}
			names = append(names, info.Name)
		}
		return names
	}

	got := list(sup, "bob")
	want := []string{"list_sessions", "read_session", "session_send", "session_approve", "session_answer", "session_cancel", "session_spawn"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("supervisor tools = %v", got)
	}
	if got := list(plain, "bob"); len(got) != 0 {
		t.Fatalf("an ordinary thread got %v", got)
	}
	if got := list(sup, "bob"); len(got) == 0 {
		t.Fatal("tools vanished")
	}
	if r.sv.Tools().Name() != "supervisor" {
		t.Fatal("provider name")
	}
	// unbound: nothing, rather than a crash
	if tl, _, err := (&Supervisor{team: r.tm}).Tools().Tools(r.ctx, tmRunContext(sup, "bob")); err != nil || len(tl) != 0 {
		t.Fatalf("unbound supervisor offered %v, %v", tl, err)
	}
}

func TestTmAttentionEventsAndRecipients(t *testing.T) {
	for _, tc := range []struct {
		ev   FeedEvent
		what string
		ok   bool
	}{
		{FeedEvent{Type: "run.finished", Status: "waiting_approval"}, "waits for an approval", true},
		{FeedEvent{Type: "run.finished", Status: "waiting_input"}, "asks a question", true},
		{FeedEvent{Type: "run.finished", Status: "failed"}, "failed", true},
		{FeedEvent{Type: "run.finished", Status: "interrupted"}, "failed", true},
		{FeedEvent{Type: "run.finished", Status: "completed"}, "", false},
		{FeedEvent{Type: "run.finished", Status: "cancelled"}, "", false},
		{FeedEvent{Type: "run.started", Status: "failed"}, "", false},
		{FeedEvent{Type: "thread.created"}, "", false},
	} {
		what, ok := attentionEvent(tc.ev)
		if what != tc.what || ok != tc.ok {
			t.Errorf("attentionEvent(%s/%s) = %q, %v", tc.ev.Type, tc.ev.Status, what, ok)
		}
	}

	r := newTmSup(t)
	got := func(project, owner string) string {
		l := r.sv.recipients(r.ctx, project, owner)
		return strings.Join(l, ",")
	}
	if got("", "bob") != "bob" {
		t.Fatalf("personal = %q", got("", "bob"))
	}
	// a project's event goes to the members who can act (viewers cannot answer, so are not nagged)
	if g := got(r.project, "alice"); !strings.Contains(g, "alice") || !strings.Contains(g, "bob") || strings.Contains(g, "carol") {
		t.Fatalf("project recipients = %q", g)
	}
	if got("unknown-project", "alice") != "" {
		t.Fatalf("recipients of an unknown project = %q", got("unknown-project", "alice"))
	}
	if d := dedupe([]string{"a", "b", "a", "c", "b"}); strings.Join(d, "") != "abc" {
		t.Fatalf("dedupe = %v", d)
	}
}

// watch turns the watcher on for the logins.
func (r *tmSup) watch(logins ...string) {
	r.t.Helper()
	for _, l := range logins {
		if err := r.tm.SetUserPrefs(r.ctx, l, map[string]any{"supervisor": map[string]any{"watch": true, "stuckMinutes": 1}}); err != nil {
			r.t.Fatal(err)
		}
	}
}

func (r *tmSup) pending(login string) []string {
	r.sv.mu.Lock()
	defer r.sv.mu.Unlock()
	return append([]string(nil), r.sv.pending[login]...)
}

func TestTmWatcherQueuesAttentionForWatchingMembersOnly(t *testing.T) {
	// Arrange: alice and carol (a viewer) watch; bob does not
	r := newTmSup(t)
	r.watch("alice", "carol")
	th := r.thread("bob", "Fix the build")

	// Act
	r.sv.onEvent(r.ctx, FeedEvent{Type: "run.finished", Status: "waiting_approval", ProjectID: r.project, ThreadID: th, owner: "bob"})
	r.sv.onEvent(r.ctx, FeedEvent{Type: "run.finished", Status: "completed", ProjectID: r.project, ThreadID: th, owner: "bob"}) // not attention
	r.sv.onEvent(r.ctx, FeedEvent{Type: "run.finished", Status: "failed", ProjectID: r.project, owner: "bob"})                  // no thread
	r.sv.onEvent(r.ctx, FeedEvent{Type: "run.finished", Status: "failed", ThreadID: supervisorThread(r.rig, "alice"), owner: "alice"})

	// Assert
	lines := r.pending("alice")
	if len(lines) != 1 || !strings.Contains(lines[0], `"Fix the build"`) || !strings.Contains(lines[0], th) || !strings.Contains(lines[0], "waits for an approval") {
		t.Fatalf("alice's attention = %v", lines)
	}
	if len(r.pending("bob")) != 0 {
		t.Fatalf("bob does not watch: %v", r.pending("bob"))
	}
	if len(r.pending("carol")) != 0 {
		t.Fatalf("a viewer cannot act on the session, so is not nagged: %v", r.pending("carol"))
	}
}

func TestTmWatcherFlushStartsOneBatchedRunPerUserAndHonoursTheCooldown(t *testing.T) {
	// Arrange
	r := newTmSup(t)
	r.watch("alice")
	t1, t2 := r.thread("bob", "First"), r.thread("bob", "Second")
	for _, th := range []string{t1, t2, t1} {
		r.sv.onEvent(r.ctx, FeedEvent{Type: "run.finished", Status: "failed", ProjectID: r.project, ThreadID: th, owner: "bob"})
	}
	sup := supervisorThread(r.rig, "alice")

	// Act
	r.sv.flush(r.ctx)
	r.waitIdle(sup, "alice")

	// Assert: one run on alice's assistant thread, one line per distinct session, authored by the system
	msgs := r.json("GET", "/api/threads/"+sup, "alice", "", 200)["messages"].([]any)
	first := msgs[0].(map[string]any)
	content := first["content"].(string)
	if !strings.HasPrefix(content, "[watcher] These sessions need attention:") || strings.Count(content, "\n- ") != 2 {
		t.Fatalf("watcher message = %q", content)
	}
	if a := first["author"].(map[string]any); a["kind"] != "system" || a["onBehalfOf"] != "alice" {
		t.Fatalf("author = %v", a)
	}
	if len(r.pending("alice")) != 0 {
		t.Fatalf("delivered lines still pending: %v", r.pending("alice"))
	}

	// New attention inside the cooldown waits for a later flush instead of starting another run
	r.sv.onEvent(r.ctx, FeedEvent{Type: "run.finished", Status: "failed", ProjectID: r.project, ThreadID: t1, owner: "bob"})
	r.sv.flush(r.ctx)
	if len(r.pending("alice")) != 1 {
		t.Fatalf("cooldown: pending = %v", r.pending("alice"))
	}
	if n := len(r.json("GET", "/api/threads/"+sup, "alice", "", 200)["messages"].([]any)); n != len(msgs) {
		t.Fatalf("a second run started inside the cooldown: %d messages", n)
	}
	r.sv.Cooldown = 0
	r.sv.flush(r.ctx)
	r.waitIdle(sup, "alice")
	if len(r.pending("alice")) != 0 {
		t.Fatalf("pending after the cooldown = %v", r.pending("alice"))
	}
	if n := len(r.json("GET", "/api/threads/"+sup, "alice", "", 200)["messages"].([]any)); n <= len(msgs) {
		t.Fatalf("no new run after the cooldown: %d messages", n)
	}
	r.sv.flush(r.ctx) // nothing queued: nothing happens
}

func TestTmScanStuckReportsQuietRunsOnce(t *testing.T) {
	// Arrange: the store's clock runs two hours behind, so a running thread looks silent
	r := newTmSup(t)
	r.watch("alice")
	r.skew.Store(int64(2 * time.Hour))
	quiet := r.thread("alice", "Quiet one")
	run := r.run("alice", quiet, "block", false)
	r.skew.Store(0)
	fresh := r.thread("alice", "Fresh one")
	r.run("alice", fresh, "block", false)

	// Act
	r.sv.scanStuck(r.ctx)
	r.sv.scanStuck(r.ctx) // the same run is not reported twice

	// Assert
	lines := r.pending("alice")
	if len(lines) != 1 || !strings.Contains(lines[0], "Quiet one") || !strings.Contains(lines[0], "without activity for over 1 minutes") {
		t.Fatalf("stuck report = %v", lines)
	}
	if r.sv.stuck[quiet] != run {
		t.Fatalf("stuck map = %v", r.sv.stuck)
	}
	// users who do not watch are not scanned
	if len(r.pending("bob")) != 0 {
		t.Fatalf("bob = %v", r.pending("bob"))
	}
}

func TestTmScanStuckClaimsOnTheBusSoOneReplicaReports(t *testing.T) {
	// Arrange: two supervisors (replicas) sharing a bus and a store, both seeing the same quiet run
	bus := NewMemBus()
	r := newTmSup(t)
	r.watch("alice")
	r.skew.Store(int64(2 * time.Hour))
	quiet := r.thread("alice", "Quiet one")
	r.run("alice", quiet, "block", false)
	r.skew.Store(0)
	other := NewSupervisor(r.tm)
	other.s = r.sv.s
	r.sv.s.hub.bus = bus
	other.s = &server{Server: r.sv.s.Server, team: r.tm, hub: &hub{subs: map[*subscriber]struct{}{}, bus: bus}, sup: other}

	// Act
	r.sv.scanStuck(r.ctx)
	other.scanStuck(r.ctx)

	// Assert: exactly one of them queued it
	mine, theirs := r.pending("alice"), 0
	other.mu.Lock()
	theirs = len(other.pending["alice"])
	other.mu.Unlock()
	if len(mine)+theirs != 1 {
		t.Fatalf("replicas reported %d + %d times, want once", len(mine), theirs)
	}
	r.sv.s.hub.bus = nil
}

func TestTmWatchStopsWithItsContext(t *testing.T) {
	r := newTmSup(t)
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})

	go func() { r.sv.watch(ctx); close(done) }()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop")
	}
}

func TestTmWatchDeliversLocalFeedEventsAndIgnoresRemoteOnes(t *testing.T) {
	// Arrange
	r := newTmSup(t)
	r.watch("alice")
	th := r.thread("bob", "Broken")

	// Act: a remote event (from another replica) and a local one
	r.sv.s.hub.deliver(FeedEvent{Type: "run.finished", Status: "failed", ProjectID: r.project, ThreadID: th, owner: "bob", remote: true})
	r.sv.s.hub.publish(FeedEvent{Type: "run.finished", Status: "failed", ProjectID: r.project, ThreadID: th, owner: "bob"})

	// Assert: queued once, from the local one only
	deadline := time.After(5 * time.Second)
	for len(r.pending("alice")) == 0 {
		select {
		case <-deadline:
			t.Fatal("the watcher never queued the local event")
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(r.pending("alice")); n != 1 {
		t.Fatalf("queued %d times: %v", n, r.pending("alice"))
	}
}
