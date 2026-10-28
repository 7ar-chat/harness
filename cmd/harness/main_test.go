package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

func scripted(turns ...llmtest.Turn) factory {
	return func(cfg *config.Config) (*harness.Harness, error) {
		return harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New(turns...)}, Backend: sandboxtest.NewBackend()})
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
