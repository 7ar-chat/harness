package isolation_test

import (
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

const staticToken = "s3cret-token-0123456789"

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func rawAuthorization(value string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", value) }
}

// bothPrefixes returns every route under /api and in its legacy unprefixed form.
func bothPrefixes(routes []route) []route {
	out := append([]route(nil), routes...)
	for _, r := range routes {
		r.path = strings.TrimPrefix(r.path, "/api")
		out = append(out, r)
	}
	return out
}

func TestStaticTokenIsRequiredByEveryAPIRouteButNotByHealthOrTheUI(t *testing.T) {
	// Arrange
	s := newStack(t, stackOpts{web: httpapi.Options{Token: staticToken, LocalUser: "owner"}})
	owner := s.as(bearer(staticToken))
	thread := owner.newThread("owner notes")
	run := owner.startRun(thread, "write alice")
	owner.complete(run)
	owner.waitIdle(thread)
	before := owner.must("GET", "/api/threads/"+thread, "", 200).Body
	callers := map[string]*actor{
		"no credential":          s.as(nil),
		"wrong token":            s.as(bearer("wrong")),
		"token with a suffix":    s.as(bearer(staticToken + "x")),
		"truncated token":        s.as(bearer(staticToken[:len(staticToken)-1])),
		"empty bearer":           s.as(bearer("")),
		"lowercase scheme":       s.as(rawAuthorization("bearer " + staticToken)),
		"basic scheme":           s.as(rawAuthorization("Basic " + staticToken)),
		"bare token, no scheme":  s.as(rawAuthorization(staticToken)),
		"token in another field": s.as(func(r *http.Request) { r.Header.Set("X-Api-Key", staticToken) }),
	}
	routes := bothPrefixes(append(globalRoutes(), idRoutes(thread, run)...))

	for name, caller := range callers {
		for _, rt := range routes {
			// Act
			got := caller.do(rt.method, rt.path, rt.body)

			// Assert
			if got.Status != 401 {
				t.Errorf("%s: %s answered %d, want 401", name, rt, got.Status)
			}
			if got.json()["error"].(map[string]any)["code"] != "unauthenticated" {
				t.Errorf("%s: %s body = %s", name, rt, got.Body)
			}
			if strings.Contains(string(got.Body), staticToken) || strings.Contains(headerDump(got.Header), staticToken) {
				t.Errorf("%s: %s leaked the token: %s %v", name, rt, got.Body, got.Header)
			}
		}
	}

	// Assert: the refused requests had no effect, and the credential still works (positive control).
	after := owner.must("GET", "/api/threads/"+thread, "", 200).Body
	if string(before) != string(after) {
		t.Fatalf("the thread changed under refused requests:\n%s\n%s", before, after)
	}
	if got := owner.must("GET", "/api/runs/"+run, "", 200).json()["status"]; got != "completed" {
		t.Fatalf("run status = %v", got)
	}
	if ids := owner.threadIDs(); len(ids) != 1 || ids[0] != thread {
		t.Fatalf("threads = %v", ids)
	}
}

func TestStaticTokenLeavesHealthTheUIAndTheClientConfigOpen(t *testing.T) {
	// Arrange
	s := newStack(t, stackOpts{web: httpapi.Options{Token: staticToken}})
	anonymous := s.as(nil)
	wantUI := 200
	if _, err := fs.Stat(httpapi.UI(), "index.html"); err != nil {
		wantUI = 404 // a checkout without the web build serves a notice instead
	}

	// Act
	health := anonymous.do("GET", "/healthz", "")
	ui := anonymous.do("GET", "/", "")
	config := anonymous.do("GET", "/api/client-config", "")

	// Assert
	if health.Status != 200 || health.json()["status"] != "ok" {
		t.Fatalf("healthz = %d %s", health.Status, health.Body)
	}
	if ui.Status != wantUI {
		t.Fatalf("GET / = %d, want %d", ui.Status, wantUI)
	}
	if config.Status != 200 {
		t.Fatalf("client-config = %d", config.Status)
	}
}

func TestStaticTokenRequestsActAsTheLocalUser(t *testing.T) {
	// Arrange
	s := newStack(t, stackOpts{web: httpapi.Options{Token: staticToken, LocalUser: "owner"}})
	caller := s.as(bearer(staticToken))

	// Act
	thread := caller.newThread("mine")
	run := caller.startRun(thread, "write alice")
	caller.complete(run)
	caller.waitIdle(thread)
	models := caller.must("GET", "/api/models", "", 200).json()

	// Assert: the thread belongs to the configured local user, not to the token or to "local".
	if _, err := s.h.Store.GetThread(s.ctx, "owner", thread); err != nil {
		t.Fatalf("thread is not owned by the local user: %v", err)
	}
	if _, err := s.h.Store.GetThread(s.ctx, "local", thread); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("thread visible to another user: %v", err)
	}
	if _, err := s.h.Store.GetUserRun(s.ctx, "owner", run); err != nil {
		t.Fatalf("run is not owned by the local user: %v", err)
	}
	if models["default"] != "gpt-test" {
		t.Fatalf("models = %v", models)
	}
	if ids := caller.threadIDs(); len(ids) != 1 || ids[0] != thread {
		t.Fatalf("threads = %v", ids)
	}
}

func headerDump(h http.Header) string {
	var b strings.Builder
	for k, v := range h {
		b.WriteString(k + ": " + strings.Join(v, ",") + "\n")
	}
	return b.String()
}
