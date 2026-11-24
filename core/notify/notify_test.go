package notify_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/notify"
)

type rec struct {
	mu   sync.Mutex
	sent []sentMsg
	err  error
}
type sentMsg struct {
	T notify.Target
	M notify.Message
}

func (r *rec) Send(_ context.Context, t notify.Target, m notify.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, sentMsg{t, m})
	return nil
}
func (r *rec) n() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.sent) }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newD(t *testing.T) (*notify.Dispatcher, *notify.MemStore, map[string]*rec, *clock) {
	t.Helper()
	st := notify.NewMemStore(nil)
	ck := &clock{t: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)}
	senders := map[string]*rec{}
	m := map[string]notify.Sender{}
	for _, k := range []string{"webhook", "wecom", "discord", "telegram", "slack", "email"} {
		senders[k] = &rec{}
		m[k] = senders[k]
	}
	d := &notify.Dispatcher{Store: st, Senders: m, Now: ck.now,
		Open: notify.OpenPair,
		Linked: func(_ context.Context, login string) []notify.Via {
			if login == "ann" {
				return []notify.Via{notify.Telegram, notify.Slack, notify.Email}
			}
			return nil
		},
		Redact: func(s string) string { return strings.ReplaceAll(s, "ghp_SECRET", "[redacted]") }}
	return d, st, senders, ck
}

func TestDefaultsReachLinkedChannelsOfUrgentEventsOnly(t *testing.T) {
	d, _, s, _ := newD(t)
	ctx := context.Background()
	d.Emit(ctx, notify.Event{Kind: notify.TaskBlocked, ProjectID: "p", Title: "Fix it", Login: "ann"})
	if s["telegram"].n() != 1 || s["slack"].n() != 1 || s["email"].n() != 1 {
		t.Fatalf("a blocked task must reach every linked channel by default: tg=%d slack=%d mail=%d", s["telegram"].n(), s["slack"].n(), s["email"].n())
	}
	d.Emit(ctx, notify.Event{Kind: notify.TaskDone, ProjectID: "p", Title: "Done", Login: "ann"})
	if s["telegram"].n() != 1 {
		t.Fatal("a noisy event (task done) stays in the app by default")
	}
	d.Emit(ctx, notify.Event{Kind: notify.TaskBlocked, ProjectID: "p", Title: "Other", Login: "bob"})
	if s["telegram"].n() != 1 {
		t.Fatal("a person without links gets nothing outside the app")
	}
}

func TestPersonalChoiceOverridesDefaults(t *testing.T) {
	d, st, s, _ := newD(t)
	ctx := context.Background()
	_ = st.PutPrefs(ctx, notify.Prefs{Login: "ann", Events: map[notify.Kind][]notify.Via{notify.TaskBlocked: {notify.Email}, notify.TaskDone: {notify.Telegram}}})
	d.Emit(ctx, notify.Event{Kind: notify.TaskBlocked, Title: "a", Login: "ann"})
	d.Emit(ctx, notify.Event{Kind: notify.TaskDone, Title: "b", Login: "ann"})
	if s["telegram"].n() != 1 || s["slack"].n() != 0 || s["email"].n() != 1 {
		t.Fatalf("tg=%d slack=%d mail=%d", s["telegram"].n(), s["slack"].n(), s["email"].n())
	}
}

func TestProjectRulesChannelsAndPersonToggle(t *testing.T) {
	d, st, s, _ := newD(t)
	ctx := context.Background()
	_ = st.PutChannel(ctx, notify.Channel{ID: "c1", ProjectID: "p", Kind: "webhook", Name: "ops", Enabled: true, Sealed: notify.SealPair("https://example.com/h", "")})
	_ = st.PutChannel(ctx, notify.Channel{ID: "c2", ProjectID: "p", Kind: "discord", Name: "off", Enabled: false, Sealed: notify.SealPair("https://discord.com/api/webhooks/1/x", "")})
	_ = st.PutRules(ctx, notify.Rules{ProjectID: "p", Events: map[notify.Kind]notify.Rule{notify.TaskBlocked: {Channels: []string{"c1", "c2"}, Person: false}}})
	d.Emit(ctx, notify.Event{Kind: notify.TaskBlocked, ProjectID: "p", Title: "T", Login: "ann"})
	if s["webhook"].n() != 1 || s["discord"].n() != 0 {
		t.Fatalf("webhook=%d discord=%d", s["webhook"].n(), s["discord"].n())
	}
	if s["telegram"].n() != 0 {
		t.Fatal("the owner switched the person off for this event")
	}
	if s["webhook"].sent[0].T.URL != "https://example.com/h" {
		t.Fatal("the sealed address was not opened")
	}
	log, _ := st.ListLog(ctx, "p", "", 10)
	if len(log) != 1 || log[0].Status != notify.Delivered || log[0].Channel != "ops (webhook)" {
		t.Fatalf("log = %+v", log)
	}
}

