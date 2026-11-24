package product

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/notify"
	"github.com/7ar-chat/harness/core/store"
)

// hookSink is a webhook receiver that remembers what it was sent.
type hookSink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	got  []hookCall
	code int
}

type hookCall struct {
	body []byte
	hdr  http.Header
}

func newHookSink(t *testing.T) *hookSink {
	t.Helper()
	h := &hookSink{code: 200}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.got = append(h.got, hookCall{b, r.Header.Clone()})
		code := h.code
		h.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookSink) wait(t *testing.T, n int) []hookCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		if len(h.got) >= n {
			out := append([]hookCall(nil), h.got...)
			h.mu.Unlock()
			return out
		}
		h.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("webhook received %d calls, want %d", len(h.got), n)
	return nil
}

func (h *hookSink) count() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.got) }

func nfEnv(t *testing.T, o fxOpts, allowPrivate bool) *fxEnv {
	t.Helper()
	o.prod = func(p *Options) {
		p.SecretKey, p.PublicURL = cnKey, "https://harness.test"
		p.Notifications = &NotificationsOptions{AllowPrivate: allowPrivate, Sleep: func(time.Duration) {}}
	}
	return newFxEnv(t, o)
}

func (e *fxEnv) nfPath(rest string) string {
	return "/api/projects/" + e.project + "/notification-channels" + rest
}

func (e *fxEnv) addWebhook(user, url, secret string) map[string]any {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"kind": "webhook", "name": "Ops hook", "url": url, "secret": secret})
	return e.json("POST", e.nfPath(""), user, string(body), 201)
}

func TestNotificationChannelsAreOwnerOnlyAndNeverReturnTheirAddressOrSecret(t *testing.T) {
	sink := newHookSink(t)
	e := nfEnv(t, fxOpts{}, true)
	const secret = "topsecret-signing-value"
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	ch := e.addWebhook("alice", sink.srv.URL+"/hook?token=abc123", secret)
	id := ch["id"].(string)
	if ch["hint"] != strings.TrimPrefix(sink.srv.URL, "http://") || ch["hasSecret"] != true {
		t.Fatalf("channel = %v", ch)
	}
	e.json("PUT", e.nfPath("/"+id), "alice", `{"name":"Renamed","secret":"`+secret+`2"}`, 200)

	// Every way to read it back, as the owner and as a member.
	var bodies []string
	for _, c := range []struct{ path, user string }{
		{e.nfPath(""), "alice"}, {"/api/projects/" + e.project + "/notifications", "alice"},
		{"/api/projects/" + e.project + "/notifications", "bob"}, {"/api/projects/" + e.project + "/notifications/log", "alice"},
		{"/api/me/notifications", "alice"},
	} {
		resp := e.do("GET", c.path, c.user, "")
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s as %s: %d %s", c.path, c.user, resp.StatusCode, b)
		}
		bodies = append(bodies, string(b))
	}
	for _, b := range bodies {
		for _, leak := range []string{"abc123", secret, "sealed", "/hook"} {
			if strings.Contains(b, leak) {
				t.Fatalf("a response leaks %q: %s", leak, b)
			}
		}
	}
	if strings.Contains(bodies[2], "hint") {
		t.Fatalf("a member sees the host of the address: %s", bodies[2])
	}
	e.json("POST", e.nfPath("/"+id+"/test"), "alice", "", 200)
	if strings.Contains(logs.String(), "abc123") || strings.Contains(logs.String(), secret) {
		t.Fatalf("the logs leak the address or the secret: %s", logs.String())
	}

	// Access: members read the rules, only owners change anything; outsiders see nothing.
	e.json("GET", "/api/projects/"+e.project+"/notifications", "carol", "", 200)
	e.json("GET", "/api/projects/"+e.project+"/notifications", "dave", "", 404)
	for _, c := range []struct{ method, path, user, body string }{
		{"GET", e.nfPath(""), "bob", ""},
		{"POST", e.nfPath(""), "bob", `{"kind":"webhook","name":"x","url":"` + sink.srv.URL + `"}`},
		{"PUT", e.nfPath("/" + id), "bob", `{"enabled":false}`},
		{"DELETE", e.nfPath("/" + id), "bob", ""},
		{"POST", e.nfPath("/" + id + "/test"), "bob", ""},
		{"PUT", "/api/projects/" + e.project + "/notifications", "bob", `{"events":{},"dedupeMinutes":5}`},
		{"GET", "/api/projects/" + e.project + "/notifications/log", "bob", ""},
		{"GET", e.nfPath(""), "dave", ""},
		{"POST", e.nfPath("/" + id + "/test"), "dave", ""},
	} {
		if st := e.do(c.method, c.path, c.user, c.body).StatusCode; st != 403 && st != 404 {
			t.Errorf("%s %s as %s: status %d, want 403 or 404", c.method, c.path, c.user, st)
		}
	}
	if st := e.do("GET", "/api/projects/"+e.project+"/notifications", "", "").StatusCode; st != 401 {
		t.Errorf("anonymous: %d", st)
	}
}

