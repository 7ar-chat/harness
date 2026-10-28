package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/hosts/client"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

// ask is a whole front end: it sends one message and prints the answer as it streams. It only knows
// client.Client, so it works the same on client.InProcess and on client.HTTP.
func ask(ctx context.Context, c client.Client, question string) error {
	th, err := c.CreateThread(ctx, "")
	if err != nil {
		return err
	}
	run, err := c.Send(ctx, th.ID, question, client.SendOptions{})
	if err != nil {
		return err
	}
	events, err := c.Events(ctx, run.ID, 0)
	if err != nil {
		return err
	}
	for ev := range events {
		switch ev.Type {
		case agui.TextMessageContent:
			var e struct{ Delta string }
			_ = json.Unmarshal(ev.Data, &e)
			fmt.Print(e.Delta)
		case agui.RunFinished, agui.RunError:
			fmt.Println()
		}
	}
	return nil
}

// A front end such as a Slack bot or a desktop app is written against client.Client. Swap
// client.InProcess(h, user) for client.HTTP("http://127.0.0.1:8080", token) to talk to a remote
// harness-web instead: nothing else changes.
func Example() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "gpt-test")
	cfg, _ := config.Load()
	model := llmtest.NewFunc(func(llmtest.Call) llmtest.Turn { return llmtest.Turn{Chunks: llmtest.Text("Hello ", "from the engine")} })
	h, _ := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}, Backend: sandboxtest.NewBackend()})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	c := client.InProcess(h, "alice", client.WithModels(cfg.LLMModels, "gpt-test"))
	if err := ask(ctx, c, "Say hello"); err != nil {
		fmt.Println("error:", err)
	}
	// Output: Hello from the engine
}
