package product

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

const (
	skToken  = "xoxb-secret"
	skSecret = "signing-secret"
)

// fakeSlack is a Slack Web API that records the calls.
type fakeSlack struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []skCall
}

type skCall struct {
	method string
	body   map[string]any
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+skToken {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		method := strings.TrimPrefix(r.URL.Path, "/")
		f.mu.Lock()
		f.calls = append(f.calls, skCall{method, in})
		f.mu.Unlock()
		switch method {
		case "auth.test":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user_id": "UBOT"})
		case "users.info":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"name": "alice", "real_name": "Alice A"}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) posts() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.method == "chat.postMessage" {
			out = append(out, c.body)
		}
	}
	return out
}

func (f *fakeSlack) waitPost(t *testing.T, contains string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range f.posts() {
			if strings.Contains(p["text"].(string), contains) {
				return p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no post containing %q; posts: %v", contains, f.posts())
	return nil
}

func skBridge(t *testing.T, f *fakeSlack, tm store.TeamStore) *slackBridge {
	t.Helper()
	box, _ := newSecretBox(cnKey)
	s := &server{team: tm, box: box, hub: newHub()}
	s.opt.Slack = &Slack{Token: skToken, SigningSecret: skSecret, API: f.srv.URL}
	return newSlackBridge(s)
}

func signed(body string, ts time.Time, secret string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/slack/events", strings.NewReader(body))
	t := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + t + ":" + body))
	r.Header.Set("X-Slack-Request-Timestamp", t)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return r
}

func TestSlackVerifyRejectsBadStaleAndUnsignedRequests(t *testing.T) {
	sl := &Slack{SigningSecret: skSecret}
	now := time.Now()
	body := `{"a":1}`
	for name, tc := range map[string]struct {
		r    *http.Request
		want bool
	}{
		"good":        {signed(body, now, skSecret), true},
		"wrong key":   {signed(body, now, "other"), false},
		"stale":       {signed(body, now.Add(-10*time.Minute), skSecret), false},
		"from future": {signed(body, now.Add(10*time.Minute), skSecret), false},
		"unsigned":    {httptest.NewRequest(http.MethodPost, "/", nil), false},
	} {
		if got := sl.verify(tc.r.Header, []byte(body), now); got != tc.want {
			t.Errorf("%s: verify = %v, want %v", name, got, tc.want)
		}
	}
	if (&Slack{}).verify(signed(body, now, "").Header, []byte(body), now) {
		t.Error("an empty signing secret accepted a request")
	}
}

