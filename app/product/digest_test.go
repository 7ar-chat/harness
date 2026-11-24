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
	"github.com/7ar-chat/harness/adapters/store/projdatastore"
	"github.com/7ar-chat/harness/adapters/store/schedstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/adapters/store/usagestore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/usageledger"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

type digestRig struct {
	*rig
	clock   *fakeClock
	srv0    *server
	pd      *projdatastore.Mem
	project string
}

func berlin(t *testing.T, v string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	tm, err := time.ParseInLocation("2006-01-02 15:04", v, loc)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// newDigestRig is a team server with the usage ledger (a session run is activity), the notification engine (private
// addresses allowed: the webhook sink is on loopback) and the digest job switched off: tests call digestTick.
func newDigestRig(t *testing.T, start time.Time) *digestRig {
	t.Helper()
	tm := teamstore.NewMem()
	for _, l := range []string{"alice", "bob", "vic", "eve"} {
		if _, err := tm.UpsertUser(context.Background(), store.User{Login: l, Name: l, Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	d := &digestRig{clock: &fakeClock{t: start}, pd: projdatastore.NewMem()}
	us, ss := usagestore.NewMem(), schedstore.NewMem()
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		return answer("done")
	})
	prod := Options{
		Team: tm, Schedules: &SchedulesOptions{Store: ss, Price: priceOf, NoRun: true},
		Usage:     &UsageOptions{Store: us, Now: d.clock.now},
		SecretKey: cnKey, PublicURL: "https://harness.test",
		Notifications: &NotificationsOptions{AllowPrivate: true, Sleep: func(time.Duration) {}},
		ProjectData:   &ProjectDataOptions{Store: d.pd, NoJob: true, onInstall: func(s *server) { d.srv0 = s }},
	}
	d.rig = newRig(t, rigOpts{
		team: true, model: model, prod: prod,
		mod: func(ho *harness.Options) {
			ho.Hooks = append(ho.Hooks, usageledger.Hook(usageledger.Options{
				Store: us, Price: priceOf, Resolve: UsageAttribution(tm, ss, nil), Now: d.clock.now,
			}))
		},
	})
	d.project = d.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	d.json("PUT", "/api/projects/"+d.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	d.json("PUT", "/api/projects/"+d.project+"/members/vic", "alice", `{"role":"viewer"}`, 204)
	if d.srv0 == nil {
		t.Fatal("the digest was not installed")
	}
	return d
}

func (d *digestRig) path(rest string) string { return "/api/projects/" + d.project + "/digest" + rest }

func (d *digestRig) activity() {
	d.t.Helper()
	th := d.json("POST", "/api/threads", "alice", `{"projectId":"`+d.project+`"}`, 201)["id"].(string)
	run := d.startRun("alice", th, "hello")
	finished(d.t, d.stream(run, "alice"))
	d.waitIdle(th, "alice")
}

func (d *digestRig) enable(weekday, hour int, zone string) {
	d.t.Helper()
	body, _ := json.Marshal(map[string]any{"enabled": true, "weekday": weekday, "hour": hour, "timezone": zone})
	d.json("PUT", d.path(""), "alice", string(body), 200)
}

// tick runs the job as n replicas at once.
func (d *digestRig) tick(n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.srv0.digestTick(context.Background(), d.clock.now())
		}()
	}
	wg.Wait()
}

func (d *digestRig) history() []any {
	d.t.Helper()
	return d.json("GET", d.path(""), "alice", "", 200)["digests"].([]any)
}

func (d *digestRig) routeToWebhook(sink *hookSink) string {
	d.t.Helper()
	body, _ := json.Marshal(map[string]any{"kind": "webhook", "name": "Ops hook", "url": sink.srv.URL + "/hook"})
	id := d.json("POST", "/api/projects/"+d.project+"/notification-channels", "alice", string(body), 201)["id"].(string)
	rules, _ := json.Marshal(map[string]any{"events": map[string]any{"digest.ready": map[string]any{"channels": []string{id}, "person": true}}, "dedupeMinutes": 10})
	d.json("PUT", "/api/projects/"+d.project+"/notifications", "alice", string(rules), 200)
	return id
}

func TestWeeklyDigestIsSentOnceAtTheOwnersHourInTheirZone(t *testing.T) {
	d := newDigestRig(t, berlin(t, "2026-11-22 20:00")) // Sunday evening
	sink := newHookSink(t)
	d.routeToWebhook(sink)

	// Only the owner changes the setting; a viewer reads it; an outsider sees nothing.
	d.json("PUT", d.path(""), "bob", `{"enabled":true,"weekday":1,"hour":9,"timezone":"Europe/Berlin"}`, 403)
	d.json("PUT", d.path(""), "vic", `{"enabled":true}`, 403)
	d.json("GET", d.path(""), "eve", "", 404)
	d.json("PUT", d.path(""), "alice", `{"enabled":true,"weekday":1,"hour":24,"timezone":"Europe/Berlin"}`, 400)
	d.json("PUT", d.path(""), "alice", `{"enabled":true,"weekday":8,"hour":9}`, 400)
	d.json("PUT", d.path(""), "alice", `{"enabled":true,"weekday":1,"hour":9,"timezone":"Mars/Base"}`, 400)
	d.enable(1, 9, "Europe/Berlin")
	v := d.json("GET", d.path(""), "vic", "", 200)
	set := v["setting"].(map[string]any)
	if set["enabled"] != true || set["hour"].(float64) != 9 || set["timezone"] != "Europe/Berlin" || set["nextAt"] == nil {
		t.Fatalf("setting = %v", set)
	}

	d.clock.set(berlin(t, "2026-11-22 21:00"))
	d.activity()

	d.clock.set(berlin(t, "2026-11-23 08:59"))
	d.tick(1)
	if len(d.history()) != 0 || sink.count() != 0 {
		t.Fatal("a digest went out before the hour")
	}

	d.clock.set(berlin(t, "2026-11-23 09:01"))
	d.tick(6) // six replicas, one slot
	d.clock.set(berlin(t, "2026-11-23 09:30"))
	d.tick(2)
	h := d.history()
	if len(h) != 1 {
		t.Fatalf("history has %d digests", len(h))
	}
	calls := sink.wait(t, 1)
	time.Sleep(150 * time.Millisecond)
	if sink.count() != 1 {
		t.Fatalf("digest.ready reached the channel %d times", sink.count())
	}
	var ev map[string]any
	_ = json.Unmarshal(calls[0].body, &ev)
	if !strings.Contains(string(calls[0].body), "Weekly digest: api") {
		t.Fatalf("the channel got %s", calls[0].body)
	}
	item := h[0].(map[string]any)
	if item["trigger"] != "schedule" || !strings.HasSuffix(item["url"].(string), "/report") {
		t.Fatalf("item = %v", item)
	}
	if got, _ := time.Parse(time.RFC3339, item["to"].(string)); !got.Equal(berlin(t, "2026-11-23 09:00")) {
		t.Fatalf("the digest ends at %v", got)
	}
	if got, _ := time.Parse(time.RFC3339, item["from"].(string)); !got.Equal(berlin(t, "2026-11-16 09:00")) {
		t.Fatalf("the digest starts at %v", got)
	}

	// The report is stored: the link works for the project's people, not for others, with a locked-down page.
	url := item["url"].(string)
	resp := d.do("GET", url, "vic", "")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") ||
		!strings.Contains(string(body), "<html") || !strings.Contains(string(body), "api") {
		t.Fatalf("report: %d %s", resp.StatusCode, resp.Header)
	}
	if r := d.do("GET", url+"?download=1", "alice", ""); !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal("no download header")
	}
	d.json("GET", url, "eve", "", 404)
	d.json("GET", d.path("/nope/report"), "alice", "", 404)

	// The delivery log shows the webhook delivery.
	log := d.do("GET", "/api/projects/"+d.project+"/notifications/log", "alice", "")
	lb, _ := io.ReadAll(log.Body)
	if !strings.Contains(string(lb), "digest.ready") || !strings.Contains(string(lb), "delivered") {
		t.Fatalf("log: %s", lb)
	}

	// The next Monday nothing happened in its week: no digest, nothing sent.
	d.clock.set(berlin(t, "2026-11-30 09:01"))
	d.tick(3)
	if len(d.history()) != 1 || sink.count() != 1 {
		t.Fatalf("an empty week sent a digest: history %d, calls %d", len(d.history()), sink.count())
	}
	if s, _ := d.pd.GetDigestSettings(context.Background(), d.project); s.LastSlot != "2026-11-30" {
		t.Fatalf("the empty week was not recorded as handled: %+v", s)
	}
}

