package inproc_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/inproc"
)

const model = "gpt-test"

// rig is a harness running in this process with a scripted model and a fake sandbox backend.
type rig struct {
	h       *harness.Harness
	ctx     context.Context
	backend *sandboxtest.Backend
}

type rigOptions struct {
	model *llmtest.Model
	// noBackend leaves the harness without a sandbox.
	noBackend bool
	// backend replaces the fake backend.
	backend sandbox.Backend
	// durable uses the file store (which lists sessions).
	durable bool
}

func newRig(t *testing.T, o rigOptions) *rig {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HAR_HOME", t.TempDir())
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", model)
	t.Setenv("LLM_STEP_RETRIES", "0")
	t.Setenv("AGENT_PERMISSIONS", "")
	t.Setenv("AGENT_PERMISSIONS_DENY", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if o.model == nil {
		o.model = llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
			if len(c.Tools) == 0 {
				return llmtest.Turn{Chunks: llmtest.Text("A title")}
			}
			return llmtest.Turn{Chunks: llmtest.Text("Hi there")}
		})
	}
	r := &rig{backend: sandboxtest.NewBackend()}
	ho := harness.Options{Config: cfg, Models: llmtest.Factory{M: o.model}}
	switch {
	case o.backend != nil:
		ho.Backend = o.backend
	case !o.noBackend:
		ho.Backend = r.backend
	}
	if o.durable {
		st, err := harness.OpenLocalStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		ho.Store = st
	}
	r.h, err = harness.New(ho)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	r.ctx = ctx
	go func() { _ = r.h.Run(ctx) }()
	return r
}

func (r *rig) client(user string, opts ...inproc.Option) client.Client {
	return inproc.New(r.h, user, append([]inproc.Option{inproc.WithModels([]string{model}, model)}, opts...)...)
}

// blocking is a model whose first agent step waits for release; started is closed when it begins.
func blocking() (m *llmtest.Model, started <-chan struct{}, release func()) {
	st, gate := make(chan struct{}), make(chan struct{})
	var once, rel sync.Once
	m = llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		once.Do(func() { close(st) })
		<-gate
		return llmtest.Turn{Chunks: llmtest.Text("unblocked")}
	})
	return m, st, func() { rel.Do(func() { close(gate) }) }
}

func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out")
	}
}

// drain reads a stream until it closes.
func drain(t *testing.T, ch <-chan client.Event) []client.Event {
	t.Helper()
	var out []client.Event
	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("the stream stayed open after %d events", len(out))
		}
	}
}

func newThread(t *testing.T, r *rig, c client.Client, title string) string {
	t.Helper()
	th, err := c.CreateThread(r.ctx, title)
	if err != nil {
		t.Fatal(err)
	}
	return th.ID
}

