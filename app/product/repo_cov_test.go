package product

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
)

// cnFailSandbox is a sandbox whose commands print output and exit with a code.
type cnFailSandbox struct {
	fakeSandbox
	err error
}

func (f *cnFailSandbox) Exec(ctx context.Context, r sandbox.ExecRequest) (sandbox.ExecResult, error) {
	res, _ := f.fakeSandbox.Exec(ctx, r)
	if r.OnStdout != nil {
		r.OnStdout("stdout-line\n")
	}
	return res, f.err
}

// cnTokenAPI is a GitHub API that mints a fixed installation token and counts the requests.
func cnTokenAPI(t *testing.T, status int) (*GitHubApp, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if status != 0 {
			http.Error(w, "no", status)
			return
		}
		w.Write([]byte(`{"token":"ghs_secret","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
	}))
	t.Cleanup(api.Close)
	app, _ := testApp(t, api.URL)
	return app, &n
}

func TestBranchForUsesAShortThreadSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"11111111-2222-3333-4444-555555555555": "har/11111111",
		"abc":                                  "har/abc",
		"a-b-c":                                "har/abc",
		"":                                     "har/",
	} {
		if got := branchFor(in); got != want {
			t.Errorf("branchFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCloneURLAcceptsOnlySafeHTTPS(t *testing.T) {
	for url, tc := range map[string]struct {
		want string
		ok   bool
	}{
		"git@github.com:acme/api.git":        {"https://github.com/acme/api.git", true},
		"https://github.com/acme/api":        {"https://github.com/acme/api.git", true},
		"https://git.example.com/team/r.git": {"https://git.example.com/team/r.git", true},
		"http://git.example.com/team/r.git":  {"", false},
		"ssh://git@git.example.com/team/r":   {"", false},
		"https://tok:x@git.example.com/r":    {"", false},
		"https://tok@git.example.com/r":      {"", false},
		"file:///etc":                        {"", false},
		"-oProxyCommand=evil":                {"", false},
		"%zz":                                {"", false},
	} {
		got, err := cloneURL(&store.Project{RepoURL: url})
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("cloneURL(%q) = %q, %v; want %q ok=%v", url, got, err, tc.want, tc.ok)
		}
	}
}

func TestCheckoutRefusesUnsafeBranchNames(t *testing.T) {
	for _, branch := range []string{"--upload-pack=evil", "a b", "main;rm -rf /", "x$(id)", strings.Repeat("a", 101), "feat\nx"} {
		// Arrange
		rc, thread := repoRig(t, "https://example.com/team/repo.git", 0)
		p, _ := rc.Team.GetThreadMeta(context.Background(), thread)
		_, _ = rc.Team.UpdateProject(context.Background(), p.ProjectID, store.ProjectPatch{DefaultBranch: &branch})
		sb := &fakeSandbox{}

		// Act
		note := rc.setup(context.Background(), thread, sb)

		// Assert: nothing runs in the sandbox, the agent is told why.
		if len(sb.reqs) != 0 || !strings.Contains(note, "default branch name is not valid") {
			t.Errorf("branch %q: note %q, %d commands", branch, note, len(sb.reqs))
		}
	}
	for _, branch := range []string{"main", "release/1.2.x", "feat_a-b.c"} {
		rc, thread := repoRig(t, "https://example.com/team/repo.git", 0)
		p, _ := rc.Team.GetThreadMeta(context.Background(), thread)
		_, _ = rc.Team.UpdateProject(context.Background(), p.ProjectID, store.ProjectPatch{DefaultBranch: &branch})
		sb := &fakeSandbox{}
		rc.setup(context.Background(), thread, sb)
		if len(sb.reqs) != 1 || sb.reqs[0].Env["REPO_BRANCH"] != branch {
			t.Errorf("branch %q: %+v", branch, sb.reqs)
		}
	}
}

func TestCheckoutOfThreadsWithoutARepositoryDoesNothing(t *testing.T) {
	ctx := context.Background()
	tm := teamstore.NewMem()
	cnUsers(t, tm, "alice")
	noRepo, _ := tm.CreateProject(ctx, store.Project{Name: "empty"}, "alice")
	_ = tm.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t-norepo", ProjectID: noRepo.ID, Owner: "alice", CreatedBy: "alice"})
	_ = tm.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t-personal", Owner: "alice", CreatedBy: "alice"})
	_ = tm.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t-gone", ProjectID: "deleted", Owner: "alice", CreatedBy: "alice"})
	rc := &RepoCheckout{Team: tm}

	for _, thread := range []string{"t-personal", "t-gone", "t-unknown"} {
		sb := &fakeSandbox{}
		if note := rc.setup(ctx, thread, sb); note != "" || len(sb.reqs) != 0 {
			t.Errorf("%s: note %q, %d commands", thread, note, len(sb.reqs))
		}
	}
}

func TestCheckoutFailureReportsRedactedTailOfTheOutput(t *testing.T) {
	// Arrange
	app, _ := cnTokenAPI(t, 0)
	rc, thread := repoRig(t, "https://github.com/acme/api", 42)
	rc.GitHub = app
	long := strings.Repeat("x", 500) + " fatal: auth failed for https://x-access-token:ghs_secret@github.com"
	sb := &cnFailSandbox{fakeSandbox: fakeSandbox{exit: 128, out: long}}

	// Act
	note := rc.setup(context.Background(), thread, sb)

	// Assert: the message says it failed, keeps only the end of the output, and never contains the token.
	if !strings.HasPrefix(note, "Cloning the project repository into "+RepoDir+" failed: ") || strings.Contains(note, "ghs_secret") ||
		!strings.Contains(note, "x-access-token:***@github.com") || !strings.Contains(note, "stdout-line") {
		t.Fatalf("note = %q", note)
	}
	if msg := strings.TrimPrefix(note, "Cloning the project repository into "+RepoDir+" failed: "); len(msg) > 400 {
		t.Fatalf("the failure message is %d bytes, want at most 400", len(msg))
	}
	// A failed clone leaves no credential file behind.
	for _, r := range sb.reqs {
		if strings.Contains(r.Command, "git-credentials") {
			t.Fatalf("credentials written after a failed clone: %q", r.Command)
		}
	}
	// A sandbox that cannot even run the command fails the same way.
	sb2 := &cnFailSandbox{err: errors.New("sandbox gone")}
	if note := rc.setup(context.Background(), thread, sb2); !strings.Contains(note, "failed") {
		t.Fatalf("exec error note = %q", note)
	}
}

func TestCheckoutWithAFailingTokenStillClonesPublicly(t *testing.T) {
	// Arrange: GitHub refuses to mint a token (outage, revoked installation).
	app, calls := cnTokenAPI(t, http.StatusForbidden)
	rc, thread := repoRig(t, "https://github.com/acme/api", 42)
	rc.GitHub = app
	sb := &fakeSandbox{}

	// Act
	note := rc.setup(context.Background(), thread, sb)

	// Assert: unauthenticated clone, no credential write, and the agent is told where it is.
	if calls.Load() == 0 || len(sb.reqs) != 1 || sb.reqs[0].Env["GH_TOKEN"] != "" || !strings.Contains(note, "is checked out in") {
		t.Fatalf("calls %d, commands %d, note %q", calls.Load(), len(sb.reqs), note)
	}
}

func TestCredentialFileFailureIsReportedAsNotInPlace(t *testing.T) {
	// Arrange
	rc, _ := repoRig(t, "https://github.com/acme/api", 42)
	p := &store.Project{ID: "p", RepoURL: "https://github.com/acme/api"}

	// Act / Assert
	if ok := rc.writeCredentials(context.Background(), &fakeSandbox{exit: 1}, p, "tok"); ok {
		t.Fatal("a failing credential script was reported as success")
	}
	ok := &fakeSandbox{}
	if !rc.writeCredentials(context.Background(), ok, p, "tok") || ok.reqs[0].Env["GH_TOKEN"] != "tok" || !strings.Contains(ok.reqs[0].Command, "umask 077") {
		t.Fatalf("credential script = %+v", ok.reqs)
	}
	// A broker is only usable for a GitHub repository: the bindings need owner and repository.
	if rc.writeCredentials(context.Background(), &brokerSandbox{}, &store.Project{RepoURL: "https://example.com/x/y"}, "tok") {
		t.Fatal("broker credentials written for a non-GitHub repository")
	}
}

func TestCheckoutNeverGivesAnInstallationTokenForAnotherHost(t *testing.T) {
	// Arrange: the project was connected to the GitHub App, then its repository URL was changed to another host.
	app, _ := cnTokenAPI(t, 0)
	rc, thread := repoRig(t, "https://git.example.com/team/repo.git", 42)
	rc.GitHub = app
	plain, brokered := &fakeSandbox{}, &brokerSandbox{}

	// Act
	rc.setup(context.Background(), thread, plain)
	rc.setup(context.Background(), thread, brokered)

	// Assert: the GitHub token is never sent to (or written for) a host that is not GitHub.
	for _, sb := range []*fakeSandbox{plain, &brokered.fakeSandbox} {
		for _, r := range sb.reqs {
			if r.Env["GH_TOKEN"] != "" || strings.Contains(r.Command, "ghs_secret") {
				t.Fatalf("the GitHub token reached a foreign host's clone: %+v", r.Env)
			}
		}
	}
	if len(brokered.sets) != 0 {
		t.Fatalf("a GitHub token was handed to the broker for a foreign host: %+v", brokered.sets)
	}
}

func TestRepoRefreshRenewsCredentialsOnlyWhenThereIsAConnectedProjectAndSandbox(t *testing.T) {
	ctx := context.Background()
	app, calls := cnTokenAPI(t, 0)
	rc, thread := repoRig(t, "https://github.com/acme/api", 42)
	rc.GitHub = app
	run := &store.Run{ThreadID: thread}
	brk := &brokerSandbox{}

	// A live sandbox gets the fresh token.
	repoRefresh{rc: rc, mgr: cnMgr{sb: brk}}.BeforeRun(ctx, run)
	if len(brk.sets) != 1 || brk.sets[0].Credentials[1].Value != "ghs_secret" {
		t.Fatalf("broker sets = %+v", brk.sets)
	}
	// A plain sandbox gets the credential file instead.
	plain := &fakeSandbox{}
	repoRefresh{rc: rc, mgr: cnMgr{sb: plain}}.BeforeRun(ctx, run)
	if len(plain.reqs) != 1 || plain.reqs[0].Env["GH_TOKEN"] != "ghs_secret" {
		t.Fatalf("plain sandbox commands = %+v", plain.reqs)
	}
	// No sandbox yet: nothing to do (its creation sets credentials up), and no token is minted for nothing.
	before := calls.Load()
	repoRefresh{rc: rc, mgr: cnMgr{err: sandbox.ErrNoSandbox}}.BeforeRun(ctx, run)
	// A thread outside any project, a project without an installation, and a failing token mint do nothing.
	repoRefresh{rc: rc, mgr: cnMgr{sb: brk}}.BeforeRun(ctx, &store.Run{ThreadID: "unknown-thread"})
	noInst, thread2 := repoRig(t, "https://github.com/acme/api", 0)
	noInst.GitHub = app
	repoRefresh{rc: noInst, mgr: cnMgr{sb: brk}}.BeforeRun(ctx, &store.Run{ThreadID: thread2})
	failing, _ := cnTokenAPI(t, 500)
	rc2, thread3 := repoRig(t, "https://github.com/acme/api", 42)
	rc2.GitHub = failing
	repoRefresh{rc: rc2, mgr: cnMgr{sb: brk}}.BeforeRun(ctx, &store.Run{ThreadID: thread3})
	if len(brk.sets) != 1 || calls.Load() != before {
		t.Fatalf("unexpected credential writes: sets %d, token calls %d->%d", len(brk.sets), before, calls.Load())
	}
}

// cnMgr is a sandbox.Manager that only answers Get.
type cnMgr struct {
	sandbox.Manager
	sb  sandbox.Sandbox
	err error
}

func (m cnMgr) Get(context.Context, string) (sandbox.Sandbox, error) { return m.sb, m.err }

func TestRepoCheckoutFeatureCloneOnSandboxCreationAndPromptSection(t *testing.T) {
	for name, brokered := range map[string]bool{"plain": false, "brokered": true} {
		t.Run(name, func(t *testing.T) {
			// Arrange: a project thread whose run uses the sandbox, so the checkout hook fires.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			app, _ := cnTokenAPI(t, 0)
			tm := teamstore.NewMem()
			cnUsers(t, tm, "alice")
			p, _ := tm.CreateProject(ctx, store.Project{Name: "api", RepoURL: "https://github.com/acme/api", DefaultBranch: "main", GitHubInstallation: 42}, "alice")
			model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
				if len(c.Tools) == 0 {
					return llmtest.Turn{Chunks: llmtest.Text("A title")}
				}
				if c.Messages[len(c.Messages)-1].Role == ai.RoleTool {
					return llmtest.Turn{Chunks: llmtest.Text("done")}
				}
				return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "execute_command", `{"command":"echo hi"}`), "tool_calls")}}
			})
			backend := sandboxtest.NewBackend()
			rc := &RepoCheckout{Team: tm, GitHub: app, Brokered: brokered}
			if brokered {
				rc.Factory = factorystore.NewMem(nil) // the factory is on: the agent is told when a pull request is wanted
			}
			h, err := harness.New(harness.Options{Config: cnConfig(t), Models: llmtest.Factory{M: model}, Backend: backend,
				Features: append(harness.ProfileMinimal(), rc.Feature())})
			if err != nil {
				t.Fatal(err)
			}
			go func() { _ = h.Run(ctx) }()
			th, _ := h.NewThread(ctx, "alice", "")
			_ = tm.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: th.ID, ProjectID: p.ID, Owner: "alice", CreatedBy: "alice"})

			// Act
			run, err := h.Send(ctx, "alice", th.ID, "work on it", harness.RunOptions{Model: "gpt-test"})
			if err != nil {
				t.Fatal(err)
			}
			var last string
			for ev := range h.Events(ctx, run.ID, 0) {
				last = ev.Type
			}

			// Assert: the sandbox was cloned into on the thread's branch...
			if last != "RUN_FINISHED" || len(backend.Created()) != 1 {
				t.Fatalf("last event %q, sandboxes %d", last, len(backend.Created()))
			}
			var clone *sandbox.ExecRequest
			for _, e := range backend.Created()[0].Execs() {
				if e.Env["REPO_URL"] != "" {
					e := e
					clone = &e
				}
			}
			if clone == nil || clone.Env["REPO_URL"] != "https://github.com/acme/api.git" || clone.Env["THREAD_BRANCH"] != branchFor(th.ID) || clone.Env["REPO_BRANCH"] != "main" {
				t.Fatalf("clone request = %+v", clone)
			}
			// ...and the system prompt tells the agent where the repository is.
			var system strings.Builder
			for _, m := range model.Calls()[0].Messages {
				system.WriteString(m.Content)
			}
			sys := system.String()
			if !strings.Contains(sys, "# Project repository") || !strings.Contains(sys, RepoDir) || !strings.Contains(sys, branchFor(th.ID)) {
				t.Fatalf("system prompt lacks the repository section")
			}
			if got := strings.Contains(sys, "# Pull requests") && strings.Contains(sys, "call open_pr"); got != brokered {
				t.Fatalf("factory=%v but prompt says pull-request guidance=%v", brokered, got)
			}
			if got := strings.Contains(sys, "send no token and look for none"); got != brokered {
				t.Fatalf("brokered=%v but prompt says no-token=%v", brokered, got)
			}
		})
	}
}

func cnConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCheckoutLeavesFactoryTaskThreadsAlone(t *testing.T) {
	cases := []struct {
		name      string
		info      *factory.ThreadInfo
		lookupErr error
		cloned    bool
	}{
		{"an ordinary project thread is cloned as before", nil, nil, true},
		{"a factory task thread is left to the factory", &factory.ThreadInfo{ThreadID: "T", TaskID: "task-1", Repo: "acme/api"}, nil, false},
		{"a delegated sub-agent thread is left alone", &factory.ThreadInfo{ThreadID: "T", TaskID: "task-1", ParentThreadID: "P"}, nil, false},
		{"a thread info without a task or repository is not a task thread", &factory.ThreadInfo{ThreadID: "T", Role: "planner"}, nil, true},
		{"a failing lookup leaves the checkout out rather than risk a token", nil, errors.New("store down"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			app, _ := cnTokenAPI(t, 0)
			rc, thread := repoRig(t, "https://github.com/acme/api", 42)
			rc.GitHub = app
			fs := &threadInfoStore{Mem: factorystore.NewMem(nil), err: c.lookupErr}
			if c.info != nil {
				c.info.ThreadID = thread
				_ = fs.SetThreadInfo(context.Background(), *c.info)
			}
			rc.Factory = fs
			sb := &fakeSandbox{}

			// Act
			note := rc.setup(context.Background(), thread, sb)

			// Assert: a left-alone thread runs nothing in its sandbox, so it gets no clone and no token.
			if got := len(sb.reqs) > 0; got != c.cloned {
				t.Errorf("commands = %d, note %q; want cloned = %v", len(sb.reqs), note, c.cloned)
			}
			for _, r := range sb.reqs {
				if !c.cloned && r.Env["GH_TOKEN"] != "" {
					t.Error("a token was handed to a factory task thread")
				}
			}
		})
	}
}

// threadInfoStore is a factory store whose thread lookup can be made to fail.
type threadInfoStore struct {
	*factorystore.Mem
	err error
}

func (s *threadInfoStore) GetThreadInfo(ctx context.Context, id string) (*factory.ThreadInfo, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.Mem.GetThreadInfo(ctx, id)
}

func TestCheckoutClonesEveryRepositoryOfTheProject(t *testing.T) {
	// Arrange
	rc, thread := repoRig(t, "https://github.com/acme/api", 0)
	p := rc.project(context.Background(), thread)
	more := []string{"https://github.com/acme/web", "https://github.com/other/web", "https://example.com/x/docs.git"}
	p, err := rc.Team.UpdateProject(context.Background(), p.ID, store.ProjectPatch{ExtraRepos: &more})
	if err != nil {
		t.Fatal(err)
	}
	sb := &fakeSandbox{}
	// Act
	msg := rc.setup(context.Background(), thread, sb)
	// Assert
	if len(sb.reqs) != 4 {
		t.Fatalf("want one clone per repository, got %d: %+v", len(sb.reqs), sb.reqs)
	}
	dirs := []string{RepoDir, ExtraDir + "/web", ExtraDir + "/web-2", ExtraDir + "/docs"}
	urls := []string{"https://github.com/acme/api.git", "https://github.com/acme/web.git", "https://github.com/other/web.git", "https://example.com/x/docs.git"}
	for i, r := range sb.reqs {
		if r.Env["REPO_DIR"] != dirs[i] || r.Env["REPO_URL"] != urls[i] {
			t.Errorf("clone %d = %v, want %s into %s", i, r.Env, urls[i], dirs[i])
		}
	}
	if sb.reqs[0].Env["REPO_BRANCH"] != "main" || sb.reqs[1].Env["REPO_BRANCH"] != "" {
		t.Errorf("only the main repository uses the project's default branch: %v / %v", sb.reqs[0].Env, sb.reqs[1].Env)
	}
	if !strings.Contains(msg, ExtraDir+"/docs") || strings.Contains(msg, "failed") {
		t.Errorf("message = %q", msg)
	}
	_ = p
}

func TestCheckoutKeepsGoingWhenAnAdditionalRepositoryFails(t *testing.T) {
	// Arrange
	rc, thread := repoRig(t, "https://github.com/acme/api", 0)
	p := rc.project(context.Background(), thread)
	more := []string{"ssh://git@example.com/x/y", "https://github.com/acme/web"}
	if _, err := rc.Team.UpdateProject(context.Background(), p.ID, store.ProjectPatch{ExtraRepos: &more}); err != nil {
		t.Fatal(err)
	}
	sb := &fakeSandbox{}
	// Act
	msg := rc.setup(context.Background(), thread, sb)
	// Assert
	if len(sb.reqs) != 2 {
		t.Fatalf("the unsupported URL is skipped, the others cloned: %d requests", len(sb.reqs))
	}
	if !strings.Contains(msg, "Not checked out") || !strings.Contains(msg, "ssh://git@example.com/x/y") || !strings.Contains(msg, ExtraDir+"/web") {
		t.Errorf("message = %q", msg)
	}
}

func TestBrokeredCredentialsCoverEveryGitHubRepository(t *testing.T) {
	// Arrange
	set := brokeredGitHubRepos([][2]string{{"acme", "api"}, {"acme", "web"}}, "tok")
	// Assert
	git := set.Bindings[0].Paths
	if len(git) != 4 || git[2] != "/acme/web.git*" {
		t.Errorf("git paths = %v", git)
	}
	if api := set.Bindings[1].Paths; len(api) != 4 || api[2] != "/repos/acme/web" {
		t.Errorf("api paths = %v", api)
	}
}

func TestCleanExtraRepos(t *testing.T) {
	for _, tc := range []struct {
		name, main string
		in         []string
		want       []string
		bad        bool
	}{
		{"trims and drops blanks and duplicates", "https://github.com/a/api", []string{" https://github.com/a/web ", "", "https://github.com/a/web", "https://github.com/a/api.git"}, []string{"https://github.com/a/web"}, false},
		{"needs a main repository", "", []string{"https://github.com/a/web"}, nil, true},
		{"refuses a bad URL", "https://github.com/a/api", []string{"not a url"}, nil, true},
		{"limits the count", "https://github.com/a/api", func() []string {
			var l []string
			for i := 0; i <= MaxExtraRepos; i++ {
				l = append(l, fmt.Sprintf("https://github.com/a/r%d", i))
			}
			return l
		}(), nil, true},
		{"empty is fine without a main", "", nil, []string{}, false},
	} {
		got, bad := cleanExtraRepos(tc.main, tc.in)
		if (bad != "") != tc.bad || (!tc.bad && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("%s: got %v, %q", tc.name, got, bad)
		}
	}
}
