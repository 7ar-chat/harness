package isolation_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/hosts/httpapi"
)

var signingKey = []byte("isolation-test-signing-key-0001")

// signedToken is "<user>.<hex hmac(user)>": a credential only the holder of the key can mint.
func signedToken(key []byte, user string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(user))
	return user + "." + hex.EncodeToString(mac.Sum(nil))
}

// signedAuthenticator maps "Authorization: Bearer <user>.<sig>" to the user, and refuses anything else.
func signedAuthenticator(r *http.Request) (string, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	user, _, found := strings.Cut(tok, ".")
	if !ok || !found || user == "" || !hmac.Equal([]byte(tok), []byte(signedToken(signingKey, user))) {
		return "", http.ErrNoCookie
	}
	return user, nil
}

func (s *stack) user(name string) *actor { return s.as(bearer(signedToken(signingKey, name))) }

func newSignedStack(t *testing.T, holds ...string) *stack {
	t.Helper()
	return newStack(t, stackOpts{web: httpapi.Options{Authenticate: signedAuthenticator}, holds: holds})
}

func TestEveryRouteAnswersNotFoundForAnotherUsersThreadsAndRuns(t *testing.T) {
	// Arrange: alice has a finished thread with a file, and a second thread whose run is still running.
	s := newSignedStack(t, "h1")
	alice, bob, carol := s.user("alice"), s.user("bob"), s.user("carol")
	done := alice.newThread("alice private")
	doneRun := alice.startRun(done, "write alice")
	alice.complete(doneRun)
	alice.waitIdle(done)
	live := alice.newThread("alice live")
	liveRun := alice.startRun(live, "hold h1")
	s.waitStarted("h1")
	doneBefore := alice.must("GET", "/api/threads/"+done, "", 200).Body
	liveBefore := alice.must("GET", "/api/threads/"+live, "", 200).Body

	// Positive control: the owner reaches every one of these resources.
	if got := alice.must("GET", "/api/threads/"+done+"/files/download?path=/workspace/outputs/alice.txt", "", 200).Body; string(got) != "alice-secret" {
		t.Fatalf("alice's own download = %q", got)
	}
	if got := alice.must("GET", "/api/runs/"+liveRun, "", 200).json()["status"]; got != "running" {
		t.Fatalf("alice's live run is %v", got)
	}

	for name, intruder := range map[string]*actor{"bob": bob, "carol": carol} {
		for _, target := range []struct{ thread, run string }{{done, doneRun}, {live, liveRun}} {
			for _, rt := range idRoutes(target.thread, target.run) {
				// Act
				got := intruder.do(rt.method, rt.path, rt.body)

				// Assert
				if got.Status != 404 {
					t.Errorf("%s: %s answered %d, want 404: %s", name, rt, got.Status, got.Body)
				}
				if code := got.json()["error"].(map[string]any)["code"]; code != "not_found" {
					t.Errorf("%s: %s error code = %v", name, rt, code)
				}
				if got.Header.Get("Content-Type") == "text/event-stream" || strings.Contains(string(got.Body), "alice-secret") {
					t.Errorf("%s: %s leaked data: %s", name, rt, got.Body)
				}
			}
		}
	}

	// Assert: nothing happened to alice's resources.
	if after := alice.must("GET", "/api/threads/"+done, "", 200).Body; string(after) != string(doneBefore) {
		t.Fatalf("alice's finished thread changed:\n%s\n%s", doneBefore, after)
	}
	if after := alice.must("GET", "/api/threads/"+live, "", 200).Body; string(after) != string(liveBefore) {
		t.Fatalf("alice's live thread changed:\n%s\n%s", liveBefore, after)
	}
	if got := alice.must("GET", "/api/runs/"+liveRun, "", 200).json()["status"]; got != "running" {
		t.Fatalf("a foreign cancel reached alice's run: status %v", got)
	}
	if got := alice.must("GET", "/api/threads/"+done+"/files/download?path=/workspace/outputs/alice.txt", "", 200).Body; string(got) != "alice-secret" {
		t.Fatalf("alice's file after the attacks = %q", got)
	}

	// The held run then completes normally: not cancelled, and the steer text never reached the conversation.
	s.holds["h1"].open()
	alice.complete(liveRun)
	alice.waitIdle(live)
	run := alice.must("GET", "/api/runs/"+liveRun, "", 200).json()
	if run["status"] != "completed" {
		t.Fatalf("alice's live run ended %v", run["status"])
	}
	thread := alice.must("GET", "/api/threads/"+live, "", 200)
	if strings.Contains(string(thread.Body), "pwned") {
		t.Fatalf("an intruder's text reached alice's thread: %s", thread.Body)
	}
	roles, contents := messages(thread)
	if !slices.Equal(roles, []string{"user", "assistant"}) || contents[0] != "hold h1" || contents[1] != "held done" {
		t.Fatalf("alice's live thread = %v %v", roles, contents)
	}
	if title := alice.must("GET", "/api/threads/"+done, "", 200).json()["title"]; title != "alice private" {
		t.Fatalf("alice's title = %v", title)
	}
}

