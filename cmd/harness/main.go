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
	"github.com/GhaziBenDahmane/harness/features/gitsafe"
	"github.com/GhaziBenDahmane/harness/hosts/tui"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/filestore"
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
// st is the session store (nil: in memory).
type factory func(cfg *config.Config, st harness.Store) (*harness.Harness, error)

func newHarness(cfg *config.Config, st harness.Store) (*harness.Harness, error) {
	return harness.New(harness.Options{Config: cfg, Store: st, Version: "cli"})
}

type options struct {
	model, user, prompt string
	json, auto          bool
	ephemeral, cont     bool
	resume              string
	list, undo          bool
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
	fs.BoolVar(&o.ephemeral, "ephemeral", false, "keep nothing: do not save the conversation (default: saved under LOCAL_STATE_DIR)")
	fs.BoolVar(&o.cont, "continue", false, "continue the most recent conversation")
	fs.StringVar(&o.resume, "resume", "", "continue the conversation with this id (see -sessions)")
	fs.BoolVar(&o.undo, "undo", false, "restore the files of the workspace (a git repository) to the last snapshot taken before a run of the conversation (-resume <id>, default the most recent), then exit")
	fs.BoolVar(&o.list, "sessions", false, "list the saved conversations and exit")
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
	if !printMode && !isTTY && !o.list && !o.undo {
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
	var st harness.Store
	var sessions filestore.Sessions
	if !o.ephemeral {
		fsStore, err := harness.OpenLocalStore(cfg.LocalStateDir)
		if err != nil {
			return fail(stderr, fmt.Errorf("open the session store (use -ephemeral to skip it): %w", err))
		}
		defer fsStore.Close()
		st, sessions = fsStore, fsStore.Sessions()
	}
	if o.list {
		if st == nil {
			return fail(stderr, errors.New("-sessions needs the session store (drop -ephemeral)"))
		}
		return listSessions(ctx, sessions, o.user, stdout, stderr)
	}
	if o.undo {
		return undoRun(ctx, cfg, sessions, o, stdout, stderr)
	}
	h, err := mk(cfg, st)
	if err != nil {
		return fail(stderr, err)
	}
	hctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = h.Run(hctx) }()

	th, err := pickThread(hctx, h, sessions, st != nil, o)
	if err != nil {
		return fail(stderr, err)
	}
	be := &tui.HarnessBackend{H: h, User: o.user, ThreadID: th.ID, ModelID: o.model, Interactive: !printMode && !o.auto}
	if !cfg.SandboxEnabled() && !o.json {
		fmt.Fprintln(stderr, "(no sandbox: set SANDBOX_BACKEND=local or OPENSANDBOX_URL; the agent has no tools)")
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
// pickThread starts a new conversation, or continues the one asked for.
func pickThread(ctx context.Context, h *harness.Harness, sessions filestore.Sessions, saved bool, o options) (*store.Thread, error) {
	if (o.cont || o.resume != "") && !saved {
		return nil, errors.New("-continue and -resume need the session store (drop -ephemeral)")
	}
	switch {
	case o.resume != "":
		return h.Store.GetThread(ctx, o.user, o.resume)
	case o.cont:
		list, err := sessions.List(ctx, o.user, 1)
		if err != nil {
			return nil, err
		}
		if len(list) > 0 {
			return h.Store.GetThread(ctx, o.user, list[0].ID)
		}
	}
	return h.NewThread(ctx, o.user, "")
}

// undoRun restores the workspace to the snapshot taken before the conversation's last run. The
// snapshot lives in refs/harness/snapshots/<thread>/ of the repository; the state it replaces is
// kept under refs/harness/backups/, so the undo can itself be undone.
func undoRun(ctx context.Context, cfg *config.Config, sessions filestore.Sessions, o options, stdout, stderr io.Writer) int {
	thread := o.resume
	if thread == "" {
		list, err := sessions.List(ctx, o.user, 1)
		if err != nil || len(list) == 0 {
			return fail(stderr, errors.New("no saved conversation to undo; pass -resume <id>"))
		}
		thread = list[0].ID
	}
	dir := cfg.LocalWorkspace
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return fail(stderr, err)
		}
	}
	backup, restored, err := gitsafe.Undo(ctx, dir, thread)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "restored %s to the snapshot %s; the replaced state is kept as %s\n", dir, restored.Ref, backup.Ref)
	return 0
}

func listSessions(ctx context.Context, sessions filestore.Sessions, user string, stdout, stderr io.Writer) int {
	list, err := sessions.List(ctx, user, 50)
	if err != nil {
		return fail(stderr, err)
	}
	for _, s := range list {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(stdout, "%s  %s  %3d msgs  %s\n      %s\n", s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), s.Messages, title, s.Preview)
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no saved conversations")
	}
	return 0
}

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
