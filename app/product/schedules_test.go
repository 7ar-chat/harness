package product

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/schedstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/adapters/store/usagestore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/schedule"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/usageledger"
	"github.com/7ar-chat/harness/modelinfo"
)

// schedRig is a team rig with scheduled tasks and the usage ledger: alice owns the project, bob is a member, vic a
// viewer, eve an outsider.
type schedRig struct {
	*rig
	team    *teamstore.Mem
	sched   *schedstore.Mem
	usage   *usagestore.Mem
	project string
	now     func() time.Time
}

// priceOf is one cent per thousand input tokens and five per thousand output, cache reads at a tenth.
func priceOf(model string) (modelinfo.Info, bool) {
	if model != "gpt-test" {
		return modelinfo.Info{}, false
	}
	return modelinfo.Info{Priced: true, InputPerToken: 1e-5, OutputPerToken: 5e-5, CacheReadPerToken: 1e-6, CacheWritePerToken: 1.25e-5}, true
}

// answer is a turn whose usage is 1000 prompt tokens (600 from the cache, 100 written to it) and 200 completion tokens.
func answer(text string) llmtest.Turn {
	m := llmtest.WithCachedUsage(&ai.Message{Role: ai.RoleAssistant, Content: text}, 1000, 600, 200)
	m.ResponseMeta.Usage.PromptTokenDetails.CacheWriteTokens = 100
	return llmtest.Turn{Chunks: []*ai.Message{m}}
}

func newSchedRig(t *testing.T, model *llmtest.Model, so SchedulesOptions) *schedRig {
	t.Helper()
	tm := teamstore.NewMem()
	for _, l := range []string{"alice", "bob", "vic", "eve"} {
		if _, err := tm.UpsertUser(context.Background(), store.User{Login: l, Name: l, Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	sr := &schedRig{team: tm, sched: schedstore.NewMem(), usage: usagestore.NewMem(), now: time.Now}
	if model == nil {
		model = llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
			if len(c.Tools) == 0 {
				return llmtest.Turn{Chunks: llmtest.Text("A title")}
			}
			return answer("report ready")
		})
	}
	so.Store, so.Price, so.NoRun = sr.sched, priceOf, true
	r := newRig(t, rigOpts{
		team: true, model: model,
		prod: Options{Team: tm, Schedules: &so, Usage: &UsageOptions{Store: sr.usage, Now: func() time.Time { return sr.now() }}},
		mod: func(ho *harness.Options) {
			ho.Hooks = append(ho.Hooks, usageledger.Hook(usageledger.Options{
				Store: sr.usage, Price: priceOf, Resolve: UsageAttribution(tm, sr.sched, nil), Now: func() time.Time { return sr.now() },
			}))
			ho.RunGate = usageledger.Gate(sr.usage, UsageGateResolver(tm), func() time.Time { return sr.now() })
		},
	})
	sr.rig = r
	p := r.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)
	sr.project = p["id"].(string)
	r.json("PUT", "/api/projects/"+sr.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	r.json("PUT", "/api/projects/"+sr.project+"/members/vic", "alice", `{"role":"viewer"}`, 204)
	return sr
}

func (r *schedRig) path(rest string) string {
	return "/api/projects/" + r.project + "/schedules" + rest
}

const nightly = `{"name":"Nightly report","prompt":"Summarise yesterday's merged pull requests.","cron":"0 7 * * *","timezone":"Europe/Paris","timeoutMinutes":20}`

func (r *schedRig) create(user, body string) map[string]any {
	r.t.Helper()
	return r.json("POST", r.path(""), user, body, 201)
}

// waitRun polls the run history until the newest run has left "running".
func (r *schedRig) waitRun(sid, user string) map[string]any {
	r.t.Helper()
	for i := 0; i < 400; i++ {
		resp := r.do("GET", r.path("/"+sid+"/runs"), user, "")
		var runs []map[string]any
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &runs)
		if len(runs) > 0 && runs[0]["status"] != "running" {
			return runs[0]
		}
		time.Sleep(25 * time.Millisecond)
	}
	r.t.Fatal("the run did not finish")
	return nil
}

