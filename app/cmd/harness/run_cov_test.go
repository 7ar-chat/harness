package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

// hvEnv is a loadable single-model configuration.
func hvEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_DEFAULT_MODEL", "gpt-test")
	t.Setenv("LOCAL_STATE_DIR", t.TempDir())
}

func hvTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestRunFlagAndConfigurationErrorsMapToExitCodes(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		code int
		want string // in stderr
	}{
		{"help exits zero", nil, []string{"-h"}, 0, "-remote"},
		{"unknown flag is a usage error", nil, []string{"-nope"}, 2, "-nope"},
		{"invalid provider fails before any work", map[string]string{"LLM_PROVIDER": "magic"}, []string{"-p", "x"}, 1, "LLM_PROVIDER"},
		{"config-dump of an invalid configuration fails", map[string]string{"LLM_PROVIDER": "magic"}, []string{"-config-dump"}, 1, "LLM_PROVIDER"},
		{"sessions need the store", nil, []string{"-sessions", "-ephemeral"}, 1, "-sessions needs the session store"},
		{"undo of an unsaved conversation needs an id", nil, []string{"-undo", "-ephemeral"}, 1, "-undo needs -resume"},
		{"undo with nothing saved", nil, []string{"-undo"}, 1, "no saved conversation to undo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			hvEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			var out, errb bytes.Buffer

			// Act
			code := run(hvTimeout(t), c.args, strings.NewReader(""), &out, &errb, false, scripted())

			// Assert
			if code != c.code || !strings.Contains(errb.String(), c.want) {
				t.Fatalf("exit %d stderr %q, want exit %d mentioning %q", code, errb.String(), c.code, c.want)
			}
		})
	}
}

func TestRunExplainsAnUnusableSessionStore(t *testing.T) {
	// Arrange: the state directory is a file
	hvEnv(t)
	file := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCAL_STATE_DIR", file)
	var out, errb bytes.Buffer

	// Act
	code := run(hvTimeout(t), []string{"-p", "x"}, strings.NewReader(""), &out, &errb, false, scripted())

	// Assert
	if code != 1 || !strings.Contains(errb.String(), "-ephemeral") {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
}

func TestRunWarnsWhenTheAgentHasNoToolsExceptInJSONMode(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		warn bool
	}{
		{"plain print mode", []string{"-p", "hi", "-ephemeral"}, true},
		{"json keeps stderr clean", []string{"-p", "hi", "-ephemeral", "-json"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			hvEnv(t)
			t.Setenv("SANDBOX_BACKEND", "")
			t.Setenv("OPENSANDBOX_URL", "")
			var out, errb bytes.Buffer

			// Act
			code := run(hvTimeout(t), c.args, strings.NewReader(""), &out, &errb, false, scripted(llmtest.Turn{Chunks: llmtest.Text("ok")}))

			// Assert
			if code != 0 || strings.Contains(errb.String(), "no sandbox") != c.warn {
				t.Fatalf("exit %d, stderr %q, want warning=%v", code, errb.String(), c.warn)
			}
		})
	}
}