func TestTheTestButtonSendsASignedMarkedMessageAndIsLoggedAndAudited(t *testing.T) {
	sink := newHookSink(t)
	e := nfEnv(t, fxOpts{}, true)
	id := e.addWebhook("alice", sink.srv.URL, "shh")["id"].(string)

	res := e.json("POST", e.nfPath("/"+id+"/test"), "alice", "", 200)
	if res["status"] != "delivered" {
		t.Fatalf("result = %v", res)
	}
	call := sink.wait(t, 1)[0]
	mac := hmac.New(sha256.New, []byte("shh"))
	mac.Write(call.body)
	if call.hdr.Get("X-7ar-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("bad signature %q", call.hdr.Get("X-7ar-Signature"))
	}
	var p map[string]any
	_ = json.Unmarshal(call.body, &p)
	if p["event"] != "test" || !strings.Contains(p["title"].(string), "Test") || p["project"] != "api" {
		t.Fatalf("payload = %v", p)
	}
	log := e.listAny("/api/projects/"+e.project+"/notifications/log", "alice")
	if len(log) != 1 || log[0]["status"] != "delivered" || log[0]["event"] != "test" {
		t.Fatalf("log = %v", log)
	}
	if audit := e.rawGet("/api/projects/"+e.project+"/audit", "alice"); !strings.Contains(audit, "notifications.channel.test") || !strings.Contains(audit, "notifications.channel.add") {
		t.Fatalf("audit lacks the channel actions: %s", audit)
	}

	// A receiver that fails: the test says so, with a safe error, and the log highlights it.
	sink.mu.Lock()
	sink.code = 500
	sink.mu.Unlock()
	time.Sleep(1100 * time.Millisecond) // the rate limit of the test button
	res = e.json("POST", e.nfPath("/"+id+"/test"), "alice", "", 200)
	if res["status"] != "failed" || !strings.Contains(res["error"].(string), "500") || strings.Contains(res["error"].(string), sink.srv.URL) {
		t.Fatalf("result = %v", res)
	}
	log = e.listAny("/api/projects/"+e.project+"/notifications/log", "alice")
	if len(log) != 2 || log[0]["status"] != "failed" {
		t.Fatalf("log = %v", log)
	}
}

func (e *fxEnv) rawGet(path, user string) string {
	e.t.Helper()
	resp := e.do("GET", path, user, "")
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func (e *fxEnv) listAny(path, user string) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	if err := json.Unmarshal([]byte(e.rawGet(path, user)), &out); err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
	return out
}