func TestOnlyMembersWriteSchedulesAndTheServerDecidesWhoTheyRunAs(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})

	r.json("POST", r.path(""), "vic", nightly, 403)
	r.json("POST", r.path(""), "eve", nightly, 404)
	r.json("GET", r.path(""), "eve", "", 404)
	sc := r.create("alice", `{"name":"Nightly","prompt":"p","cron":"0 7 * * *","runAs":"bob","owner":"bob","id":"mine"}`)

	if sc["runAs"] != "alice" || sc["owner"] != "alice" || sc["id"] == "mine" || sc["enabled"] != true || sc["timeoutMinutes"] != 30.0 || sc["canEdit"] != true {
		t.Fatalf("created: %v", sc)
	}
	if next, _ := time.Parse(time.RFC3339, sc["nextRunAt"].(string)); !next.After(time.Now()) {
		t.Fatalf("nextRunAt = %v", sc["nextRunAt"])
	}
	sid := sc["id"].(string)
	if got := r.json("GET", r.path("/"+sid), "vic", "", 200); got["canEdit"] != false {
		t.Fatalf("a viewer can edit: %v", got)
	}
	r.json("PATCH", r.path("/"+sid), "vic", `{"name":"x"}`, 403)
	r.json("DELETE", r.path("/"+sid), "vic", "", 403)
	r.json("POST", r.path("/"+sid+"/run"), "vic", "", 403)

	// Saving it again makes the saver the account it runs as; a client cannot choose one.
	patched := r.json("PATCH", r.path("/"+sid), "bob", `{"prompt":"new prompt","runAs":"alice","enabled":false}`, 200)
	if patched["runAs"] != "bob" || patched["owner"] != "alice" || patched["prompt"] != "new prompt" || patched["enabled"] != false || patched["nextRunAt"] != nil {
		t.Fatalf("patched: %v", patched)
	}
	list := r.do("GET", r.path(""), "vic", "")
	var got []map[string]any
	b, _ := io.ReadAll(list.Body)
	_ = json.Unmarshal(b, &got)
	if len(got) != 1 {
		t.Fatalf("list: %s", b)
	}
	r.json("DELETE", r.path("/"+sid), "bob", "", 204)
	r.json("GET", r.path("/"+sid), "bob", "", 404)
}

func TestInvalidSchedulesAreRefused(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	for name, body := range map[string]string{
		"bad cron":      `{"name":"a","prompt":"p","cron":"whenever"}`,
		"six fields":    `{"name":"a","prompt":"p","cron":"0 0 7 * * *"}`,
		"bad timezone":  `{"name":"a","prompt":"p","cron":"@daily","timezone":"Mars/Base"}`,
		"too frequent":  `{"name":"a","prompt":"p","cron":"@every 5s"}`,
		"no prompt":     `{"name":"a","cron":"@daily"}`,
		"timeout":       `{"name":"a","prompt":"p","cron":"@daily","timeoutMinutes":300}`,
		"policy":        `{"name":"a","prompt":"p","cron":"@daily","policy":"yolo"}`,
		"unknown model": `{"name":"a","prompt":"p","cron":"@daily","model":"gpt-imaginary"}`,
	} {
		if m := r.json("POST", r.path(""), "alice", body, 400); m["error"] == nil {
			t.Errorf("%s: %v", name, m)
		}
	}
}

func TestPreviewListsTheNextFireTimesInTheZone(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})

	got := r.json("GET", "/api/schedules/preview?cron=0+9+*+*+*&timezone=Asia/Tokyo&count=3", "bob", "", 200)

	times := got["times"].([]any)
	if len(times) != 3 {
		t.Fatalf("times: %v", got)
	}
	first, _ := time.Parse(time.RFC3339, times[0].(string))
	if first.UTC().Hour() != 0 { // 09:00 in Tokyo is 00:00 UTC
		t.Fatalf("first = %v", first)
	}
	r.json("GET", "/api/schedules/preview?cron=nope", "bob", "", 400)
}