func TestThreadListsHoldOnlyTheCallersOwnThreads(t *testing.T) {
	// Arrange
	s := newSignedStack(t)
	alice, bob := s.user("alice"), s.user("bob")
	a1, a2 := alice.newThread("a1"), alice.newThread("a2")

	// Act
	bobEmpty := bob.threadIDs()
	b1 := bob.newThread("b1")
	bobList := bob.threadIDs()
	aliceList := alice.threadIDs()
	sessions := bob.must("GET", "/api/sessions", "", 501) // the in-memory store has no durable sessions to list

	// Assert
	if len(bobEmpty) != 0 {
		t.Fatalf("bob saw %v before creating anything", bobEmpty)
	}
	if !slices.Equal(bobList, []string{b1}) {
		t.Fatalf("bob's threads = %v, want only %s", bobList, b1)
	}
	if len(aliceList) != 2 || !slices.Contains(aliceList, a1) || !slices.Contains(aliceList, a2) || slices.Contains(aliceList, b1) {
		t.Fatalf("alice's threads = %v", aliceList)
	}
	if strings.Contains(string(sessions.Body), a1) {
		t.Fatalf("sessions leaked alice's thread: %s", sessions.Body)
	}
}

func TestIdsOfDeletedMissingAndOtherKindsAreNotFound(t *testing.T) {
	// Arrange
	s := newSignedStack(t)
	alice, bob := s.user("alice"), s.user("bob")
	kept := alice.newThread("kept")
	keptRun := alice.startRun(kept, "write alice")
	alice.complete(keptRun)
	alice.waitIdle(kept)
	deleted := alice.newThread("doomed")
	alice.must("DELETE", "/api/threads/"+deleted, "", 204)
	foreignGuess := bob.do("GET", "/api/threads/"+kept, "")
	missingGuess := bob.do("GET", "/api/threads/does-not-exist", "")

	// Act / Assert: a deleted thread is gone for everybody.
	for name, who := range map[string]*actor{"alice": alice, "bob": bob} {
		for _, rt := range idRoutes(deleted, "run-of-nothing")[:11] {
			if got := who.do(rt.method, rt.path, rt.body); got.Status != 404 {
				t.Errorf("%s: %s on a deleted thread answered %d", name, rt, got.Status)
			}
		}
	}
	if ids := alice.threadIDs(); !slices.Equal(ids, []string{kept}) {
		t.Fatalf("alice's threads = %v, want only %s", ids, kept)
	}

	// A foreign id is indistinguishable from an unknown one.
	if foreignGuess.Status != missingGuess.Status || string(foreignGuess.json()["error"].(map[string]any)["code"].(string)) != missingGuess.json()["error"].(map[string]any)["code"] {
		t.Fatalf("foreign %d %s vs unknown %d %s", foreignGuess.Status, foreignGuess.Body, missingGuess.Status, missingGuess.Body)
	}

	// A run id used as a thread id, and a thread id as a run id, name nothing for anyone.
	for name, who := range map[string]*actor{"alice": alice, "bob": bob} {
		for _, rt := range []route{
			{"GET", "/api/threads/" + keptRun, ""},
			{"POST", "/api/threads/" + keptRun + "/runs", `{"message":"x"}`},
			{"DELETE", "/api/threads/" + keptRun, ""},
			{"GET", "/api/runs/" + kept, ""},
			{"GET", "/api/runs/" + kept + "/events", ""},
			{"POST", "/api/runs/" + kept + "/cancel", ""},
		} {
			if got := who.do(rt.method, rt.path, rt.body); got.Status != 404 {
				t.Errorf("%s: %s answered %d, want 404", name, rt, got.Status)
			}
		}
	}

	// Positive control: alice's real ids work, and bob's attempts changed nothing.
	if got := alice.must("GET", "/api/threads/"+kept, "", 200).json()["title"]; got != "kept" {
		t.Fatalf("title = %v", got)
	}
	if got := alice.must("GET", "/api/runs/"+keptRun, "", 200).json()["status"]; got != "completed" {
		t.Fatalf("run status = %v", got)
	}
	if evs := alice.complete(keptRun); len(evs) < 3 {
		t.Fatalf("events = %v", evs)
	}
}

