package isolation_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

func TestProjectThreadsAreVisibleToProjectMembersOnly(t *testing.T) {
	// Arrange: alice owns a project with a finished thread; bob has an account but is not a member.
	team := teamstore.NewMem()
	for _, login := range []string{"alice", "bob"} {
		if _, err := team.UpsertUser(context.Background(), store.User{Login: login, Name: login, Email: login + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	s := newStack(t, stackOpts{
		web:     httpapi.Options{Authenticate: signedAuthenticator},
		product: &product.Options{Team: team},
	})
	alice, bob := s.user("alice"), s.user("bob")
	project := alice.must("POST", "/api/projects", `{"name":"api","repoUrl":"https://github.com/x/api"}`, 201).json()["id"].(string)
	thread := alice.must("POST", "/api/threads", `{"title":"shared","projectId":"`+project+`"}`, 201).json()["id"].(string)
	run := alice.startRun(thread, "write alice")
	alice.complete(run)
	alice.waitIdle(thread)
	reads := []route{
		{"GET", "/api/threads/" + thread, ""},
		{"GET", "/api/runs/" + run, ""},
		{"GET", "/api/runs/" + run + "/events", ""},
		{"GET", "/api/threads/" + thread + "/files", ""},
		{"GET", "/api/threads/" + thread + "/files/download?path=/workspace/outputs/alice.txt", ""},
	}

	// Act: bob is not a member.
	before := make([]int, len(reads))
	for i, rt := range reads {
		before[i] = bob.do(rt.method, rt.path, rt.body).Status
	}
	bobWrite := bob.do("POST", "/api/threads/"+thread+"/runs", `{"message":"pwned"}`)
	bobProject := bob.do("GET", "/api/projects/"+project, "")
	bobProjectThreads := bob.do("GET", "/api/threads?project="+project, "")
	bobList := bob.threadIDs()

	// Assert
	for i, rt := range reads {
		if before[i] != 404 {
			t.Errorf("non-member bob: %s answered %d, want 404", rt, before[i])
		}
	}
	if bobWrite.Status != 404 || bobProject.Status != 404 || bobProjectThreads.Status != 404 {
		t.Fatalf("bob's write %d, project %d, project threads %d, want 404", bobWrite.Status, bobProject.Status, bobProjectThreads.Status)
	}
	if slices.Contains(bobList, thread) {
		t.Fatalf("the project thread is in bob's list: %v", bobList)
	}
	alice.must("GET", "/api/threads/"+thread, "", 200) // positive control: the owner reads it

	// Act: alice adds bob as a member.
	alice.must("PUT", "/api/projects/"+project+"/members/bob", `{"role":"member"}`, 204)

	// Assert: bob now reads it, including the file, and the thread was not changed by his earlier attempts.
	for _, rt := range reads {
		if got := bob.do(rt.method, rt.path, rt.body); got.Status != 200 {
			t.Errorf("member bob: %s answered %d, want 200", rt, got.Status)
		}
	}
	if got := bob.must("GET", "/api/threads/"+thread+"/files/download?path=/workspace/outputs/alice.txt", "", 200).Body; string(got) != "alice-secret" {
		t.Fatalf("bob's download = %q", got)
	}
	view := bob.must("GET", "/api/threads/"+thread, "", 200)
	if strings.Contains(string(view.Body), "pwned") {
		t.Fatalf("the refused write reached the thread: %s", view.Body)
	}
	if ids := bob.must("GET", "/api/threads?project="+project, "", 200).list(); len(ids) != 1 || ids[0]["id"] != thread {
		t.Fatalf("project threads for bob = %v", ids)
	}

	// Removing him closes the door again.
	alice.must("DELETE", "/api/projects/"+project+"/members/bob", "", 204)
	if got := bob.do("GET", "/api/threads/"+thread, "").Status; got != 404 {
		t.Fatalf("removed member bob answered %d, want 404", got)
	}
}
