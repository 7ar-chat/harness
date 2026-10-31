package harness_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/memory"
)

// nilProvider is a tool provider used as a typed nil pointer.
type nilProvider struct{}

func (*nilProvider) Name() string { return "nil" }
func (*nilProvider) Tools(context.Context, tools.RunContext) ([]ai.BaseTool, func(), error) {
	panic("a typed nil provider must never be asked for tools")
}

func TestAddToolsIgnoresNilProvidersEvenTypedOnes(t *testing.T) {
	// Arrange
	var typedNil *nilProvider
	feature := harness.FeatureFunc("nils", func(b *harness.Builder) error {
		b.AddTools(nil, typedNil)
		return nil
	})
	h := buildFacade(t, nil, llmtest.New(), func(o *harness.Options) { o.Features = append(harness.ProfileMinimal(), feature) })

	// Act
	names := toolNamesOf(t, h)

	// Assert
	if !contains(names, "update_plan") || contains(names, "nil") {
		t.Fatalf("tools %v", names)
	}
}

func TestCodecheckNeedsTheSandboxAndValidCommands(t *testing.T) {
	// Arrange
	cases := []struct {
		name     string
		env      map[string]string
		features []harness.Feature
		want     string
	}{
		{"without the sandbox feature", nil, []harness.Feature{harness.Codecheck(true)}, `feature "codecheck" requires feature "sandbox"`},
		{"a check that is not .ext=command", map[string]string{"CODE_CHECKS": "go=gofmt -l {path}"}, []harness.Feature{harness.Sandbox(), harness.Codecheck(false)}, "CODE_CHECKS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			_, err := harness.New(harness.Options{Config: featureConfig(t, c.env), Models: llmtest.Factory{M: llmtest.New()},
				Backend: sandboxtest.NewBackend(), Features: c.features})

			// Assert
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
		})
	}
}

func TestVerifyAndWorkspaceNeedTheArtifactsFeature(t *testing.T) {
	// Arrange / Act
	_, err := harness.New(harness.Options{Config: featureConfig(t, nil), Models: llmtest.Factory{M: llmtest.New()},
		Backend: sandboxtest.NewBackend(), Features: []harness.Feature{harness.Sandbox(), harness.Verify()}})

	// Assert
	if err == nil || !strings.Contains(err.Error(), `feature "verify" requires feature "artifacts"`) {
		t.Fatalf("err %v", err)
	}
}

func TestArtifactsRefusesToShareASlotAnotherFeatureAlreadyTook(t *testing.T) {
	// Arrange: the artifacts feature sets the answer checker, the image attacher and the purger itself
	cases := []struct {
		name string
		take func(*harness.Builder) error
		want string
	}{
		{"answer checker", func(b *harness.Builder) error { return b.SetAnswers(nopAnswers{}) }, "an answer checker is already set"},
		{"image attacher", func(b *harness.Builder) error { return b.SetImages(nopImages{}) }, "an image attacher is already set"},
		{"purger", func(b *harness.Builder) error { return b.SetPurger(nopPurger{}) }, "a purger is already set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			_, err := newFeatureHarness(t, harness.FeatureFunc("squatter", c.take), harness.Artifacts())

			// Assert
			if err == nil || !strings.Contains(err.Error(), `feature "artifacts"`) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want it to name feature \"artifacts\" and say %q", err, c.want)
			}
		})
	}
}

func TestVerifyRefusesASecondDeliverableChecker(t *testing.T) {
	// Arrange / Act
	_, err := newFeatureHarness(t,
		harness.FeatureFunc("squatter", func(b *harness.Builder) error { return b.SetChecker(nopChecker{}, nil) }),
		harness.Artifacts(), harness.Verify())

	// Assert
	if err == nil || !strings.Contains(err.Error(), `feature "verify"`) || !strings.Contains(err.Error(), "a deliverable checker is already set") {
		t.Fatalf("err %v", err)
	}
}

func TestSkillsChainOntoTheAcquireHookOfEarlierFeatures(t *testing.T) {
	// Arrange: a feature installed before Skills already hooks the moment a sandbox is acquired by a tool
	var mu sync.Mutex
	acquired := 0
	earlier := harness.FeatureFunc("earlier", func(b *harness.Builder) error {
		b.SandboxTools(func(o *sandboxtools.Options) {
			o.OnAcquire = func(context.Context, tools.RunContext, sandbox.Sandbox) { mu.Lock(); acquired++; mu.Unlock() }
		})
		return nil
	})
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("title")}
		}
		if c.Messages[len(c.Messages)-1].Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("done")}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "l1", "list_files", `{"path":"/workspace"}`), "tool_calls")}}
	})
	h, ctx := facade(t, nil, model, func(o *harness.Options) {
		o.Features = []harness.Feature{harness.Sandbox(), earlier, harness.Skills()}
	})
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	exchange(ctx, t, h, "alice", th.ID, "list", harness.RunOptions{})

	// Assert
	mu.Lock()
	defer mu.Unlock()
	if acquired != 1 {
		t.Fatalf("the earlier feature's OnAcquire ran %d times, want 1", acquired)
	}
}