func TestRunNowIsAnOrdinaryRunInAFreshProjectSessionWithItsUsage(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	sc := r.create("bob", nightly)
	sid := sc["id"].(string)

	run := r.json("POST", r.path("/"+sid+"/run"), "alice", "", 202)
	done := r.waitRun(sid, "alice")

	if run["trigger"] != "manual" || run["startedBy"] != "alice" {
		t.Fatalf("started: %v", run)
	}
	if done["status"] != "succeeded" || done["threadId"] == "" || done["agentRunId"] == "" {
		t.Fatalf("finished: %v", done)
	}
	usage := done["usage"].(map[string]any)
	// 1000 prompt tokens, 600 read from and 100 written to the cache, 200 completion (and a title call besides).
	if usage["inputTokens"].(float64) < 1000 || usage["cacheReadTokens"].(float64) < 600 || usage["cacheWriteTokens"].(float64) < 100 || usage["costUsd"].(float64) <= 0 {
		t.Fatalf("usage: %v", usage)
	}
	// The session is a project thread the members can open, executed as the account that saved the schedule (bob).
	tid := done["threadId"].(string)
	th := r.json("GET", "/api/threads/"+tid, "vic", "", 200)
	if th["projectId"] != r.project || th["createdBy"] != "bob" {
		t.Fatalf("thread: %v", th)
	}
	msgs := th["messages"].([]any)
	first := msgs[0].(map[string]any)
	if au, _ := first["author"].(map[string]any); au["kind"] != store.AuthorSystem || au["onBehalfOf"] != "bob" || !strings.Contains(first["content"].(string), "merged pull requests") {
		t.Fatalf("first message: %v", first)
	}
	// A fresh session each time.
	r.json("POST", r.path("/"+sid+"/run"), "bob", "", 202)
	second := r.waitRunCount(sid, "bob", 2)
	if second[0]["threadId"] == tid {
		t.Fatal("two runs shared a session without reuseSession")
	}
	// The schedule shows its last run.
	got := r.json("GET", r.path("/"+sid), "bob", "", 200)
	if lr, _ := got["lastRun"].(map[string]any); lr == nil || got["lastStatus"] != "succeeded" {
		t.Fatalf("schedule: %v", got)
	}
}

// waitRunCount waits until n runs exist and the newest has finished.
func (r *schedRig) waitRunCount(sid, user string, n int) []map[string]any {
	r.t.Helper()
	for i := 0; i < 400; i++ {
		var runs []map[string]any
		b, _ := io.ReadAll(r.do("GET", r.path("/"+sid+"/runs"), user, "").Body)
		_ = json.Unmarshal(b, &runs)
		if len(runs) >= n && runs[0]["status"] != "running" {
			return runs
		}
		time.Sleep(25 * time.Millisecond)
	}
	r.t.Fatal("the runs did not finish")
	return nil
}

func TestReuseSessionKeepsOneConversation(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	sid := r.create("alice", `{"name":"Standup","prompt":"status?","cron":"@daily","reuseSession":true}`)["id"].(string)

	r.json("POST", r.path("/"+sid+"/run"), "alice", "", 202)
	first := r.waitRunCount(sid, "alice", 1)[0]
	r.json("POST", r.path("/"+sid+"/run"), "alice", "", 202)
	runs := r.waitRunCount(sid, "alice", 2)

	if runs[0]["threadId"] != first["threadId"] || runs[0]["status"] != "succeeded" {
		t.Fatalf("runs: %v", runs)
	}
	th := r.json("GET", "/api/threads/"+first["threadId"].(string), "alice", "", 200)
	if n := len(th["messages"].([]any)); n < 4 {
		t.Fatalf("the conversation has %d messages", n)
	}
}