func TestABlockedTaskReachesTheChannelsTheRulesNameAndNotTheOthers(t *testing.T) {
	sink, other := newHookSink(t), newHookSink(t)
	e := nfEnv(t, fxOpts{}, true)
	id := e.addWebhook("alice", sink.srv.URL, "")["id"].(string)
	body, _ := json.Marshal(map[string]any{"kind": "webhook", "name": "Unused", "url": other.srv.URL})
	unused := e.json("POST", e.nfPath(""), "alice", string(body), 201)["id"].(string)
	_ = unused
	e.json("PUT", "/api/projects/"+e.project+"/notifications", "alice",
		`{"events":{"task.blocked":{"channels":["`+id+`"],"person":true}},"dedupeMinutes":10,"digest":false}`, 200)

	task := e.newTask("Ship it")
	e.block(task, "needs a decision")

	call := sink.wait(t, 1)[0]
	var p map[string]any
	_ = json.Unmarshal(call.body, &p)
	if p["event"] != "task.blocked" || !strings.Contains(p["title"].(string), "Ship it") || p["project"] != "api" {
		t.Fatalf("payload = %v", p)
	}
	if other.count() != 0 {
		t.Fatal("a channel the rule does not name was told")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		log := e.listAny("/api/projects/"+e.project+"/notifications/log", "alice")
		if len(log) > 0 && log[0]["event"] == "task.blocked" && log[0]["status"] == "delivered" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no delivery log entry: %v", log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Blocking the same task for the same reason again within the window collapses.
	e.block(task, "needs a decision")
	time.Sleep(200 * time.Millisecond)
	if sink.count() != 1 {
		t.Fatalf("the repeat was sent: %d calls", sink.count())
	}
}

func TestRulesAreValidatedAndAChannelDeletedIsRemovedFromThem(t *testing.T) {
	sink := newHookSink(t)
	e := nfEnv(t, fxOpts{}, true)
	id := e.addWebhook("alice", sink.srv.URL, "")["id"].(string)
	url := "/api/projects/" + e.project + "/notifications"
	e.json("PUT", url, "alice", `{"events":{"task.blocked":{"channels":["nope"],"person":true}}}`, 400)
	e.json("PUT", url, "alice", `{"events":{"made.up":{"channels":[],"person":true}}}`, 400)
	e.json("PUT", url, "alice", `{"events":{},"dedupeMinutes":-3}`, 400)
	e.json("PUT", url, "alice", `{"events":{"task.failed":{"channels":["`+id+`"],"person":false}},"dedupeMinutes":0,"digest":true}`, 200)
	got := e.json("GET", url, "carol", "", 200)
	rules := got["rules"].(map[string]any)
	if rules["digest"] != true || rules["dedupeMinutes"] != 0.0 || got["canEdit"] != false {
		t.Fatalf("view = %v", got)
	}
	e.json("DELETE", e.nfPath("/"+id), "alice", "", 204)
	rules = e.json("GET", url, "alice", "", 200)["rules"].(map[string]any)
	if ch := rules["events"].(map[string]any)["task.failed"].(map[string]any)["channels"].([]any); len(ch) != 0 {
		t.Fatalf("rule still names the deleted channel: %v", ch)
	}
}

func TestChannelAddressesAreValidatedAndPrivateOnesAreRefusedWhenTheOperatorDidNotAllowThem(t *testing.T) {
	e := nfEnv(t, fxOpts{}, false)
	post := func(kind, url string, want int) map[string]any {
		b, _ := json.Marshal(map[string]any{"kind": kind, "name": "n", "url": url})
		return e.json("POST", e.nfPath(""), "alice", string(b), want)
	}
	post("webhook", "http://example.com/x", 400) // not https
	post("webhook", "ftp://example.com/x", 400)
	post("webhook", "https://user:pw@example.com/x", 400)
	post("wecom", "https://evil.example.com/cgi-bin/webhook/send?key=k", 400)
	post("discord", "https://discord.com.evil.io/api/webhooks/1/x", 400)
	post("sms", "https://example.com/x", 400)
	post("wecom", "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc", 201)
	post("discord", "https://discord.com/api/webhooks/1/abc", 201)

	// A private address is accepted as text but never connected to: the test fails with a plain reason.
	for _, addr := range []string{"https://127.0.0.1/hook", "https://169.254.169.254/latest/meta-data", "https://10.1.2.3/x", "https://[::1]/x"} {
		id := post("webhook", addr, 201)["id"].(string)
		res := e.json("POST", e.nfPath("/"+id+"/test"), "alice", "", 200)
		if res["status"] != "failed" || !strings.Contains(res["error"].(string), "not allowed") {
			t.Fatalf("%s: %v", addr, res)
		}
		if strings.Contains(res["error"].(string), "127.0.0.1") || strings.Contains(res["error"].(string), "169.254") {
			t.Fatalf("the error names the address: %v", res)
		}
		e.json("DELETE", e.nfPath("/"+id), "alice", "", 204)
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAtMostTenChannelsAndRateLimitedTests(t *testing.T) {
	sink := newHookSink(t)
	e := nfEnv(t, fxOpts{}, true)
	var id string
	for i := 0; i < maxNotificationChannels; i++ {
		id = e.addWebhook("alice", sink.srv.URL, "")["id"].(string)
	}
	b, _ := json.Marshal(map[string]any{"kind": "webhook", "name": "n", "url": sink.srv.URL})
	e.json("POST", e.nfPath(""), "alice", string(b), 400)
	e.json("POST", e.nfPath("/"+id+"/test"), "alice", "", 200)
	e.json("POST", e.nfPath("/"+id+"/test"), "alice", "", 429)
}

func TestWithoutASecretKeyChannelsCannotBeAdded(t *testing.T) {
	e := newFxEnv(t, fxOpts{prod: func(p *Options) { p.SecretKey = nil }})
	e.json("POST", e.nfPath(""), "alice", `{"kind":"webhook","name":"n","url":"https://example.com/x"}`, 501)
	// The rules and the preferences still work: the defaults need no key.
	e.json("GET", "/api/projects/"+e.project+"/notifications", "alice", "", 200)
	got := e.json("GET", "/api/me/notifications", "bob", "", 200)
	if got["effective"].(map[string]any)["task.blocked"].([]any)[0] != "inapp" {
		t.Fatalf("effective = %v", got["effective"])
	}
}

func TestPersonalPreferencesDefaultsSaveAndNeverReturnTheWebhook(t *testing.T) {
	sink := newHookSink(t)
	tg := newCnTG(t)
	e := nfEnv(t, fxOpts{tg: tg}, true)

	// Nothing linked: everything stays in the app.
	got := e.json("GET", "/api/me/notifications", "bob", "", 200)
	eff := got["effective"].(map[string]any)
	if v := eff["task.blocked"].([]any); len(v) != 1 || v[0] != "inapp" {
		t.Fatalf("effective = %v", eff)
	}
	if len(got["events"].([]any)) < 12 {
		t.Fatalf("events = %v", got["events"])
	}

	// Linked Telegram: the defaults reach it for what needs a person, not for the quiet events.
	e.linkTelegram(t, "bob", 222)
	eff = e.json("GET", "/api/me/notifications", "bob", "", 200)["effective"].(map[string]any)
	if v := eff["task.blocked"].([]any); len(v) != 2 || v[1] != "telegram" {
		t.Fatalf("blocked = %v", v)
	}
	if v := eff["task.done"].([]any); len(v) != 1 {
		t.Fatalf("done = %v", v)
	}

	// Save: telegram off for blocked, on for done; quiet hours; a webhook with a secret.
	body := `{"events":{"task.blocked":["inapp"],"task.done":["inapp","telegram"]},"quietStart":"22:00","quietEnd":"07:00","timezone":"Europe/Paris","digest":true,
		"webhook":{"url":"` + sink.srv.URL + `/me?k=zzz","secret":"mine"}}`
	resp := e.do("PUT", "/api/me/notifications", "bob", body)
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.Contains(string(raw), "zzz") || strings.Contains(string(raw), "mine") {
		t.Fatalf("PUT: %d %s", resp.StatusCode, raw)
	}
	var saved map[string]any
	_ = json.Unmarshal(raw, &saved)
	eff = saved["effective"].(map[string]any)
	if v := eff["task.blocked"].([]any); len(v) != 1 {
		t.Fatalf("blocked = %v", v)
	}
	if v := eff["task.done"].([]any); len(v) != 2 {
		t.Fatalf("done = %v", v)
	}
	if saved["quietStart"] != "22:00" || saved["timezone"] != "Europe/Paris" || saved["digest"] != true {
		t.Fatalf("saved = %v", saved)
	}
	wh := saved["webhook"].(map[string]any)
	if wh["set"] != true || wh["hasSecret"] != true || wh["hint"] != strings.TrimPrefix(sink.srv.URL, "http://") {
		t.Fatalf("webhook = %v", wh)
	}
	// It is the person's own: another member sees nothing of it.
	other := e.json("GET", "/api/me/notifications", "alice", "", 200)
	if other["webhook"].(map[string]any)["set"] == true || other["quietStart"] != "" {
		t.Fatalf("preferences leaked to another person: %v", other)
	}
	// Choices that match the defaults are not frozen: dropping to the defaults and linking later keeps working.
	e.json("PUT", "/api/me/notifications", "bob", `{"events":{"task.blocked":["inapp","telegram"]}}`, 200)

	// Invalid input.
	e.json("PUT", "/api/me/notifications", "bob", `{"events":{"nope":["inapp"]}}`, 400)
	e.json("PUT", "/api/me/notifications", "bob", `{"events":{"task.done":["pigeon"]}}`, 400)
	e.json("PUT", "/api/me/notifications", "bob", `{"quietStart":"22:00"}`, 400)
	e.json("PUT", "/api/me/notifications", "bob", `{"quietStart":"22:00","quietEnd":"07:00","timezone":"Mars/Base"}`, 400)
	e.json("PUT", "/api/me/notifications", "bob", `{"webhook":{"url":"ftp://x"}}`, 400)
	e.json("PUT", "/api/me/notifications", "bob", `{"webhook":{"clear":true}}`, 200)
	if got := e.json("GET", "/api/me/notifications", "bob", "", 200); got["webhook"].(map[string]any)["set"] == true {
		t.Fatal("the webhook was not cleared")
	}
}

func TestSendMeATestReachesEveryChannelSetUpForTheCaller(t *testing.T) {
	sink := newHookSink(t)
	tg := newCnTG(t)
	e := nfEnv(t, fxOpts{tg: tg}, true)
	e.linkTelegram(t, "bob", 222)
	e.json("PUT", "/api/me/notifications", "bob", `{"webhook":{"url":"`+sink.srv.URL+`","secret":"s"}}`, 200)

	res := e.json("POST", "/api/me/notifications/test", "bob", "", 200)["results"].([]any)
	got := map[string]string{}
	for _, r := range res {
		m := r.(map[string]any)
		got[m["channel"].(string)] = m["status"].(string)
	}
	if got["Telegram"] != "delivered" || got["Your webhook"] != "delivered" {
		t.Fatalf("results = %v", res)
	}
	if msg := tg.waitSent(t, 222, "Test message")["text"].(string); !strings.Contains(msg, "[TEST]") {
		t.Fatalf("telegram test = %q", msg)
	}
	call := sink.wait(t, 1)[0]
	mac := hmac.New(sha256.New, []byte("s"))
	mac.Write(call.body)
	if call.hdr.Get("X-7ar-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("bad signature")
	}
	// The test shows in the person's recent deliveries.
	recent := e.json("GET", "/api/me/notifications", "bob", "", 200)["recent"].([]any)
	if len(recent) < 2 {
		t.Fatalf("recent = %v", recent)
	}
	e.json("POST", "/api/me/notifications/test", "bob", "", 429)
}

func TestTheDefaultsStillReachTheOwnersTelegramWhenAProjectSetNoRules(t *testing.T) {
	// The regression the rules engine must not cause: a blocked task reaches the owner's linked chat with the title,
	// the reason and the link, with no configuration at all.
	tg := newCnTG(t)
	e := nfEnv(t, fxOpts{tg: tg}, false)
	e.linkTelegram(t, "bob", 222)
	id := e.newTask("Ship it")
	e.block(id, "needs a human")
	msg := tg.waitSent(t, 222, "Ship it")["text"].(string)
	if !strings.Contains(msg, "needs a human") || !strings.Contains(msg, "https://harness.test/#/projects/"+e.project) {
		t.Fatalf("message = %q", msg)
	}
	// And the person can switch it off for themselves.
	e.json("PUT", "/api/me/notifications", "bob", `{"events":{"task.blocked":["inapp"]}}`, 200)
	before := tg.count("sendMessage")
	id2 := e.newTask("Second")
	e.block(id2, "again")
	time.Sleep(300 * time.Millisecond)
	if tg.count("sendMessage") != before {
		t.Fatal("Telegram was used although the person turned it off for this event")
	}
}

func TestMemberJoinedTellsTheOwnersChannel(t *testing.T) {
	sink := newHookSink(t)
	e := nfEnv(t, fxOpts{}, true)
	id := e.addWebhook("alice", sink.srv.URL, "")["id"].(string)
	e.json("PUT", "/api/projects/"+e.project+"/notifications", "alice",
		`{"events":{"member.joined":{"channels":["`+id+`"],"person":false}},"dedupeMinutes":10}`, 200)
	e.json("PUT", "/api/projects/"+e.project+"/members/dave", "alice", `{"role":"member"}`, 204)
	var p map[string]any
	_ = json.Unmarshal(sink.wait(t, 1)[0].body, &p)
	if p["event"] != "member.joined" || !strings.Contains(p["title"].(string), "dave") {
		t.Fatalf("payload = %v", p)
	}
	// A role change of an existing member is not a join.
	e.json("PUT", "/api/projects/"+e.project+"/members/dave", "alice", `{"role":"viewer"}`, 204)
	time.Sleep(200 * time.Millisecond)
	if sink.count() != 1 {
		t.Fatalf("calls = %d", sink.count())
	}
}

func TestNotificationsOfADeletedProjectAreRemoved(t *testing.T) {
	sink := newHookSink(t)
	e := nfEnv(t, fxOpts{}, true)
	e.addWebhook("alice", sink.srv.URL, "")
	e.json("DELETE", "/api/projects/"+e.project, "alice", "", 204)

	if chans, _ := e.boundServer().nf.st.ListChannels(e.ctx, e.project); len(chans) != 0 {
		t.Fatal("channels survived the project")
	}
}

func TestPersonalEmailNeedsAProfileAddressAndAMailer(t *testing.T) {
	var sent []string
	var mu sync.Mutex
	e := nfEnv(t, fxOpts{}, true)
	_ = e
	n := &notifier{s: &server{opt: Options{Mail: mailFunc(func(to []string, subject, body string) {
		mu.Lock()
		sent = append(sent, strings.Join(to, ",")+"|"+subject+"|"+body)
		mu.Unlock()
	})}, team: e.tm}}
	if err := n.sendEmail(e.ctx, notify.Target{Login: "bob"}, notify.Message{Title: "t"}); err == nil || !strings.Contains(err.Error(), "no email") {
		t.Fatalf("a profile without an address: %v", err)
	}
	if _, err := e.tm.UpsertUser(e.ctx, userWithEmail("bob", "bob@example.com")); err != nil {
		t.Fatal(err)
	}
	if err := n.sendEmail(e.ctx, notify.Target{Login: "bob"}, notify.Message{Title: "Blocked", ProjectName: "api", Text: "why", URL: "https://x/y", Test: true}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "bob@example.com|[api] [TEST] Blocked|") || !strings.Contains(sent[0], "https://x/y") {
		t.Fatalf("sent = %v", sent)
	}
	n.s.opt.Mail = nil
	if err := n.sendEmail(e.ctx, notify.Target{Login: "bob"}, notify.Message{Title: "t"}); err == nil || !strings.Contains(err.Error(), "SMTP") {
		t.Fatalf("no mailer: %v", err)
	}
}

type mailFunc func(to []string, subject, body string)

func (f mailFunc) Send(_ context.Context, to []string, subject, body string) error {
	f(to, subject, body)
	return nil
}

func userWithEmail(login, email string) store.User {
	return store.User{Login: login, Name: login, Email: email}
}

// boundServer is the server the escalator bound itself to.
func (e *fxEnv) boundServer() *server {
	e.esc.mu.Lock()
	defer e.esc.mu.Unlock()
	return e.esc.s
}
