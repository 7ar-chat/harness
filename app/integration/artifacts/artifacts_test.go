// Package artifacts_test runs the real harness (default profile, HTTP API, in-memory
// store) over a sandbox with real bash and git behind its Exec, driven by a scripted
// model: files the agent writes become versioned artifacts, workspace checkpoints
// bring an older version back, and a lost sandbox is rebuilt from the saved files.
package artifacts_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// requireShell skips tests that run real git / tar scripts behind the sandbox when the tools are missing.
func requireShell(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "git", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

// step is one tool call of a scripted turn.
type step struct{ tool, args string }

// script is the scripted model: the turn answering a user message calls its steps one
// after the other (one per model request), then answers "done".
type script struct {
	mu    sync.Mutex
	turns map[string][]step // user message → steps
}

func (s *script) plan(prompt string, steps ...step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns[prompt] = steps
}

func (s *script) turn(c llmtest.Call) llmtest.Turn {
	if len(c.Tools) == 0 { // title and other utility calls
		return llmtest.Turn{Chunks: llmtest.Text("A title")}
	}
	last := -1
	for i, m := range c.Messages {
		if m.Role == ai.RoleUser {
			last = i
		}
	}
	if last < 0 {
		return llmtest.Turn{Chunks: llmtest.Text("done")}
	}
	prompt := c.Messages[last].Content
	done := 0
	for _, m := range c.Messages[last+1:] {
		if m.Role == ai.RoleTool {
			done++
		}
	}
	s.mu.Lock()
	steps := s.turns[prompt]
	s.mu.Unlock()
	if done >= len(steps) {
		return llmtest.Turn{Chunks: llmtest.Text("done")}
	}
	st := steps[done]
	id := fmt.Sprintf("%s#%d", prompt, done)
	return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, id, st.tool, st.args), "tool_calls")}}
}

// rig is a harness behind the HTTP API over a shared store and sandbox backend; boot
// starts a fresh harness process over the same store and backend (no cached sandbox handle).
type rig struct {
	t       *testing.T
	st      *memstore.Store
	backend *sandboxtest.Backend
	script  *script
	model   *llmtest.Model
	cfg     *config.Config
	h       *harness.Harness
	srv     *httptest.Server
	stop    context.CancelFunc
	srvStop func()
}

func newRig(t *testing.T) *rig {
	t.Helper()
	requireShell(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_STEP_RETRIES", "0")
	t.Setenv("AGENT_PERMISSIONS", "")
	t.Setenv("AGENT_PERMISSIONS_DENY", "")
	t.Setenv("HARNESS_WORKSPACE_CHECKPOINTS", "true")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	sc := &script{turns: map[string][]step{}}
	r := &rig{t: t, st: memstore.New(), backend: sandboxtest.NewBackend(), script: sc, model: llmtest.NewFunc(sc.turn), cfg: cfg}
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) { sb.SetExec(sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb))) }
	r.boot()
	t.Cleanup(r.shutdown)
	return r
}

// boot starts a new harness process (and its HTTP server) over the rig's store and backend.
func (r *rig) boot() {
	r.t.Helper()
	r.shutdown()
	h, err := harness.New(harness.Options{Config: r.cfg, Store: r.st, Backend: r.backend, Models: llmtest.Factory{M: r.model}})
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	go func() { _ = h.Run(ctx) }()
	srv := httptest.NewServer(httpapi.New(h, httpapi.Options{
		Authenticate: func(req *http.Request) (string, error) { return req.Header.Get("X-User"), nil },
		Models:       r.cfg.LLMModels, DefaultModel: "gpt-test"}))
	r.h, r.srv, r.stop, r.srvStop = h, srv, cancel, srv.Close
}

func (r *rig) shutdown() {
	if r.stop != nil {
		r.stop()
		r.srvStop()
		r.stop = nil
	}
}

// do sends a request as user and returns status and body.
func (r *rig) do(method, path, user, body string) (int, []byte) {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-User", user)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (r *rig) getJSON(path, user string, into any) {
	r.t.Helper()
	code, b := r.do("GET", path, user, "")
	if code != 200 {
		r.t.Fatalf("GET %s: status %d: %s", path, code, b)
	}
	if err := json.Unmarshal(b, into); err != nil {
		r.t.Fatalf("GET %s: %v: %s", path, err, b)
	}
}

func (r *rig) newThread(user string) string {
	r.t.Helper()
	code, b := r.do("POST", "/api/threads", user, `{}`)
	var th struct{ ID string }
	if code != 201 || json.Unmarshal(b, &th) != nil {
		r.t.Fatalf("create thread: %d %s", code, b)
	}
	return th.ID
}

