package product

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/usage"
)

// sessionRun runs one message in a new project thread as user and waits for the run to finish.
func (r *schedRig) sessionRun(user string) string {
	r.t.Helper()
	th := r.json("POST", "/api/threads", user, `{"projectId":"`+r.project+`"}`, 201)["id"].(string)
	run := r.startRun(user, th, "hello")
	finished(r.t, r.stream(run, user))
	r.waitIdle(th, user)
	return th
}

func (r *schedRig) usageURL(q string) string { return "/api/usage?" + q }

// perRun is the cost of the rig's answer turn alone: 300 fresh and 600 cached prompt tokens ... see priceOf.
// 1000 prompt = 300 fresh + 600 read + 100 written; 200 completion.
const answerCost = 300*1e-5 + 600*1e-6 + 100*1.25e-5 + 200*5e-5

func TestFinishedRunsAreBookedWithTheirCacheSplitAndKind(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	r.sessionRun("alice")
	r.sessionRun("bob")
	sid := r.create("bob", nightly)["id"].(string)
	r.json("POST", r.path("/"+sid+"/run"), "bob", "", 202)
	r.waitRun(sid, "bob")

	got := r.json("GET", r.usageURL("scope=project&project="+r.project+"&range=month&group=model"), "vic", "", 200)

	total := got["total"].(map[string]any)
	if total["runs"] != 3.0 || total["cacheReadTokens"].(float64) < 1800 || total["cacheWriteTokens"].(float64) < 300 || total["unpriced"] != 0.0 {
		t.Fatalf("total: %v", total)
	}
	// Three answers, each priced, plus the titles of the two chat sessions (a title call has no cache): never zero, never less.
	if cost := total["costUsd"].(float64); cost < 3*answerCost-1e-9 {
		t.Fatalf("cost %v < %v", cost, 3*answerCost)
	}
	by := got["bySource"].(map[string]any)
	if by["session"].(map[string]any)["runs"] != 2.0 || by["schedule"].(map[string]any)["runs"] != 1.0 || by["task"] != nil {
		t.Fatalf("by source: %v", by)
	}
	rows := got["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["key"] != "gpt-test" {
		t.Fatalf("rows: %v", rows)
	}
	// The same ledger by person: bob has two of the three runs.
	byUser := r.json("GET", r.usageURL("scope=project&project="+r.project+"&group=user"), "alice", "", 200)["rows"].([]any)
	if len(byUser) != 2 || byUser[0].(map[string]any)["key"] != "bob" || byUser[0].(map[string]any)["runs"] != 2.0 {
		t.Fatalf("by user: %v", byUser)
	}
	// By thread: the rows carry the conversations' titles.
	byThread := r.json("GET", r.usageURL("scope=project&project="+r.project+"&group=thread"), "alice", "", 200)
	if len(byThread["rows"].([]any)) != 3 || len(byThread["titles"].(map[string]any)) != 3 {
		t.Fatalf("by thread: %v", byThread)
	}
	// My own usage is only mine.
	mine := r.json("GET", r.usageURL("scope=me&group=day"), "alice", "", 200)
	if mine["total"].(map[string]any)["runs"] != 1.0 || len(mine["rows"].([]any)) != 1 {
		t.Fatalf("me: %v", mine)
	}
}

func TestUsageScopesAndParametersAreChecked(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})

	r.json("GET", r.usageURL("scope=project&project="+r.project), "eve", "", 404)
	r.json("GET", r.usageURL("scope=project&project=nope"), "alice", "", 404)
	r.json("GET", r.usageURL("scope=team"), "alice", "", 400)
	r.json("GET", r.usageURL("range=fortnight"), "alice", "", 400)
	r.json("GET", r.usageURL("group=colour"), "alice", "", 400)
	r.json("GET", r.usageURL("range=2026-11-09..2026-11-01"), "alice", "", 400)
	got := r.json("GET", r.usageURL("range=2026-11-01..2026-11-07&group=day"), "alice", "", 200)
	if got["from"] != "2026-11-01T00:00:00Z" || got["to"] != "2026-11-08T00:00:00Z" {
		t.Fatalf("the last day of a date range is included: %v..%v", got["from"], got["to"])
	}
	for _, rg := range []string{"day", "week", "month"} {
		r.json("GET", r.usageURL("range="+rg), "alice", "", 200)
	}
}

