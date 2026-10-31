package isolation_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

const sessionCookie = "harness_session"

var sessionKey = []byte("0123456789abcdef0123456789abcdef")

// oidcStack is the product handler with OIDC login against a fake identity provider.
type oidcStack struct {
	*stack
	idp  *fakeIdP
	team *teamstore.Mem
}

func newOIDCStack(t *testing.T, allowedDomains []string, holds ...string) *oidcStack {
	t.Helper()
	idp := newFakeIdP(t)
	team := teamstore.NewMem()
	s := newStack(t, stackOpts{
		holds:   holds,
		product: &product.Options{Team: team},
		configure: func(appURL string, web *httpapi.Options, prod *product.Options) {
			prod.OIDC = &product.OIDCConfig{
				Issuer: idp.srv.URL, ClientID: "cid", ClientSecret: "secret", RedirectURL: appURL + "/auth/callback",
				SessionKey: sessionKey, AllowedDomains: allowedDomains,
			}
		},
	})
	return &oidcStack{stack: s, idp: idp, team: team}
}

// browser is a client with its own cookie jar, like one person's browser.
func (s *oidcStack) browser() *actor {
	jar, _ := cookiejar.New(nil)
	return &actor{s: s.stack, c: &http.Client{Jar: jar}}
}

// login runs the whole redirect flow (login -> identity provider -> callback) as the identity described by claims.
func (s *oidcStack) login(a *actor, claims map[string]any) resp {
	s.t.Helper()
	s.idp.loginAs(claims)
	res, err := a.c.Get(s.srv.URL + "/auth/login?next=/")
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp{Status: res.StatusCode, Body: []byte(b.String()), Header: res.Header}
}

func (s *oidcStack) sessionValue(a *actor) string {
	u, _ := url.Parse(s.srv.URL)
	for _, c := range a.c.Jar.Cookies(u) {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	return ""
}

// seal mints a session cookie value exactly as the server does: base64(json).base64(hmac-sha256).
func seal(key []byte, login string, exp time.Time) string {
	b, _ := json.Marshal(map[string]any{"l": login, "e": exp.Unix()})
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + sign(key, body)
}

func sign(key []byte, body string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func withCookie(value string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: value}) }
}

