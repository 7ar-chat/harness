package product

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/projdatastore"
	"github.com/7ar-chat/harness/adapters/store/reqstore"
	"github.com/7ar-chat/harness/core/requirements"
)

type fakeTapd struct {
	srv     *httptest.Server
	mu      sync.Mutex
	stories []string
	ends    string
	noEnds  bool
	down    bool
	auths   []string
	calls   int
}

func newFakeTapd(t *testing.T) *fakeTapd {
	t.Helper()
	f := &fakeTapd{ends: `{"resolved":"done","rejected":"no"}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		if u, p, ok := r.BasicAuth(); !ok || u != "api_user" || p != "api_secret_pw" {
			if r.Header.Get("Authorization") != "Bearer tok_secret_value" {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"status":401,"data":[],"info":"unauthorized"}`))
				return
			}
		}
		switch r.URL.Path {
		case "/workflows/last_steps":
			if f.noEnds {
				_, _ = w.Write([]byte(`{"status":0,"data":[],"info":"no permission"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"status":1,"data":%s,"info":"success"}`, f.ends)
		case "/stories":
			if f.down {
				_, _ = w.Write([]byte(`{"status":0,"data":[],"info":"workspace not found"}`))
				return
			}
			page := r.URL.Query().Get("page")
			if page != "" && page != "1" {
				_, _ = w.Write([]byte(`{"status":1,"data":[],"info":"success"}`))
				return
			}
			var parts []string
			for _, s := range f.stories {
				parts = append(parts, `{"Story":`+s+`}`)
			}
			_, _ = fmt.Fprintf(w, `{"status":1,"data":[%s],"info":"success"}`, strings.Join(parts, ","))
		}
	}))
	t.Cleanup(f.srv.Close)
	f.stories = []string{
		`{"id":"1010104801001","name":"Login page","status":"planning","priority_label":"High","owner":"ann;","creator":"bob","description":"<p>Users <b>log in</b></p><script>alert(1)</script>"}`,
		`{"id":"1010104801002","name":"Old thing","status":"resolved"}`,
		`{"id":"1010104801003","name":"Export","status":"developing","priority_label":"Low"}`,
	}
	return f
}

type tapdEnv struct {
	*rig
	pd      *projdatastore.Mem
	reqs    *reqstore.Mem
	project string
}