func TestDedupeCollapsesTheSameKeyWithinTheWindow(t *testing.T) {
	d, st, s, ck := newD(t)
	ctx := context.Background()
	ev := notify.Event{Kind: notify.TaskBlocked, ProjectID: "p", Key: "task:1:blocked", Title: "T", Login: "ann"}
	d.Emit(ctx, ev)
	ck.t = ck.t.Add(5 * time.Minute)
	d.Emit(ctx, ev)
	if s["telegram"].n() != 1 {
		t.Fatalf("the repeat within 10 minutes must collapse, got %d", s["telegram"].n())
	}
	log, _ := st.ListLog(ctx, "p", "", 20)
	var collapsed int
	for _, e := range log {
		if e.Status == notify.Collapsed {
			collapsed++
		}
	}
	if collapsed == 0 {
		t.Fatal("the collapse is not in the log")
	}
	ck.t = ck.t.Add(10 * time.Minute)
	d.Emit(ctx, ev)
	if s["telegram"].n() != 2 {
		t.Fatal("after the window the event is sent again")
	}
	d.Emit(ctx, notify.Event{Kind: notify.TaskBlocked, ProjectID: "p", Key: "task:2:blocked", Title: "T2", Login: "ann"})
	if s["telegram"].n() != 3 {
		t.Fatal("another key is not a repeat")
	}
}

func TestDedupeOffSendsEveryOne(t *testing.T) {
	d, st, s, _ := newD(t)
	ctx := context.Background()
	_ = st.PutRules(ctx, notify.Rules{ProjectID: "p", DedupeMinutes: 0})
	ev := notify.Event{Kind: notify.TaskBlocked, ProjectID: "p", Key: "k", Title: "T", Login: "ann"}
	d.Emit(ctx, ev)
	d.Emit(ctx, ev)
	if s["telegram"].n() != 2 {
		t.Fatal("with 0 minutes every event is sent")
	}
}

func TestQuietHoursHoldThenFlush(t *testing.T) {
	d, st, s, ck := newD(t)
	ctx := context.Background()
	_ = st.PutPrefs(ctx, notify.Prefs{Login: "ann", QuietStart: "22:00", QuietEnd: "07:00"})
	ck.t = time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC)
	d.Emit(ctx, notify.Event{Kind: notify.TaskBlocked, Key: "a", Title: "one", Login: "ann"})
	d.Emit(ctx, notify.Event{Kind: notify.TaskFailed, Key: "b", Title: "two", Login: "ann"})
	if s["telegram"].n() != 0 || d.Held() != 6 {
		t.Fatalf("held = %d, sent = %d", d.Held(), s["telegram"].n())
	}
	d.Flush(ctx)
	if s["telegram"].n() != 0 {
		t.Fatal("still quiet")
	}
	ck.t = time.Date(2026, 3, 11, 7, 1, 0, 0, time.UTC)
	d.Flush(ctx)
	if s["telegram"].n() != 1 || !strings.Contains(s["telegram"].sent[0].M.Text, "one") || !strings.Contains(s["telegram"].sent[0].M.Text, "two") {
		t.Fatalf("one batched message with both lines expected: %+v", s["telegram"].sent)
	}
	if d.Held() != 0 {
		t.Fatal("queue not drained")
	}
}

func TestQuietWindow(t *testing.T) {
	p := notify.Prefs{QuietStart: "22:00", QuietEnd: "07:00", Timezone: "Asia/Shanghai"}
	// 15:00 UTC is 23:00 in Shanghai.
	q, until := p.Quiet(time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC))
	if !q || until.UTC().Hour() != 23 || until.UTC().Day() != 10 {
		t.Fatalf("quiet=%v until=%v", q, until)
	}
	if q, _ := p.Quiet(time.Date(2026, 3, 10, 6, 0, 0, 0, time.UTC)); q {
		t.Fatal("14:00 in Shanghai is not quiet")
	}
	day := notify.Prefs{QuietStart: "09:00", QuietEnd: "17:00"}
	if q, _ := day.Quiet(time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)); !q {
		t.Fatal("same-day window")
	}
	if q, _ := (notify.Prefs{}).Quiet(time.Now()); q {
		t.Fatal("no quiet hours")
	}
}

