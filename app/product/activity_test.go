package product

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/store"
)

func TestActivitySummaryCountsOnlyTheCallersOwnWork(t *testing.T) {
	tm := teamstore.NewMem()
	ctx := context.Background()
	for _, l := range []string{"alice", "bob", "eve"} {
		if _, err := tm.UpsertUser(ctx, store.User{Login: l, Name: l, Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	r := newRig(t, rigOpts{team: true, prod: Options{Team: tm}})
	proj := r.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	r.json("PUT", "/api/projects/"+proj+"/members/bob", "alice", `{"role":"member"}`, 204)
	r.json("POST", "/api/projects/"+proj+"/invites", "alice", `{"email":"new@x.io","role":"member"}`, 201)

	r.json("POST", "/api/threads", "alice", `{"projectId":"`+proj+`"}`, 201)
	r.json("POST", "/api/threads", "alice", `{}`, 201) // a personal session counts too
	r.json("POST", "/api/threads", "bob", `{"projectId":"`+proj+`"}`, 201)

	audit := func(project, actor, action string, detail map[string]any) {
		t.Helper()
		if err := tm.Audit(ctx, store.AuditEntry{ProjectID: project, Actor: actor, Action: action, Detail: detail}); err != nil {
			t.Fatal(err)
		}
	}
	audit(proj, "alice", "run.resume", map[string]any{"decision": "approve"})
	audit(proj, "alice", "run.resume", map[string]any{"decision": "deny"})
	audit(proj, "alice", "run.resume", map[string]any{"decision": "respond"}) // an answer to a question is not an approval
	audit(proj, "alice", "task.resume", map[string]any{"task": "t1"})
	audit(proj, "alice", "workflow.gate.approve", nil)
	audit(proj, "alice", "task.create", nil)
	audit("", "alice", "run.resume", map[string]any{"decision": "approve"}) // a personal session
	audit(proj, "bob", "run.resume", map[string]any{"decision": "approve"})
	audit(proj, "bob", "task.create", nil)

	get := func(user string) ActivitySummary {
		t.Helper()
		var v ActivitySummary
		resp := r.do("GET", "/api/me/activity-summary", user, "")
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", user, resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	a := get("alice")
	if a.ApprovalsDecided != 5 || a.TasksHandedOff != 1 || a.SessionsStarted != 2 || a.MembersInvited != 1 {
		t.Fatalf("alice = %+v", a)
	}
	b := get("bob")
	if b.ApprovalsDecided != 1 || b.TasksHandedOff != 1 || b.SessionsStarted != 1 || b.MembersInvited != 0 {
		t.Fatalf("bob = %+v", b)
	}
	// Someone with no history, and not a member of the project, sees zeros: never another person's counts.
	if e := get("eve"); e != (ActivitySummary{}) {
		t.Fatalf("eve = %+v", e)
	}
	if got := r.do("GET", "/api/me/activity-summary", "", "").StatusCode; got != 401 && got != 403 {
		t.Fatalf("anonymous = %d", got)
	}
}