func TestSlackEventsEndpoint(t *testing.T) {
	f := newFakeSlack(t)
	b := skBridge(t, f, newCnFaultTeam())

	// A bad signature is refused.
	w := httptest.NewRecorder()
	b.events(w, signed(`{}`, time.Now(), "wrong"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", w.Code)
	}
	// Slack's URL check echoes the challenge.
	w = httptest.NewRecorder()
	b.events(w, signed(`{"type":"url_verification","challenge":"abc"}`, time.Now(), skSecret))
	if w.Code != 200 || w.Body.String() != "abc" {
		t.Fatalf("challenge: %d %q", w.Code, w.Body.String())
	}
	// Messages by bots, and edits, are ignored: nothing is posted.
	for _, body := range []string{
		`{"type":"event_callback","event_id":"1","event":{"type":"message","channel_type":"im","bot_id":"B1","user":"U1","text":"hi","channel":"D1"}}`,
		`{"type":"event_callback","event_id":"2","event":{"type":"message","channel_type":"im","subtype":"message_changed","user":"U1","text":"hi","channel":"D1"}}`,
		`{"type":"event_callback","event_id":"3","event":{"type":"message","channel_type":"channel","user":"U1","text":"hi","channel":"C1"}}`,
	} {
		w = httptest.NewRecorder()
		b.events(w, signed(body, time.Now(), skSecret))
		if w.Code != 200 {
			t.Fatalf("ignored event answered %d", w.Code)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.posts()); n != 0 {
		t.Fatalf("%d posts for ignored events", n)
	}
}

func TestSlackUnlinkedUserIsToldHowToLinkInTheThreadOfTheMention(t *testing.T) {
	f := newFakeSlack(t)
	b := skBridge(t, f, newCnFaultTeam())
	body := `{"type":"event_callback","event_id":"e1","event":{"type":"app_mention","user":"U1","text":"<@UBOT> do it","channel":"C1","ts":"111.1"}}`

	w := httptest.NewRecorder()
	b.events(w, signed(body, time.Now(), skSecret))

	p := f.waitPost(t, "not linked")
	if p["channel"] != "C1" || p["thread_ts"] != "111.1" {
		t.Fatalf("reply went to %v thread %v", p["channel"], p["thread_ts"])
	}
	// Slack redelivers an event it thinks was lost: handled once.
	b.events(httptest.NewRecorder(), signed(body, time.Now(), skSecret))
	time.Sleep(100 * time.Millisecond)
	if n := len(f.posts()); n != 1 {
		t.Fatalf("%d posts after a redelivery, want 1", n)
	}
}

func TestSlackLinkFlow(t *testing.T) {
	f := newFakeSlack(t)
	tm := newCnFaultTeam()
	cnUsers(t, tm, "alice")
	b := skBridge(t, f, tm)
	ctx := context.Background()
	code := b.newCode("alice").Code
	if p := b.pendingFor("alice"); p == nil || p.Code != code {
		t.Fatalf("pending = %+v", p)
	}
	dm := func(text string) slackEnvelope {
		var e slackEnvelope
		e.Event.Type, e.Event.ChannelType, e.Event.User, e.Event.Channel, e.Event.Text = "message", "im", "U1", "D1", text
		return e
	}

	// A wrong code does not link; a mention is never the place to link.
	b.handleMessage(ctx, dm("link nope"))
	f.waitPost(t, "expired")
	var m slackEnvelope
	m.Event.Type, m.Event.User, m.Event.Channel, m.Event.TS, m.Event.Text = "app_mention", "U1", "C1", "1.1", "<@UBOT> link "+code
	b.handleMessage(ctx, m)
	if cn, err := tm.FindConnector(ctx, connectorSlack, "U1"); err == nil {
		t.Fatalf("linked from a channel: %+v", cn)
	}

	// The right code, in a direct message, links the Slack user and labels it with their name.
	b.handleMessage(ctx, dm("Link "+strings.ToUpper(code)))
	f.waitPost(t, "Linked")
	cn, err := tm.FindConnector(ctx, connectorSlack, "U1")
	if err != nil || cn.Login != "alice" || cn.Label != "Alice A" {
		t.Fatalf("connector = %+v, %v", cn, err)
	}
	// The code is single use.
	if _, ok := b.takeCode(code); ok {
		t.Fatal("code reusable")
	}
}

func TestSlackTextAndSplitting(t *testing.T) {
	got := slackText("# Title\nSome **bold** and a [link](https://x.io/a?b=1).")
	want := "*Title*\nSome *bold* and a <https://x.io/a?b=1|link>."
	if got != want {
		t.Fatalf("slackText = %q, want %q", got, want)
	}

	f := newFakeSlack(t)
	b := skBridge(t, f, newCnFaultTeam())
	long := strings.Repeat("é", 3500*2+5)
	blocks := []any{map[string]any{"type": "actions"}}
	if err := b.send(context.Background(), slackDest{channel: "C1", threadTS: "9.9"}, long, blocks); err != nil {
		t.Fatal(err)
	}
	posts := f.posts()
	if len(posts) != 3 {
		t.Fatalf("%d posts, want 3", len(posts))
	}
	for i, p := range posts {
		if _, has := p["blocks"]; has != (i == 2) || p["thread_ts"] != "9.9" {
			t.Fatalf("post %d: blocks=%v thread=%v", i, has, p["thread_ts"])
		}
	}
	// A bad token is an error carrying Slack's reason, not the token.
	bad := &Slack{Token: "nope", API: f.srv.URL}
	if err := bad.call(context.Background(), "chat.postMessage", map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "invalid_auth") || strings.Contains(err.Error(), "nope") {
		t.Fatalf("error = %v", err)
	}
}

func TestSlackButtonsOnlyForTheLinkedUser(t *testing.T) {
	f := newFakeSlack(t)
	tm := newCnFaultTeam()
	cnUsers(t, tm, "alice")
	b := skBridge(t, f, tm)
	ctx := context.Background()
	_ = tm.SetConnector(ctx, store.Connector{Login: "alice", Kind: connectorSlack, Account: "U1"})
	key := b.addAction(slackAction{login: "alice", thread: "t1", interrupt: "i1", approve: true})

	// Someone else pressing it is refused; an unknown key is invalid. Neither touches the thread.
	b.handleButton(ctx, "ap:"+key, "U2", "D2", "5.5")
	f.waitPost(t, "Not allowed")
	b.handleButton(ctx, "ap:unknown", "U1", "D1", "5.5")
	f.waitPost(t, "no longer valid")
	if _, ok := b.action(key); !ok {
		t.Fatal("a refused press consumed the button")
	}

	// The interactive endpoint verifies the signature and takes a form-encoded payload.
	form := url.Values{"payload": {`{"type":"block_actions","user":{"id":"U1"},"channel":{"id":"D1"},"message":{"ts":"5.5"},"actions":[{"value":"ap:unknown"}]}`}}.Encode()
	r := signed(form, time.Now(), skSecret)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	b.interactive(w, r)
	if w.Code != 200 {
		t.Fatalf("interactive = %d", w.Code)
	}
	w = httptest.NewRecorder()
	b.interactive(w, signed(form, time.Now(), "wrong"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned interactive = %d", w.Code)
	}
}