func newTapdEnv(t *testing.T, allowPrivate bool) *tapdEnv {
	t.Helper()
	e := &tapdEnv{pd: projdatastore.NewMem(), reqs: reqstore.NewMem(nil)}
	tm := newCnFaultTeam()
	cnUsers(t, tm, "alice", "bob", "carol", "dave")
	prod := Options{Team: tm, Requirements: e.reqs, SecretKey: cnKey, ProjectData: &ProjectDataOptions{Store: e.pd, NoJob: true, TapdAllowPrivate: allowPrivate}}
	e.rig = newRig(t, rigOpts{team: true, prod: prod, mod: func(ho *harness.Options) { ho.Features = append(harness.ProfileMinimal(), RequirementsFeature(e.reqs)) }})
	e.project = e.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	e.json("PUT", "/api/projects/"+e.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	e.json("PUT", "/api/projects/"+e.project+"/members/carol", "alice", `{"role":"viewer"}`, 204)
	return e
}

func (e *tapdEnv) path(rest string) string {
	return "/api/projects/" + e.project + "/requirements" + rest
}

func tapdConnectBody(f *fakeTapd, extra string) string {
	return fmt.Sprintf(`{"workspaceId":"1010104801","baseUrl":%q,"user":"api_user","password":"api_secret_pw"%s}`, f.srv.URL, extra)
}

func TestConnectTapdIsOwnerOnlyAndNeverShowsOrLogsTheCredentials(t *testing.T) {
	f := newFakeTapd(t)
	e := newTapdEnv(t, true)
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	e.json("PUT", e.path("/tapd"), "bob", tapdConnectBody(f, ""), 403)
	e.json("PUT", e.path("/tapd"), "carol", tapdConnectBody(f, ""), 403)
	e.json("PUT", e.path("/tapd"), "dave", tapdConnectBody(f, ""), 404)
	st := e.json("PUT", e.path("/tapd"), "alice", tapdConnectBody(f, `,"openStatuses":""`), 200)
	if st["connected"] != true || st["workspaceId"] != "1010104801" || st["user"] != "api_user" || st["hasPassword"] != true {
		t.Fatalf("status = %v", st)
	}
	if !strings.HasPrefix(f.auths[0], "Basic ") {
		t.Fatalf("TAPD was called with %q", f.auths[0])
	}
	for _, c := range []struct{ user string }{{"alice"}, {"bob"}, {"carol"}} {
		resp := e.do("GET", e.path("/tapd"), c.user, "")
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || strings.Contains(string(b), "api_secret_pw") || strings.Contains(string(b), "sealed") {
			t.Fatalf("GET as %s: %d %s", c.user, resp.StatusCode, b)
		}
	}
	sealed, err := e.pd.GetSecret(context.Background(), e.project, "tapd")
	if err != nil || bytes.Contains(sealed, []byte("api_secret_pw")) || bytes.Contains(sealed, []byte("api_user")) {
		t.Fatalf("the stored credentials are not sealed: %v", err)
	}
	// A blank password on save keeps the stored one; a changed account without one is refused by TAPD's check.
	e.json("PUT", e.path("/tapd"), "alice", fmt.Sprintf(`{"workspaceId":"1010104801","baseUrl":%q,"user":"api_user","openStatuses":"planning"}`, f.srv.URL), 200)
	e.json("PUT", e.path("/tapd"), "alice", fmt.Sprintf(`{"workspaceId":"1010104801","baseUrl":%q,"user":"other"}`, f.srv.URL), 400)
	if strings.Contains(logs.String(), "api_secret_pw") || strings.Contains(logs.String(), "tok_secret_value") {
		t.Fatalf("a credential reached the log: %s", logs.String())
	}
	e.json("DELETE", e.path("/tapd"), "bob", "", 403)
	e.do("DELETE", e.path("/tapd"), "alice", "")
	if st := e.json("GET", e.path("/tapd"), "alice", "", 200); st["connected"] == true {
		t.Fatal("still connected after disconnect")
	}
	e.json("POST", e.path("/import/tapd"), "bob", "", 409)
}

func TestImportTapdMapsOpenStoriesAndDedupes(t *testing.T) {
	f := newFakeTapd(t)
	e := newTapdEnv(t, true)
	e.json("PUT", e.path("/tapd"), "alice", tapdConnectBody(f, ""), 200)
	e.json("POST", e.path("/import/tapd"), "carol", "", 403)

	out := e.json("POST", e.path("/import/tapd"), "bob", "", 200)
	if out["imported"].(float64) != 2 || out["skipped"].(float64) != 0 || out["source"] != "workflow" {
		t.Fatalf("import = %v", out)
	}
	reqs := out["requirements"].([]any)
	var login map[string]any
	for _, r := range reqs {
		if m := r.(map[string]any); m["title"] == "Login page" {
			login = m
		}
	}
	if login == nil {
		t.Fatalf("requirements = %v", reqs)
	}
	body := login["body"].(string)
	if !strings.Contains(body, "Users log in") || strings.Contains(body, "<") || strings.Contains(body, "alert") ||
		!strings.Contains(body, "Owner: ann") || !strings.Contains(body, "Creator: bob") {
		t.Fatalf("body = %q", body)
	}
	src := login["source"].(map[string]any)
	if src["kind"] != "tapd_story" || src["id"] != "1010104801001" || src["repo"] != "1010104801" ||
		src["url"] != f.srv.URL+"/1010104801/prong/stories/view/1010104801001" {
		t.Fatalf("source = %v", src)
	}
	if login["status"] != "backlog" || login["priority"].(float64) != 3 || login["author"] != "bob" {
		t.Fatalf("requirement = %v", login)
	}
	labels, _ := json.Marshal(login["labels"])
	if !strings.Contains(string(labels), "tapd") || !strings.Contains(string(labels), "priority:High") {
		t.Fatalf("labels = %s", labels)
	}
	// Again: nothing new.
	out = e.json("POST", e.path("/import/tapd"), "bob", "", 200)
	if out["imported"].(float64) != 0 || out["skipped"].(float64) != 2 {
		t.Fatalf("second import = %v", out)
	}
	list, _ := e.reqs.List(context.Background(), requirements.Filter{ProjectID: e.project})
	if len(list) != 2 {
		t.Fatalf("%d requirements", len(list))
	}
	if k := list[0].Source.Key(); !strings.HasPrefix(k, "tapd:1010104801:10101048010") {
		t.Fatalf("key = %q", k)
	}
}

func TestImportTapdFallsBackToTheUsersStatusList(t *testing.T) {
	f := newFakeTapd(t)
	f.noEnds = true
	e := newTapdEnv(t, true)
	// Saving works with the filter (it never asks for the end statuses) ...
	e.json("PUT", e.path("/tapd"), "alice", tapdConnectBody(f, `,"openStatuses":"developing, status_2"`), 200)
	out := e.json("POST", e.path("/import/tapd"), "bob", "", 200)
	if out["source"] != "filter" || out["imported"].(float64) != 1 || out["requirements"].([]any)[0].(map[string]any)["title"] != "Export" {
		t.Fatalf("import = %v", out)
	}
	// ... without it the import says what to do.
	e.json("PUT", e.path("/tapd"), "alice", tapdConnectBody(f, ""), 200)
	resp := e.do("POST", e.path("/import/tapd"), "bob", "")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 409 || !strings.Contains(string(b), "tapd_no_statuses") {
		t.Fatalf("import without statuses: %d %s", resp.StatusCode, b)
	}
}

func TestTapdErrorsAreShortAndSecretFree(t *testing.T) {
	f := newFakeTapd(t)
	e := newTapdEnv(t, true)
	bad := fmt.Sprintf(`{"workspaceId":"1010104801","baseUrl":%q,"user":"api_user","password":"nope-nope"}`, f.srv.URL)
	resp := e.do("PUT", e.path("/tapd"), "alice", bad)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "tapd_refused") || strings.Contains(string(b), "nope-nope") {
		t.Fatalf("bad password: %d %s", resp.StatusCode, b)
	}
	e.json("PUT", e.path("/tapd"), "alice", `{"workspaceId":"abc","user":"u","password":"p"}`, 400)
	e.json("PUT", e.path("/tapd"), "alice", `{"workspaceId":"1","user":"u"}`, 400)
	e.json("PUT", e.path("/tapd"), "alice", `{"workspaceId":"1","baseUrl":"ftp://x","user":"u","password":"p"}`, 400)
	// A token works too, and replaces the account.
	st := e.json("PUT", e.path("/tapd"), "alice", fmt.Sprintf(`{"workspaceId":"1010104801","baseUrl":%q,"token":"tok_secret_value"}`, f.srv.URL), 200)
	if st["hasToken"] != true || st["hasPassword"] == true {
		t.Fatalf("status = %v", st)
	}
	if last := f.auths[len(f.auths)-1]; last != "Bearer tok_secret_value" {
		t.Fatalf("auth = %q", last)
	}
	// An error envelope (status != 1) after saving.
	f.mu.Lock()
	f.down = true
	f.mu.Unlock()
	resp = e.do("POST", e.path("/import/tapd"), "bob", "")
	b, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 502 || !strings.Contains(string(b), "workspace not found") || strings.Contains(string(b), f.srv.URL) {
		t.Fatalf("envelope error: %d %s", resp.StatusCode, b)
	}
}