func TestOIDCSessionsKeepEachUsersThreadsAndRunsPrivate(t *testing.T) {
	// Arrange
	s := newOIDCStack(t, nil, "live")
	alice, bob, anonymous := s.browser(), s.browser(), s.browser()
	if got := anonymous.do("GET", "/api/threads", "").Status; got != 401 {
		t.Fatalf("anonymous /api/threads = %d, want 401", got)
	}
	s.login(alice, user("alice", "alice@x.io"))
	s.login(bob, user("bob", "bob@x.io"))
	if me := alice.must("GET", "/api/me", "", 200).json()["user"].(map[string]any)["login"]; me != "alice" {
		t.Fatalf("alice's session is for %v", me)
	}
	if me := bob.must("GET", "/api/me", "", 200).json()["user"].(map[string]any)["login"]; me != "bob" {
		t.Fatalf("bob's session is for %v", me)
	}
	done := alice.newThread("alice private")
	doneRun := alice.startRun(done, "write alice")
	alice.complete(doneRun)
	alice.waitIdle(done)
	live := alice.newThread("alice live")
	liveRun := alice.startRun(live, "hold live")
	s.waitStarted("live")
	bobThread := bob.newThread("bob private")

	// Act / Assert: the lists are disjoint.
	if ids := alice.threadIDs(); len(ids) != 2 || ids[0] == bobThread || ids[1] == bobThread {
		t.Fatalf("alice's threads = %v", ids)
	}
	if ids := bob.threadIDs(); len(ids) != 1 || ids[0] != bobThread {
		t.Fatalf("bob's threads = %v, want only %s", ids, bobThread)
	}
	if ids := anonymous.do("GET", "/api/threads", "").Status; ids != 401 {
		t.Fatalf("anonymous list = %d", ids)
	}

	// Bob (and the anonymous browser) can read, stream, cancel, steer or write to nothing of alice's.
	for name, intruder := range map[string]*actor{"bob": bob} {
		for _, target := range []struct{ thread, run string }{{done, doneRun}, {live, liveRun}} {
			for _, rt := range idRoutes(target.thread, target.run) {
				if got := intruder.do(rt.method, rt.path, rt.body); got.Status != 404 {
					t.Errorf("%s: %s answered %d, want 404: %s", name, rt, got.Status, got.Body)
				}
			}
		}
	}
	for _, rt := range idRoutes(done, doneRun) {
		if got := anonymous.do(rt.method, rt.path, rt.body); got.Status != 401 {
			t.Errorf("anonymous: %s answered %d, want 401", rt, got.Status)
		}
	}

	// Alice's resources are untouched and she still reaches them.
	if got := alice.must("GET", "/api/runs/"+liveRun, "", 200).json()["status"]; got != "running" {
		t.Fatalf("alice's live run = %v", got)
	}
	if got := alice.must("GET", "/api/threads/"+done+"/files/download?path=/workspace/outputs/alice.txt", "", 200).Body; string(got) != "alice-secret" {
		t.Fatalf("alice's file = %q", got)
	}
	s.holds["live"].open()
	alice.complete(liveRun)
	alice.waitIdle(live)
	thread := alice.must("GET", "/api/threads/"+live, "", 200)
	if got := alice.must("GET", "/api/runs/"+liveRun, "", 200).json()["status"]; got != "completed" || strings.Contains(string(thread.Body), "pwned") {
		t.Fatalf("live run %v, thread %s", got, thread.Body)
	}
	if got := alice.must("GET", "/api/threads/"+done, "", 200).json()["title"]; got != "alice private" {
		t.Fatalf("alice's title = %v", got)
	}
}

func TestOIDCRefusesForgedTamperedExpiredAndForeignSessionCookies(t *testing.T) {
	// Arrange
	s := newOIDCStack(t, nil)
	alice := s.browser()
	s.login(alice, user("alice", "alice@x.io"))
	thread := alice.newThread("alice private")
	real := s.sessionValue(alice)
	if real == "" {
		t.Fatal("login set no session cookie")
	}
	realBody, realSig, _ := strings.Cut(real, ".")
	bobBody := strings.SplitN(seal(sessionKey, "bob", time.Now().Add(time.Hour)), ".", 2)[0]
	future := time.Now().Add(time.Hour)

	// The control: a cookie minted with the right key is accepted, which proves the forgeries below fail
	// because of what is wrong with them and not because the test mints cookies incorrectly.
	minted := s.as(withCookie(seal(sessionKey, "alice", future)))
	if got := minted.do("GET", "/api/threads/"+thread, "").Status; got != 200 {
		t.Fatalf("a correctly minted cookie answered %d", got)
	}

	cases := map[string]string{
		"expired":                        seal(sessionKey, "alice", time.Now().Add(-time.Minute)),
		"signed with another key":        seal([]byte("another-key-another-key-another!"), "alice", future),
		"payload swapped to bob":         bobBody + "." + realSig,
		"signature flipped":              realBody + "." + sign(sessionKey, realBody+"x"),
		"signature truncated":            realBody + "." + realSig[:len(realSig)-2],
		"signature removed":              realBody + ".",
		"no separator":                   realBody,
		"garbage":                        "x.y",
		"empty login":                    seal(sessionKey, "", future),
		"alg-none style unsigned bearer": base64.RawURLEncoding.EncodeToString([]byte(`{"l":"alice","e":99999999999}`)) + ".",
	}
	for name, cookie := range cases {
		forger := s.as(withCookie(cookie))
		for _, rt := range []route{
			{"GET", "/api/threads", ""},
			{"GET", "/api/threads/" + thread, ""},
			{"DELETE", "/api/threads/" + thread, ""},
			{"GET", "/api/me", ""},
		} {
			// Act
			got := forger.do(rt.method, rt.path, rt.body)

			// Assert
			if got.Status != 401 {
				t.Errorf("%s: %s answered %d, want 401", name, rt, got.Status)
			}
		}
	}

	// A session of someone who never logged in is valid by signature alone, but owns nothing of alice's.
	stranger := s.as(withCookie(seal(sessionKey, "mallory", future)))
	if got := stranger.do("GET", "/api/threads/"+thread, "").Status; got != 404 {
		t.Errorf("a correctly signed session of another login answered %d, want 404", got)
	}
	if got := alice.must("GET", "/api/threads/"+thread, "", 200).json()["title"]; got != "alice private" {
		t.Fatalf("alice's thread = %v", got)
	}
}

