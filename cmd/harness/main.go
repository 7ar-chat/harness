// Command harness is the terminal client of the harness library: one
// in-process harness, one thread.
//
//	LLM_PROVIDER=openai LLM_API_KEY=… LLM_MODELS=gpt-5 \
//	OPENSANDBOX_URL=http://localhost:8090 harness -model gpt-5
//
// Modes:
//
//	harness                  interactive terminal UI (when stdin is a terminal)
//	harness -p "prompt"      print mode: the answer on stdout, tool activity on stderr
//	echo prompt | harness    print mode, the prompt read from stdin
//	harness -p … -json       print mode emitting the AG-UI events as JSON lines
//
// Print mode exits 0 on success, 1 when the run failed or was cancelled and 2
// when it ended waiting for an answer or an approval.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/tui"
)

func main() {
	stat, _ := os.Stdin.Stat()
	isTTY := stat != nil && stat.Mode()&os.ModeCharDevice != 0
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, isTTY, newHarness)
	stop()
	os.Exit(code)
}

// factory builds the harness of a run of the command.
type factory func(cfg *config.Config) (*harness.Harness, error)

func newHarness(cfg *config.Config) (*harness.Harness, error) {
	return harness.New(harness.Options{Config: cfg, Version: "cli"})
}

type options struct {
	model, user, prompt string
	json, auto          bool
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, isTTY bool, mk factory) int {
	var o options
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.model, "model", "", "model id (default LLM_DEFAULT_MODEL)")
	fs.StringVar(&o.user, "user", "local", "user login")
	fs.StringVar(&o.prompt, "p", "", "print mode: send this prompt (and stdin, if piped), print the answer, exit")
	fs.BoolVar(&o.json, "json", false, "print mode: emit the AG-UI events as JSON lines on stdout")
	fs.BoolVar(&o.auto, "auto", false, "interactive UI: do not ask for approval of risky tool calls")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	prompt, err := resolvePrompt(o.prompt, stdin, isTTY)
	if err != nil {
		return fail(stderr, err)
	}
	printMode := prompt != ""
	if !printMode && !isTTY {
		return fail(stderr, errors.New("no prompt: pass -p, pipe one on stdin, or run in a terminal"))
	}

	cfg, err := config.Load()
	if err != nil {
		return fail(stderr, err)
	}
	if o.model == "" {
		o.model = cfg.LLMDefaultModel
	}
	if o.model == "" {
		return fail(stderr, errors.New("no model: pass -model or set LLM_DEFAULT_MODEL"))
	}
	h, err := mk(cfg)
	if err != nil {
		return fail(stderr, err)
	}
	hctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = h.Run(hctx) }()

	th, err := h.NewThread(hctx, o.user, "")
	if err != nil {
		return fail(stderr, err)
	}
	be := &tui.HarnessBackend{H: h, User: o.user, ThreadID: th.ID, ModelID: o.model, Interactive: !printMode && !o.auto}
	if !cfg.SandboxEnabled() && !o.json {
		fmt.Fprintln(stderr, "(OPENSANDBOX_URL is not set: the agent has no sandbox tools)")
	}
	if printMode {
		return tui.RunPrint(hctx, be, prompt, tui.PrintOptions{Stdout: stdout, Stderr: stderr, JSON: o.json})
	}
	if err := tui.Run(hctx, be); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// resolvePrompt combines the -p flag with piped stdin ("" when neither).
func resolvePrompt(flagPrompt string, stdin io.Reader, isTTY bool) (string, error) {
	var piped string
	if !isTTY && stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		piped = strings.TrimSpace(string(b))
	}
	switch {
	case flagPrompt != "" && piped != "":
		return flagPrompt + "\n\n" + piped, nil
	case flagPrompt != "":
		return flagPrompt, nil
	}
	return piped, nil
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "harness:", err)
	return 1
}