func TestARunThatLosesItsAccountsWriteAccessIsSkipped(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	sid := r.create("bob", nightly)["id"].(string)
	r.json("DELETE", "/api/projects/"+r.project+"/members/bob", "alice", "", 204)

	r.json("POST", r.path("/"+sid+"/run"), "alice", "", 202)
	run := r.waitRun(sid, "alice")

	if run["status"] != "skipped" || !strings.Contains(run["error"].(string), "bob") {
		t.Fatalf("run: %v", run)
	}
	if run["threadId"] != nil && run["threadId"] != "" {
		t.Fatalf("a skipped run made a session: %v", run)
	}
}

func TestARunPastItsTimeoutIsCancelled(t *testing.T) {
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		return llmtest.Turn{Chunks: llmtest.Text("thinking"), Block: true}
	})
	r := newSchedRig(t, model, SchedulesOptions{Timeout: func(schedule.Schedule) time.Duration { return 300 * time.Millisecond }})
	sid := r.create("alice", nightly)["id"].(string)

	r.json("POST", r.path("/"+sid+"/run"), "alice", "", 202)
	run := r.waitRun(sid, "alice")

	if run["status"] != "timeout" || run["threadId"] == "" {
		t.Fatalf("run: %v", run)
	}
	// The agent run was cancelled too: the conversation is idle again.
	r.waitIdle(run["threadId"].(string), "alice")
}

func TestOneRunAtATimeAnd409OnRunNow(t *testing.T) {
	gate := make(chan struct{})
	var once sync.Once
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		<-gate
		return answer("done")
	})
	r := newSchedRig(t, model, SchedulesOptions{})
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	sid := r.create("alice", nightly)["id"].(string)

	r.json("POST", r.path("/"+sid+"/run"), "alice", "", 202)
	busy := r.json("POST", r.path("/"+sid+"/run"), "bob", "", 409)
	release()
	run := r.waitRun(sid, "alice")

	if busy["error"].(map[string]any)["code"] != "run_in_progress" || run["status"] != "succeeded" {
		t.Fatalf("busy %v, run %v", busy, run)
	}
}

func TestPersonalSchedulesBelongToOnePerson(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	sc := r.json("POST", "/api/me/schedules", "bob", `{"name":"Inbox","prompt":"triage","cron":"0 8 * * 1-5","projectId":"`+r.project+`"}`, 201)
	sid := sc["id"].(string)

	if sc["projectId"] != nil {
		t.Fatalf("a personal schedule landed in a project: %v", sc)
	}
	r.json("GET", "/api/me/schedules/"+sid, "alice", "", 404)
	r.json("PATCH", "/api/me/schedules/"+sid, "alice", `{"name":"x"}`, 404)
	r.json("POST", "/api/me/schedules/"+sid+"/run", "alice", "", 404)
	r.json("GET", r.path("/"+sid), "alice", "", 404) // not reachable through a project either
	r.json("POST", "/api/me/schedules/"+sid+"/run", "bob", "", 202)
	var run map[string]any
	for i := 0; i < 400; i++ {
		var runs []map[string]any
		b, _ := io.ReadAll(r.do("GET", "/api/me/schedules/"+sid+"/runs", "bob", "").Body)
		_ = json.Unmarshal(b, &runs)
		if len(runs) > 0 && runs[0]["status"] != "running" {
			run = runs[0]
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if run == nil || run["status"] != "succeeded" {
		t.Fatalf("run: %v", run)
	}
	// Its conversation is private to bob.
	r.json("GET", "/api/threads/"+run["threadId"].(string), "bob", "", 200)
	r.json("GET", "/api/threads/"+run["threadId"].(string), "alice", "", 404)
	r.json("DELETE", "/api/me/schedules/"+sid, "bob", "", 204)
}

func TestDeletingAProjectDeletesItsSchedules(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	sid := r.create("alice", nightly)["id"].(string)

	r.json("DELETE", "/api/projects/"+r.project, "alice", "", 204)

	if _, err := r.sched.Get(context.Background(), sid); err == nil {
		t.Fatal("the schedule outlived its project")
	}
}
