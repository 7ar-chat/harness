//go:build unix

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	binary    string
	buildErr  error
	buildDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// harnessBinary builds cmd/harness once per test binary.
func harnessBinary(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go toolchain is not on PATH: cannot build the harness binary")
	}
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "harness-cli-")
		if buildErr != nil {
			return
		}
		binary = filepath.Join(buildDir, "harness")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/harness")
		cmd.Dir = filepath.Join("..", "..") // the app module
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = errors.New("go build ./cmd/harness: " + err.Error() + "\n" + string(out))
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binary
}

// env is an isolated machine for the binary: its own HOME, workspace and state.
type env struct {
	t         *testing.T
	bin       string
	home      string
	workspace string // the cwd of the process and the local sandbox's workspace
	state     string // LOCAL_STATE_DIR: the durable store
	llm       *fakeLLM
	extra     []string
}

func newEnv(t *testing.T, llm *fakeLLM) *env {
	t.Helper()
	bin := harnessBinary(t)
	root := t.TempDir()
	e := &env{t: t, bin: bin, llm: llm,
		home: filepath.Join(root, "home"), workspace: filepath.Join(root, "ws"), state: filepath.Join(root, "state")}
	// root/.git ends the walk up from the workspace looking for a project .har directory.
	for _, d := range []string{e.home, e.workspace, e.state, filepath.Join(root, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// environ is the complete environment of the process: nothing is inherited but PATH.
func (e *env) environ() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + e.home,
		"HARNESS_NO_USER_DIR=1",    // never read the developer's ~/.har
		"HARNESS_MODEL_PRICES=off", // no price table download
		"LLM_PROVIDER=openai",
		"LLM_BASE_URL=" + e.llm.baseURL(),
		"LLM_API_KEY=x",
		"LLM_MODELS=gpt-test",
		"LLM_DEFAULT_MODEL=gpt-test",
		"LLM_STEP_RETRIES=0",
		"SANDBOX_BACKEND=local",
		"LOCAL_WORKSPACE=" + e.workspace,
		"LOCAL_STATE_DIR=" + e.state,
	}
	return append(env, e.extra...)
}

// result is a finished process.
type result struct {
	stdout, stderr string
	code           int
}

// run starts the binary in the workspace, waits for it (killing it at the deadline)
// and captures stdout and stderr separately.
func (e *env) run(stdin string, args ...string) result {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, args...)
	cmd.Dir = e.workspace
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		e.t.Fatalf("harness %v timed out\nstdout: %q\nstderr: %q", args, res.stdout, res.stderr)
	case errors.As(err, &ee):
		res.code = ee.ExitCode()
	case err != nil:
		e.t.Fatalf("harness %v: %v", args, err)
	}
	return res
}

func TestPromptFlagPrintsTheAnswerOnStdoutAndExitsZero(t *testing.T) {
	// Arrange
	llm := newFakeLLM(t, answer("Hi from the fake"))
	e := newEnv(t, llm)

	// Act
	res := e.run("", "-p", "say hi")

	// Assert
	if res.code != 0 {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	if res.stdout != "Hi from the fake\n" {
		t.Fatalf("stdout %q", res.stdout)
	}
	reqs := llm.agentRequests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].text("user"), "say hi") {
		t.Fatalf("requests %+v", reqs)
	}
}