func TestTapdBaseURLIsGuardedAgainstPrivateAddresses(t *testing.T) {
	f := newFakeTapd(t) // on loopback
	e := newTapdEnv(t, false)
	before := f.calls
	for _, base := range []string{f.srv.URL, "http://127.0.0.1:1", "https://127.0.0.1", "https://169.254.169.254", "https://10.0.0.5", "https://localhost", "https://[::1]", "https://metadata.internal", "https://u:p@example.com", "https://example.com?x=1"} {
		resp := e.do("PUT", e.path("/tapd"), "alice", fmt.Sprintf(`{"workspaceId":"1","baseUrl":%q,"user":"u","password":"p"}`, base))
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d %s", base, resp.StatusCode, b)
		}
	}
	if f.calls != before {
		t.Fatal("the server connected to a refused address")
	}
	// A hostname that points at loopback passes the syntax check and is refused when connecting.
	_, port, _ := strings.Cut(strings.TrimPrefix(f.srv.URL, "http://"), ":")
	resp := e.do("PUT", e.path("/tapd"), "alice", fmt.Sprintf(`{"workspaceId":"1","baseUrl":"https://127.0.0.1.nip.invalid:%s","user":"u","password":"p"}`, port))
	if resp.StatusCode == 200 || f.calls != before {
		t.Fatalf("status %d, calls %d", resp.StatusCode, f.calls)
	}
}