func TestForgedAndForeignCredentialsAreRefusedWhileTheOwnerKeepsAccess(t *testing.T) {
	// Arrange
	s := newSignedStack(t)
	alice := s.user("alice")
	thread := alice.newThread("alice private")
	aliceToken := signedToken(signingKey, "alice")
	user, sig, _ := strings.Cut(aliceToken, ".")
	callers := map[string]*actor{
		"no credential":       s.as(nil),
		"unsigned user":       s.as(rawAuthorization("Bearer alice")),
		"wrong signature":     s.as(rawAuthorization("Bearer " + user + ".00" + sig[2:])),
		"another key":         s.as(bearer(signedToken([]byte("another-key-another-key-0000"), "alice"))),
		"signature of bob":    s.as(rawAuthorization("Bearer alice." + strings.SplitN(signedToken(signingKey, "bob"), ".", 2)[1])),
		"X-User is not trust": s.as(func(r *http.Request) { r.Header.Set("X-User", "alice") }),
	}

	for name, caller := range callers {
		// Act
		list := caller.do("GET", "/api/threads", "")
		read := caller.do("GET", "/api/threads/"+thread, "")
		del := caller.do("DELETE", "/api/threads/"+thread, "")

		// Assert
		if list.Status != 401 || read.Status != 401 || del.Status != 401 {
			t.Errorf("%s: list %d, read %d, delete %d, want 401", name, list.Status, read.Status, del.Status)
		}
	}
	if got := alice.must("GET", "/api/threads/"+thread, "", 200).json()["title"]; got != "alice private" {
		t.Fatalf("alice's thread = %v", got)
	}
}

// messages returns the roles and contents of a GET /threads/{id} answer.
func messages(r resp) (roles, contents []string) {
	msgs, _ := r.json()["messages"].([]any)
	for _, m := range msgs {
		m := m.(map[string]any)
		roles = append(roles, m["role"].(string))
		contents = append(contents, m["content"].(string))
	}
	return
}

func TestOwnerSteersAndCancelsTheRunsAnIntruderCannotTouch(t *testing.T) {
	// Arrange: one run waits in the model call, another blocks until it is cancelled.
	s := newSignedStack(t, "steered", "cancelled")
	alice, bob := s.user("alice"), s.user("bob")
	steerThread := alice.newThread("steer me")
	steerRun := alice.startRun(steerThread, "hold steered")
	cancelThread := alice.newThread("cancel me")
	cancelRun := alice.startRun(cancelThread, "block cancelled")
	s.waitStarted("steered")
	s.waitStarted("cancelled")

	// Act
	bobSteer := bob.do("POST", "/api/runs/"+steerRun+"/steer", `{"text":"from bob"}`)
	bobCancel := bob.do("POST", "/api/runs/"+cancelRun+"/cancel", "")
	aliceSteer := alice.do("POST", "/api/runs/"+steerRun+"/steer", `{"text":"from alice"}`)
	aliceCancel := alice.do("POST", "/api/runs/"+cancelRun+"/cancel", "")
	s.holds["steered"].open()
	alice.complete(steerRun)
	alice.complete(cancelRun)
	alice.waitIdle(steerThread)
	alice.waitIdle(cancelThread)

	// Assert
	if bobSteer.Status != 404 || bobCancel.Status != 404 {
		t.Fatalf("bob's steer %d and cancel %d, want 404", bobSteer.Status, bobCancel.Status)
	}
	if aliceSteer.Status != 202 || aliceCancel.Status != 202 {
		t.Fatalf("alice's steer %d and cancel %d, want 202", aliceSteer.Status, aliceCancel.Status)
	}
	if got := alice.must("GET", "/api/runs/"+cancelRun, "", 200).json()["status"]; got != "cancelled" {
		t.Fatalf("cancelled run status = %v", got)
	}
	if got := alice.must("GET", "/api/runs/"+steerRun, "", 200).json()["status"]; got != "completed" {
		t.Fatalf("steered run status = %v", got)
	}
	thread := alice.must("GET", "/api/threads/"+steerThread, "", 200)
	if !strings.Contains(string(thread.Body), "from alice") || strings.Contains(string(thread.Body), "from bob") {
		t.Fatalf("steer texts in alice's thread: %s", thread.Body)
	}
}