func TestPrintModeWritesTheFileAndFeedsTheToolResultBackToTheModel(t *testing.T) {
	// Arrange
	llm := newFakeLLM(t, toolThen("write_file", `{"path":"report.md","content":"# Report\n"}`, "Report written"))
	e := newEnv(t, llm)

	// Act
	res := e.run("", "-p", "write a file")

	// Assert
	if res.code != 0 {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	if res.stdout != "Report written\n" {
		t.Fatalf("stdout %q", res.stdout)
	}
	if !strings.Contains(res.stderr, "> write_file report.md") {
		t.Fatalf("stderr %q", res.stderr)
	}
	got, err := os.ReadFile(filepath.Join(e.workspace, "report.md"))
	if err != nil || string(got) != "# Report\n" {
		t.Fatalf("report.md %q, %v", got, err)
	}
	reqs := llm.agentRequests()
	if len(reqs) != 2 {
		t.Fatalf("agent requests = %d, want 2", len(reqs))
	}
	if !contains(reqs[0].toolNames(), "write_file") || !contains(reqs[0].toolNames(), "execute_command") {
		t.Fatalf("tools offered: %v", reqs[0].toolNames())
	}
	if reqs[0].toolMessage() != nil {
		t.Fatalf("the first request already holds a tool result: %v", reqs[0].Messages)
	}
	tm := reqs[1].toolMessage()
	if tm == nil || tm["tool_call_id"] != "call_1" {
		t.Fatalf("second request tool message = %v (messages %v)", tm, reqs[1].Messages)
	}
	if !strings.Contains(reqs[1].text("tool"), "report.md") {
		t.Fatalf("tool result %q does not mention the file", reqs[1].text("tool"))
	}
}

func TestJSONModeEmitsAGUIEventsAsJSONLines(t *testing.T) {
	// Arrange
	llm := newFakeLLM(t, toolThen("write_file", `{"path":"j.txt","content":"x"}`, "json done"))
	e := newEnv(t, llm)

	// Act
	res := e.run("", "-p", "write a file", "-json")

	// Assert
	if res.code != 0 {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	var types []string
	var text strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(res.stdout), "\n") {
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Type == "" {
			t.Fatalf("line %q is not an AG-UI event: %v", line, err)
		}
		types = append(types, ev.Type)
		text.WriteString(ev.Delta)
	}
	if types[0] != "RUN_STARTED" || types[len(types)-1] != "RUN_FINISHED" {
		t.Fatalf("types %v", types)
	}
	if !contains(types, "TOOL_CALL_START") || !contains(types, "TOOL_CALL_RESULT") {
		t.Fatalf("no tool events in %v", types)
	}
	if !strings.Contains(text.String(), "json done") {
		t.Fatalf("streamed text %q", text.String())
	}
}

func TestPromptPipedOnStdinRunsInPrintMode(t *testing.T) {
	// Arrange
	llm := newFakeLLM(t, answer("stdin answer"))
	e := newEnv(t, llm)

	// Act
	res := e.run("what is in stdin?\n")

	// Assert
	if res.code != 0 || res.stdout != "stdin answer\n" {
		t.Fatalf("exit %d stdout %q stderr %q", res.code, res.stdout, res.stderr)
	}
	reqs := llm.agentRequests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].text("user"), "what is in stdin?") {
		t.Fatalf("requests %+v", reqs)
	}
}

func TestModelErrorsExitOneWithAReadableStderrAndNoAnswer(t *testing.T) {
	for _, status := range []int{401, 500} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			// Arrange
			llm := newFakeLLM(t, func(request) reply { return reply{status: status} })
			e := newEnv(t, llm)

			// Act
			res := e.run("", "-p", "say hi")

			// Assert
			if res.code != 1 {
				t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
			}
			if res.stdout != "" {
				t.Fatalf("stdout %q", res.stdout)
			}
			if !strings.Contains(res.stderr, "harness: run failed:") || !strings.Contains(res.stderr, "scripted failure") {
				t.Fatalf("stderr %q", res.stderr)
			}
			if n := len(llm.agentRequests()); n != 1 {
				t.Fatalf("the model was called %d times, want 1 (no retries)", n)
			}
		})
	}
}

func TestPrintModeExitsTwoWhenACommandNeedsApprovalAndDoesNotRunIt(t *testing.T) {
	// Arrange
	script := toolThen("execute_command", `{"command":"bash mk.sh"}`, "script finished")
	llm := newFakeLLM(t, script)
	e := newEnv(t, llm)
	writeFile(t, filepath.Join(e.workspace, "mk.sh"), "echo ran > ran.txt\n")

	// Act
	res := e.run("", "-p", "run the script")

	// Assert
	if res.code != 2 {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "waiting for input (tool_approval)") || !strings.Contains(res.stderr, "execute_command") {
		t.Fatalf("stderr %q", res.stderr)
	}
	if res.stdout != "" {
		t.Fatalf("stdout %q", res.stdout)
	}
	if _, err := os.Stat(filepath.Join(e.workspace, "ran.txt")); err == nil {
		t.Fatal("the command ran without approval")
	}
	if n := len(llm.agentRequests()); n != 1 {
		t.Fatalf("the model was called %d times: the run went on past the approval", n)
	}
}