func TestRunStopsWhenTheFactoryFails(t *testing.T) {
	hvEnv(t)
	var out, errb bytes.Buffer

	code := run(hvTimeout(t), []string{"-p", "x", "-ephemeral"}, strings.NewReader(""), &out, &errb, false, newHarness)

	if code != 1 || !strings.Contains(errb.String(), "harness:") {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
}

func TestNewHarnessBuildsTheEngineOnceAKeyIsConfigured(t *testing.T) {
	// Arrange
	hvEnv(t)
	t.Setenv("LLM_API_KEY", "sk-test-key-0001")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	h, err := newHarness(cfg, nil)

	// Assert
	if err != nil || h == nil {
		t.Fatalf("newHarness = %v, %v", h, err)
	}
}

func TestSessionsListsTheSavedConversationsNewestFirstWithAPlaceholderTitle(t *testing.T) {
	// Arrange: two saved runs
	hvEnv(t)
	ctx := hvTimeout(t)
	var out, errb bytes.Buffer
	for _, p := range []string{"first question", "second question"} {
		turns := []llmtest.Turn{{Chunks: llmtest.Text("answer to " + p)}, {Chunks: llmtest.Text("T")}}
		if code := run(ctx, []string{"-p", p, "-model", "gpt-test"}, strings.NewReader(""), &out, &errb, false, scripted(turns...)); code != 0 {
			t.Fatalf("seeding: exit %d %s", code, errb.String())
		}
		time.Sleep(5 * time.Millisecond) // distinct update times for the ordering
	}
	out.Reset()

	// Act
	code := run(ctx, []string{"-sessions"}, strings.NewReader(""), &out, &errb, false, scripted())

	// Assert
	got := out.String()
	first, second := strings.Index(got, "answer to first question"), strings.Index(got, "answer to second question")
	if code != 0 || first < 0 || second < 0 || second > first {
		t.Fatalf("exit %d, want the newest conversation first:\n%s", code, got)
	}
}

func TestPrintSessionsShowsAPlaceholderForUntitledAndAMessageWhenThereAreNone(t *testing.T) {
	// Arrange
	var withOne, none bytes.Buffer
	at := time.Date(2025, 3, 4, 5, 6, 0, 0, time.UTC)

	// Act
	printSessions([]client.Session{{ID: "t1", Messages: 3, Preview: "hello there", UpdatedAt: at}}, &withOne)
	printSessions(nil, &none)

	// Assert
	for _, want := range []string{"t1", "2025-03-04 05:06", "  3 msgs", "(untitled)", "hello there"} {
		if !strings.Contains(withOne.String(), want) {
			t.Errorf("listing lacks %q:\n%s", want, withOne.String())
		}
	}
	if none.String() != "no saved conversations\n" {
		t.Errorf("empty listing = %q", none.String())
	}
}

func TestUndoWithoutAnIdRestoresTheMostRecentConversationAndReportsWhenThereIsNoRepository(t *testing.T) {
	// Arrange: one saved conversation, a workspace that is not a git repository
	hvEnv(t)
	ctx := hvTimeout(t)
	var out, errb bytes.Buffer
	turns := []llmtest.Turn{{Chunks: llmtest.Text("ok")}, {Chunks: llmtest.Text("T")}}
	if code := run(ctx, []string{"-p", "hi", "-model", "gpt-test"}, strings.NewReader(""), &out, &errb, false, scripted(turns...)); code != 0 {
		t.Fatalf("seeding: %d %s", code, errb.String())
	}
	t.Setenv("LOCAL_WORKSPACE", t.TempDir())
	errb.Reset()

	// Act
	code := run(ctx, []string{"-undo"}, strings.NewReader(""), &out, &errb, false, scripted())

	// Assert: it got past the thread lookup and failed on the snapshot, not on a missing conversation
	if code != 1 || strings.Contains(errb.String(), "no saved conversation") || errb.Len() == 0 {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
}

func TestUndoFallsBackToTheWorkingDirectoryWithoutALocalWorkspace(t *testing.T) {
	// Arrange
	hvEnv(t)
	t.Setenv("LOCAL_WORKSPACE", "")
	t.Chdir(t.TempDir())
	var out, errb bytes.Buffer

	// Act: an explicit id that has no snapshot in the (non-git) working directory
	code := run(hvTimeout(t), []string{"-undo", "-resume", "ghost", "-ephemeral"}, strings.NewReader(""), &out, &errb, false, scripted())

	// Assert
	if code != 1 || errb.Len() == 0 || out.Len() != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errb.String())
	}
}

