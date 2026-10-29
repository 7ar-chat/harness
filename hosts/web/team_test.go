package httpserver_test

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	httpserver "github.com/GhaziBenDahmane/harness/hosts/web"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/teamstore"
)

// teamRig is a team-mode rig with alice (project owner), bob (invited member)
// and carol (outside the project).
type teamRig struct {
	*rig
	team    *teamstore.Mem
	project string
}

func newTeamRig(t *testing.T) *teamRig {
	t.Helper()
	tm := teamstore.NewMem()
	ctx := context.Background()
	for _, l := range []string{"alice", "bob", "carol"} {
		if _, err := tm.UpsertUser(ctx, store.User{Login: l, Name: strings.ToUpper(l[:1]) + l[1:], Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	r := newRig(t, rigOpts{team: true, web: httpserver.Options{Team: tm}})
	p := r.json("POST", "/api/projects", "alice", `{"name":"api","repoUrl":"https://github.com/x/api"}`, 201)
	id := p["id"].(string)
	r.json("PUT", "/api/projects/"+id+"/members/bob", "alice", `{"role":"member"}`, 204)
	return &teamRig{rig: r, team: tm, project: id}
}

func TestProjectThreadsAreSharedWithMembersOnly(t *testing.T) {
	r := newTeamRig(t)
	th := r.json("POST", "/api/threads", "alice", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)

	// bob (member) sees and reads it; carol (outsider) does not know it exists.
	r.json("GET", "/api/threads/"+th, "bob", "", 200)
	r.json("GET", "/api/threads/"+th, "carol", "", 404)
	r.json("POST", "/api/threads/"+th+"/runs", "carol", `{"message":"hi"}`, 404)
	r.json("GET", "/api/projects/"+r.project, "carol", "", 404)

	// Project lists: members see it, others get 404.
	if got := r.do("GET", "/api/threads?project="+r.project, "bob", "").StatusCode; got != 200 {
		t.Fatalf("bob list = %d", got)
	}
	if got := r.do("GET", "/api/threads?project="+r.project, "carol", "").StatusCode; got != 404 {
		t.Fatalf("carol list = %d", got)
	}
	// Personal listings do not leak project threads.
	if body := rawBody(r.do("GET", "/api/threads", "alice", "").Body); strings.Contains(body, th) {
		t.Fatalf("project thread in personal list: %s", body)
	}

	// A viewer reads but cannot write.
	r.json("PUT", "/api/projects/"+r.project+"/members/carol", "alice", `{"role":"viewer"}`, 204)
	r.json("GET", "/api/threads/"+th, "carol", "", 200)
	r.json("POST", "/api/threads/"+th+"/runs", "carol", `{"message":"hi"}`, 403)
	r.json("POST", "/api/threads", "carol", `{"projectId":"`+r.project+`"}`, 403)
}

func TestMessagesRecordWhoSentThem(t *testing.T) {
	r := newTeamRig(t)
	th := r.json("POST", "/api/threads", "alice", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)
	r.waitIdle(th, "alice")
	run1 := r.startRun("alice", th, "first from alice")
	finished(t, r.stream(run1, "alice"))
	r.waitIdle(th, "bob")
	run2 := r.startRun("bob", th, "second from bob")
	finished(t, r.stream(run2, "bob"))

	msgs := r.json("GET", "/api/threads/"+th, "alice", "", 200)["messages"].([]any)
	authors := map[string]string{}
	for _, m := range msgs {
		m := m.(map[string]any)
		if m["role"] != "user" {
			if m["author"] != nil {
				t.Fatalf("assistant message has an author: %v", m)
			}
			continue
		}
		a := m["author"].(map[string]any)
		authors[m["content"].(string)] = a["login"].(string) + "/" + a["name"].(string)
	}
	if authors["first from alice"] != "alice/Alice" || authors["second from bob"] != "bob/Bob" {
		t.Fatalf("authors = %v", authors)
	}

	// The audit log names the actors.
	resp := r.do("GET", "/api/projects/"+r.project+"/audit", "alice", "")
	body := rawBody(resp.Body)
	if !strings.Contains(body, `"actor":"bob"`) || !strings.Contains(body, "run.send") {
		t.Fatalf("audit = %s", body)
	}
}

func TestOnlyOwnersManageMembers(t *testing.T) {
	r := newTeamRig(t)
	r.json("PUT", "/api/projects/"+r.project+"/members/carol", "bob", `{"role":"member"}`, 403)
	r.json("POST", "/api/projects/"+r.project+"/invites", "bob", `{"email":"x@y.io"}`, 403)
	r.json("DELETE", "/api/projects/"+r.project, "bob", "", 403)
	// The last owner cannot leave.
	r.json("DELETE", "/api/projects/"+r.project+"/members/alice", "alice", "", 409)
	// A member may leave.
	if got := r.do("DELETE", "/api/projects/"+r.project+"/members/bob", "bob", "").StatusCode; got != 204 {
		t.Fatalf("leave = %d", got)
	}
	r.json("GET", "/api/projects/"+r.project, "bob", "", 404)
}

func TestFeedShowsRunsToMembersOnly(t *testing.T) {
	r := newTeamRig(t)
	th := r.json("POST", "/api/threads", "alice", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)
	bobFeed := r.do("GET", "/api/events", "bob", "")
	carolFeed := r.do("GET", "/api/events", "carol", "")
	got := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(bobFeed.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				got <- sc.Text()
			}
		}
	}()
	carol := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(carolFeed.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				carol <- sc.Text()
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	run := r.startRun("alice", th, "go")
	finished(t, r.stream(run, "alice"))
	var started, done bool
	timeout := time.After(5 * time.Second)
	for !(started && done) {
		select {
		case l := <-got:
			started = started || strings.Contains(l, `"run.started"`)
			done = done || strings.Contains(l, `"run.finished"`)
		case <-timeout:
			t.Fatalf("bob's feed: started=%v finished=%v", started, done)
		}
	}
	select {
	case l := <-carol:
		t.Fatalf("an outsider saw %s", l)
	case <-time.After(200 * time.Millisecond):
	}
}

func rawBody(resp io.Reader) string {
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}