func TestOIDCLogoutClearsTheCookieButStatelessSessionsStayValidUntilExpiry(t *testing.T) {
	// Arrange
	s := newOIDCStack(t, nil)
	alice := s.browser()
	s.login(alice, user("alice", "alice@x.io"))
	thread := alice.newThread("alice private")
	old := s.sessionValue(alice)
	replay := s.as(withCookie(old))

	// Act
	logout := alice.do("POST", "/auth/logout", "")
	afterLogout := alice.do("GET", "/api/threads/"+thread, "")
	replayed := replay.do("GET", "/api/threads/"+thread, "")

	// Assert: the response expires the cookie, and the browser that logged out is anonymous.
	if logout.Status != 204 {
		t.Fatalf("logout = %d", logout.Status)
	}
	cleared := false
	for _, c := range logout.Header["Set-Cookie"] {
		if strings.HasPrefix(c, sessionCookie+"=;") && strings.Contains(c, "Max-Age=0") {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("logout did not expire the session cookie: %v", logout.Header["Set-Cookie"])
	}
	if afterLogout.Status != 401 {
		t.Fatalf("the browser that logged out answered %d, want 401", afterLogout.Status)
	}
	// Observation (documented behaviour of oidc.go): sessions are stateless signed cookies, there is no
	// server-side revocation, so a copy of the cookie taken before logout keeps working until it expires.
	if replayed.Status != 200 {
		t.Fatalf("replayed pre-logout cookie answered %d; if sessions became revocable, update this test", replayed.Status)
	}
}

func TestOIDCRefusesALoginFromADisallowedEmailDomainAndCreatesNoSession(t *testing.T) {
	// Arrange
	s := newOIDCStack(t, []string{"x.io"})
	eve, alice := s.browser(), s.browser()

	// Act
	refused := s.login(eve, user("eve", "eve@evil.io"))
	allowed := s.login(alice, user("alice", "alice@x.io"))

	// Assert
	if refused.Status != 403 || !strings.Contains(string(refused.Body), "not allowed") {
		t.Fatalf("disallowed login = %d %s", refused.Status, refused.Body)
	}
	if v := s.sessionValue(eve); v != "" {
		t.Fatalf("a session cookie was issued to the refused login: %q", v)
	}
	if got := eve.do("GET", "/api/threads", "").Status; got != 401 {
		t.Fatalf("refused user's /api/threads = %d, want 401", got)
	}
	if _, err := s.team.GetUser(context.Background(), "eve"); err != store.ErrNotFound {
		t.Fatalf("the refused login was recorded as a user: %v", err)
	}
	// Positive control: the allowed domain logs in through the same flow.
	if allowed.Status == 403 || s.sessionValue(alice) == "" {
		t.Fatalf("allowed login = %d (session %q)", allowed.Status, s.sessionValue(alice))
	}
	if got := alice.do("GET", "/api/threads", "").Status; got != 200 {
		t.Fatalf("allowed user's /api/threads = %d", got)
	}
}