func TestModelLookupIsOffWhenPricesAreDisabledAndLoadsTheTableInTheBackgroundOtherwise(t *testing.T) {
	// Arrange
	prices := filepath.Join(t.TempDir(), "prices.json")
	table := `{"price-model":{"litellm_provider":"openai","mode":"chat","max_input_tokens":1000,"input_cost_per_token":0.001,"output_cost_per_token":0.002}}`
	if err := os.WriteFile(prices, []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LLMProvider: "openai"}

	// Act / Assert: disabled
	t.Setenv("HARNESS_MODEL_PRICES", "off")
	if got := modelLookup(context.Background(), cfg); got != nil {
		t.Fatal("a lookup was built although HARNESS_MODEL_PRICES=off")
	}

	// enabled from a file: the table fills in asynchronously
	t.Setenv("HARNESS_MODEL_PRICES", prices)
	t.Setenv("HARNESS_NO_USER_DIR", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lookup := modelLookup(ctx, cfg)
	if lookup == nil {
		t.Fatal("no lookup although prices are enabled")
	}
	deadline := time.Now().Add(10 * time.Second)
	var info client.ModelInfo
	var ok bool
	for time.Now().Before(deadline) {
		if info, ok = lookup("price-model"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok || info.ContextWindow != 1000 || !info.Priced || info.InputPerToken != 0.001 {
		t.Fatalf("lookup = %+v, %v", info, ok)
	}
	if _, ok := lookup("not-in-the-table"); ok {
		t.Error("an unknown model was found")
	}
}

func TestDiscovererAsksTheConfiguredProviderWithItsKeyAndSortsTheIds(t *testing.T) {
	// Arrange
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"zeta"},{"id":"alpha"},{"id":""}]}`))
	}))
	defer srv.Close()
	cfg := &config.Config{LLMProvider: "openai", LLMBaseURL: srv.URL + "/v1", LLMAPIKey: "sk-disc"}

	// Act
	ids, err := discoverer(cfg)(context.Background())

	// Assert
	if err != nil || strings.Join(ids, ",") != "alpha,zeta" {
		t.Fatalf("ids %v err %v", ids, err)
	}
	if gotPath != "/v1/models" || gotAuth != "Bearer sk-disc" {
		t.Errorf("request %s with %q", gotPath, gotAuth)
	}
}

func TestDiscovererReportsAProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()

	_, err := discoverer(&config.Config{LLMProvider: "openai", LLMBaseURL: srv.URL})(context.Background())

	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusFuncDescribesTheConfigurationInEffect(t *testing.T) {
	// Arrange
	cfg := &config.Config{LLMProvider: "openai", LLMProviderName: "zen"}

	// Act
	items, err := statusFunc(cfg)(context.Background())

	// Assert
	by := map[string]string{}
	for _, it := range items {
		by[it.Label] = it.Value
	}
	if err != nil || by["provider"] != "zen (type openai)" || by["api key"] != "not set" || by["permissions"] != "ask (default)" {
		t.Fatalf("err %v items %v", err, by)
	}
	if _, has := by["base url"]; has {
		t.Error("a base url is shown although none is configured")
	}
}

func TestStatusItemsShowTrustAndTheFilesInUse(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	t.Setenv("HAR_HOME", dir)
	t.Setenv("HARNESS_NO_USER_DIR", "")
	if err := os.WriteFile(filepath.Join(dir, tuiConfigFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		LLMProvider: "openai",
		Layers: &config.Layers{
			AuthFile: "/u/auth.json", MCPSources: []string{"/p/.mcp.json"}, ProjectRoot: "/p", Trusted: true, TrustedBy: "trust.json",
			Ignored: []string{"permissions.allow ignored"},
		},
	}

	cfg.AgentProfile = "coding"

	// Act
	items := statusItems(cfg)

	// Assert
	by := map[string]string{}
	for _, it := range items {
		by[it.Label] = it.Value
	}
	for label, want := range map[string]string{
		"profile":          "coding",
		"auth.json · user": "/u/auth.json",
		"tui.json · user":  filepath.Join(dir, tuiConfigFile),
		"mcp file":         "/p/.mcp.json",
		"project":          "/p, trusted (trust.json)",
		"ignored":          "permissions.allow ignored",
	} {
		if by[label] != want {
			t.Errorf("%s = %q, want %q", label, by[label], want)
		}
	}
}

func TestHostOfKeepsOnlySchemeAndHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://u:p@llm.example.com:8443/v1?k=1": "https://llm.example.com:8443",
		"llm.example.com/v1":                      "",
		"":                                        "",
		"http://%zz":                              "",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTUIOptionsReadOnlyTheUserDirectory(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	t.Setenv("HAR_HOME", dir)
	t.Setenv("HARNESS_NO_USER_DIR", "")
	if err := os.WriteFile(filepath.Join(dir, tuiConfigFile), []byte(`{"mouse":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Act
	o := tuiOptions()
	t.Setenv("HARNESS_NO_USER_DIR", "1")
	disabled := tuiOptions()

	// Assert
	if string(o.ConfigData) != `{"mouse":true}` || o.StateDir != dir {
		t.Errorf("options = %q, %q", o.ConfigData, o.StateDir)
	}
	if len(disabled.ConfigData) != 0 || disabled.StateDir != "" {
		t.Errorf("HARNESS_NO_USER_DIR still read the user directory: %+v", disabled)
	}
}

func TestReadKeyWithoutATerminalReadsOneTrimmedLineAndRejectsEmptyInput(t *testing.T) {
	cases := []struct {
		name    string
		stdin   string
		tty     bool
		fromIn  bool
		want    string
		wantErr string
	}{
		{"last line without a newline", "sk-abc", false, false, "sk-abc", ""},
		{"only the first line", " sk-abc \nrest\n", false, true, "sk-abc", ""},
		{"nothing at all", "", false, false, "", "no key on stdin"},
		{"blank line", "   \n", false, false, "", "empty API key"},
		{"a terminal prompt needs a real file", "sk", true, false, "", "cannot prompt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			got, err := readKey(c.fromIn, "p", strings.NewReader(c.stdin), &bytes.Buffer{}, c.tty)

			// Assert
			if c.wantErr == "" && (err != nil || got != c.want) {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
}

func TestReadKeyPromptFailsCleanlyWhenStdinIsNotARealTerminal(t *testing.T) {
	// Arrange: a pipe claims to be a terminal
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	// Act
	key, err := readKey(false, "p", r, &bytes.Buffer{}, true)

	// Assert
	if err == nil || key != "" {
		t.Fatalf("got %q, %v; want an error and no key", key, err)
	}
}

func TestManagementCommandsNeedAHomeDirectory(t *testing.T) {
	// Arrange: no HAR_HOME and no HOME
	t.Setenv("HAR_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("HARNESS_NO_USER_DIR", "")
	for _, args := range [][]string{{"auth", "list"}, {"trust", "list"}, {"untrust", "x"}, {"config", "init"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// Act
			code, out, errOut := cli(t, "", args...)

			// Assert
			if code != 1 || out != "" || !strings.Contains(errOut, "HAR_HOME") {
				t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
			}
		})
	}
	// `config path` still reports the situation rather than failing
	if code, out, _ := cli(t, "", "config", "path"); code != 0 || !strings.Contains(out, "user dir:    (none)") {
		t.Errorf("config path: %d %q", code, out)
	}
}

func TestAuthCommandUsageErrors(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
		args  []string
		want  string
	}{
		{"unknown action", "", []string{"auth", "frobnicate"}, "usage:"},
		{"login without a provider", "k\n", []string{"auth", "login", "-key-stdin"}, "usage: harness auth login"},
		{"login with a second positional", "k\n", []string{"auth", "login", "a", "-key-stdin", "--", "b"}, "usage: harness auth login"},
		{"login with an unknown flag", "k\n", []string{"auth", "login", "a", "-bogus"}, "-bogus"},
		{"remove without a provider", "", []string{"auth", "remove"}, "usage: harness auth remove"},
		{"remove with two providers", "", []string{"auth", "remove", "a", "b"}, "usage: harness auth remove"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			harHome(t)

			code, out, errOut := cli(t, c.stdin, c.args...)

			if code != 2 || out != "" || !strings.Contains(errOut, c.want) {
				t.Fatalf("exit %d stdout %q stderr %q, want exit 2 mentioning %q", code, out, errOut, c.want)
			}
		})
	}
}

func TestAuthListSaysWhenNoKeyIsStored(t *testing.T) {
	harHome(t)

	code, out, _ := cli(t, "", "auth", "list")

	if code != 0 || !strings.Contains(out, "no keys stored") {
		t.Fatalf("exit %d stdout %q", code, out)
	}
}

func TestAuthLoginWithoutAKeyOnStdinFails(t *testing.T) {
	home := harHome(t)

	code, _, errOut := cli(t, "", "auth", "login", "openai", "-key-stdin")

	if _, err := os.Stat(filepath.Join(home, "auth.json")); code != 1 || !strings.Contains(errOut, "no key on stdin") || err == nil {
		t.Fatalf("exit %d stderr %q auth.json err %v", code, errOut, err)
	}
}

func TestTrustCommandErrorsAndHelp(t *testing.T) {
	// Arrange
	harHome(t)
	other, _ := filepath.EvalSymlinks(t.TempDir())
	file := filepath.Join(other, "f.txt")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(other) // not inside a project
	t.Setenv("LOCAL_WORKSPACE", "")

	// Act
	codeHelp, outHelp, _ := cli(t, "", "trust", "--help")
	codeList, outList, _ := cli(t, "", "trust", "list")
	codeMany, _, errMany := cli(t, "", "trust", "a", "b")
	codeFile, _, errFile := cli(t, "", "trust", file)
	codeMCPArgs, _, errMCPArgs := cli(t, "", "trust", "mcp", "a", "b")
	codeUntrustMCP, _, errUntrustMCP := cli(t, "", "untrust", "mcp")
	codeMCPNoProject, _, errMCPNoProject := cli(t, "", "trust", "mcp")

	// Assert
	if codeHelp != 0 || !strings.Contains(outHelp, "harness trust [path]") {
		t.Errorf("help: %d %q", codeHelp, outHelp)
	}
	if codeList != 0 || !strings.Contains(outList, "no trusted projects") {
		t.Errorf("list: %d %q", codeList, outList)
	}
	if codeMany != 2 || !strings.Contains(errMany, "too many arguments") || !strings.Contains(errMany, "usage:") {
		t.Errorf("many: %d %q", codeMany, errMany)
	}
	if codeFile != 2 || !strings.Contains(errFile, "not a directory") {
		t.Errorf("file: %d %q", codeFile, errFile)
	}
	if codeMCPArgs != 2 || !strings.Contains(errMCPArgs, "usage:") || codeUntrustMCP != 2 || !strings.Contains(errUntrustMCP, "usage:") {
		t.Errorf("mcp usage: %d %q / %d %q", codeMCPArgs, errMCPArgs, codeUntrustMCP, errUntrustMCP)
	}
	if codeMCPNoProject != 1 || !strings.Contains(errMCPNoProject, "not trusted") {
		t.Errorf("mcp in an untrusted directory: %d %q", codeMCPNoProject, errMCPNoProject)
	}
}

func TestTrustMCPReportsWhatCannotBeApprovedOrWithdrawn(t *testing.T) {
	// Arrange: a trusted project with no stdio server waiting
	harHome(t)
	project(t, map[string]string{".har/config.yml": "limits:\n  max_steps: 3\n"})
	if code, _, e := cli(t, "", "trust"); code != 0 {
		t.Fatal(e)
	}

	// Act
	codeNothing, _, errNothing := cli(t, "", "trust", "mcp")
	codeRevoke, _, errRevoke := cli(t, "", "untrust", "mcp", "ghost")

	// Assert
	if codeNothing != 1 || !strings.Contains(errNothing, "waiting for approval") {
		t.Errorf("nothing pending: %d %q", codeNothing, errNothing)
	}
	if codeRevoke != 1 || !strings.Contains(errRevoke, `no approved MCP server "ghost"`) {
		t.Errorf("revoke: %d %q", codeRevoke, errRevoke)
	}
}

func TestTrustMCPOnlyApprovesTheServerNamed(t *testing.T) {
	// Arrange: two pending stdio servers
	home := harHome(t)
	t.Setenv("SANDBOX_BACKEND", "local")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yml"), []byte("mcp:\n  allow_stdio: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	project(t, map[string]string{".mcp.json": `{"mcpServers":{"fs":{"command":"echo","args":["a"]},"db":{"command":"echo","args":["b"]}}}`})
	if code, _, e := cli(t, "", "trust"); code != 0 {
		t.Fatal(e)
	}

	// Act
	code, out, errOut := cli(t, "", "trust", "mcp", "db")
	cfg, err := config.LoadWith(config.LoadOptions{})

	// Assert
	if code != 0 || !strings.Contains(out, "approved MCP server db") || strings.Contains(out, "server fs") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
	}
	if err != nil || len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Name != "db" || len(cfg.Layers.MCPPending) != 1 {
		t.Fatalf("after approving db: servers %v pending %v err %v", cfg.MCPServers, cfg.Layers.MCPPending, err)
	}
}

func TestConfigCommandUsageAndFailures(t *testing.T) {
	// Arrange
	home := harHome(t)

	// Act
	codeNoArg, _, errNoArg := cli(t, "", "config")
	codeUnknown, _, errUnknown := cli(t, "", "config", "frobnicate")

	// Assert
	if codeNoArg != 2 || !strings.Contains(errNoArg, "harness config init") {
		t.Errorf("no arg: %d %q", codeNoArg, errNoArg)
	}
	if codeUnknown != 2 || !strings.Contains(errUnknown, "unknown command: harness config frobnicate") {
		t.Errorf("unknown: %d %q", codeUnknown, errUnknown)
	}

	// init cannot create the directory below a regular file
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAR_HOME", filepath.Join(blocker, "har"))
	if code, _, e := cli(t, "", "config", "init"); code != 1 || e == "" {
		t.Errorf("init below a file: %d %q", code, e)
	}
	// init does not overwrite a config.yml that is there
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAR_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.yml"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := cli(t, "", "config", "init"); code != 0 {
		t.Fatal("init failed on an existing file")
	}
	if b, _ := os.ReadFile(filepath.Join(home, "config.yml")); string(b) != "mine\n" {
		t.Errorf("config.yml overwritten: %q", b)
	}
}

