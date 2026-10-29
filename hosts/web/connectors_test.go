package httpserver_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	httpserver "github.com/GhaziBenDahmane/harness/hosts/web"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/teamstore"
)

func newPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func (r *rig) raw(method, path, user string, body []byte, ctype string) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("X-User", user)
	req.Header.Set("Content-Type", ctype)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestProfileDisplayNameAndAvatar(t *testing.T) {
	r := newTeamRig(t)
	me := r.json("GET", "/api/me", "alice", "", 200)["user"].(map[string]any)
	if me["name"] != "Alice" || me["displayName"] != "" || me["hasCustomAvatar"] != false {
		t.Fatalf("me = %v", me)
	}
	// A chosen display name replaces the default everywhere; resetting brings the default back.
	u := r.json("PATCH", "/api/me/profile", "alice", `{"displayName":"  Alice   Liddell "}`, 200)
	if u["name"] != "Alice Liddell" || u["displayName"] != "Alice Liddell" || u["defaultName"] != "Alice" {
		t.Fatalf("after rename = %v", u)
	}
	r.json("PATCH", "/api/me/profile", "alice", `{"displayName":"`+strings.Repeat("x", 81)+`"}`, 400)
	members := rawBody(r.do("GET", "/api/projects/"+r.project+"/members", "bob", "").Body)
	if !strings.Contains(members, "Alice Liddell") {
		t.Fatalf("members = %s", members)
	}
	if u = r.json("PATCH", "/api/me/profile", "alice", `{"displayName":""}`, 200); u["name"] != "Alice" {
		t.Fatalf("after reset = %v", u)
	}

	// Avatar: only real images are accepted, whatever the declared type.
	img := newPNG(t)
	if got := r.raw("PUT", "/api/me/avatar", "alice", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "image/png").StatusCode; got != 415 {
		t.Fatalf("svg accepted: %d", got)
	}
	if got := r.raw("PUT", "/api/me/avatar", "alice", append(img, make([]byte, 600<<10)...), "image/png").StatusCode; got != 413 {
		t.Fatalf("huge avatar: %d", got)
	}
	resp := r.raw("PUT", "/api/me/avatar", "alice", img, "text/plain")
	var up map[string]any
	json.NewDecoder(resp.Body).Decode(&up)
	if resp.StatusCode != 200 || up["hasCustomAvatar"] != true || !strings.HasPrefix(up["avatarUrl"].(string), "/api/users/alice/avatar?v=") {
		t.Fatalf("upload = %d %v", resp.StatusCode, up)
	}
	got := r.do("GET", up["avatarUrl"].(string), "bob", "")
	body, _ := io.ReadAll(got.Body)
	if got.StatusCode != 200 || got.Header.Get("Content-Type") != "image/png" || !bytes.Equal(body, img) || got.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("avatar served: %d %q", got.StatusCode, got.Header.Get("Content-Type"))
	}
	if members = rawBody(r.do("GET", "/api/projects/"+r.project+"/members", "bob", "").Body); !strings.Contains(members, "/api/users/alice/avatar?v=") {
		t.Fatalf("members lack the avatar: %s", members)
	}
	if u = r.json("DELETE", "/api/me/avatar", "alice", "", 200); u["hasCustomAvatar"] != false {
		t.Fatalf("after delete = %v", u)
	}
	r.json("GET", "/api/users/alice/avatar", "bob", "", 404)
	r.json("GET", "/api/users/alice/avatar", "", "", 401)
}

// ---- GitHub ----

type fakeGitHub struct {
	agents string // the repository's AGENTS.md ("" for none)
	srv    *httptest.Server
	repos  map[int][]map[string]any // installation id → repositories
	users  map[string]string        // access token → github login
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{repos: map[int][]map[string]any{}, users: map[string]string{"tok-alice": "AliceGH", "tok-bob": "bobgh"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("client_secret") != "csecret" {
			json.NewEncoder(w).Encode(map[string]any{"error": "bad_client"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + r.Form.Get("code"), "token_type": "bearer"})
	})
	user := func(r *http.Request) string {
		return f.users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	}
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if u := user(r); u != "" {
			json.NewEncoder(w).Encode(map[string]any{"login": u})
			return
		}
		http.Error(w, "no", 401)
	})
	mux.HandleFunc("/user/installations", func(w http.ResponseWriter, r *http.Request) {
		var ins []map[string]any
		if user(r) == "AliceGH" {
			ins = append(ins, map[string]any{"id": 11})
		}
		json.NewEncoder(w).Encode(map[string]any{"installations": ins})
	})
	mux.HandleFunc("/app/installations/11/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_inst", "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("/repos/acme/web/contents/AGENTS.md", func(w http.ResponseWriter, r *http.Request) {
		if f.agents == "" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(f.agents))
	})
	mux.HandleFunc("/user/installations/11/repositories", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"repositories": f.repos[11]})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.repos[11] = []map[string]any{
		{"full_name": "acme/api", "private": true, "description": "The API", "default_branch": "main"},
		{"full_name": "acme/web", "private": false, "description": "Website", "default_branch": "trunk"},
	}
	return f
}