// event is a decoded AG-UI event.
type event map[string]any

// turn scripts the model's steps for prompt, runs it on the thread as user and returns the
// run id with the run's events once the run is over (events consumed, run idle, archives done).
func (r *rig) turn(user, thread, prompt string, steps ...step) (string, []event) {
	r.t.Helper()
	r.script.plan(prompt, steps...)
	quoted, _ := json.Marshal(prompt)
	code, b := r.do("POST", "/api/threads/"+thread+"/runs", user, `{"message":`+string(quoted)+`}`)
	var run struct{ ID string }
	if code != 202 || json.Unmarshal(b, &run) != nil {
		r.t.Fatalf("start run: %d %s", code, b)
	}
	evs := r.stream(run.ID, user)
	if len(evs) == 0 || evs[len(evs)-1]["type"] != "RUN_FINISHED" {
		r.t.Fatalf("stream did not end with RUN_FINISHED: %v", types(evs))
	}
	if out, _ := evs[len(evs)-1]["outcome"].(map[string]any); out["type"] != "success" {
		r.t.Fatalf("run outcome %v", out)
	}
	r.waitIdle(thread, user)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !r.h.Workspace.WaitArchives(ctx) {
		r.t.Fatal("workspace archives did not finish")
	}
	return run.ID, evs
}

func (r *rig) stream(runID, user string) []event {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.srv.URL+"/api/runs/"+runID+"/events", nil)
	req.Header.Set("X-User", user)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		r.t.Fatalf("events status %d", resp.StatusCode)
	}
	timer := time.AfterFunc(30*time.Second, func() { resp.Body.Close() })
	defer timer.Stop()
	raw, _ := io.ReadAll(resp.Body)
	var out []event
	for _, line := range strings.Split(string(raw), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var e event
			if err := json.Unmarshal([]byte(data), &e); err != nil {
				r.t.Fatalf("bad event %q: %v", data, err)
			}
			out = append(out, e)
		}
	}
	return out
}

// waitIdle waits until the thread has no active run.
func (r *rig) waitIdle(thread, user string) {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var th struct{ ActiveRun any }
		r.getJSON("/api/threads/"+thread, user, &th)
		if th.ActiveRun == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatal("thread still has an active run")
}

func (r *rig) ctx() context.Context {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	r.t.Cleanup(cancel)
	return ctx
}

func (r *rig) artifacts(thread string) []*store.Artifact {
	r.t.Helper()
	list, err := r.h.Artifacts.ListArtifacts(r.ctx(), thread)
	if err != nil {
		r.t.Fatal(err)
	}
	return list
}

func (r *rig) artifact(thread, name string) *store.Artifact {
	r.t.Helper()
	for _, a := range r.artifacts(thread) {
		if a.Name == name {
			return a
		}
	}
	r.t.Fatalf("no artifact %q in %d artifacts", name, len(r.artifacts(thread)))
	return nil
}

func (r *rig) content(a *store.Artifact, version int) string {
	r.t.Helper()
	data, _, err := r.h.Artifacts.Content(r.ctx(), a, version)
	if err != nil {
		r.t.Fatalf("content v%d: %v", version, err)
	}
	return string(data)
}

func (r *rig) versions(a *store.Artifact) []store.ArtifactVersion {
	r.t.Helper()
	list, err := r.h.Artifacts.ListArtifactVersions(r.ctx(), a.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return list
}

// download returns the status and bytes of a file of the thread's sandbox.
func (r *rig) download(thread, user, path string) (int, string) {
	r.t.Helper()
	code, b := r.do("GET", "/api/threads/"+thread+"/files/download?path="+path, user, "")
	return code, string(b)
}

// messages returns the thread history as served by the API.
func (r *rig) messages(thread, user string) []message {
	r.t.Helper()
	var th struct{ Messages []message }
	r.getJSON("/api/threads/"+thread, user, &th)
	return th.Messages
}

type message struct {
	Role       string
	Content    string
	ToolCallID string
	Metadata   map[string]any
}

func types(evs []event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		t, _ := e["type"].(string)
		if t == "CUSTOM" {
			t += ":" + fmt.Sprint(e["name"])
		}
		out = append(out, t)
	}
	return out
}

// artifactEvents returns the values of the run's work.artifact events.
func artifactEvents(evs []event) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e["type"] == "CUSTOM" && e["name"] == "work.artifact" {
			out = append(out, e["value"].(map[string]any))
		}
	}
	return out
}