// --- remote mode ---

// hvDurableRemote is remoteServer over a durable store, which is what lists sessions.
func hvDurableRemote(t *testing.T, ctx context.Context, turns ...llmtest.Turn) *httptest.Server {
	t.Helper()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	st, err := harness.OpenLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h, err := harness.New(harness.Options{Config: cfg, Store: st, Models: llmtest.Factory{M: llmtest.New(turns...)}, Backend: sandboxtest.NewBackend()})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = h.Run(ctx) }()
	srv := httptest.NewServer(httpapi.New(h, httpapi.Options{Token: "s3cret", Models: cfg.LLMModels, DefaultModel: "gpt-test"}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRemoteListsTheServersSessionsAndContinuesTheLatestOne(t *testing.T) {
	// Arrange: one conversation exists on the server
	ctx := hvTimeout(t)
	srv := hvDurableRemote(t, ctx,
		llmtest.Turn{Chunks: llmtest.Text("first answer")}, llmtest.Turn{Chunks: llmtest.Text("T")},
		llmtest.Turn{Chunks: llmtest.Text("second answer")}, llmtest.Turn{Chunks: llmtest.Text("T")})
	t.Setenv("HARNESS_REMOTE_TOKEN", "s3cret")
	var out, errb bytes.Buffer
	if code := run(ctx, []string{"-remote", srv.URL, "-p", "hello"}, strings.NewReader(""), &out, &errb, false, nil); code != 0 {
		t.Fatalf("seeding: %d %s", code, errb.String())
	}

	// Act
	var list bytes.Buffer
	codeList := run(ctx, []string{"-remote", srv.URL, "-sessions"}, strings.NewReader(""), &list, &errb, true, nil)
	out.Reset()
	codeCont := run(ctx, []string{"-remote", srv.URL, "-continue", "-p", "again"}, strings.NewReader(""), &out, &errb, false, nil)
	var after bytes.Buffer
	run(ctx, []string{"-remote", srv.URL, "-sessions"}, strings.NewReader(""), &after, &errb, true, nil)

	// Assert
	if codeList != 0 || !strings.Contains(list.String(), "first answer") || strings.Count(list.String(), " msgs") != 1 {
		t.Fatalf("list exit %d:\n%s\nstderr %s", codeList, list.String(), errb.String())
	}
	if codeCont != 0 || out.String() != "second answer\n" {
		t.Fatalf("continue exit %d stdout %q stderr %q", codeCont, out.String(), errb.String())
	}
	if strings.Count(after.String(), " msgs") != 1 {
		t.Errorf("-continue opened a second conversation:\n%s", after.String())
	}
}

func TestRemoteResumeOfAnUnknownConversationFails(t *testing.T) {
	// Arrange
	ctx := hvTimeout(t)
	srv := remoteServer(t, ctx)
	t.Setenv("HARNESS_REMOTE_TOKEN", "s3cret")
	var out, errb bytes.Buffer

	// Act
	code := run(ctx, []string{"-remote", srv.URL, "-resume", "no-such-thread", "-p", "x"}, strings.NewReader(""), &out, &errb, false, nil)

	// Assert
	if code != 1 || out.Len() != 0 || errb.Len() == 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errb.String())
	}
}

