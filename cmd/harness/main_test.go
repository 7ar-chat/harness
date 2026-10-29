package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/features/gitsafe"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

// TestMain keeps the tests' saved conversations out of the user's state directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "harness-cli-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("LOCAL_STATE_DIR", dir)
	os.Setenv("HOME", dir)                   // not the developer's ~/.har
	os.Setenv("HARNESS_MODEL_PRICES", "off") // no download of the price table
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func scripted(turns ...llmtest.Turn) factory {
	return func(cfg *config.Config, st harness.Store) (*harness.Harness, error) {
		return harness.New(harness.Options{Config: cfg, Store: st, Models: llmtest.Factory{M: llmtest.New(turns...)}, Backend: sandboxtest.NewBackend()})
	}
}

func TestResolvePromptCombinesTheFlagWithPipedStdin(t *testing.T) {
	cases := map[string]struct {
		flag, stdin string
		tty         bool
		want        string
	}{
		"flag only":         {flag: "hi", tty: true, want: "hi"},
		"stdin only":        {stdin: " from stdin \n", want: "from stdin"},
		"both":              {flag: "summarise", stdin: "text", want: "summarise\n\ntext"},
		"tty ignores stdin": {stdin: "ignored", tty: true, want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			got, err := resolvePrompt(tc.flag, strings.NewReader(tc.stdin), tc.tty)

			// Assert
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestPrintModeWithPromptFlagRunsAndExitsZero(t *testing.T) {
	// Arrange
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer

	// Act
	code := run(ctx, []string{"-p", "hello", "-model", "gpt-test"}, strings.NewReader(""), &stdout, &stderr, false,
		scripted(llmtest.Turn{Chunks: llmtest.Text("hi ", "there")}))

	// Assert
	if code != 0 || stdout.String() != "hi there\n" {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestPrintModeReadsThePromptFromPipedStdin(t *testing.T) {
	// Arrange
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer

	// Act
	code := run(ctx, []string{"-model", "gpt-test"}, strings.NewReader("what is 2+2\n"), &stdout, &stderr, false,
		scripted(llmtest.Turn{Chunks: llmtest.Text("four")}))

	// Assert
	if code != 0 || stdout.String() != "four\n" {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestWithoutPromptAndWithoutTerminalItFailsWithAMessage(t *testing.T) {
	// Arrange
	var stdout, stderr bytes.Buffer

	// Act
	code := run(context.Background(), nil, strings.NewReader(""), &stdout, &stderr, false, scripted())

	// Assert
	if code != 1 || !strings.Contains(stderr.String(), "no prompt") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

func TestPrintModeFailsWhenTheModelIsNotAllowed(t *testing.T) {
	// Arrange
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	var stdout, stderr bytes.Buffer

	// Act
	code := run(context.Background(), []string{"-p", "x", "-model", "nope"}, strings.NewReader(""), &stdout, &stderr, false, scripted())

	// Assert
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestContinueResumesTheSavedConversationAcrossProcesses(t *testing.T) {
	// Arrange
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("Nice to meet you, Ann.")}, llmtest.Turn{Chunks: llmtest.Text("T")})
	second := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("Your name is Ann.")}, llmtest.Turn{Chunks: llmtest.Text("T")})
	mk := func(m *llmtest.Model) factory {
		return func(cfg *config.Config, st harness.Store) (*harness.Harness, error) {
			return harness.New(harness.Options{Config: cfg, Store: st, Models: llmtest.Factory{M: m}, Backend: sandboxtest.NewBackend()})
		}
	}
	var out, errb bytes.Buffer

	// Act
	c1 := run(ctx, []string{"-p", "my name is Ann", "-model", "gpt-test"}, strings.NewReader(""), &out, &errb, false, mk(first))
	out.Reset()
	c2 := run(ctx, []string{"-p", "what is my name?", "-model", "gpt-test", "-continue"}, strings.NewReader(""), &out, &errb, false, mk(second))
	var list bytes.Buffer
	c3 := run(ctx, []string{"-sessions"}, strings.NewReader(""), &list, &errb, false, mk(second))

	// Assert
	if c1 != 0 || c2 != 0 || c3 != 0 || out.String() != "Your name is Ann.\n" {
		t.Fatalf("exits %d %d %d stdout %q stderr %q", c1, c2, c3, out.String(), errb.String())
	}
	var sawFirstTurn bool
	for _, m := range second.Calls()[0].Messages {
		if strings.Contains(m.Content, "my name is Ann") {
			sawFirstTurn = true
		}
	}
	if !sawFirstTurn {
		t.Error("the continued run did not see the first turn")
	}
	if strings.Count(list.String(), " msgs") != 1 {
		t.Errorf("want one saved conversation:\n%s", list.String())
	}
}

func TestEphemeralKeepsNothing(t *testing.T) {
	// Arrange
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	dir := t.TempDir()
	t.Setenv("LOCAL_STATE_DIR", dir)
	var out, errb bytes.Buffer

	// Act
	code := run(context.Background(), []string{"-p", "hi", "-model", "gpt-test", "-ephemeral"}, strings.NewReader(""), &out, &errb, false,
		scripted(llmtest.Turn{Chunks: llmtest.Text("ok")}))
	cont := run(context.Background(), []string{"-p", "hi", "-model", "gpt-test", "-ephemeral", "-continue"}, strings.NewReader(""), &out, &errb, false, scripted())

	// Assert
	if entries, _ := os.ReadDir(dir); code != 0 || len(entries) != 0 || cont == 0 {
		t.Fatalf("exit %d, state dir has %d entries, -continue with -ephemeral exited %d", code, len(entries), cont)
	}
}

func TestUndoRestoresTheWorkspaceToTheSnapshotBeforeARun(t *testing.T) {
	// Arrange
	repo := t.TempDir()
	sh := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	sh("init", "-q")
	file := filepath.Join(repo, "a.txt")
	_ = os.WriteFile(file, []byte("before\n"), 0o644)
	sh("add", ".")
	sh("commit", "-qm", "init")
	if _, err := gitsafe.Snapshot(context.Background(), repo, "thread-1"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(file, []byte("the agent broke it\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repo, "new.txt"), []byte("junk"), 0o644)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LOCAL_WORKSPACE", repo)
	var out, errb bytes.Buffer

	// Act
	code := run(context.Background(), []string{"-undo", "-resume", "thread-1", "-model", "gpt-test"}, strings.NewReader(""), &out, &errb, false, scripted())

	// Assert
	b, _ := os.ReadFile(file)
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); code != 0 || string(b) != "before\n" || err == nil {
		t.Fatalf("exit %d content %q new.txt err %v stderr %q", code, b, err, errb.String())
	}
}