func TestDigestBatchesNonUrgentOnly(t *testing.T) {
	d, st, s, ck := newD(t)
	ctx := context.Background()
	_ = st.PutPrefs(ctx, notify.Prefs{Login: "ann", Digest: true, Events: map[notify.Kind][]notify.Via{notify.TaskReview: {notify.Telegram}, notify.TaskBlocked: {notify.Telegram}}})
	d.Emit(ctx, notify.Event{Kind: notify.TaskReview, Key: "1", Title: "PR one", Login: "ann"})
	d.Emit(ctx, notify.Event{Kind: notify.TaskReview, Key: "2", Title: "PR two", Login: "ann"})
	d.Emit(ctx, notify.Event{Kind: notify.TaskBlocked, Key: "3", Title: "Blocked", Login: "ann"})
	if s["telegram"].n() != 1 || s["telegram"].sent[0].M.Title != "Blocked" {
		t.Fatalf("urgent goes at once, got %+v", s["telegram"].sent)
	}
	ck.t = ck.t.Add(30 * time.Minute)
	d.Flush(ctx)
	if s["telegram"].n() != 1 {
		t.Fatal("digest is hourly")
	}
	ck.t = ck.t.Add(31 * time.Minute)
	d.Flush(ctx)
	if s["telegram"].n() != 2 {
		t.Fatal("digest not sent")
	}
	m := s["telegram"].sent[1].M
	if !strings.Contains(m.Text, "PR one") || !strings.Contains(m.Text, "PR two") || !strings.Contains(m.Title, "2") {
		t.Fatalf("digest = %+v", m)
	}
}

func TestProjectDigestBatchesChannelMessages(t *testing.T) {
	d, st, s, ck := newD(t)
	ctx := context.Background()
	_ = st.PutChannel(ctx, notify.Channel{ID: "c1", ProjectID: "p", Kind: "wecom", Name: "grp", Enabled: true, Sealed: notify.SealPair("https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=k", "")})
	_ = st.PutRules(ctx, notify.Rules{ProjectID: "p", Digest: true, Events: map[notify.Kind]notify.Rule{notify.TaskDone: {Channels: []string{"c1"}}}})
	d.Emit(ctx, notify.Event{Kind: notify.TaskDone, ProjectID: "p", Title: "a"})
	d.Emit(ctx, notify.Event{Kind: notify.TaskDone, ProjectID: "p", Title: "b"})
	ck.t = ck.t.Add(2 * time.Hour)
	d.Flush(ctx)
	if s["wecom"].n() != 1 {
		t.Fatalf("wecom=%d", s["wecom"].n())
	}
}

func TestRedactionAndLogDoNotLeak(t *testing.T) {
	d, st, s, _ := newD(t)
	ctx := context.Background()
	d.Emit(ctx, notify.Event{Kind: notify.TaskBlocked, Title: "token ghp_SECRET", Body: "ghp_SECRET here", Login: "ann", Data: map[string]any{"x": "ghp_SECRET"}})
	m := s["telegram"].sent[0].M
	if strings.Contains(m.Title+m.Text+m.Data["x"].(string), "ghp_SECRET") {
		t.Fatalf("message not redacted: %+v", m)
	}
	s["telegram"].err = errors.New("boom ghp_SECRET")
	d.Emit(ctx, notify.Event{Kind: notify.TaskBlocked, Key: "z", Title: "t", Login: "ann"})
	log, _ := st.ListLog(ctx, "", "ann", 10)
	var failed bool
	for _, e := range log {
		if strings.Contains(e.Error, "ghp_SECRET") {
			t.Fatal("log leaks")
		}
		failed = failed || e.Status == notify.Failed
	}
	if !failed {
		t.Fatal("failure not logged")
	}
}

func TestSkippedWhenChosenButNotLinked(t *testing.T) {
	d, st, _, _ := newD(t)
	ctx := context.Background()
	_ = st.PutPrefs(ctx, notify.Prefs{Login: "bob", Events: map[notify.Kind][]notify.Via{notify.TaskBlocked: {notify.Telegram}}})
	d.Emit(ctx, notify.Event{Kind: notify.TaskBlocked, Title: "t", Login: "bob"})
	log, _ := st.ListLog(ctx, "", "bob", 10)
	if len(log) != 1 || log[0].Status != notify.Skipped || !strings.Contains(log[0].Error, "not set up") {
		t.Fatalf("log = %+v", log)
	}
}

