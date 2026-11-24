package tapd_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/core/tapd"
)

type fake struct {
	mu       sync.Mutex
	srv      *httptest.Server
	stories  []string // JSON of each Story object
	ends     string   // data of last_steps
	endsFail bool
	calls    []string
	auth     []string
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{ends: `{"resolved":"done","rejected":"no"}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.URL.Path+"?"+r.URL.RawQuery)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		u, p, ok := r.BasicAuth()
		bearer := r.Header.Get("Authorization") == "Bearer tok"
		if !(ok && u == "api" && p == "pw") && !bearer {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"status":401,"data":[],"info":"auth"}`))
			return
		}
		if r.URL.Query().Get("workspace_id") == "" {
			_, _ = w.Write([]byte(`{"status":0,"data":[],"info":"workspace_id required"}`))
			return
		}
		switch r.URL.Path {
		case "/workflows/last_steps":
			if f.endsFail {
				_, _ = w.Write([]byte(`{"status":0,"data":[],"info":"no permission"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"status":1,"data":%s,"info":"success"}`, f.ends)
		case "/stories":
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if limit < 1 || limit > 200 || page < 1 {
				_, _ = w.Write([]byte(`{"status":0,"data":[],"info":"bad paging"}`))
				return
			}
			lo, hi := (page-1)*limit, page*limit
			if lo > len(f.stories) {
				lo = len(f.stories)
			}
			if hi > len(f.stories) {
				hi = len(f.stories)
			}
			var parts []string
			for _, s := range f.stories[lo:hi] {
				parts = append(parts, `{"Story":`+s+`}`)
			}
			_, _ = fmt.Fprintf(w, `{"status":1,"data":[%s],"info":"success"}`, strings.Join(parts, ","))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func story(id int, status string) string {
	return fmt.Sprintf(`{"id":"%d","name":"Story %d","status":%q,"description":"<p>Hello <b>%d</b></p>","priority_label":"High","owner":"o;","creator":"c"}`, id, id, status, id)
}

func client(f *fake) *tapd.Client {
	return &tapd.Client{HTTP: f.srv.Client(), Cfg: tapd.Config{BaseURL: f.srv.URL, WorkspaceID: "1010104801", User: "api", Password: "pw"}}
}

func TestOpenStoriesUsesWorkflowEndStatusesAndBasicAuth(t *testing.T) {
	f := newFake(t)
	f.stories = []string{story(1, "planning"), story(2, "resolved"), story(3, "developing"), story(4, "rejected")}
	res, err := client(f).OpenStories(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "workflow" || len(res.Stories) != 2 || res.Stories[0].ID != "1" || res.Stories[1].ID != "3" {
		t.Fatalf("result = %+v", res)
	}
	s := res.Stories[0]
	if s.Description != "Hello 1" || s.Priority != "High" || s.Creator != "c" ||
		!strings.HasSuffix(s.URL, "/1010104801/prong/stories/view/1") || !strings.HasPrefix(s.URL, f.srv.URL) {
		t.Fatalf("story = %+v", s)
	}
	if !strings.HasPrefix(f.auth[0], "Basic ") {
		t.Fatalf("auth = %q", f.auth[0])
	}
	if !strings.Contains(f.calls[0], "system=story") {
		t.Fatalf("first call = %s", f.calls[0])
	}
}

func TestBearerToken(t *testing.T) {
	f := newFake(t)
	f.stories = []string{story(1, "planning")}
	c := client(f)
	c.Cfg.User, c.Cfg.Password, c.Cfg.Token = "", "", "tok"
	res, err := c.OpenStories(context.Background(), 0)
	if err != nil || len(res.Stories) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestPagingAndCaps(t *testing.T) {
	f := newFake(t)
	for i := 1; i <= 650; i++ {
		f.stories = append(f.stories, story(i, "planning"))
	}
	res, err := client(f).OpenStories(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stories) != tapd.MaxImport || !res.Truncated {
		t.Fatalf("got %d truncated=%v", len(res.Stories), res.Truncated)
	}
	pages := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "/stories") {
			pages++
			if !strings.Contains(c, "limit=200") {
				t.Fatalf("call %s", c)
			}
		}
	}
	if pages != 3 {
		t.Fatalf("pages = %d, calls %v", pages, f.calls)
	}
	// A smaller cap stops early.
	res, err = client(f).OpenStories(context.Background(), 10)
	if err != nil || len(res.Stories) != 10 {
		t.Fatalf("%d %v", len(res.Stories), err)
	}
}

func TestUserFilterOverridesAndSkipsWorkflowEndpoint(t *testing.T) {
	f := newFake(t)
	f.endsFail = true
	f.stories = []string{story(1, "planning"), story(2, "Developing"), story(3, "resolved")}
	c := client(f)
	c.Cfg.OpenStatuses = "developing | status_3"
	res, err := c.OpenStories(context.Background(), 0)
	if err != nil || res.Source != "filter" || len(res.Stories) != 1 || res.Stories[0].ID != "2" {
		t.Fatalf("%+v %v", res, err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "/workflows") {
			t.Fatal("the workflow endpoint was called despite a user filter")
		}
	}
}

func TestWorkflowEndpointUnavailableWithoutFilter(t *testing.T) {
	f := newFake(t)
	f.endsFail = true
	_, err := client(f).OpenStories(context.Background(), 0)
	if !errors.Is(err, tapd.ErrNoStatusInfo) {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorEnvelopeAndAuth(t *testing.T) {
	f := newFake(t)
	c := client(f)
	c.Cfg.Password = "wrong"
	if _, err := c.OpenStories(context.Background(), 0); !errors.Is(err, tapd.ErrAuth) {
		t.Fatalf("err = %v", err)
	}
	c = client(f)
	c.Cfg.WorkspaceID = "abc"
	if _, err := c.OpenStories(context.Background(), 0); err == nil {
		t.Fatal("a non numeric workspace id was accepted")
	}
	// status != 1 on the stories call.
	c = client(f)
	c.Cfg.OpenStatuses = "planning"
	c.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) *http.Response {
		return &http.Response{StatusCode: 200, Body: nopBody(`{"status":0,"data":[],"info":"project not found"}`), Header: http.Header{}}
	})}
	_, err := c.OpenStories(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "project not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheck(t *testing.T) {
	f := newFake(t)
	if err := client(f).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := client(f)
	c.Cfg.User = "x"
	if err := c.Check(context.Background()); !errors.Is(err, tapd.ErrAuth) {
		t.Fatalf("err = %v", err)
	}
}

func TestEmptyEndStatusesAsArray(t *testing.T) {
	f := newFake(t)
	f.ends = `[]`
	f.stories = []string{story(1, "resolved")}
	res, err := client(f).OpenStories(context.Background(), 0)
	if err != nil || len(res.Stories) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestNumericIDsAndStoryURLForPrivateDeployment(t *testing.T) {
	f := newFake(t)
	f.stories = []string{`{"id":1010104801001234567,"name":"N","status":"planning"}`}
	res, err := client(f).OpenStories(context.Background(), 0)
	if err != nil || res.Stories[0].ID != "1010104801001234567" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := tapd.StoryURL("", "7", "9"); got != "https://www.tapd.cn/7/prong/stories/view/9" {
		t.Fatal(got)
	}
	if got := tapd.StoryURL("https://tapd.corp.example/api", "7", "9"); got != "https://tapd.corp.example/7/prong/stories/view/9" {
		t.Fatal(got)
	}
}

func TestHTMLToText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{"<p>One</p><p>Two &amp; three</p>", "One\nTwo & three"},
		{"a<br/>b<br>c", "a\nb\nc"},
		{"<ul><li>x</li><li>y</li></ul>", "- x\n- y"},
		{`<script>alert("x")</script>kept<style>p{}</style>`, "kept"},
		{`<SCRIPT type="a>b">evil()</SCRIPT>ok`, "ok"},
		{`<a href="javascript:alert(1)" onclick='x>y'>link</a>`, "link"},
		{"1 < 2 and 3 > 2", "1 < 2 and 3 > 2"},
		{"&lt;script&gt;alert(1)&lt;/script&gt;", "<script>alert(1)</script>"},
		{"<!-- hidden -->shown", "shown"},
		{"<p>a</p>\n\n\n\n<p>b</p>", "a\n\nb"},
		{"unterminated <div class=\"x", "unterminated <div class=\"x"},
		{"x&nbsp;y", "x y"},
	} {
		if got := tapd.HTMLToText(c.in); got != c.want {
			t.Errorf("HTMLToText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPriorityRank(t *testing.T) {
	for in, want := range map[string]int{"": 0, "Urgent": 4, "High": 3, "Middle": 2, "Low": 1, "Nice To Have": 1, "高": 3, "中": 2, "紧急": 4, "other": 0} {
		if got := tapd.PriorityRank(in); got != want {
			t.Errorf("PriorityRank(%q) = %d, want %d", in, got, want)
		}
	}
}

type roundTrip func(*http.Request) *http.Response

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }

type body struct{ *strings.Reader }

func (body) Close() error { return nil }

func nopBody(s string) body { return body{strings.NewReader(s)} }
