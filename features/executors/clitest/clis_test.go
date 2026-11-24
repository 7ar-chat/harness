package clitest_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/executors"
	exrun "github.com/7ar-chat/harness/features/executors"
	"github.com/7ar-chat/harness/features/executors/clitest"
)

// The tests here run the REAL CLIs through the real runner (features/executors.Provider.Run, with the builtin
// executor commands and parsers). They are skipped unless HARNESS_REAL_CLIS names a directory laid out as
//
//	<dir>/claude/node_modules/.bin/claude
//	<dir>/codex/node_modules/.bin/codex
//	<dir>/omp/node_modules/.bin/omp
//
// (npm installs of @anthropic-ai/claude-code and @openai/codex, a bun install of @oh-my-pi/pi-coding-agent). They
// talk only to fake model servers on 127.0.0.1, so they cost nothing and need no key; each CLI runs with a clean
// environment (PATH, a throwaway HOME and what the executor declares) in a temporary git repository.
// HARNESS_UPDATE_FIXTURES=1 rewrites core/executors/testdata/real/ from the real output of the runs.
func cliDir(t *testing.T, name string) string {
	t.Helper()
	root := os.Getenv("HARNESS_REAL_CLIS")
	if root == "" {
		t.Skip("set HARNESS_REAL_CLIS to run the real CLIs against the fake model servers")
	}
	bin := filepath.Join(root, name, "node_modules", ".bin")
	if _, err := os.Stat(filepath.Join(bin, name)); err != nil {
		t.Skipf("%s is not installed under %s", name, root)
	}
	return bin
}

const helloContent = "hello from the scripted model\n"

// writeThenSummarize is the scripted conversation of every CLI: turn 1 writes hello.txt, once a tool result is back
// the model answers with a summary (and reports usage).
func writeThenSummarize(summary string) clitest.Script {
	return func(r *clitest.Request) clitest.Step {
		if len(r.ToolResults) == 0 {
			return clitest.Step{Text: "I will create hello.txt.", Tools: []clitest.ToolUse{clitest.WriteFileStep(r, "hello.txt", helloContent)}, In: 2100, Out: 87}
		}
		return clitest.Step{Text: summary, In: 2300, Out: 41, CacheRead: 1000}
	}
}

// sleepy has the model start a long command (to test the timeout and the cancel).
func sleepy(r *clitest.Request) clitest.Step {
	if len(r.ToolResults) == 0 {
		return clitest.Step{Text: "Running the slow job.", Tools: []clitest.ToolUse{clitest.ShellStep(r, "echo start; sleep 61; echo end")}}
	}
	return clitest.Step{Text: "finished"}
}

func rejected(*clitest.Request) clitest.Step { return clitest.Step{Status: 401} }

// rig is one CLI wired to one fake server.
type rig struct {
	sb    *clitest.Sandbox
	srv   *clitest.Server
	creds map[string]string
	name  string // executor name
	bin   string // binary
}

func newRig(t *testing.T, cli string, script clitest.Script) *rig {
	t.Helper()
	bin := cliDir(t, cli)
	sb := clitest.NewWorkspace(t, bin)
	var srv *clitest.Server
	r := &rig{sb: sb}
	switch cli {
	case "claude":
		srv = clitest.NewAnthropic(script)
		r.name, r.bin = executors.ClaudeCode, "claude"
		r.creds = map[string]string{"ANTHROPIC_API_KEY": "test-key", "ANTHROPIC_BASE_URL": srv.Base()}
		sb.Base["CLAUDE_CODE_MAX_RETRIES"] = "1" // a rejected key is otherwise retried for three minutes
	case "codex":
		srv = clitest.NewResponses(script)
		r.name, r.bin = executors.Codex, "codex"
		// The key and the base URL are what a user seals for the executor; the model is pinned in the CLI's own
		// config because the default model of this version needs a different tool shape than the fake serves.
		sb.Base["CODEX_HOME"] = sb.Home() + "/.codex"
		sb.HomeFile(t, ".codex/config.toml", "model = \"gpt-5.1-codex\"\n")
		r.creds = map[string]string{"OPENAI_API_KEY": "test-key", "OPENAI_BASE_URL": srv.Base() + "/v1"}
	case "omp":
		srv = clitest.NewOpenAIChat(script)
		r.name, r.bin = executors.OMP, "omp"
		sb.HomeFile(t, ".omp/agent/models.yml", "providers:\n  fake:\n    baseUrl: "+srv.Base()+"/v1\n    apiKey: test-key\n    api: openai-completions\n    models:\n      - id: fake-model\n        name: Fake\n")
		sb.HomeFile(t, ".omp/agent/config.yml", "modelRoles:\n  default: fake/fake-model\n")
		r.creds = map[string]string{}
	}
	r.srv = srv
	t.Cleanup(srv.Close)
	return r
}