func TestValidation(t *testing.T) {
	if err := (notify.Prefs{QuietStart: "22:00"}).Validate(); err == nil {
		t.Fatal("half quiet hours")
	}
	if err := (notify.Prefs{QuietStart: "25:00", QuietEnd: "07:00"}).Validate(); err == nil {
		t.Fatal("bad clock")
	}
	if err := (notify.Prefs{Events: map[notify.Kind][]notify.Via{"nope": nil}}).Validate(); err == nil {
		t.Fatal("unknown event")
	}
	if err := (notify.Prefs{Events: map[notify.Kind][]notify.Via{notify.TaskDone: {"sms"}}}).Validate(); err == nil {
		t.Fatal("unknown via")
	}
	if err := (notify.Prefs{QuietStart: "22:00", QuietEnd: "07:00", Timezone: "Europe/Paris"}).Validate(); err != nil {
		t.Fatal(err)
	}
	r := notify.Rules{Events: map[notify.Kind]notify.Rule{notify.TaskDone: {Channels: []string{"zz"}}}}
	if err := r.Validate(map[string]bool{"c1": true}); err == nil {
		t.Fatal("unknown channel")
	}
	if err := (notify.Rules{DedupeMinutes: -1}).Validate(nil); err == nil {
		t.Fatal("negative window")
	}
}

// ---- senders ----

func httpSender(kind string) *notify.HTTPSender {
	return &notify.HTTPSender{Kind: kind, Client: notify.NewHTTPClient(true), Sleep: func(time.Duration) {}}
}

func TestWebhookPayloadAndSignature(t *testing.T) {
	var body []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
	}))
	defer srv.Close()
	at := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	err := httpSender("webhook").Send(context.Background(), notify.Target{URL: srv.URL, Secret: "s3cret"},
		notify.Message{Event: notify.TaskBlocked, ProjectName: "Shop", Title: "T", URL: "https://x/y", At: at, Data: map[string]any{"task": "1"}, Text: "why"})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); hdr.Get("X-7ar-Signature") != want {
		t.Fatalf("signature %q want %q", hdr.Get("X-7ar-Signature"), want)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["event"] != "task.blocked" || got["project"] != "Shop" || got["title"] != "T" || got["url"] != "https://x/y" || got["at"] != "2026-03-10T12:00:00Z" {
		t.Fatalf("payload = %v", got)
	}
	if d := got["data"].(map[string]any); d["task"] != "1" || d["body"] != "why" {
		t.Fatalf("data = %v", d)
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Fatal("content type")
	}
}

func TestWebhookWithoutSecretIsUnsigned(t *testing.T) {
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hdr = r.Header.Clone() }))
	defer srv.Close()
	if err := httpSender("webhook").Send(context.Background(), notify.Target{URL: srv.URL}, notify.Message{Event: notify.Test, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if hdr.Get("X-7ar-Signature") != "" {
		t.Fatal("no secret, no signature")
	}
}

func TestWebhookRetriesThenGivesUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	var slept []time.Duration
	s := httpSender("webhook")
	s.Sleep = func(d time.Duration) { slept = append(slept, d) }
	if err := s.Send(context.Background(), notify.Target{URL: srv.URL}, notify.Message{Title: "t"}); err != nil || calls.Load() != 3 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	if len(slept) != 2 || slept[1] != 2*slept[0] {
		t.Fatalf("backoff = %v", slept)
	}
	calls.Store(-100)
	err := s.Send(context.Background(), notify.Target{URL: srv.URL}, notify.Message{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("err = %v", err)
	}
}

func TestWebhookDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(404) }))
	defer srv.Close()
	err := httpSender("webhook").Send(context.Background(), notify.Target{URL: srv.URL}, notify.Message{Title: "t"})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestSSRFGuardRefusesPrivateAddressesAndRedirects(t *testing.T) {
	var hit atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Add(1) }))
	defer target.Close()
	guarded := &notify.HTTPSender{Kind: "webhook", Client: notify.NewHTTPClient(false), Sleep: func(time.Duration) {}}
	for _, addr := range []string{target.URL, "http://127.0.0.1:1/", "http://169.254.169.254/latest/meta-data", "http://10.0.0.5/", "http://[::1]:9/", "http://[::ffff:127.0.0.1]:9/", "http://0.0.0.0:9/"} {
		err := guarded.Send(context.Background(), notify.Target{URL: addr}, notify.Message{Title: "t"})
		if err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("%s: err = %v", addr, err)
		}
		if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "169.254") {
			t.Fatalf("the error names the address: %v", err)
		}
	}
	if hit.Load() != 0 {
		t.Fatal("a private server was reached")
	}
	// Redirects to private addresses are refused by the dialer (see core/netguard's tests).
	chain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, r.URL.Path+"x", http.StatusFound) }))
	defer chain.Close()
	if err := httpSender("webhook").Send(context.Background(), notify.Target{URL: chain.URL + "/a"}, notify.Message{Title: "t"}); err == nil {
		t.Fatal("endless redirects must stop")
	}
}