func TestPrintModeRunsTheSameCommandWhenPermissionsAreOpen(t *testing.T) {
	// Arrange: the positive control of the approval test above.
	llm := newFakeLLM(t, toolThen("execute_command", `{"command":"bash mk.sh"}`, "script finished"))
	e := newEnv(t, llm)
	e.extra = []string{"AGENT_PERMISSIONS=yolo"}
	writeFile(t, filepath.Join(e.workspace, "mk.sh"), "echo ran > ran.txt\n")

	// Act
	res := e.run("", "-p", "run the script")

	// Assert
	if res.code != 0 || res.stdout != "script finished\n" {
		t.Fatalf("exit %d stdout %q stderr %q", res.code, res.stdout, res.stderr)
	}
	got, err := os.ReadFile(filepath.Join(e.workspace, "ran.txt"))
	if err != nil || string(got) != "ran\n" {
		t.Fatalf("ran.txt %q, %v", got, err)
	}
}

func TestContinueFlagResumesTheLastConversationAcrossProcesses(t *testing.T) {
	// Arrange
	llm := newFakeLLM(t, func(r request) reply {
		if strings.Contains(r.text("user"), "second question") {
			return reply{text: "second answer"}
		}
		return reply{text: "first answer"}
	})
	e := newEnv(t, llm)
	first := e.run("", "-p", "first question")
	if first.code != 0 || first.stdout != "first answer\n" {
		t.Fatalf("first run: exit %d stdout %q stderr %q", first.code, first.stdout, first.stderr)
	}

	// Act
	second := e.run("", "-p", "second question", "-continue")

	// Assert
	if second.code != 0 || second.stdout != "second answer\n" {
		t.Fatalf("second run: exit %d stdout %q stderr %q", second.code, second.stdout, second.stderr)
	}
	reqs := llm.agentRequests()
	if len(reqs) != 2 {
		t.Fatalf("agent requests = %d, want 2", len(reqs))
	}
	history := reqs[1].text("user") + reqs[1].text("assistant")
	for _, want := range []string{"first question", "first answer", "second question"} {
		if !strings.Contains(history, want) {
			t.Fatalf("the continued request lacks %q: %v", want, reqs[1].Messages)
		}
	}
	list := e.run("", "-sessions")
	if list.code != 0 || strings.Count(list.stdout, " msgs") != 1 {
		t.Fatalf("-sessions: exit %d stdout %q stderr %q (want one conversation)", list.code, list.stdout, list.stderr)
	}
}

func TestWithoutContinueAnInvocationStartsAFreshConversation(t *testing.T) {
	// Arrange: the negative control of the continue test above.
	llm := newFakeLLM(t, answer("ok"))
	e := newEnv(t, llm)
	e.run("", "-p", "first question")

	// Act
	res := e.run("", "-p", "second question")

	// Assert
	if res.code != 0 {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	reqs := llm.agentRequests()
	if len(reqs) != 2 || strings.Contains(reqs[1].text("user"), "first question") {
		t.Fatalf("the second conversation saw the first: %v", reqs[len(reqs)-1].Messages)
	}
	list := e.run("", "-sessions")
	if strings.Count(list.stdout, " msgs") != 2 {
		t.Fatalf("-sessions %q, want two conversations", list.stdout)
	}
}

func TestContinueWithEphemeralIsRejectedWithoutCallingTheModel(t *testing.T) {
	// Arrange
	llm := newFakeLLM(t, answer("never"))
	e := newEnv(t, llm)

	// Act
	res := e.run("", "-p", "hi", "-ephemeral", "-continue")

	// Assert
	if res.code != 1 || !strings.Contains(res.stderr, "-continue and -resume need the session store") {
		t.Fatalf("exit %d stderr %q", res.code, res.stderr)
	}
	if res.stdout != "" || len(llm.agentRequests()) != 0 {
		t.Fatalf("stdout %q, %d model calls", res.stdout, len(llm.agentRequests()))
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