func githubRig(t *testing.T) (*rig, *fakeGitHub, *teamstore.Mem) {
	t.Helper()
	gh := newFakeGitHub(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	app, err := httpserver.NewGitHubApp("1", "my-app", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	app.API, app.WebBase, app.ClientID, app.ClientSecret = gh.srv.URL, gh.srv.URL, "cid", "csecret"
	tm := teamstore.NewMem()
	for _, l := range []string{"alice", "bob"} {
		tm.UpsertUser(context.Background(), store.User{Login: l})
	}
	r := newRig(t, rigOpts{team: true, web: httpserver.Options{Team: tm, GitHub: app, SecretKey: []byte("0123456789abcdef0123456789abcdef"), PublicURL: "https://harness.test"}})
	return r, gh, tm
}

// connectGitHub plays the OAuth round trip for user with the given code.
func connectGitHub(t *testing.T, r *rig, user, code string) string {
	t.Helper()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", r.srv.URL+"/api/connectors/github/connect", nil)
	req.Header.Set("X-User", user)
	resp, err := noFollow.Do(req)
	if err != nil || resp.StatusCode != 302 {
		t.Fatalf("connect: %v %v", resp, err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Path != "/login/oauth/authorize" || loc.Query().Get("client_id") != "cid" || loc.Query().Get("redirect_uri") != "https://harness.test/auth/github/callback" {
		t.Fatalf("authorize redirect = %s", loc)
	}
	cb, _ := http.NewRequest("GET", r.srv.URL+"/auth/github/callback?code="+code+"&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	cb.Header.Set("X-User", user)
	back, err := noFollow.Do(cb)
	if err != nil || back.StatusCode != 302 {
		t.Fatalf("callback: %v %v", back, err)
	}
	return back.Header.Get("Location")
}

func TestGitHubConnectorImportsProjects(t *testing.T) {
	r, _, tm := githubRig(t)
	if got := connectGitHub(t, r, "alice", "alice"); got != "/#/profile?github=connected" {
		t.Fatalf("callback redirect = %s", got)
	}
	conns := r.do("GET", "/api/connectors", "alice", "")
	body := rawBody(conns.Body)
	if !strings.Contains(body, `"account":"alicegh"`) || !strings.Contains(body, `"label":"@AliceGH"`) {
		t.Fatalf("connectors = %s", body)
	}
	// The token is not stored in clear.
	cn, _ := tm.GetConnector(context.Background(), "alice", "github")
	if bytes.Contains(cn.Data, []byte("tok-alice")) {
		t.Fatal("the GitHub token is stored in clear")
	}

	repos := r.json("GET", "/api/connectors/github/repos?q=api", "alice", "", 200)
	list := repos["repos"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["fullName"] != "acme/api" || list[0].(map[string]any)["installationId"] != float64(11) || repos["installUrl"] == nil {
		t.Fatalf("repos = %v", repos)
	}
	p := r.json("POST", "/api/projects/import", "alice", `{"repo":"acme/web"}`, 201)
	if p["name"] != "web" || p["repoUrl"] != "https://github.com/acme/web" || p["defaultBranch"] != "trunk" || p["role"] != "owner" {
		t.Fatalf("imported = %v", p)
	}
	if proj, _ := tm.GetProject(context.Background(), p["id"].(string)); proj.GitHubInstallation != 11 {
		t.Fatalf("installation = %d: the import must connect the project to the App", proj.GitHubInstallation)
	}
	r.json("POST", "/api/projects/import", "alice", `{"repo":"acme/web"}`, 409)
	if all := rawBody(r.do("GET", "/api/connectors/github/repos", "alice", "").Body); !strings.Contains(all, `"imported":"`+p["id"].(string)+`"`) {
		t.Fatalf("imported flag missing: %s", all)
	}
	// A repository the user cannot reach through the App cannot be imported, whatever the request says.
	r.json("POST", "/api/projects/import", "alice", `{"repo":"acme/secret"}`, 404)
	if got := r.do("POST", "/api/projects/import", "bob", `{"repo":"acme/api"}`).StatusCode; got != 409 { // bob is not connected
		t.Fatalf("bob (not connected) = %d", got)
	}

	if got := r.do("DELETE", "/api/connectors/github", "alice", "").StatusCode; got != 204 {
		t.Fatalf("disconnect = %d", got)
	}
	r.json("GET", "/api/connectors/github/repos", "alice", "", 409)
}

func TestGitHubCallbackRejectsForgedStateAndSharedAccounts(t *testing.T) {
	r, _, _ := githubRig(t)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, state := range []string{"", "garbage", "AAAA"} {
		req, _ := http.NewRequest("GET", r.srv.URL+"/auth/github/callback?code=alice&state="+state, nil)
		req.Header.Set("X-User", "alice")
		resp, _ := noFollow.Do(req)
		if loc := resp.Header.Get("Location"); loc != "/#/profile?github=invalid_state" {
			t.Fatalf("state %q: %s", state, loc)
		}
	}
	// A state minted for alice does not work for bob.
	connectReq, _ := http.NewRequest("GET", r.srv.URL+"/api/connectors/github/connect", nil)
	connectReq.Header.Set("X-User", "alice")
	resp, _ := noFollow.Do(connectReq)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	req, _ := http.NewRequest("GET", r.srv.URL+"/auth/github/callback?code=alice&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	req.Header.Set("X-User", "bob")
	resp, _ = noFollow.Do(req)
	if got := resp.Header.Get("Location"); got != "/#/profile?github=invalid_state" {
		t.Fatalf("alice's state used by bob: %s", got)
	}
	// One GitHub account cannot be linked to two users.
	connectGitHub(t, r, "alice", "alice")
	if got := connectGitHub(t, r, "bob", "alice"); got != "/#/profile?github=account_in_use" {
		t.Fatalf("shared account: %s", got)
	}
}

// ---- Telegram ----

type fakeTG struct {
	srv     *httptest.Server
	mu      sync.Mutex
	queue   []map[string]any
	nextID  int
	sent    []map[string]any
	answers []map[string]any
}

func newFakeTG(t *testing.T) *fakeTG {
	f := &fakeTG{nextID: 1}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v}) }
		switch method {
		case "getMe":
			ok(map[string]any{"username": "harness_bot"})
		case "getUpdates":
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				f.mu.Lock()
				if len(f.queue) > 0 {
					q := f.queue
					f.queue = nil
					f.mu.Unlock()
					ok(q)
					return
				}
				f.mu.Unlock()
				time.Sleep(10 * time.Millisecond)
			}
			ok([]any{})
		case "sendMessage":
			f.mu.Lock()
			f.sent = append(f.sent, in)
			f.mu.Unlock()
			ok(map[string]any{"message_id": 1})
		case "answerCallbackQuery":
			f.mu.Lock()
			f.answers = append(f.answers, in)
			f.mu.Unlock()
			ok(true)
		default:
			ok(true)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTG) push(u map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u["update_id"] = f.nextID
	f.nextID++
	f.queue = append(f.queue, u)
}

func (f *fakeTG) say(chat int, text string) {
	f.push(map[string]any{"message": map[string]any{"message_id": 1, "text": text,
		"chat": map[string]any{"id": chat, "type": "private"}, "from": map[string]any{"username": "alice_tg", "first_name": "Alice"}}})
}

func (f *fakeTG) waitSent(t *testing.T, chat int, contains string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, m := range f.sent {
			if int(m["chat_id"].(float64)) == chat && strings.Contains(m["text"].(string), contains) {
				f.mu.Unlock()
				return m
			}
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("no message to chat %d containing %q; sent: %v", chat, contains, f.sent)
	return nil
}

func tgRig(t *testing.T, next func() (string, string)) (*teamRig, *fakeTG) {
	t.Helper()
	tg := newFakeTG(t)
	tm := teamstore.NewMem()
	ctx := context.Background()
	for _, l := range []string{"alice", "bob"} {
		tm.UpsertUser(ctx, store.User{Login: l})
	}
	r := newRig(t, rigOpts{team: true, model: supModel(next), sup: httpserver.NewSupervisor(tm), web: httpserver.Options{Team: tm,
		SecretKey: []byte("0123456789abcdef0123456789abcdef"),
		Telegram:  &httpserver.Telegram{Token: "123:SECRET", Username: "harness_bot", API: tg.srv.URL, PollSeconds: 1}}})
	return &teamRig{rig: r, team: tm}, tg
}

func linkTelegram(t *testing.T, r *rig, tg *fakeTG, user string, chat int) {
	t.Helper()
	link := r.json("POST", "/api/connectors/telegram/link", user, `{}`, 200)
	if !strings.HasPrefix(link["url"].(string), "https://t.me/harness_bot?start=") {
		t.Fatalf("link = %v", link)
	}
	tg.say(chat, "/start "+link["code"].(string))
	tg.waitSent(t, chat, "Linked")
}

func TestTelegramLinkingIsOneTimeAndPerAccount(t *testing.T) {
	r, tg := tgRig(t, func() (string, string) { return "list_sessions", `{}` })
	pend := rawBody(r.do("GET", "/api/connectors", "alice", "").Body)
	if !strings.Contains(pend, `"kind":"telegram","configured":true,"connected":false`) {
		t.Fatalf("connectors = %s", pend)
	}
	link := r.json("POST", "/api/connectors/telegram/link", "alice", `{}`, 200)
	if body := rawBody(r.do("GET", "/api/connectors", "alice", "").Body); !strings.Contains(body, link["code"].(string)) {
		t.Fatalf("pending link not shown: %s", body)
	}
	// An unknown chat gets told to link; a wrong code does not link.
	tg.say(222, "hello?")
	tg.waitSent(t, 222, "not linked")
	tg.say(222, "/start nonsense")
	tg.waitSent(t, 222, "expired")

	tg.say(111, "/start "+link["code"].(string))
	tg.waitSent(t, 111, "Linked")
	body := rawBody(r.do("GET", "/api/connectors", "alice", "").Body)
	if !strings.Contains(body, `"connected":true`) || !strings.Contains(body, "@alice_tg") {
		t.Fatalf("after link = %s", body)
	}
	// The code was single use.
	tg.say(333, "/start "+link["code"].(string))
	tg.waitSent(t, 333, "expired")
	if got := r.do("POST", "/api/connectors/telegram/test", "alice", "").StatusCode; got != 204 {
		t.Fatalf("test message = %d", got)
	}
	tg.waitSent(t, 111, "Hello from your assistant")
	// One chat belongs to one account.
	l2 := r.json("POST", "/api/connectors/telegram/link", "bob", `{}`, 200)
	tg.say(111, "/start "+l2["code"].(string))
	tg.waitSent(t, 111, "already linked")
	r.json("POST", "/api/connectors/telegram/test", "bob", `{}`, 409)

	if got := r.do("DELETE", "/api/connectors/telegram", "alice", "").StatusCode; got != 204 {
		t.Fatalf("disconnect = %d", got)
	}
	tg.say(111, "still there?")
	tg.waitSent(t, 111, "not linked")
}

func TestTelegramTalksToTheAssistantAndAsksForApprovals(t *testing.T) {
	var target string
	r, tg := tgRig(t, func() (string, string) {
		return "session_send", `{"thread_id":"` + target + `","message":"please add tests"}`
	})
	p := r.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	target = r.json("POST", "/api/threads", "alice", `{"projectId":"`+p+`"}`, 201)["id"].(string)
	linkTelegram(t, r.rig, tg, "alice", 111)

	// A message in the chat reaches the assistant, which wants to act: the approval comes with buttons.
	tg.say(111, "tell the session to add tests")
	m := tg.waitSent(t, 111, "session_send")
	if m["reply_markup"] == nil {
		t.Fatalf("approval message without buttons: %v", m)
	}
	kb := m["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)
	if len(kb) != 2 || kb[0].(map[string]any)["text"] != "Approve" || kb[1].(map[string]any)["text"] != "Deny" {
		t.Fatalf("buttons = %v", kb)
	}
	// The assistant thread shows the message as typed by alice.
	sup := supervisorThread(r.rig, "alice")
	msgs := r.json("GET", "/api/threads/"+sup, "alice", "", 200)["messages"].([]any)
	if a := msgs[0].(map[string]any)["author"].(map[string]any); a["login"] != "alice" {
		t.Fatalf("first message author = %v", a)
	}

	// Another chat cannot press alice's button.
	approve := kb[0].(map[string]any)["callback_data"].(string)
	tg.push(map[string]any{"callback_query": map[string]any{"id": "cb0", "data": approve,
		"message": map[string]any{"message_id": 5, "chat": map[string]any{"id": 999}}}})
	// Alice's chat approves.
	tg.push(map[string]any{"callback_query": map[string]any{"id": "cb1", "data": approve,
		"message": map[string]any{"message_id": 5, "chat": map[string]any{"id": 111}}}})
	r.waitIdle(target, "alice")
	deadline := time.Now().Add(10 * time.Second)
	var found bool
	for time.Now().Before(deadline) && !found {
		list, _ := r.json("GET", "/api/threads/"+target, "alice", "", 200)["messages"].([]any)
		for _, mm := range list {
			mm := mm.(map[string]any)
			if mm["role"] == "user" && strings.Contains(mm["content"].(string), "add tests") {
				a := mm["author"].(map[string]any)
				found = a["kind"] == "supervisor" && a["onBehalfOf"] == "alice"
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatal("the approved action did not reach the session")
	}
	// The assistant's final answer is delivered to the chat.
	tg.waitSent(t, 111, "done")
	tg.mu.Lock()
	defer tg.mu.Unlock()
	if len(tg.answers) < 2 || !strings.Contains(tg.answers[0]["text"].(string), "Not allowed") || tg.answers[1]["text"] != "Approved" {
		t.Fatalf("callback answers = %v", tg.answers)
	}
}