func TestWeComPayload(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RequestURI()
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()
	err := httpSender("wecom").Send(context.Background(), notify.Target{URL: srv.URL + "/cgi-bin/webhook/send?key=abc"}, notify.Message{Event: notify.TaskBlocked, ProjectName: "Shop", Title: "Fix", Text: "reason", URL: "https://x/p"})
	if err != nil {
		t.Fatal(err)
	}
	md := got["markdown"].(map[string]any)["content"].(string)
	if got["msgtype"] != "markdown" || path != "/cgi-bin/webhook/send?key=abc" || !strings.Contains(md, "[Shop] Fix") || !strings.Contains(md, "reason") || !strings.Contains(md, "(https://x/p)") {
		t.Fatalf("payload = %v path = %s", got, path)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":93000,"errmsg":"invalid webhook url"}`))
	}))
	defer bad.Close()
	err = httpSender("wecom").Send(context.Background(), notify.Target{URL: bad.URL}, notify.Message{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "93000") {
		t.Fatalf("a WeCom errcode must be a failure: %v", err)
	}
	long := strings.Repeat("é", 5000)
	if n := len(notify.WeComMarkdown(notify.Message{Title: "t", Text: long})); n > 4096 {
		t.Fatalf("markdown is %d bytes", n)
	}
}

func TestDiscordPayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := httpSender("discord").Send(context.Background(), notify.Target{URL: srv.URL}, notify.Message{Title: "Build @everyone", Text: "line", URL: "https://x", Test: true}); err != nil {
		t.Fatal(err)
	}
	c := got["content"].(string)
	if !strings.Contains(c, "[TEST]") || !strings.Contains(c, "https://x") || got["username"] != "7ar" {
		t.Fatalf("payload = %v", got)
	}
	if p := got["allowed_mentions"].(map[string]any)["parse"].([]any); len(p) != 0 {
		t.Fatal("mentions must be disabled")
	}
	if len(notify.DiscordText(notify.Message{Title: strings.Repeat("a", 5000)})) > 2000 {
		t.Fatal("discord limit")
	}
}

func TestValidateAddress(t *testing.T) {
	ok := map[string]string{
		"wecom":   "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc",
		"discord": "https://discord.com/api/webhooks/1/abc",
		"webhook": "https://hooks.example.com/x",
	}
	for k, u := range ok {
		if err := notify.ValidateAddress(k, u, true); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	for k, u := range map[string]string{
		"wecom":   "https://evil.example.com/cgi-bin/webhook/send?key=abc",
		"discord": "https://discord.com.evil.io/api/webhooks/1/abc",
		"webhook": "http://insecure.example.com/x",
	} {
		if err := notify.ValidateAddress(k, u, true); err == nil {
			t.Fatalf("%s %s must be refused", k, u)
		}
	}
	if err := notify.ValidateAddress("webhook", "ftp://x", false); err == nil {
		t.Fatal("scheme")
	}
	if err := notify.ValidateAddress("webhook", "https://user:pw@x.com/", false); err == nil {
		t.Fatal("credentials in the address")
	}
	if err := notify.ValidateAddress("webhook", "http://127.0.0.1:9/x", false); err != nil {
		t.Fatalf("relaxed: %v", err)
	}
}

func TestTestMessageIsMarkedAndLogged(t *testing.T) {
	d, st, s, _ := newD(t)
	ctx := context.Background()
	if err := d.Test(ctx, "p", "Shop", "ann", "webhook", "ops (webhook)", notify.Target{URL: "https://x"}); err != nil {
		t.Fatal(err)
	}
	m := s["webhook"].sent[0].M
	if !m.Test || m.Event != notify.Test || !strings.Contains(m.Text, "Nothing needs your attention") {
		t.Fatalf("m = %+v", m)
	}
	s["webhook"].err = errors.New("the server answered 500")
	if err := d.Test(ctx, "p", "Shop", "ann", "webhook", "ops", notify.Target{URL: "https://x"}); err == nil {
		t.Fatal("failure must be returned")
	}
	log, _ := st.ListLog(ctx, "p", "", 10)
	if len(log) != 2 || log[0].Status != notify.Failed || log[1].Status != notify.Delivered {
		t.Fatalf("log = %+v", log)
	}
}