func TestRemoteSessionListFailsWithAWrongToken(t *testing.T) {
	ctx := hvTimeout(t)
	srv := remoteServer(t, ctx)
	t.Setenv("HARNESS_REMOTE_TOKEN", "wrong")
	var out, errb bytes.Buffer

	code := runRemote(ctx, options{remote: srv.URL, list: true}, "", false, &out, &errb)

	if code != 1 || out.Len() != 0 || !strings.Contains(errb.String(), "unauthenticated") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errb.String())
	}
}

func TestRemoteWithoutAnyModelFails(t *testing.T) {
	// Arrange: a server that names no default model
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[],"default":""}`))
	}))
	defer srv.Close()
	var out, errb bytes.Buffer

	// Act
	code := runRemote(hvTimeout(t), options{remote: srv.URL}, "hi", true, &out, &errb)

	// Assert
	if code != 1 || !strings.Contains(errb.String(), "no model") {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
}

func TestRemoteUnreachableServerNamesTheAddress(t *testing.T) {
	// Arrange: a closed port
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	var out, errb bytes.Buffer

	// Act
	code := runRemote(hvTimeout(t), options{remote: url}, "hi", true, &out, &errb)

	// Assert
	if code != 1 || !strings.Contains(errb.String(), url) {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
}

// --- attach / pickThread ---

type hvClient struct {
	client.Client
	sessions []client.Session
	err      error
	created  int
}

func (c *hvClient) Sessions(context.Context, int) ([]client.Session, error) { return c.sessions, c.err }
func (c *hvClient) GetThread(_ context.Context, id string) (*client.Thread, error) {
	return &client.Thread{ID: id}, nil
}
func (c *hvClient) CreateThread(context.Context, string) (*client.Thread, error) {
	c.created++
	return &client.Thread{ID: "new"}, nil
}

func TestPickThreadChoosesTheConversationToContinue(t *testing.T) {
	ctx := context.Background()
	someErr := context.DeadlineExceeded
	cases := []struct {
		name    string
		c       *hvClient
		o       options
		saved   bool
		want    string
		created int
		wantErr string
	}{
		{"new conversation by default", &hvClient{}, options{}, true, "new", 1, ""},
		{"resume by id", &hvClient{}, options{resume: "t9"}, true, "t9", 0, ""},
		{"continue the most recent", &hvClient{sessions: []client.Session{{ID: "latest"}}}, options{cont: true}, true, "latest", 0, ""},
		{"continue with nothing saved starts a new one", &hvClient{}, options{cont: true}, true, "new", 1, ""},
		{"continue when listing fails", &hvClient{err: someErr}, options{cont: true}, true, "", 0, someErr.Error()},
		{"continue without a store", &hvClient{}, options{cont: true}, false, "", 0, "-ephemeral"},
		{"resume without a store", &hvClient{}, options{resume: "x"}, false, "", 0, "-ephemeral"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			th, err := pickThread(ctx, c.c, c.saved, c.o)

			// Assert
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || th.ID != c.want || c.c.created != c.created {
				t.Fatalf("thread %v err %v created %d, want %q / %d", th, err, c.c.created, c.want, c.created)
			}
		})
	}
}

func TestAttachReportsAThreadFailureAndTheEndOfAnInterruptedTerminalUI(t *testing.T) {
	// Arrange
	hvEnv(t)
	t.Setenv("HAR_HOME", t.TempDir())
	var out, errb bytes.Buffer

	// Act: no store and -continue
	codeNoStore := attach(context.Background(), &hvClient{}, options{cont: true}, "", false, false, &out, &errb)
	// the UI is started on a context that is already over: it ends with an error, not a hang
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	be := &hvClient{}
	codeUI := attach(ctx, be, options{auto: true, remote: "http://x", connector: nil}, "", false, true, &out, &errb)

	// Assert
	if codeNoStore != 1 || !strings.Contains(errb.String(), "-ephemeral") {
		t.Errorf("no store: %d %q", codeNoStore, errb.String())
	}
	if codeUI != 1 || be.created != 1 {
		t.Errorf("terminal UI on a cancelled context: exit %d (threads created %d)", codeUI, be.created)
	}
}