func TestDigestAcrossTheAutumnClockChange(t *testing.T) {
	// Berlin leaves summer time on Sunday 2026-10-25: Monday's week is 169 hours long and still ends at 09:00 local.
	d := newDigestRig(t, berlin(t, "2026-10-24 12:00"))
	d.enable(1, 9, "Europe/Berlin")
	d.clock.set(berlin(t, "2026-10-24 13:00"))
	d.activity()
	d.clock.set(berlin(t, "2026-10-26 09:02"))
	d.tick(2)
	h := d.history()
	if len(h) != 1 {
		t.Fatalf("history has %d digests", len(h))
	}
	item := h[0].(map[string]any)
	from, _ := time.Parse(time.RFC3339, item["from"].(string))
	to, _ := time.Parse(time.RFC3339, item["to"].(string))
	if !to.Equal(berlin(t, "2026-10-26 09:00")) || !from.Equal(berlin(t, "2026-10-19 09:00")) || to.Sub(from) != 169*time.Hour {
		t.Fatalf("range %v to %v", from, to)
	}
}

func TestSwitchingTheDigestOnDoesNotSendThisWeeksPastSlot(t *testing.T) {
	d := newDigestRig(t, berlin(t, "2026-11-23 10:00")) // Monday, after 09:00
	d.activity()
	d.enable(1, 9, "Europe/Berlin")
	d.clock.set(berlin(t, "2026-11-23 10:05"))
	d.tick(1)
	if len(d.history()) != 0 {
		t.Fatal("the digest of a slot before it was switched on was sent")
	}
	// Switched off, it never goes out either.
	d.json("PUT", d.path(""), "alice", `{"enabled":false,"weekday":1,"hour":9,"timezone":"Europe/Berlin"}`, 200)
	d.clock.set(berlin(t, "2026-11-30 09:05"))
	d.tick(1)
	if len(d.history()) != 0 {
		t.Fatal("a digest went out while off")
	}
}