func TestSkillsOfTheProjectAreReadFromTheWorkingDirectoryWithTheLocalBackend(t *testing.T) {
	// Arrange: no LOCAL_WORKSPACE and no .har directory: the project is the current directory
	cwd := t.TempDir()
	writeSkillFile(t, filepath.Join(cwd, ".agents", "skills"), "deploy-notes", "How we deploy.")
	t.Chdir(cwd)
	env := map[string]string{"SANDBOX_BACKEND": "local", "LOCAL_WORKSPACE": "", "LOCAL_STATE_DIR": t.TempDir()}

	// Act
	local := buildFacade(t, env, llmtest.New(), func(o *harness.Options) {
		o.Backend = nil
		o.Features = []harness.Feature{harness.Sandbox(), harness.Skills()}
	})
	remote := buildFacade(t, map[string]string{"SANDBOX_BACKEND": "", "LOCAL_WORKSPACE": ""}, llmtest.New(), func(o *harness.Options) { o.Features = []harness.Feature{harness.Sandbox(), harness.Skills()} })

	// Assert: only the local backend reads the host's project directories
	if !local.Skills.Exists(context.Background(), "deploy-notes") {
		t.Fatal("the local backend must find the project's skill in the working directory")
	}
	if remote.Skills.Exists(context.Background(), "deploy-notes") {
		t.Fatal("a remote sandbox must not read the host's project skills")
	}
}

func TestSkillsOfTheProjectFolderAreFoundThroughItsHarDirectory(t *testing.T) {
	// Arrange: the working directory has a .har folder: it is the project root even when a workspace is not set
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, filepath.Join(cwd, ".har", "skills"), "release", "How we release.")
	t.Chdir(cwd)
	env := map[string]string{"SANDBOX_BACKEND": "local", "LOCAL_WORKSPACE": "", "LOCAL_STATE_DIR": t.TempDir()}

	// Act
	h := buildFacade(t, env, llmtest.New(), func(o *harness.Options) {
		o.Backend = nil
		o.Features = []harness.Feature{harness.Sandbox(), harness.Skills()}
	})

	// Assert
	if !h.Skills.Exists(context.Background(), "release") {
		t.Fatal("the project's .har/skills must be discovered")
	}
}