func (r *rig) run(t *testing.T, req executors.Request) (*executors.Result, string) {
	t.Helper()
	res, live, err := r.runCtx(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res, live
}

func (r *rig) runCtx(ctx context.Context, req executors.Request) (*executors.Result, string, error) {
	p := exrun.New(exrun.Options{Sandboxes: clitest.Manager{SB: r.sb}, Credentials: clitest.Creds(r.creds)})
	req.Login = "tester"
	req.Executor = r.name
	if req.Task == "" {
		req.Task = "create hello.txt"
	}
	var live strings.Builder
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	res, err := p.Run(ctx, req, func(s string) { live.WriteString(s) })
	return res, live.String(), err
}

func (r *rig) hello() string {
	b, _ := os.ReadFile(filepath.Join(r.sb.Repo(), "hello.txt"))
	return string(b)
}

// fixture saves the scrubbed raw stdout of the CLI's last run under core/executors/testdata/real/ when asked.
func (r *rig) fixture(t *testing.T, name string) {
	t.Helper()
	if os.Getenv("HARNESS_UPDATE_FIXTURES") == "" {
		return
	}
	out, _, _ := r.sb.CLIOutput(r.bin)
	dir := filepath.Join("..", "..", "..", "core", "executors", "testdata", "real")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(r.sb.Scrub(out)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func clis() []string { return []string{"claude", "codex", "omp"} }

// TestEditLevel: the CLI really uses its file tool in the repository, the runner streams it, reads the final
// summary, the usage, the changed files and the diff stat.
func TestEditLevel(t *testing.T) {
	for _, cli := range clis() {
		t.Run(cli, func(t *testing.T) {
			r := newRig(t, cli, writeThenSummarize("Created hello.txt with the greeting."))
			res, live := r.run(t, executors.Request{Policy: "sandboxed"})
			if res.Failed() || res.Level != executors.LevelEdit {
				t.Fatalf("result %+v\n%s", res, live)
			}
			if got := r.hello(); got != helloContent {
				t.Fatalf("hello.txt = %q\n%s", got, live)
			}
			if res.Summary != "Created hello.txt with the greeting." {
				t.Errorf("summary %q", res.Summary)
			}
			if res.Tokens <= 0 {
				t.Errorf("no tokens parsed: %+v", res)
			}
			if len(res.Changed) != 1 || res.Changed[0] != "hello.txt" || !strings.Contains(res.DiffStat, "hello.txt") {
				t.Errorf("changes %v stat %q", res.Changed, res.DiffStat)
			}
			if !strings.Contains(live, "I will create hello.txt.") {
				t.Errorf("progress lacks the model's words:\n%s", live)
			}
			if strings.Contains(live, `{"type"`) {
				t.Errorf("raw JSON leaked into the progress:\n%s", live)
			}
			if n := len(r.srv.Log.MainRequests()); n < 2 {
				t.Errorf("the CLI made %d agent requests, want the tool turn and the answer", n)
			}
			r.fixture(t, cli+"-edit")
			t.Logf("tokens=%d usd=%v\n%s", res.Tokens, res.USD, live)
		})
	}
}

// TestFullLevel: the autonomous flags (skip permissions, bypass the sandbox, yolo) let the same write through.
func TestFullLevel(t *testing.T) {
	for _, cli := range clis() {
		t.Run(cli, func(t *testing.T) {
			r := newRig(t, cli, writeThenSummarize("Done."))
			res, live := r.run(t, executors.Request{Policy: "autonomous"})
			if res.Failed() || res.Level != executors.LevelFull || r.hello() != helloContent {
				t.Fatalf("result %+v hello=%q\n%s", res, r.hello(), live)
			}
		})
	}
}

// TestReadOnlyLevelRefusesToWrite: the scripted model tries to write while the run is read-only. The CLI must not
// write, and the failure must be visible in the progress.
func TestReadOnlyLevelRefusesToWrite(t *testing.T) {
	want := map[string]string{
		"claude": "[denied] Write", // plan mode: "Cannot write to ... while in plan mode"
		// The OS sandbox (seatbelt) refuses the redirect ("operation not permitted"), but codex exec --json does not emit
		// an item for a command that was refused, so only the model's own words show it. The file is what proves it.
		"codex": "I will create hello.txt.",
		"omp":   "Tool bash not found", // the tool is not offered at all
	}
	for _, cli := range clis() {
		t.Run(cli, func(t *testing.T) {
			r := newRig(t, cli, writeThenSummarize("Done."))
			res, live := r.run(t, executors.Request{ReadOnly: true})
			if res.Level != executors.LevelReadOnly {
				t.Fatalf("level %s", res.Level)
			}
			if got := r.hello(); got != "" {
				t.Fatalf("read-only run wrote hello.txt = %q", got)
			}
			if len(res.Changed) != 0 {
				t.Errorf("changed %v", res.Changed)
			}
			if !strings.Contains(strings.ToLower(live), strings.ToLower(want[cli])) {
				t.Errorf("the refusal is not in the progress (want %q):\n%s", want[cli], live)
			}
			r.fixture(t, cli+"-readonly")
			t.Logf("exit=%d summary=%q\n%s", res.ExitCode, res.Summary, live)
		})
	}
}

// TestRejectedKey: a key the provider answers 401 to ends in a failed run with a diagnosis and no hang.
func TestRejectedKey(t *testing.T) {
	for _, cli := range clis() {
		t.Run(cli, func(t *testing.T) {
			r := newRig(t, cli, rejected)
			start := time.Now()
			res, live := r.run(t, executors.Request{Policy: "sandboxed", Timeout: 90 * time.Second})
			if !res.Failed() || res.TimedOut {
				t.Fatalf("result %+v\n%s", res, live)
			}
			if !strings.Contains(res.Problem, "could not authenticate") {
				t.Errorf("no diagnosis: problem=%q summary=%q output=%q", res.Problem, res.Summary, res.Output)
			}
			if res.Summary == "" {
				t.Errorf("no summary of the failure")
			}
			r.fixture(t, cli+"-badkey")
			t.Logf("took %s exit=%d summary=%q problem=%q", time.Since(start).Round(time.Second), res.ExitCode, res.Summary, res.Problem)
		})
	}
}

// TestTimeoutKillsTheCLI: a CLI stuck in a long command is killed at the timeout, with its whole process tree.
func TestTimeoutKillsTheCLI(t *testing.T) {
	for _, cli := range clis() {
		t.Run(cli, func(t *testing.T) {
			r := newRig(t, cli, sleepy)
			start := time.Now()
			res, live := r.run(t, executors.Request{Policy: "autonomous", Timeout: 6 * time.Second})
			if !res.TimedOut || !res.Failed() {
				t.Fatalf("result %+v\n%s", res, live)
			}
			if d := time.Since(start); d > 30*time.Second {
				t.Errorf("took %s", d)
			}
			if out, err := exec(`pgrep -f "sleep 61"`); err == nil && strings.TrimSpace(out) != "" {
				t.Errorf("the command of the killed CLI is still running: %s", out)
			}
		})
	}
}

// TestCancelStopsTheCLI: cancelling the call stops the CLI and returns the context error.
func TestCancelStopsTheCLI(t *testing.T) {
	for _, cli := range clis() {
		t.Run(cli, func(t *testing.T) {
			r := newRig(t, cli, sleepy)
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				clitest.Wait(60*time.Second, func() bool { return len(r.srv.Log.MainRequests()) >= 1 })
				time.Sleep(2 * time.Second)
				cancel()
			}()
			start := time.Now()
			_, _, err := r.runCtx(ctx, executors.Request{Policy: "autonomous", Timeout: 5 * time.Minute})
			if err == nil {
				t.Fatal("a cancelled run returned no error")
			}
			if d := time.Since(start); d > 40*time.Second {
				t.Errorf("took %s to stop", d)
			}
		})
	}
}