func TestADigestThatFailsIsRetriedAndNotLost(t *testing.T) {
	d := newDigestRig(t, berlin(t, "2026-11-22 20:00"))
	d.enable(1, 9, "Europe/Berlin")
	d.clock.set(berlin(t, "2026-11-22 21:00"))
	d.activity()
	d.clock.set(berlin(t, "2026-11-23 09:01"))
	// Someone else already holds the claim (another replica): nothing is generated here.
	if ok, _ := d.pd.ClaimSlot(context.Background(), d.project, "2026-11-23", d.clock.now()); !ok {
		t.Fatal("claim")
	}
	d.tick(1)
	if len(d.history()) != 0 {
		t.Fatal("a replica generated a digest whose slot another one holds")
	}
	// The holder failed and gave it back: the next tick, still within the grace, sends it.
	_ = d.pd.ReleaseSlot(context.Background(), d.project, "2026-11-23")
	d.clock.set(berlin(t, "2026-11-23 09:03"))
	d.tick(1)
	if len(d.history()) != 1 {
		t.Fatal("the released slot was not sent")
	}
	// Too late (past the grace) a slot is not sent any more.
	d2 := newDigestRig(t, berlin(t, "2026-11-22 20:00"))
	d2.enable(1, 9, "Europe/Berlin")
	d2.clock.set(berlin(t, "2026-11-22 21:00"))
	d2.activity()
	d2.clock.set(berlin(t, "2026-11-23 22:00"))
	d2.tick(1)
	if len(d2.history()) != 0 {
		t.Fatal("a stale digest was sent")
	}
}

func TestSendOneNowIsOwnerOnlyRateLimitedAndWorksForAnEmptyWeek(t *testing.T) {
	d := newDigestRig(t, berlin(t, "2026-11-25 10:00"))
	sink := newHookSink(t)
	d.routeToWebhook(sink)
	d.json("POST", d.path("/send"), "bob", "", 403)
	d.json("POST", d.path("/send"), "vic", "", 403)
	d.json("POST", d.path("/send"), "eve", "", 404)
	got := d.json("POST", d.path("/send"), "alice", "", 200)
	item := got["digest"].(map[string]any)
	if item["trigger"] != "manual" || item["id"] == "" {
		t.Fatalf("digest = %v", item)
	}
	d.json("POST", d.path("/send"), "alice", "", 429) // the clock did not move
	sink.wait(t, 1)
	d.clock.set(berlin(t, "2026-11-25 10:01"))
	d.json("POST", d.path("/send"), "alice", "", 200)
	if h := d.history(); len(h) != 2 {
		t.Fatalf("history = %d", len(h))
	}
	if r := d.do("GET", item["url"].(string), "bob", ""); r.StatusCode != 200 {
		t.Fatalf("report: %d", r.StatusCode)
	}
}

func TestDeletingTheProjectDropsItsDigests(t *testing.T) {
	d := newDigestRig(t, berlin(t, "2026-11-25 10:00"))
	d.json("POST", d.path("/send"), "alice", "", 200)
	d.json("DELETE", "/api/projects/"+d.project, "alice", "", 204)
	if l, _ := d.pd.ListDigests(context.Background(), d.project, 0); len(l) != 0 {
		t.Fatal("digests of a deleted project remain")
	}
}