func writeSkillFile(t *testing.T, base, name, description string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: \"" + description + "\"\n---\n\nText.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCodingProfileOffersLoadSkillOnlyToRunsThatHaveUserSkills(t *testing.T) {
	// Arrange
	extra := fstest.MapFS{"house-style/SKILL.md": {Data: []byte("---\nname: house-style\ndescription: \"Our style.\"\n---\n\nBody\n")}}
	plain := buildFacade(t, nil, llmtest.New(), func(o *harness.Options) { o.Features = harness.ProfileCoding() })
	withHost := buildFacade(t, nil, llmtest.New(), func(o *harness.Options) { o.Features = harness.ProfileCoding(); o.ExtraSkills = extra })
	ctx := context.Background()
	thread, _ := plain.NewThread(ctx, "alice", "")
	_, _, err := plain.Store.CreateRun(ctx, store.NewRun{RunID: "run-explicit", ThreadID: thread.ID, UserLogin: "alice", Model: "gpt-test",
		Message: store.Message{Role: store.RoleUser, Content: "x"}, Skills: []string{"pdf"}})
	if err != nil {
		t.Fatal(err)
	}
	toolsOf := func(h *harness.Harness, runID string) []string {
		list, cleanup, err := h.Agent.Tools.Tools(ctx, tools.RunContext{ThreadID: thread.ID, RunID: runID, Login: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		var names []string
		for _, tl := range list {
			info, _ := tl.Info(ctx)
			names = append(names, info.Name)
		}
		return names
	}

	// Act
	none := toolsOf(plain, "run-none")
	requested := toolsOf(plain, "run-explicit")
	host := toolsOf(withHost, "run-none")

	// Assert
	if contains(none, "load_skill") {
		t.Fatalf("a coding run with only built-in skills must not carry load_skill: %v", none)
	}
	if !contains(requested, "load_skill") {
		t.Fatalf("a run that explicitly requested a skill needs load_skill: %v", requested)
	}
	if !contains(host, "load_skill") {
		t.Fatalf("a host skill counts as a user skill: %v", host)
	}
}

// embeddingServer records the embedding requests it gets.
type embeddingServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []embeddingRequest
	got  chan struct{}
}

type embeddingRequest struct{ path, auth, model string }

func newEmbeddingServer(t *testing.T) *embeddingServer {
	t.Helper()
	e := &embeddingServer{got: make(chan struct{}, 100)}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		e.reqs = append(e.reqs, embeddingRequest{r.URL.Path, r.Header.Get("Authorization"), body.Model})
		e.mu.Unlock()
		data := make([]map[string]any, len(body.Input))
		for i := range body.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		e.got <- struct{}{}
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *embeddingServer) first(t *testing.T) embeddingRequest {
	t.Helper()
	<-e.got
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reqs[0]
}

func memoryHarness(t *testing.T, env map[string]string, mod func(*harness.Options)) (*harness.Harness, context.Context) {
	t.Helper()
	return facade(t, env, llmtest.New(), func(o *harness.Options) {
		o.Features = []harness.Feature{harness.Sandbox(), harness.Memory()}
		if mod != nil {
			mod(o)
		}
	})
}

func remember(ctx context.Context, t *testing.T, h *harness.Harness, login, content string) memory.Result {
	t.Helper()
	return h.Memory.Create(ctx, memory.Writer{Login: login}, memory.CreateInput{Content: content})
}

func TestMemoryEmbeddingsFallBackToTheLLMEndpointAndKey(t *testing.T) {
	// Arrange: the embedding endpoint and key are not set: the LLM's are used
	srv := newEmbeddingServer(t)
	h, ctx := memoryHarness(t, map[string]string{
		"HARNESS_MEMORY_EMBEDDING_BACKEND": "openai", "LLM_BASE_URL": srv.URL + "/llm", "LLM_API_KEY": "llm-key",
		"HARNESS_MEMORY_EMBEDDING_MODEL": "embed-x",
	}, nil)

	// Act
	res := remember(ctx, t, h, "alice", "the staging cluster is called aurora")
	req := srv.first(t)

	// Assert
	if res.IsError {
		t.Fatalf("create: %v", res.Data)
	}
	if req.path != "/llm/embeddings" || req.auth != "Bearer llm-key" || req.model != "embed-x" {
		t.Fatalf("embedding request %+v", req)
	}
}

func TestMemoryEmbeddingsPreferTheirOwnEndpointAndKey(t *testing.T) {
	// Arrange
	srv := newEmbeddingServer(t)
	h, ctx := memoryHarness(t, map[string]string{
		"HARNESS_MEMORY_EMBEDDING_BACKEND": "OpenAI", "LLM_BASE_URL": "http://127.0.0.1:1/never", "LLM_API_KEY": "llm-key",
		"HARNESS_MEMORY_EMBEDDING_URL": srv.URL + "/emb", "HARNESS_MEMORY_EMBEDDING_KEY": "emb-key",
	}, nil)

	// Act
	remember(ctx, t, h, "alice", "deploys happen on tuesdays")
	req := srv.first(t)

	// Assert
	if req.path != "/emb/embeddings" || req.auth != "Bearer emb-key" || req.model != "text-embedding-3-small" {
		t.Fatalf("embedding request %+v", req)
	}
}

func TestMemoryWithTheSimpleBackendCallsNoEmbeddingEndpoint(t *testing.T) {
	// Arrange
	srv := newEmbeddingServer(t)
	h, ctx := memoryHarness(t, map[string]string{"LLM_BASE_URL": srv.URL}, nil)

	// Act
	res := remember(ctx, t, h, "alice", "the cache is redis")
	found := h.Memory.Search(ctx, "alice", memory.SearchInput{Query: "cache"})

	// Assert
	if res.IsError || found.IsError {
		t.Fatalf("create %v search %v", res.Data, found.Data)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.reqs) != 0 {
		t.Fatalf("the simple backend made %d embedding requests", len(srv.reqs))
	}
}

func TestMemoryMaxEntriesZeroMeansNoLimitAndAPositiveValueIsEnforced(t *testing.T) {
	// Arrange
	unlimited, uctx := memoryHarness(t, map[string]string{"HARNESS_MEMORY_MAX_ENTRIES": "0"}, nil)
	capped, cctx := memoryHarness(t, map[string]string{"HARNESS_MEMORY_MAX_ENTRIES": "2"}, nil)

	// Act
	var unlimitedErrs, cappedErrs []string
	for _, c := range []string{"alpha entry", "bravo entry", "charlie entry", "delta entry"} {
		if r := remember(uctx, t, unlimited, "alice", c); r.IsError {
			unlimitedErrs = append(unlimitedErrs, c)
		}
		if r := remember(cctx, t, capped, "alice", c); r.IsError {
			cappedErrs = append(cappedErrs, c)
		}
	}

	// Assert
	if len(unlimitedErrs) != 0 {
		t.Fatalf("MAX_ENTRIES=0 must not limit the memory, refused %v", unlimitedErrs)
	}
	if len(cappedErrs) != 2 {
		t.Fatalf("MAX_ENTRIES=2 must refuse the 3rd and 4th entry, refused %v", cappedErrs)
	}
}

func TestMemoryExtractionProposesAPendingMemoryUnderTheHostsMemoryKey(t *testing.T) {
	// Arrange: the model answers the conversation, then, asked to curate memory, proposes one durable fact
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Messages) > 0 && strings.HasPrefix(c.Messages[0].Content, "You curate the long-term memory") {
			return llmtest.Turn{Chunks: llmtest.Text(`{"memories":[{"op":"add","kind":"decisions","audience":"project","title":"Storage","content":"The project stores its data in Postgres 16.","reason":"decided in chat"}]}`)}
		}
		return llmtest.Turn{Chunks: llmtest.Text("Noted, Postgres 16 it is.")}
	})
	h, ctx := facade(t, map[string]string{"HARNESS_MEMORY_EXTRACT": "true"}, model, func(o *harness.Options) {
		o.Features = []harness.Feature{harness.Sandbox(), harness.Memory()}
		o.MemoryKey = func(_ context.Context, login, threadID string) string { return "project:p1" }
		o.MemoryShared = func(key string) bool { return strings.HasPrefix(key, "project:") }
	})
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	exchange(ctx, t, h, "alice", th.ID, "We decided that the project stores all its data in Postgres 16.", harness.RunOptions{})
	var pending []*store.MemoryEntry
	eventually(ctx, t, "the extracted memory", func() bool {
		pending, _ = h.Store.ListMemoryEntries(ctx, "project:p1", store.MemoryPending)
		return len(pending) > 0
	})

	// Assert
	if len(pending) != 1 || !strings.Contains(pending[0].Content, "Postgres 16") {
		t.Fatalf("pending memories %+v", pending)
	}
	if own, _ := h.Store.ListMemoryEntries(ctx, "alice", store.MemoryPending, store.MemoryActive); len(own) != 0 {
		t.Fatalf("the memory must live under the host's key, alice has %d entries", len(own))
	}
}