// converse sends a message and waits for the run to end.
func converse(t *testing.T, r *rig, c client.Client, threadID, text string, o client.SendOptions) *client.Run {
	t.Helper()
	run, err := c.Send(r.ctx, threadID, text, o)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := c.Events(r.ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)
	if len(evs) == 0 || evs[len(evs)-1].Type != agui.RunFinished {
		t.Fatalf("the run did not finish: %+v", evs)
	}
	for range 400 {
		th, err := c.GetThread(r.ctx, threadID)
		if err != nil {
			t.Fatal(err)
		}
		if th.ActiveRun == nil {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the thread stayed busy")
	return nil
}

func isErr(err error, want *client.Error) bool { return errors.Is(err, want) }

func TestModelsListsTheConfiguredModelsAndTheirInfo(t *testing.T) {
	r := newRig(t, rigOptions{})
	var asked []string
	lookup := func(id string) (client.ModelInfo, bool) {
		asked = append(asked, id)
		if id == "known" || id == "default" || id == "" {
			return client.ModelInfo{ContextWindow: 1000, Priced: true}, true
		}
		return client.ModelInfo{}, false
	}

	list, err := inproc.New(r.h, "alice", inproc.WithModels([]string{"known", "unknown"}, "default"), inproc.WithModelInfo(lookup)).Models(r.ctx)

	if err != nil || list.Default != "default" || len(list.Models) != 2 {
		t.Fatalf("models %+v, %v", list, err)
	}
	if len(list.Info) != 2 || list.Info["known"].ContextWindow != 1000 || list.Info["default"].ContextWindow != 1000 {
		t.Fatalf("info %+v: the default model and the listed ones the lookup knows are described, the others are left out", list.Info)
	}
	if _, ok := list.Info["unknown"]; ok {
		t.Fatal("a model the lookup does not know must have no entry")
	}
}

func TestModelsNeverDescribesTheEmptyModelID(t *testing.T) {
	r := newRig(t, rigOptions{})
	lookup := func(string) (client.ModelInfo, bool) { return client.ModelInfo{ContextWindow: 1}, true }

	list, err := inproc.New(r.h, "alice", inproc.WithModelInfo(lookup)).Models(r.ctx)

	if err != nil || list.Models == nil || len(list.Models) != 0 || list.Info != nil {
		t.Fatalf("a client without models reports an empty (non-nil) list and no info: %+v, %v", list, err)
	}
}

func TestModelsWithoutALookupCarryNoInfo(t *testing.T) {
	r := newRig(t, rigOptions{})

	list, err := r.client("alice").Models(r.ctx)

	if err != nil || list.Info != nil {
		t.Fatalf("%+v, %v", list, err)
	}
}

func TestListThreadsHonoursTheLimitAndNeverShowsOtherUsersThreads(t *testing.T) {
	r := newRig(t, rigOptions{})
	alice, bob := r.client("alice"), r.client("bob")
	for _, title := range []string{"one", "two", "three"} {
		newThread(t, r, alice, title)
	}
	newThread(t, r, bob, "bobs")

	two, err := alice.ListThreads(r.ctx, 2)
	all, err2 := alice.ListThreads(r.ctx, 0)

	if err != nil || err2 != nil || len(two) != 2 || len(all) != 3 {
		t.Fatalf("limit 2: %d (%v), default: %d (%v)", len(two), err, len(all), err2)
	}
	for _, th := range all {
		if th.Title == "bobs" {
			t.Fatal("alice sees bob's thread")
		}
	}
}

func TestRenameThreadTrimsTheTitleAndBoundsItsLength(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.client("alice")
	id := newThread(t, r, c, "start")
	set := func(s string) *string { return &s }

	trimmed, err := c.RenameThread(r.ctx, id, client.ThreadPatch{Title: set("  Spaced  ")})
	max, errMax := c.RenameThread(r.ctx, id, client.ThreadPatch{Title: set(strings.Repeat("é", 200))})
	_, errLong := c.RenameThread(r.ctx, id, client.ThreadPatch{Title: set(strings.Repeat("é", 201))})
	unpinned, errPin := c.RenameThread(r.ctx, id, client.ThreadPatch{Pinned: new(bool)})
	_, errMissing := c.RenameThread(r.ctx, "no-such-thread", client.ThreadPatch{Title: set("x")})

	if err != nil || trimmed.Title != "Spaced" {
		t.Fatalf("trimmed %+v, %v", trimmed, err)
	}
	if errMax != nil || len([]rune(max.Title)) != 200 {
		t.Fatalf("200 characters is the limit and counts characters, not bytes: %v", errMax)
	}
	if !isErr(errLong, client.ErrInvalidRequest) {
		t.Fatalf("201 characters: %v", errLong)
	}
	if errPin != nil || unpinned.Pinned || unpinned.Title != max.Title {
		t.Fatalf("a pin-only patch must keep the title: %+v, %v", unpinned, errPin)
	}
	if !isErr(errMissing, client.ErrNotFound) {
		t.Fatalf("unknown thread: %v", errMissing)
	}
}

func TestDeleteThreadOfAnotherUsersOrMissingThreadIsNotFound(t *testing.T) {
	r := newRig(t, rigOptions{})
	id := newThread(t, r, r.client("alice"), "mine")

	byBob := r.client("bob").DeleteThread(r.ctx, id)
	missing := r.client("alice").DeleteThread(r.ctx, "no-such-thread")
	_, still := r.client("alice").GetThread(r.ctx, id)

	if !isErr(byBob, client.ErrNotFound) || !isErr(missing, client.ErrNotFound) || still != nil {
		t.Fatalf("bob %v, missing %v, alice's thread afterwards %v", byBob, missing, still)
	}
}

func TestSendRecordsTheConfiguredAuthorOnTheUserMessage(t *testing.T) {
	r := newRig(t, rigOptions{})
	plain, supervised := r.client("alice"), r.client("alice", inproc.WithAuthor(harness.Author{Login: "bot", Kind: "supervisor", OnBehalfOf: "alice"}))
	id := newThread(t, r, plain, "")

	converse(t, r, plain, id, "mine", client.SendOptions{})
	converse(t, r, supervised, id, "from the supervisor", client.SendOptions{})
	th, err := plain.GetThread(r.ctx, id)

	if err != nil || len(th.Messages) != 4 {
		t.Fatalf("thread %+v, %v", th, err)
	}
	if th.Messages[0].Author != nil || th.Messages[1].Author != nil {
		t.Fatalf("a plain message and the model's answer have no author: %+v %+v", th.Messages[0].Author, th.Messages[1].Author)
	}
	want := client.Author{Login: "bot", Kind: "supervisor", OnBehalfOf: "alice"}
	if a := th.Messages[2].Author; a == nil || *a != want {
		t.Fatalf("author %+v, want %+v", a, want)
	}
	if th.Messages[3].Author != nil {
		t.Fatalf("the model's answer to a supervisor has no author: %+v", th.Messages[3].Author)
	}
}

func TestSendStoresImageAttachmentsOnTheMessageAndRejectsTheOthers(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.client("alice")
	id := newThread(t, r, c, "")
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32))

	_, notImage := c.Send(r.ctx, id, "see", client.SendOptions{Attachments: []client.Attachment{{Name: "a.txt", MIME: "text/plain", Data: []byte("x")}}})
	_, empty := c.Send(r.ctx, id, "see", client.SendOptions{Attachments: []client.Attachment{{Name: "a.png", MIME: "image/png"}}})
	converse(t, r, c, id, "see", client.SendOptions{Attachments: []client.Attachment{{Name: "shot", MIME: "image/png", Data: png}}})
	th, err := c.GetThread(r.ctx, id)

	if !isErr(notImage, client.ErrInvalidRequest) || !isErr(empty, client.ErrInvalidRequest) {
		t.Fatalf("not an image: %v, empty: %v", notImage, empty)
	}
	if e := client.AsError(notImage); e == nil || !strings.Contains(e.Message, "image/png") {
		t.Fatalf("the user must be told which types are accepted: %v", notImage)
	}
	if err != nil || len(th.Messages) < 1 {
		t.Fatalf("thread %+v, %v", th, err)
	}
	var meta struct {
		Attachments []struct{ Name string } `json:"attachments"`
	}
	raw, _ := json.Marshal(th.Messages[0].Metadata)
	_ = json.Unmarshal(raw, &meta)
	atts := meta.Attachments
	if len(atts) != 1 {
		t.Fatalf("metadata %v: the accepted attachment must be referenced by the message (and the refused ones not stored)", th.Messages[0].Metadata)
	}
	if name := atts[0].Name; name != "shot.png" {
		t.Fatalf("the stored name %q must carry the extension of its type", name)
	}
}

