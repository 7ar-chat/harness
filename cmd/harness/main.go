// Command harness is a minimal terminal client of the harness library: one
// in-process harness, one thread, AG-UI events rendered as text. It is a
// reference for building a real TUI.
//
//	LLM_PROVIDER=openai LLM_API_KEY=… LLM_MODELS=gpt-5 \
//	OPENSANDBOX_URL=http://localhost:8090 harness -model gpt-5
//
// Type a message and press enter; type it while a run is going to steer the
// run; Ctrl-C cancels the run; /quit leaves.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/agui"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/store"
)

func main() {
	model := flag.String("model", "", "model id (default LLM_DEFAULT_MODEL)")
	user := flag.String("user", "local", "user login")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if *model == "" {
		*model = cfg.LLMDefaultModel
	}
	if *model == "" {
		fatal(fmt.Errorf("no model: pass -model or set LLM_DEFAULT_MODEL"))
	}
	h, err := harness.New(harness.Options{Config: cfg, Version: "cli"})
	if err != nil {
		fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = h.Run(ctx) }()

	th, err := h.NewThread(ctx, *user, "")
	if err != nil {
		fatal(err)
	}
	if !cfg.SandboxEnabled() {
		fmt.Fprintln(os.Stderr, "(OPENSANDBOX_URL is not set: the agent has no sandbox tools)")
	}

	in := bufio.NewScanner(os.Stdin)
	var (
		mu     sync.Mutex
		active *store.Run
	)
	// Ctrl-C cancels the active run; with none, it leaves.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		for range sig {
			mu.Lock()
			r := active
			mu.Unlock()
			if r == nil {
				stop()
				os.Exit(0)
			}
			_ = h.Cancel(context.Background(), *user, r.ID)
		}
	}()

	for fmt.Print("> "); in.Scan(); fmt.Print("> ") {
		text := strings.TrimSpace(in.Text())
		switch {
		case text == "":
			continue
		case text == "/quit":
			return
		}
		mu.Lock()
		r := active
		mu.Unlock()
		if r != nil { // a run is going: steer it
			if err := h.Steer(ctx, *user, r.ID, text); err != nil {
				fmt.Fprintln(os.Stderr, "steer:", err)
			}
			continue
		}
		run, err := h.Send(ctx, *user, th.ID, text, harness.RunOptions{Model: *model})
		if err != nil {
			fmt.Fprintln(os.Stderr, "send:", err)
			continue
		}
		mu.Lock()
		active = run
		mu.Unlock()
		go func() {
			render(h.Events(context.Background(), run.ID, 0))
			mu.Lock()
			active = nil
			mu.Unlock()
			fmt.Print("\n> ")
		}()
	}
}

// render prints the events of a run as plain text.
func render(events <-chan store.Event) {
	for ev := range events {
		var e map[string]any
		if json.Unmarshal(ev.Data, &e) != nil {
			continue
		}
		switch ev.Type {
		case agui.TextMessageContent:
			fmt.Print(e["delta"])
		case agui.TextMessageEnd:
			fmt.Println()
		case agui.ToolCallStart:
			fmt.Printf("\n[tool] %v\n", e["toolCallName"])
		case agui.ToolCallResult:
			fmt.Printf("[result] %s\n", clip(fmt.Sprint(e["content"]), 200))
		case agui.RunError:
			fmt.Printf("\n[error] %v\n", e["message"])
		}
	}
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "harness:", err)
	os.Exit(1)
}