func TestMemoryWithoutExtractionAddsNoExtractor(t *testing.T) {
	// Arrange
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Messages) > 0 && strings.HasPrefix(c.Messages[0].Content, "You curate the long-term memory") {
			t.Error("the extraction prompt was sent although HARNESS_MEMORY_EXTRACT is off")
		}
		return llmtest.Turn{Chunks: llmtest.Text("Noted.")}
	})
	h, ctx := facade(t, nil, model, func(o *harness.Options) { o.Features = []harness.Feature{harness.Sandbox(), harness.Memory()} })
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	exchange(ctx, t, h, "alice", th.ID, "We decided that the project stores all its data in Postgres 16.", harness.RunOptions{})

	// Assert
	if pending, _ := h.Store.ListMemoryEntries(ctx, "alice", store.MemoryPending); len(pending) != 0 {
		t.Fatalf("pending %+v", pending)
	}
}

func TestPendingApprovalsExpireThroughTheHarnessSweep(t *testing.T) {
	// Arrange: a risky call paused an interactive run; the approval is older than APPROVAL_TTL_HOURS
	st := memstore.New()
	model := askingModel("execute_command", `{"command":"echo hi"}`)
	h, ctx := facade(t, map[string]string{"APPROVAL_TTL_HOURS": "1"}, model, func(o *harness.Options) { o.Store = st })
	th, _ := h.NewThread(ctx, "alice", "")
	paused := exchange(ctx, t, h, "alice", th.ID, "say hi", harness.RunOptions{Interactive: true})
	intr := interruptsOf(t, waitRun(ctx, t, h, paused))[0]
	eventually(ctx, t, "the run to be recorded as finished", func() bool {
		r, _ := st.GetRun(ctx, paused.ID)
		return r.Status == store.StatusCompleted
	})
	st.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }

	// Act
	h.Runtime.Sweep(ctx)

	// Assert
	a, err := st.GetApproval(ctx, intr.ID)
	if err != nil || a.Status != store.ApprovalExpired {
		t.Fatalf("approval %+v err %v", a, err)
	}
}

func TestNewRequiresAConfiguration(t *testing.T) {
	// Arrange / Act
	h, err := harness.New(harness.Options{})

	// Assert
	if h != nil || err == nil || !strings.Contains(err.Error(), "Config is required") {
		t.Fatalf("harness %v err %v", h, err)
	}
}