// toolResults returns the content of the TOOL_CALL_RESULT events, in order.
func toolResults(evs []event) []string {
	var out []string
	for _, e := range evs {
		if e["type"] == "TOOL_CALL_RESULT" {
			out = append(out, fmt.Sprint(e["content"]))
		}
	}
	return out
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func write(path, content string) step {
	args, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return step{"write_file", string(args)}
}

func read(path string) step {
	args, _ := json.Marshal(map[string]string{"path": path})
	return step{"read_file", string(args)}
}

func edit(path, oldText, newText string) step {
	args, _ := json.Marshal(map[string]string{"path": path, "oldText": oldText, "newText": newText})
	return step{"edit_file", string(args)}
}

func command(cmd string) step {
	args, _ := json.Marshal(map[string]string{"command": cmd})
	return step{"execute_command", string(args)}
}

func TestWrittenFileBecomesAVersionedArtifactAnnouncedRightAfterItsResult(t *testing.T) {
	// Arrange
	r := newRig(t)
	thread := r.newThread("alice")

	// Act
	runID, evs := r.turn("alice", thread, "write the report", write("report.md", "# v1"))

	// Assert: the event follows the tool result and describes the new artifact.
	ts := types(evs)
	res := -1
	for i, typ := range ts {
		if typ == "TOOL_CALL_RESULT" {
			res = i
		}
	}
	if res < 0 || ts[res+1] != "CUSTOM:work.artifact" {
		t.Fatalf("events %v", ts)
	}
	arts := r.artifacts(thread)
	if len(arts) != 1 {
		t.Fatalf("artifacts %+v", arts)
	}
	a := arts[0]
	value := evs[res+1]["value"].(map[string]any)
	if value["path"] != "/workspace/report.md" || value["change"] != "created" || value["version"] != float64(1) ||
		value["mimeType"] != "text/markdown" || value["artifactId"] != a.ID {
		t.Fatalf("event value %v", value)
	}
	if n := len(artifactEvents(evs)); n != 1 {
		t.Fatalf("%d artifact events, want 1", n)
	}

	// Assert: the stored artifact and its content.
	if a.Version != 1 || a.SizeBytes != 4 || a.SHA256 != sum("# v1") || a.RunID != runID || a.Path != "/workspace/report.md" || a.Deleted {
		t.Fatalf("artifact %+v", a)
	}
	if got := r.content(a, 1); got != "# v1" {
		t.Fatalf("content %q", got)
	}

	// Assert: the file is in the sandbox listing and downloadable with the exact bytes.
	var files []struct {
		Name, Path, Type string
		Size             int64
	}
	r.getJSON("/api/threads/"+thread+"/files", "alice", &files)
	found := false
	for _, f := range files {
		if f.Path == "/workspace/report.md" && f.Type == "file" && f.Size == 4 {
			found = true
		}
	}
	if !found {
		t.Fatalf("files %+v", files)
	}
	if code, body := r.download(thread, "alice", "/workspace/report.md"); code != 200 || body != "# v1" {
		t.Fatalf("download %d %q", code, body)
	}

	// Assert: the tool message of the history carries the artifact change.
	var tool *message
	msgs := r.messages(thread, "alice")
	for i := range msgs {
		if msgs[i].Role == "tool" {
			tool = &msgs[i]
		}
	}
	if tool == nil {
		t.Fatalf("no tool message in %+v", msgs)
	}
	items, _ := tool.Metadata["artifacts"].([]any)
	if len(items) != 1 {
		t.Fatalf("tool metadata %v", tool.Metadata)
	}
	item := items[0].(map[string]any)
	if item["artifactId"] != a.ID || item["path"] != "/workspace/report.md" || item["name"] != "report.md" ||
		item["version"] != float64(1) || item["change"] != "created" {
		t.Fatalf("metadata item %v", item)
	}
}

func TestLaterTurnsVersionEditsAndReportDeletionsAndCommandCreatedFiles(t *testing.T) {
	// Arrange: turn 1 creates report.md v1.
	r := newRig(t)
	thread := r.newThread("alice")
	r.turn("alice", thread, "write the report", write("report.md", "# v1"))

	// Act: turn 2 edits it.
	_, edited := r.turn("alice", thread, "edit the report", read("report.md"), edit("report.md", "# v1", "# v2"))

	// Assert: an update, version 2; both contents are kept.
	evs := artifactEvents(edited)
	if len(evs) != 1 || evs[0]["change"] != "updated" || evs[0]["version"] != float64(2) || evs[0]["path"] != "/workspace/report.md" {
		t.Fatalf("edit events %v", evs)
	}
	report := r.artifact(thread, "report.md")
	vs := r.versions(report)
	if report.Version != 2 || len(vs) != 2 || vs[0].SHA256 == vs[1].SHA256 {
		t.Fatalf("artifact %+v versions %+v", report, vs)
	}
	if r.content(report, 1) != "# v1" || r.content(report, 2) != "# v2" {
		t.Fatalf("contents %q %q", r.content(report, 1), r.content(report, 2))
	}

	// Act: turn 3 rewrites the very same bytes.
	_, same := r.turn("alice", thread, "rewrite the report", read("report.md"), write("report.md", "# v2"))

	// Assert: no change is reported and no version 3 exists (positive control: turn 2 did report one).
	if evs := artifactEvents(same); len(evs) != 0 {
		t.Fatalf("unchanged rewrite reported %v", evs)
	}
	report = r.artifact(thread, "report.md")
	if report.Version != 2 || len(r.versions(report)) != 2 {
		t.Fatalf("artifact %+v versions %+v", report, r.versions(report))
	}

	// Act: turn 4 creates data.csv with a command, then deletes report.md.
	_, cmds := r.turn("alice", thread, "shuffle files", command("printf 'a,b\\n' > data.csv"), command("rm report.md"))

	// Assert: both are reported; the deleted artifact keeps its versions readable.
	changes := map[string]string{}
	for _, v := range artifactEvents(cmds) {
		changes[fmt.Sprint(v["path"])] = fmt.Sprint(v["change"])
	}
	if len(changes) != 2 || changes["/workspace/data.csv"] != "created" || changes["/workspace/report.md"] != "deleted" {
		t.Fatalf("command changes %v", changes)
	}
	report = r.artifact(thread, "report.md")
	if !report.Deleted || len(r.versions(report)) != 2 || r.content(report, 1) != "# v1" || r.content(report, 2) != "# v2" {
		t.Fatalf("deleted artifact %+v versions %+v", report, r.versions(report))
	}
	data := r.artifact(thread, "data.csv")
	if data.Deleted || data.Version != 1 || r.content(data, 1) != "a,b\n" {
		t.Fatalf("data.csv %+v", data)
	}
	if code, _ := r.download(thread, "alice", "/workspace/report.md"); code == 200 {
		t.Fatal("the deleted file is still downloadable from the sandbox")
	}
	if code, body := r.download(thread, "alice", "/workspace/data.csv"); code != 200 || body != "a,b\n" {
		t.Fatalf("data.csv download %d %q", code, body)
	}
}

func TestCheckpointRestoreBringsBackAnEarlierVersionAsANewOne(t *testing.T) {
	// Arrange: v1 in turn 1, v2 in turn 2.
	r := newRig(t)
	thread := r.newThread("alice")
	r.turn("alice", thread, "write the report", write("report.md", "# v1"))
	r.turn("alice", thread, "rewrite the report", read("report.md"), write("report.md", "# v2"))
	ctx := r.ctx()

	// Assert: the turn that started with report.md in the workspace has its automatic checkpoint.
	list, err := r.h.Workspace.List(ctx, thread, "")
	if err != nil {
		t.Fatal(err)
	}
	var before *struct{ sha, label string }
	for _, c := range list {
		if c.Kind == "automatic" && c.Label == "Before AI turn" && len(c.ChangedFiles) == 1 && c.ChangedFiles[0] == "/workspace/report.md" {
			before = &struct{ sha, label string }{c.SHA, c.Label}
		}
	}
	if before == nil {
		t.Fatalf("no 'Before AI turn' checkpoint of report.md in %+v", list)
	}
	diff, err := r.h.Workspace.FileDiff(ctx, thread, before.sha, "/workspace/report.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Diff, "-# v1") || !strings.Contains(diff.Diff, "+# v2") {
		t.Fatalf("diff %q", diff.Diff)
	}

	// Act (negative cases first): outside /workspace, and an unknown checkpoint.
	_, outside := r.h.Workspace.RestoreFile(ctx, thread, "alice", before.sha, "/etc/passwd")
	_, unknown := r.h.Workspace.RestoreFile(ctx, thread, "alice", strings.Repeat("0", 40), "/workspace/report.md")

	// Assert: both refused, nothing changed.
	if outside == nil || unknown == nil {
		t.Fatalf("refusals: outside %v unknown %v", outside, unknown)
	}
	if code, body := r.download(thread, "alice", "/workspace/report.md"); code != 200 || body != "# v2" {
		t.Fatalf("file after refused restores %d %q", code, body)
	}
	if a := r.artifact(thread, "report.md"); a.Version != 2 || len(r.versions(a)) != 2 {
		t.Fatalf("artifact after refused restores %+v", a)
	}

	// Act: the real restore.
	res, err := r.h.Workspace.RestoreFile(ctx, thread, "alice", before.sha, "/workspace/report.md")

	// Assert: v1 is back in the sandbox and recorded as version 3; v1, v2 and v3 are all readable.
	if err != nil || !res.Restored || res.Path != "/workspace/report.md" {
		t.Fatalf("restore %+v, %v", res, err)
	}
	if code, body := r.download(thread, "alice", "/workspace/report.md"); code != 200 || body != "# v1" {
		t.Fatalf("file after restore %d %q", code, body)
	}
	report := r.artifact(thread, "report.md")
	vs := r.versions(report)
	if report.Version != 3 || len(vs) != 3 || vs[2].SHA256 != sum("# v1") || vs[0].SHA256 != vs[2].SHA256 {
		t.Fatalf("artifact %+v versions %+v", report, vs)
	}
	for v, want := range map[int]string{1: "# v1", 2: "# v2", 3: "# v1"} {
		if got := r.content(report, v); got != want {
			t.Fatalf("content v%d = %q, want %q", v, got, want)
		}
	}

	// Act: a third turn reads the file.
	_, evs := r.turn("alice", thread, "read it again", read("report.md"))

	// Assert: the model saw the restored content.
	results := toolResults(evs)
	if len(results) != 1 || !strings.Contains(results[0], "# v1") || strings.Contains(results[0], "# v2") {
		t.Fatalf("read result %q", results)
	}
}

func TestLostSandboxIsRebuiltFromTheSavedArtifactsAndTheModelIsTold(t *testing.T) {
	// Arrange: turn 1 saved report.md v1; the sandbox then expires at the provider and the process restarts.
	r := newRig(t)
	thread := r.newThread("alice")
	r.turn("alice", thread, "write the report", write("report.md", "# v1"))
	first := r.backend.Created()
	if len(first) != 1 {
		t.Fatalf("%d sandboxes after turn 1", len(first))
	}
	r.backend.Forget(first[0].ID())
	r.boot()

	// Act
	_, evs := r.turn("alice", thread, "read it back", read("report.md"))

	// Assert: a new sandbox holds the restored file and the tool result carries the notice.
	created := r.backend.Created()
	if len(created) != 2 {
		t.Fatalf("%d sandboxes, want 2", len(created))
	}
	if got, _ := created[1].File("/workspace/report.md"); got != "# v1" {
		t.Fatalf("restored file %q", got)
	}
	results := toolResults(evs)
	if len(results) != 1 || !strings.Contains(results[0], "1 saved file(s) were restored into /workspace") || !strings.Contains(results[0], "# v1") {
		t.Fatalf("read result %q", results)
	}

	// Assert: the restore did not create a spurious version.
	report := r.artifact(thread, "report.md")
	if report.Version != 1 || len(r.versions(report)) != 1 || report.Deleted || len(r.artifacts(thread)) != 1 {
		t.Fatalf("artifact %+v versions %+v", report, r.versions(report))
	}
	if evs := artifactEvents(evs); len(evs) != 0 {
		t.Fatalf("the restore reported artifact changes %v", evs)
	}
}

func TestAnotherUserCannotListOrDownloadTheFilesOfAThread(t *testing.T) {
	// Arrange
	r := newRig(t)
	thread := r.newThread("alice")
	r.turn("alice", thread, "write the report", write("report.md", "# v1"))

	// Act
	ownerList, _ := r.do("GET", "/api/threads/"+thread+"/files", "alice", "")
	ownerCode, ownerBody := r.download(thread, "alice", "/workspace/report.md")
	otherList, _ := r.do("GET", "/api/threads/"+thread+"/files", "bob", "")
	otherCode, otherBody := r.download(thread, "bob", "/workspace/report.md")

	// Assert: the owner is served (control); the other user gets 404 and no bytes.
	if ownerList != 200 || ownerCode != 200 || ownerBody != "# v1" {
		t.Fatalf("owner: list %d download %d %q", ownerList, ownerCode, ownerBody)
	}
	if otherList != 404 || otherCode != 404 || strings.Contains(otherBody, "# v1") {
		t.Fatalf("other user: list %d download %d %q", otherList, otherCode, otherBody)
	}
}
