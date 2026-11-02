package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

// wbDoBody is wbDo with a request body.
func wbDoBody(t *testing.T, method, url, body string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestBuildWithInstallsTheFactoryOnlyWhenSwitchedOnInTeamMode(t *testing.T) {
	// Arrange
	wbEnv(t)
	ctx := context.Background()

	build := func(team bool) *built {
		t.Helper()
		t.Setenv("LOCAL_STATE_DIR", t.TempDir()) // one process owns a state directory
		b, err := buildWith(ctx, wbConfig(t), false, team)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.closer.Close() })
		return b
	}

	// Act
	off := build(true)
	t.Setenv("FACTORY", "1")
	t.Setenv("FACTORY_RUN", "0")
	on := build(true)
	solo := build(false) // the factory is a team feature

	// Assert
	if off.factory != nil || off.h.FactoryOf() != nil {
		t.Error("the factory is on without FACTORY=1")
	}
	if solo.factory != nil || solo.h.FactoryOf() != nil {
		t.Error("the factory is on without team mode")
	}
	if on.factory == nil || on.factory.Escalator == nil || on.factory.Hooks == nil || on.h.FactoryOf() == nil {
		t.Fatalf("the factory is not wired: %+v", on.factory)
	}
}

func TestFactoryStateSurvivesARestartWithoutPostgres(t *testing.T) {
	// Arrange: a state directory, as on the production volume
	wbEnv(t)
	t.Setenv("FACTORY", "1")
	t.Setenv("FACTORY_RUN", "0")
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("LOCAL_STATE_DIR", dir)
	first, err := buildWith(ctx, wbConfig(t), false, true)
	if err != nil {
		t.Fatal(err)
	}
	f := first.h.FactoryOf()
	task, _, err := f.Tasks.Create(ctx, factory.Task{ProjectID: "p1", Owner: "ann", Title: "survive", Spec: "restart me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Store.AppendEvent(ctx, factory.TaskEvent{TaskID: task.ID, Kind: factory.EventNote, Text: "before"}); err != nil {
		t.Fatal(err)
	}

	// Act: shut down (flushing the events) and build again over the same directory
	if err := first.closer.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := buildWith(ctx, wbConfig(t), false, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.closer.Close() })
	got, getErr := second.h.FactoryOf().Tasks.Get(ctx, task.ID)
	events, _ := second.h.FactoryOf().Store.ListEvents(ctx, task.ID, 0)

	// Assert
	if getErr != nil || got.Title != "survive" || got.Owner != "ann" {
		t.Fatalf("task after a restart = %+v, %v", got, getErr)
	}
	if len(events) == 0 || events[len(events)-1].Text != "before" {
		t.Fatalf("events after a restart = %+v", events)
	}
	if _, err := os.Stat(filepath.Join(dir, "factory.json")); err != nil {
		t.Fatalf("factory.json is not beside team.json: %v", err)
	}
}

func TestBuildWithRefusesACorruptFactoryFile(t *testing.T) {
	// Arrange
	wbEnv(t)
	t.Setenv("FACTORY", "1")
	dir := t.TempDir()
	t.Setenv("LOCAL_STATE_DIR", dir)
	path := filepath.Join(dir, "factory.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := buildWith(context.Background(), wbConfig(t), false, true)

	// Assert
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want one naming %s", err, path)
	}
	if b, _ := os.ReadFile(path); string(b) != "{broken" {
		t.Errorf("the corrupt file was overwritten: %q", b)
	}
}

func TestBuildWithRefusesAFactoryThatIsMisconfigured(t *testing.T) {
	// Arrange
	wbEnv(t)
	t.Setenv("FACTORY", "1")
	t.Setenv("FACTORY_POLICY", "anarchy")

	// Act
	_, err := buildWith(context.Background(), wbConfig(t), false, true)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "FACTORY_POLICY") {
		t.Errorf("err = %v", err)
	}
}

// TestFactoryServesTasksOverHTTPInTeamMode drives the whole command: FACTORY=1 in team mode mounts the routes,
// keeps its state in factory.json, and the signed webhook needs no session.
func TestFactoryServesTasksOverHTTPInTeamMode(t *testing.T) {
	// Arrange: the dispatcher stays off so tasks keep the state the API gave them.
	wbEnv(t)
	// buildWith reads the process environment, as the command does.
	t.Setenv("FACTORY", "1")
	t.Setenv("FACTORY_RUN", "0")
	t.Setenv("FACTORY_WEBHOOK_SECRET", "whsec")
	s := wbStart(t, nil, "-team", "-dev-users", "-user", "ann")
	api := func(method, path, body string) (int, string) {
		code, out, _ := wbDoBody(t, method, s.URL+path, body, map[string]string{"Content-Type": "application/json"})
		return code, out
	}
	code, body := api("POST", "api/projects", `{"name":"api"}`)
	if code != 201 {
		t.Fatalf("project: %d %s", code, body)
	}
	var project struct{ ID string }
	_ = json.Unmarshal([]byte(body), &project)

	// Act
	code, body = api("POST", "api/projects/"+project.ID+"/tasks", `{"title":"Fix it","spec":"please"}`)
	var created struct{ ID, State, Owner string }
	_ = json.Unmarshal([]byte(body), &created)
	listCode, list := api("GET", "api/projects/"+project.ID+"/tasks", "")
	hookCode, _ := api("POST", "hooks/github", `{}`)

	// Assert
	if code != 201 || created.State != "ready" || created.Owner != "ann" || created.ID == "" {
		t.Errorf("create = %d %s", code, body)
	}
	if listCode != 200 || !strings.Contains(list, created.ID) {
		t.Errorf("list = %d %s", listCode, list)
	}
	if hookCode != 401 { // reached the webhook (not the session wall) and its signature check refused it
		t.Errorf("unsigned webhook = %d, want 401", hookCode)
	}
}

func TestFactoryCheckFlagReportsAndSetsTheExitCode(t *testing.T) {
	cases := []struct {
		name string
		repo string
		env  map[string]string
		code int
		out  string
		err  string
	}{
		{"not owner/name", "justarepo", nil, 1, "", "owner/name"},
		{"empty owner", "/app", nil, 1, "", "owner/name"},
		{"too many parts", "a/b/c", nil, 1, "", "owner/name"},
		{"nothing configured fails the checks and does not serve", "acme/app", nil, 1, "[FAIL]", ""},
		{"a key that does not parse is reported and fails", "acme/app", map[string]string{
			"FACTORY": "1", "GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY": "not a pem"}, 1, "[FAIL]", "not usable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			wbEnv(t)
			var out, errb bytes.Buffer

			// Act: no listener is opened: the flag replaces serving.
			code := run(context.Background(), []string{"-factory-check", c.repo}, env(c.env), &out, &errb)

			// Assert
			if code != c.code || !strings.Contains(out.String(), c.out) || !strings.Contains(errb.String(), c.err) {
				t.Errorf("code %d, stdout %q, stderr %q; want %d containing %q / %q", code, out.String(), errb.String(), c.code, c.out, c.err)
			}
		})
	}
}