func TestSendWithoutAModelUsesTheClientsDefault(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.client("alice")
	id := newThread(t, r, c, "")

	run := converse(t, r, c, id, "hi", client.SendOptions{})

	if run.Model != model {
		t.Fatalf("model %q, want the client's default %q", run.Model, model)
	}
}

func TestSteerRefusesBlankTextAndStopsAtTheQueueCap(t *testing.T) {
	m, started, release := blocking()
	defer release()
	r := newRig(t, rigOptions{model: m})
	c := r.client("alice")
	id := newThread(t, r, c, "")
	run, err := c.Send(r.ctx, id, "work", client.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, started)

	blank := c.Steer(r.ctx, run.ID, " \n")
	var firstErr error
	n := 0
	for ; n < 100; n++ {
		if firstErr = c.Steer(r.ctx, run.ID, "more"); firstErr != nil {
			break
		}
	}

	if !isErr(blank, client.ErrInvalidRequest) {
		t.Fatalf("blank steer: %v", blank)
	}
	if !isErr(firstErr, client.ErrTooManySteers) || client.AsError(firstErr).Status != 429 {
		t.Fatalf("steer %d: %v", n, firstErr)
	}
	if n == 0 {
		t.Fatal("the first steer must be accepted")
	}
}

func TestEventsStopWhenTheContextIsCancelledWhileTheRunIsStillGoing(t *testing.T) {
	m, started, release := blocking()
	defer release()
	r := newRig(t, rigOptions{model: m})
	c := r.client("alice")
	id := newThread(t, r, c, "")
	run, err := c.Send(r.ctx, id, "work", client.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, started)
	ctx, cancel := context.WithCancel(r.ctx)
	ch, err := c.Events(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ev := <-ch; ev.Type != agui.RunStarted || ev.Seq != 1 {
		t.Fatalf("first event %+v", ev)
	}

	cancel()
	var after []client.Event
	deadline := time.After(10 * time.Second)
	for open := true; open; {
		select {
		case ev, ok := <-ch:
			if ok {
				after = append(after, ev)
			}
			open = ok
		case <-deadline:
			t.Fatal("the channel stayed open after the context was cancelled")
		}
	}

	for _, ev := range after {
		if agui.IsTerminal(ev.Type) {
			t.Fatalf("the run is still blocked, yet it produced %s", ev.Type)
		}
	}
	if th, err := c.GetThread(r.ctx, id); err != nil || th.ActiveRun == nil {
		t.Fatalf("abandoning a stream must not cancel the run: %+v, %v", th, err)
	}
}

func TestResumeInfersTheReasonFromTheDecisionAndValidatesIt(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.client("alice")
	id := newThread(t, r, c, "")

	_, noID := c.Resume(r.ctx, id, client.ResumeRequest{Decision: client.DecisionApprove})
	_, badDecision := c.Resume(r.ctx, id, client.ResumeRequest{InterruptID: "i", Decision: "maybe"})
	_, noDecision := c.Resume(r.ctx, id, client.ResumeRequest{InterruptID: "i", Reason: client.ReasonToolApproval})
	_, otherThread := c.Resume(r.ctx, "no-such-thread", client.ResumeRequest{InterruptID: "i", Decision: client.DecisionApprove})

	if !isErr(noID, client.ErrInvalidRequest) {
		t.Errorf("no interrupt id: %v", noID)
	}
	if !isErr(badDecision, client.ErrInvalidRequest) {
		t.Errorf("a decision implies a tool approval, which only accepts approve, deny or respond: %v", badDecision)
	}
	if !isErr(noDecision, client.ErrInvalidRequest) {
		t.Errorf("a tool approval needs a decision: %v", noDecision)
	}
	if !isErr(otherThread, client.ErrNotFound) {
		t.Errorf("unknown thread: %v", otherThread)
	}
}

func TestSessionsListTheUsersThreadsOfADurableStore(t *testing.T) {
	r := newRig(t, rigOptions{durable: true})
	alice, bob := r.client("alice"), r.client("bob")
	first := newThread(t, r, alice, "first")
	converse(t, r, alice, first, "hello", client.SendOptions{})
	second := newThread(t, r, alice, "second")
	pinned := true
	if _, err := alice.RenameThread(r.ctx, second, client.ThreadPatch{Pinned: &pinned}); err != nil {
		t.Fatal(err)
	}
	newThread(t, r, bob, "bobs")

	all, err := alice.Sessions(r.ctx, 0)
	one, err2 := alice.Sessions(r.ctx, 1)
	bobs, err3 := bob.Sessions(r.ctx, 10)

	if err != nil || err2 != nil || err3 != nil {
		t.Fatalf("errors %v %v %v", err, err2, err3)
	}
	byID := map[string]client.Session{}
	for _, s := range all {
		byID[s.ID] = s
	}
	if len(all) != 2 || byID[first].Title != "first" || byID[first].Messages != 2 || !strings.Contains(byID[first].Preview, "Hi there") || byID[first].LastStatus == "" {
		t.Fatalf("sessions %+v", all)
	}
	if !byID[second].Pinned || byID[second].Messages != 0 {
		t.Fatalf("pinned empty session %+v", byID[second])
	}
	if len(one) != 1 {
		t.Fatalf("limit 1 returned %d sessions", len(one))
	}
	if len(bobs) != 1 || bobs[0].Title != "bobs" {
		t.Fatalf("bob sees %+v", bobs)
	}
}

// file is a workspace file for the Files tests.
type file struct{ path, content string }

// workspace acquires the thread's sandbox and fills it.
func workspace(t *testing.T, r *rig, threadID string, files ...file) {
	t.Helper()
	sb, err := r.h.Sandboxes.Acquire(r.ctx, threadID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := sb.WriteFile(r.ctx, f.path, []byte(f.content)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListFilesSortsDirectoriesFirstAndStaysInsideTheWorkspace(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.client("alice")
	id := newThread(t, r, c, "")
	workspace(t, r, id,
		file{"/workspace/zeta.txt", "z"}, file{"/workspace/alpha.txt", "alpha"},
		file{"/workspace/src/main.go", "package main"}, file{"/workspace/docs/readme.md", "# hi"},
		file{"/tmp/outside.txt", "not in the workspace"})

	root, err := c.ListFiles(r.ctx, id, "")
	sub, errSub := c.ListFiles(r.ctx, id, "src")
	abs, errAbs := c.ListFiles(r.ctx, id, "/workspace/docs")

	var names []string
	for _, f := range root {
		if f.Name == "docs" || f.Name == "src" || strings.HasSuffix(f.Name, ".txt") {
			names = append(names, f.Name+":"+f.Type)
		}
	}
	if err != nil || strings.Join(names, " ") != "docs:directory src:directory alpha.txt:file zeta.txt:file" {
		t.Fatalf("root %v, %v: directories first, then by name, only direct children", names, err)
	}
	for _, f := range root {
		if f.Name == "outside.txt" || strings.Contains(f.Name, "/") {
			t.Fatalf("entry %+v is not a direct child of the workspace", f)
		}
		if f.Name == "alpha.txt" && (f.Path != "/workspace/alpha.txt" || f.Size != 5) {
			t.Fatalf("entry %+v", f)
		}
	}
	for i := 1; i < len(root); i++ {
		if (root[i-1].Type != "directory") && root[i].Type == "directory" {
			t.Fatalf("a directory after a file: %+v", root)
		}
	}
	if errSub != nil || len(sub) != 1 || sub[0].Path != "/workspace/src/main.go" || sub[0].Name != "main.go" {
		t.Fatalf("relative directory %+v, %v", sub, errSub)
	}
	if errAbs != nil || len(abs) != 1 || abs[0].Name != "readme.md" {
		t.Fatalf("absolute directory %+v, %v", abs, errAbs)
	}
}

func TestListFilesOfAThreadWithoutFilesIsAnEmptyListNotAnError(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.client("alice")
	noSandbox := newThread(t, r, c, "")
	emptySandbox := newThread(t, r, c, "")
	workspace(t, r, emptySandbox, file{"/workspace/a.txt", "a"})

	fresh, err := c.ListFiles(r.ctx, noSandbox, "")
	missingDir, errDir := c.ListFiles(r.ctx, emptySandbox, "no/such/dir")

	if err != nil || fresh == nil || len(fresh) != 0 {
		t.Fatalf("a thread that never started a sandbox: %#v, %v (JSON must be [], not null)", fresh, err)
	}
	if errDir != nil || missingDir == nil || len(missingDir) != 0 {
		t.Fatalf("a missing directory: %#v, %v", missingDir, errDir)
	}
}

func TestFilesNeedASandbox(t *testing.T) {
	r := newRig(t, rigOptions{noBackend: true})
	c := r.client("alice")
	id := newThread(t, r, c, "")

	_, list := c.ListFiles(r.ctx, id, "")
	_, download := c.DownloadFile(r.ctx, id, "a.txt")
	_, otherThread := c.ListFiles(r.ctx, "no-such-thread", "")

	if !isErr(list, client.ErrNoSandbox) || !isErr(download, client.ErrNoSandbox) || client.AsError(list).Status != 501 {
		t.Fatalf("list %v, download %v", list, download)
	}
	if !isErr(otherThread, client.ErrNotFound) {
		t.Fatalf("thread ownership is checked before the sandbox: %v", otherThread)
	}
}

func TestListFilesRefusesPathsOutsideTheWorkspace(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.client("alice")
	id := newThread(t, r, c, "")
	workspace(t, r, id, file{"/workspace/a.txt", "a"})

	for _, p := range []string{"../etc", "/etc", "a/../../x", "/workspace/../tmp", "a\x00b"} {
		if _, err := c.ListFiles(r.ctx, id, p); !isErr(err, client.ErrInvalidPath) {
			t.Errorf("ListFiles(%q) = %v, want invalid_path", p, err)
		}
	}
}

func TestDownloadFileReturnsTheContentAndRefusesDirectoriesAndMissingFiles(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.client("alice")
	id := newThread(t, r, c, "")
	noSandbox := newThread(t, r, c, "")
	workspace(t, r, id, file{"/workspace/out/report.txt", "line 1\nline 2"}, file{"/workspace/empty.txt", ""})

	rel, err := c.DownloadFile(r.ctx, id, "out/report.txt")
	abs, errAbs := c.DownloadFile(r.ctx, id, "/workspace/out/../out/report.txt")
	empty, errEmpty := c.DownloadFile(r.ctx, id, "empty.txt")
	_, dir := c.DownloadFile(r.ctx, id, "out")
	_, root := c.DownloadFile(r.ctx, id, "")
	_, missing := c.DownloadFile(r.ctx, id, "nope.txt")
	_, escape := c.DownloadFile(r.ctx, id, "../../etc/passwd")
	_, fresh := c.DownloadFile(r.ctx, noSandbox, "a.txt")

	if err != nil || string(rel.Data) != "line 1\nline 2" || rel.Name != "report.txt" || rel.Path != "/workspace/out/report.txt" {
		t.Fatalf("relative %+v, %v", rel, err)
	}
	if errAbs != nil || abs.Path != "/workspace/out/report.txt" {
		t.Fatalf("absolute path is cleaned: %+v, %v", abs, errAbs)
	}
	if errEmpty != nil || len(empty.Data) != 0 || empty.Name != "empty.txt" {
		t.Fatalf("empty file %+v, %v", empty, errEmpty)
	}
	if !isErr(dir, client.ErrInvalidPath) || !isErr(root, client.ErrInvalidPath) || !isErr(escape, client.ErrInvalidPath) {
		t.Fatalf("directory %v, workspace %v, escape %v", dir, root, escape)
	}
	if !isErr(missing, client.ErrNotFound) || !isErr(fresh, client.ErrNotFound) {
		t.Fatalf("missing %v, thread without a sandbox %v", missing, fresh)
	}
}

// hugeSandbox reports a file larger than the download limit and fails the test if it is read.
type hugeSandbox struct {
	*sandboxtest.Sandbox
	t *testing.T
}

func (h hugeSandbox) Stat(ctx context.Context, p string) (sandbox.FileInfo, error) {
	info, err := h.Sandbox.Stat(ctx, p)
	if err == nil && info.Type == "file" {
		info.Size = 100<<20 + 1
	}
	return info, err
}

func (h hugeSandbox) ReadFile(ctx context.Context, p string, max int64) ([]byte, int64, error) {
	h.t.Errorf("ReadFile(%s): a file over the limit must be refused from its size, before reading it", p)
	return h.Sandbox.ReadFile(ctx, p, max)
}

type hugeBackend struct {
	*sandboxtest.Backend
	t *testing.T
}

func (b hugeBackend) Create(ctx context.Context, o sandbox.CreateOptions) (sandbox.Instance, error) {
	inst, err := b.Backend.Create(ctx, o)
	if err != nil {
		return nil, err
	}
	return hugeSandbox{inst.(*sandboxtest.Sandbox), b.t}, nil
}

func (b hugeBackend) Connect(ctx context.Context, id string) (sandbox.Instance, error) {
	inst, err := b.Backend.Connect(ctx, id)
	if err != nil {
		return nil, err
	}
	return hugeSandbox{inst.(*sandboxtest.Sandbox), b.t}, nil
}

func TestDownloadFileRefusesFilesOverTheLimitWithoutReadingThem(t *testing.T) {
	r := newRig(t, rigOptions{backend: hugeBackend{sandboxtest.NewBackend(), t}})
	c := r.client("alice")
	id := newThread(t, r, c, "")
	workspace(t, r, id, file{"/workspace/big.bin", "x"})

	d, err := c.DownloadFile(r.ctx, id, "big.bin")

	if d != nil || !isErr(err, client.ErrTooLarge) || client.AsError(err).Status != 413 {
		t.Fatalf("download %+v, %v", d, err)
	}
}

func TestCommandsListProjectUserAndBuiltinCommandsFromDiskOnEveryCall(t *testing.T) {
	r := newRig(t, rigOptions{})
	project := t.TempDir()
	cmdDir := filepath.Join(project, ".har", "commands")
	userDir := filepath.Join(os.Getenv("HAR_HOME"), "commands")
	write(t, filepath.Join(cmdDir, "review.md"), "---\ndescription: Review a file\nargument-hint: <file>\n---\nReview $1 for bugs. Notes: $ARGUMENTS\n")
	write(t, filepath.Join(userDir, "review.md"), "user review")
	write(t, filepath.Join(userDir, "ship.md"), "Ship it")
	ex := r.client("alice", inproc.WithProjectDir(project)).(client.Extras)

	list, err := ex.Commands(r.ctx)
	byName := map[string]client.Command{}
	for _, c := range list {
		byName[c.Name] = c
	}
	prompt, errExpand := ex.ExpandCommand(r.ctx, "review", `main.go "be strict"`)
	write(t, filepath.Join(cmdDir, "late.md"), "added while running")
	later, _ := ex.Commands(r.ctx)

	if err != nil {
		t.Fatal(err)
	}
	if c := byName["review"]; c.Source != "project" || c.Description != "Review a file" || c.ArgHint != "<file>" {
		t.Fatalf("review %+v: the project command wins over the user's", c)
	}
	if byName["ship"].Source != "user" || byName["init"].Source != "builtin" {
		t.Fatalf("commands %+v", byName)
	}
	if errExpand != nil || prompt != `Review main.go for bugs. Notes: main.go "be strict"` {
		t.Fatalf("prompt %q, %v", prompt, errExpand)
	}
	found := false
	for _, c := range later {
		found = found || c.Name == "late"
	}
	if !found {
		t.Fatal("a command file added while the client runs must be picked up")
	}
}

func TestExpandingAnUnknownCommandIsNotFoundAndNamesTheCommand(t *testing.T) {
	r := newRig(t, rigOptions{})
	ex := r.client("alice", inproc.WithProjectDir(t.TempDir())).(client.Extras)

	prompt, err := ex.ExpandCommand(r.ctx, "nope", "")

	e := client.AsError(err)
	if prompt != "" || !isErr(err, client.ErrNotFound) || e.Status != 404 || !strings.Contains(e.Message, `"nope"`) {
		t.Fatalf("prompt %q, error %v", prompt, err)
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