func TestOnlyAnOwnerSetsAProjectCapAndEveryoneTheirOwn(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	capPath := "/api/projects/" + r.project + "/usage/cap"

	r.json("PUT", capPath, "bob", `{"monthlyUsd":50}`, 403)
	r.json("PUT", capPath, "eve", `{"monthlyUsd":50}`, 404)
	r.json("PUT", capPath, "alice", `{"monthlyUsd":-1}`, 400)
	set := r.json("PUT", capPath, "alice", `{"monthlyUsd":50}`, 200)
	mine := r.json("PUT", "/api/me/usage/cap", "bob", `{"monthlyUsd":5}`, 200)

	if set["monthlyUsd"] != 50.0 || mine["monthlyUsd"] != 5.0 {
		t.Fatalf("caps: %v %v", set, mine)
	}
	view := func(user string) map[string]any {
		return r.json("GET", r.usageURL("scope=project&project="+r.project), user, "", 200)
	}
	if v := view("alice"); v["canSetCap"] != true || v["cap"].(map[string]any)["monthlyUsd"] != 50.0 {
		t.Fatalf("owner view: %v", v)
	}
	if v := view("bob"); v["canSetCap"] != false || v["cap"].(map[string]any)["monthlyUsd"] != 50.0 {
		t.Fatalf("member view: %v", v)
	}
	if v := r.json("GET", r.usageURL("scope=me"), "bob", "", 200); v["canSetCap"] != true || v["cap"].(map[string]any)["monthlyUsd"] != 5.0 {
		t.Fatalf("my view: %v", v)
	}
	r.json("PUT", capPath, "alice", `{"monthlyUsd":0}`, 200)
	if v := view("alice"); v["cap"].(map[string]any)["monthlyUsd"] != 0.0 {
		t.Fatalf("cap not removed: %v", v)
	}
}

func TestACapRefusesNewRunsWarnsAt80PercentAndResetsNextMonth(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	ctx := context.Background()
	// Each session costs about two answers' worth (the answer and nothing else: the title is free of cache and cheap).
	spent := func() float64 {
		v, _ := r.usage.Spent(ctx, r.project, "", usage.MonthStart(r.now()))
		return v
	}
	r.sessionRun("alice")
	one := spent()
	if one < answerCost {
		t.Fatalf("one run cost %v", one)
	}
	// A cap a little above two runs: after the first the project is well under it.
	capUSD := one*2 + one/10
	r.json("PUT", "/api/projects/"+r.project+"/usage/cap", "alice", `{"monthlyUsd":`+jsonFloat(capUSD)+`}`, 200)
	view := func() map[string]any {
		return r.json("GET", r.usageURL("scope=project&project="+r.project), "alice", "", 200)["cap"].(map[string]any)
	}
	if c := view(); c["warn"] != false || c["reached"] != false {
		t.Fatalf("under 80%%: %v", c)
	}

	r.sessionRun("bob") // 2 runs: 2*one/capUSD = 95%
	if c := view(); c["warn"] != true || c["reached"] != false || c["ratio"].(float64) < 0.8 {
		t.Fatalf("warning at 80%%: %v", c)
	}
	r.sessionRun("alice") // one more is still allowed (the check is made when a run starts) and uses the cap up
	if c := view(); c["reached"] != true || c["warn"] != true {
		t.Fatalf("cap reached: %v", c)
	}

	// Now every start is refused with 402 and a message that says what to do, for the project's members.
	th := r.json("POST", "/api/threads", "bob", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)
	refused := r.json("POST", "/api/threads/"+th+"/runs", "bob", `{"message":"one more"}`, 402)
	e := refused["error"].(map[string]any)
	if e["code"] != "spend_cap_reached" || e["message"] == "" {
		t.Fatalf("refusal: %v", refused)
	}
	// A scheduled run is refused the same way, and recorded as failed with the reason.
	sid := r.create("alice", nightly)["id"].(string)
	r.json("POST", r.path("/"+sid+"/run"), "alice", "", 202)
	run := r.waitRun(sid, "alice")
	if run["status"] != "failed" || run["error"] == "" {
		t.Fatalf("scheduled run over the cap: %v", run)
	}
	// A personal conversation is not under the project's cap.
	own := r.json("POST", "/api/threads", "bob", `{}`, 201)["id"].(string)
	r.startRun("bob", own, "personal")
	// Raising the cap lets the project work again; so does a new month.
	r.json("PUT", "/api/projects/"+r.project+"/usage/cap", "alice", `{"monthlyUsd":1000}`, 200)
	r.startRun("bob", th, "now it works")
	r.json("PUT", "/api/projects/"+r.project+"/usage/cap", "alice", `{"monthlyUsd":0.0001}`, 200)
	r.waitIdle(th, "bob")
	r.json("POST", "/api/threads/"+th+"/runs", "bob", `{"message":"again"}`, 402)
	prev := r.now
	r.now = func() time.Time { return prev().AddDate(0, 1, 0) }
	r.startRun("bob", th, "next month")
}

func TestAUsersOwnCapStopsTheirRunsInEveryProject(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	r.sessionRun("bob")
	r.json("PUT", "/api/me/usage/cap", "bob", `{"monthlyUsd":0.0001}`, 200)

	th := r.json("POST", "/api/threads", "bob", `{}`, 201)["id"].(string)
	refused := r.json("POST", "/api/threads/"+th+"/runs", "bob", `{"message":"personal"}`, 402)

	if refused["error"].(map[string]any)["code"] != "spend_cap_reached" {
		t.Fatalf("refusal: %v", refused)
	}
	// Someone else is not affected by bob's cap.
	r.startRun("alice", r.json("POST", "/api/threads", "alice", `{}`, 201)["id"].(string), "fine")
}

func jsonFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
