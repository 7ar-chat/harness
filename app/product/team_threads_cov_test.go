package product

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/client"
)

// tmModes plays the agent by what the users wrote: "block" never answers, "boom" fails, anything else answers.
func tmModes() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		last := ""
		for _, m := range c.Messages {
			if m.Role == "user" {
				last += m.Content + "\n" // a steer is one more user message: it must not unblock the run
			}
		}
		switch {
		case len(c.Tools) == 0:
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		case strings.Contains(last, "block"):
			return llmtest.Turn{Chunks: llmtest.Text("working"), Block: true}
		case strings.Contains(last, "boom"):
			return llmtest.Turn{Err: errTmBoom}
		}
		return llmtest.Turn{Chunks: llmtest.Text("done")}
	})
}

var errTmBoom = errors.New("400 bad request")

func (r *tmFaultRig) projectThread(user, title string) string {
	r.t.Helper()
	return r.json("POST", "/api/threads", user, `{"projectId":"`+r.project+`","title":`+jsonQuote(title)+`}`, 201)["id"].(string)
}

func jsonQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestTmCreateThreadAuthorization(t *testing.T) {
	r := newTmFaultRig(t)

	// A member creates in the project; viewers get 403, outsiders and unknown projects 404.
	id := r.projectThread("bob", "Bob's task")
	r.json("POST", "/api/threads", "carol", `{"projectId":"`+r.project+`"}`, 403)
	r.json("POST", "/api/threads", "dave", `{"projectId":"`+r.project+`"}`, 404)
	r.json("POST", "/api/threads", "alice", `{"projectId":"nope"}`, 404)
	r.json("POST", "/api/threads", "alice", `{`, 400)

	got := r.json("GET", "/api/threads/"+id, "alice", "", 200)
	if got["createdBy"] != "bob" || got["projectId"] != r.project || got["title"] != "Bob's task" || got["kind"] != "chat" || got["status"] != "idle" || got["role"] != "owner" {
		t.Fatalf("thread = %v", got)
	}
	// The thread is stored under its creator, who is therefore the owner of the data.
	if th, err := r.h.Store.GetThread(context.Background(), "bob", id); err != nil || th.Title != "Bob's task" {
		t.Fatalf("stored thread = %v, %v", th, err)
	}
	// Viewers read with their own role reported back.
	if got := r.json("GET", "/api/threads/"+id, "carol", "", 200); got["role"] != "viewer" {
		t.Fatalf("carol's role = %v", got["role"])
	}
	if body := rawBody(r.do("GET", "/api/projects/"+r.project+"/audit", "alice", "").Body); !strings.Contains(body, "thread.create") {
		t.Fatalf("audit = %s", body)
	}
}

func TestTmCreateThreadRollsBackWhenItCannotBeLinked(t *testing.T) {
	r := newTmFaultRig(t)
	r.fault.arm("SetThreadMeta")

	resp := r.do("POST", "/api/threads", "alice", `{"title":"orphan"}`)

	r.fault.disarm("SetThreadMeta")
	if body := rawBody(resp.Body); resp.StatusCode != 500 || strings.Contains(body, tmSecret) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	ths, err := r.h.Store.ListThreads(context.Background(), "alice", 50, nil)
	if err != nil || len(ths) != 0 {
		t.Fatalf("an unlinked thread was left behind: %v, %v", ths, err)
	}
}

func TestTmDeleteThreadRules(t *testing.T) {
	// Arrange: bob's and alice's threads in a project alice owns; carol is a viewer, dave an outsider.
	r := newTmFaultRig(t)
	aliceT, bobT := r.projectThread("alice", "a"), r.projectThread("bob", "b")
	del := func(user, id string) int { return r.do("DELETE", "/api/threads/"+id, user, "").StatusCode }

	// Act / Assert
	if got := del("bob", aliceT); got != 403 {
		t.Fatalf("a member deleted somebody else's thread: %d", got)
	}
	if got := del("carol", bobT); got != 403 {
		t.Fatalf("a viewer deleted a thread: %d", got)
	}
	if got := del("dave", bobT); got != 404 {
		t.Fatalf("an outsider got %d, want 404", got)
	}
	r.json("GET", "/api/threads/"+aliceT, "alice", "", 200) // refused deletes changed nothing
	if got := del("bob", bobT); got != 204 {                // the creator
		t.Fatalf("creator = %d", got)
	}
	if got := del("alice", aliceT); got != 204 { // the creator, who is also an owner
		t.Fatalf("owner = %d", got)
	}
	r.json("GET", "/api/threads/"+bobT, "alice", "", 404)
	if _, err := r.tm.GetThreadMeta(context.Background(), bobT); !strings.Contains(errString(err), "not found") {
		t.Fatalf("link survived the deletion: %v", err)
	}

	// An owner removes a member's thread; the member cannot remove the owner's.
	r.json("PUT", "/api/projects/"+r.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	bobT = r.projectThread("bob", "b2")
	if got := del("alice", bobT); got != 204 {
		t.Fatalf("owner deleting a member's thread = %d", got)
	}
	r.json("DELETE", "/api/threads/nope", "alice", "", 404)
	if body := rawBody(r.do("GET", "/api/projects/"+r.project+"/audit", "alice", "").Body); !strings.Contains(body, "thread.delete") {
		t.Fatalf("audit = %s", body)
	}
	// Personal threads: only their owner may delete.
	mine := r.json("POST", "/api/threads", "alice", `{}`, 201)["id"].(string)
	if got := del("bob", mine); got != 404 {
		t.Fatalf("another user deleting a personal thread = %d", got)
	}
	if got := del("alice", mine); got != 204 {
		t.Fatalf("personal delete = %d", got)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return strings.ToLower(err.Error())
}

func TestTmCanDeleteThread(t *testing.T) {
	cases := map[string]struct {
		c    Caller
		want bool
	}{
		"personal thread":          {Caller{Actor: "a", CreatedBy: "b"}, true},
		"creator in a project":     {Caller{Actor: "a", CreatedBy: "a", ProjectID: "p", Role: store.RoleMember}, true},
		"owner of the project":     {Caller{Actor: "a", CreatedBy: "b", ProjectID: "p", Role: store.RoleOwner}, true},
		"other member":             {Caller{Actor: "a", CreatedBy: "b", ProjectID: "p", Role: store.RoleMember}, false},
		"viewer":                   {Caller{Actor: "a", CreatedBy: "b", ProjectID: "p", Role: store.RoleViewer}, false},
		"no creator recorded, now": {Caller{Actor: "a", ProjectID: "p", Role: store.RoleMember}, false},
	}
	for name, tc := range cases {
		if got := canDeleteThread(tc.c); got != tc.want {
			t.Errorf("%s: canDeleteThread = %v, want %v", name, got, tc.want)
		}
	}
}

func TestTmListThreads(t *testing.T) {
	r := newTmFaultRig(t)
	pt := r.projectThread("bob", "in project")
	for _, title := range []string{"p1", "p2", "p3"} {
		r.json("POST", "/api/threads", "alice", `{"title":"`+title+`"}`, 201)
	}
	sup := r.json("POST", "/api/threads", "alice", `{"title":"hidden"}`, 201)["id"].(string)
	// A thread the team layer marks as the assistant's is not a session.
	r.tm.SetThreadMeta(context.Background(), store.ThreadMeta{ThreadID: sup, Owner: "alice", CreatedBy: "alice", Kind: store.ThreadSupervisor})

	personal := tmList(t, r.rig, "/api/threads", "alice")
	limited := tmList(t, r.rig, "/api/threads?limit=2", "alice")
	project := tmList(t, r.rig, "/api/threads?project="+r.project, "carol")

	if len(personal) != 3 {
		t.Fatalf("personal list = %v", personal)
	}
	for _, th := range personal {
		if th["id"] == pt || th["id"] == sup {
			t.Fatalf("project or assistant thread in the personal list: %v", th)
		}
	}
	if len(limited) != 2 {
		t.Fatalf("limit=2 gave %d", len(limited))
	}
	if len(project) != 1 || project[0]["id"] != pt || project[0]["createdBy"] != "bob" || project[0]["status"] != "idle" {
		t.Fatalf("project list = %v", project)
	}
	// A thread deleted from the store behind the team layer's back is skipped, not an error.
	if _, err := r.h.Store.DeleteThread(context.Background(), "bob", pt); err != nil {
		t.Fatal(err)
	}
	if got := tmList(t, r.rig, "/api/threads?project="+r.project, "alice"); len(got) != 0 {
		t.Fatalf("ghost thread listed: %v", got)
	}
	// A personal thread without a link (created before team mode) is listed as the caller's own.
	legacy, err := r.h.Store.CreateThread(context.Background(), "alice", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, th := range tmList(t, r.rig, "/api/threads", "alice") {
		if th["id"] == legacy.ID {
			found = th["createdBy"] == "alice" && th["kind"] == "chat"
		}
	}
	if !found {
		t.Fatal("an unlinked personal thread is not listed as the caller's")
	}
	r.json("GET", "/api/threads/"+legacy.ID, "alice", "", 200)
	r.json("GET", "/api/threads/"+legacy.ID, "bob", "", 404)
}

func TestTmRunLifecycleOnTheFeed(t *testing.T) {
	// Arrange
	r := newTmFaultRigModel(t, tmModes())
	th := r.projectThread("alice", "t")
	bob, carol := feedOf(r.rig, "bob"), feedOf(r.rig, "dave")

	// Act: a run that completes
	finished(t, r.stream(r.startRun("alice", th, "hello"), "alice"))

	// Assert: the finished event says "completed" (not the raw RUN_FINISHED outcome)
	line := tmWaitLine(t, bob, `"run.finished"`)
	if !strings.Contains(line, `"status":"completed"`) || !strings.Contains(line, `"actor":"alice"`) || !strings.Contains(line, th) {
		t.Fatalf("finished = %s", line)
	}
	tmQuiet(t, carol, "run")

	// A failing run reports "failed".
	r.waitIdle(th, "alice")
	run := r.startRun("bob", th, "boom")
	r.stream(run, "bob")
	if line := tmWaitLine(t, bob, `"status":"failed"`); !strings.Contains(line, `"actor":"bob"`) {
		t.Fatalf("failed = %s", line)
	}
	r.waitIdle(th, "alice")
	if rows := tmList(t, r.rig, "/api/threads?project="+r.project, "alice"); rows[0]["status"] != "failed" {
		t.Fatalf("summary = %v", rows[0])
	}
}

func TestTmCancelAndSteerAreAttributedAndPublished(t *testing.T) {
	// Arrange: a run that never finishes by itself
	r := newTmFaultRigModel(t, tmModes())
	th := r.projectThread("alice", "t")
	bob := feedOf(r.rig, "bob")
	run := r.startRun("alice", th, "block please")
	tmWaitLine(t, bob, `"run.started"`)
	if got := r.json("GET", "/api/threads/"+th, "bob", "", 200)["status"]; got != "running" {
		t.Fatalf("status while running = %v", got)
	}
	if rows := tmList(t, r.rig, "/api/threads?project="+r.project, "bob"); rows[0]["status"] != "running" || rows[0]["runId"] != run {
		t.Fatalf("summary while running = %v", rows[0])
	}

	// Act: a member steers, a viewer is refused, an outsider cannot see the run, then the member cancels
	steer := r.do("POST", "/api/runs/"+run+"/steer", "bob", `{"text":"use tabs"}`)
	viewer := r.do("POST", "/api/runs/"+run+"/cancel", "carol", "")
	outsider := r.do("POST", "/api/runs/"+run+"/cancel", "dave", "")
	cancel := r.do("POST", "/api/runs/"+run+"/cancel", "bob", "")

	// Assert
	if steer.StatusCode != 202 || viewer.StatusCode != 403 || outsider.StatusCode != 404 || cancel.StatusCode != 202 {
		t.Fatalf("steer %d, viewer cancel %d, outsider cancel %d, cancel %d", steer.StatusCode, viewer.StatusCode, outsider.StatusCode, cancel.StatusCode)
	}
	line := tmWaitLine(t, bob, `"message.sent"`)
	if !strings.Contains(line, run) || !strings.Contains(line, `"actor":"bob"`) {
		t.Fatalf("message.sent = %s", line)
	}
	if fin := tmWaitLine(t, bob, `"run.finished"`); !strings.Contains(fin, `"status":"cancelled"`) {
		t.Fatalf("finished = %s", fin)
	}
	body := rawBody(r.do("GET", "/api/projects/"+r.project+"/audit", "alice", "").Body)
	for _, want := range []string{"run.steer", "run.cancel", `"actor":"bob"`} {
		if !strings.Contains(body, want) {
			t.Errorf("audit lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"actor":"carol"`) {
		t.Errorf("the refused viewer's attempt was recorded as an action: %s", body)
	}
}

func TestTmForkFailureLeavesNoThreadBehind(t *testing.T) {
	r := newTmFaultRig(t)
	th := r.projectThread("alice", "t")
	finished(t, r.stream(r.startRun("alice", th, "hello"), "alice"))
	r.waitIdle(th, "alice")
	msgs := r.json("GET", "/api/threads/"+th, "alice", "", 200)["messages"].([]any)
	before, _ := r.h.Store.ListThreads(context.Background(), "alice", 50, nil)

	r.fault.arm("SetThreadMeta")
	resp := r.do("POST", "/api/threads/"+th+"/fork", "alice", `{"messageId":"`+msgs[0].(map[string]any)["id"].(string)+`"}`)
	r.fault.disarm("SetThreadMeta")

	if body := rawBody(resp.Body); resp.StatusCode != 500 || strings.Contains(body, tmSecret) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	after, _ := r.h.Store.ListThreads(context.Background(), "alice", 50, nil)
	if len(after) != len(before) {
		t.Fatalf("a fork that could not be linked stayed in the store: %d -> %d threads", len(before), len(after))
	}
}

func TestTmForkOfAPersonalThreadStaysPersonal(t *testing.T) {
	r := newTmFaultRig(t)
	th := r.json("POST", "/api/threads", "alice", `{}`, 201)["id"].(string)
	finished(t, r.stream(r.startRun("alice", th, "hello"), "alice"))
	r.waitIdle(th, "alice")
	msgs := r.json("GET", "/api/threads/"+th, "alice", "", 200)["messages"].([]any)

	fork := r.json("POST", "/api/threads/"+th+"/fork", "alice", `{"messageId":"`+msgs[0].(map[string]any)["id"].(string)+`"}`, 201)["id"].(string)

	got := r.json("GET", "/api/threads/"+fork, "alice", "", 200)
	if got["projectId"] != nil || got["createdBy"] != "alice" {
		t.Fatalf("fork = %v", got)
	}
	r.json("GET", "/api/threads/"+fork, "bob", "", 404)
}

func TestTmMessageAuthorsFallBackToTheLoginForUnknownUsers(t *testing.T) {
	r := newTmFaultRig(t)
	th := r.projectThread("alice", "t")
	// Dave is a project member who never logged in through the identity provider.
	r.tm.SetMember(context.Background(), r.project, "dave", store.RoleMember)
	finished(t, r.stream(r.startRun("dave", th, "from dave"), "dave"))
	r.waitIdle(th, "alice")

	msgs := r.json("GET", "/api/threads/"+th, "alice", "", 200)["messages"].([]any)

	for _, m := range msgs {
		if m := m.(map[string]any); m["role"] == "user" {
			a := m["author"].(map[string]any)
			if a["login"] != "dave" || a["name"] != nil {
				t.Fatalf("author = %v", a)
			}
		}
	}
}

func TestTmThreadSummaryAndStatus(t *testing.T) {
	cases := map[string]struct {
		t    store.Thread
		want string
	}{
		"idle":                 {store.Thread{}, "idle"},
		"running":              {store.Thread{ActiveRun: &store.ActiveRun{ID: "r", Status: store.StatusRunning}}, "running"},
		"queued":               {store.Thread{ActiveRun: &store.ActiveRun{ID: "r", Status: store.StatusQueued}}, "queued"},
		"approval wins":        {store.Thread{ActiveRun: &store.ActiveRun{ID: "r", Status: store.StatusRunning}, PendingApproval: &store.Approval{ToolName: "bash"}}, "waiting_approval"},
		"question":             {store.Thread{PendingQuestion: &store.PendingQuestion{Question: "which?"}}, "waiting_input"},
		"approval over asking": {store.Thread{PendingApproval: &store.Approval{}, PendingQuestion: &store.PendingQuestion{}}, "waiting_approval"},
		"failed last run":      {store.Thread{LastRun: &store.LastRun{Status: store.StatusFailed}}, "failed"},
		"interrupted last run": {store.Thread{LastRun: &store.LastRun{Status: store.StatusInterrupted}}, "failed"},
		"completed last run":   {store.Thread{LastRun: &store.LastRun{Status: store.StatusCompleted}}, "idle"},
		"cancelled last run":   {store.Thread{LastRun: &store.LastRun{Status: store.StatusCancelled}}, "idle"},
		"active after failure": {store.Thread{ActiveRun: &store.ActiveRun{Status: store.StatusRunning}, LastRun: &store.LastRun{Status: store.StatusFailed}}, "running"},
	}
	for name, tc := range cases {
		tc := tc
		if got := statusOf(&tc.t); got != tc.want {
			t.Errorf("%s: statusOf = %q, want %q", name, got, tc.want)
		}
	}

	sm := summarize(&store.Thread{ID: "t1", Title: "T", ActiveRun: &store.ActiveRun{ID: "run1", Status: store.StatusRunning},
		PendingApproval: &store.Approval{ToolName: "execute_command"}, PendingQuestion: &store.PendingQuestion{Question: "which?"}},
		store.ThreadMeta{ProjectID: "p", CreatedBy: "bob", Kind: store.ThreadChat})
	if sm.ID != "t1" || sm.RunID != "run1" || sm.Tool != "execute_command" || sm.Question != "which?" || sm.ProjectID != "p" || sm.CreatedBy != "bob" || sm.Status != "waiting_approval" {
		t.Fatalf("summary = %+v", sm)
	}
	if idle := summarize(&store.Thread{ID: "t2"}, store.ThreadMeta{}); idle.RunID != "" || idle.Tool != "" || idle.Question != "" || idle.Status != "idle" {
		t.Fatalf("idle summary = %+v", idle)
	}
}

func TestTmOutcomeOfTerminalEvents(t *testing.T) {
	finish := func(data string) store.Event { return store.Event{Type: agui.RunFinished, Data: json.RawMessage(data)} }
	cases := map[string]struct {
		ev   store.Event
		want string
	}{
		"success":            {finish(`{"outcome":{"type":"success"}}`), "completed"},
		"no outcome":         {finish(`{}`), "completed"},
		"garbage payload":    {finish(`not json`), "completed"},
		"cancelled":          {finish(`{"outcome":{"type":"cancelled"}}`), "cancelled"},
		"approval interrupt": {finish(`{"outcome":{"type":"interrupt","interrupts":[{"reason":"tool_approval"}]}}`), "waiting_approval"},
		"question interrupt": {finish(`{"outcome":{"type":"interrupt","interrupts":[{"reason":"ask_user"}]}}`), "waiting_input"},
		"bare interrupt":     {finish(`{"outcome":{"type":"interrupt"}}`), "waiting_approval"},
		"run error":          {store.Event{Type: agui.RunError, Data: json.RawMessage(`{"code":"x"}`)}, "failed"},
	}
	for name, tc := range cases {
		if got := outcomeOf(tc.ev); got != tc.want {
			t.Errorf("%s: outcomeOf = %q, want %q", name, got, tc.want)
		}
	}
}

func TestTmNoteSurvivesAuditFailureAndStaysOutOfOthersFeeds(t *testing.T) {
	r := newTmFaultRig(t)
	bob, dave := feedOf(r.rig, "bob"), feedOf(r.rig, "dave")
	r.fault.arm("Audit")

	// The action succeeds and is announced even though the audit trail could not be written.
	th := r.projectThread("alice", "audited")

	r.fault.disarm("Audit")
	line := tmWaitLine(t, bob, `"thread.created"`)
	if !strings.Contains(line, th) || !strings.Contains(line, `"title":"audited"`) {
		t.Fatalf("event = %s", line)
	}
	tmQuiet(t, dave, "thread.created")
	// A personal thread's event reaches its owner only, not the project's members.
	r.json("POST", "/api/threads", "alice", `{"title":"private"}`, 201)
	tmQuiet(t, bob, "private")
}

func TestTmFollowAndNoteWithoutTeamAreNoOps(t *testing.T) {
	s := &server{hub: newHub()}
	sub, done := s.hub.subscribe(nil)
	defer done()

	s.note(Caller{Actor: "a"}, "t", "x", nil, FeedEvent{Type: "thread.created"})
	s.follow(Caller{Actor: "a"}, &client.Run{ID: "r", ThreadID: "t"})
	s.AfterRun(Caller{Actor: "a"}, &client.Run{ID: "r", ThreadID: "t"}, "run.send", nil)

	select {
	case ev := <-sub.ch:
		t.Fatalf("a solo server published %+v", ev)
	default:
	}
}

func TestTmAccessEdgeCases(t *testing.T) {
	r := newTmFaultRig(t)
	ctx := context.Background()
	th := r.projectThread("alice", "t")

	// a personal link owned by somebody else is not visible, whatever the store says
	stolen, err := r.h.Store.CreateThread(ctx, "alice", "personal")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.tm.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: stolen.ID, Owner: "bob", CreatedBy: "bob"}); err != nil {
		t.Fatal(err)
	}
	r.json("GET", "/api/threads/"+stolen.ID, "alice", "", 404)

	// a membership lookup failure is an internal error, not a "not found" that would hide an outage
	r.fault.arm("MemberRole")
	resp := r.do("GET", "/api/threads/"+th, "alice", "")
	r.fault.disarm("MemberRole")
	if body := rawBody(resp.Body); resp.StatusCode != 500 || strings.Contains(body, tmSecret) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	// runs of unknown ids are "not found" for everyone
	r.json("GET", "/api/runs/nope", "alice", "", 404)
	r.json("POST", "/api/runs/nope/cancel", "alice", "", 404)

	// a failing link lookup does not break the personal listing: threads are shown as the caller's own
	r.fault.arm("GetThreadMeta")
	resp = r.do("GET", "/api/threads", "alice", "")
	r.fault.disarm("GetThreadMeta")
	if resp.StatusCode != 200 {
		t.Fatalf("personal listing during a team-store failure = %d", resp.StatusCode)
	}
}

func TestTmFeedEventVisibility(t *testing.T) {
	r := newTmFaultRig(t)
	s := tmServer(r.rig, r.tm)
	cases := map[string]struct {
		ev   FeedEvent
		user string
		want bool
	}{
		"member sees project event":     {FeedEvent{ProjectID: r.project}, "bob", true},
		"viewer sees project event":     {FeedEvent{ProjectID: r.project}, "carol", true},
		"outsider does not":             {FeedEvent{ProjectID: r.project}, "dave", false},
		"affects wins over membership":  {FeedEvent{ProjectID: r.project, affects: "dave"}, "bob", false},
		"affects reaches a former user": {FeedEvent{ProjectID: r.project, affects: "dave"}, "dave", true},
		"personal event to its owner":   {FeedEvent{owner: "alice"}, "alice", true},
		"personal event to others":      {FeedEvent{owner: "alice"}, "bob", false},
		"ownerless event is private":    {FeedEvent{}, "bob", false},
	}
	for name, tc := range cases {
		if got := s.allowEvent(tc.user)(tc.ev); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", name, got, tc.want)
		}
	}
}
