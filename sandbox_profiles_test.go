package harness_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

// Features that need a sandbox (permissions, codecheck, project docs,
// workspace checkpoints, artifacts, verify, explore) must degrade quietly for
// a harness without a backend and for runs with Sandbox off.
func TestEveryProfileRunsWithoutASandbox(t *testing.T) {
	profiles := map[string]func() []harness.Feature{"work": harness.ProfileWork, "coding": harness.ProfileCoding, "minimal": harness.ProfileMinimal}
	for name, features := range profiles {
		for _, c := range []struct {
			label   string
			backend *sandboxtest.Backend
			mode    harness.SandboxMode
		}{{"no backend", nil, ""}, {"backend, off run", sandboxtest.NewBackend(), harness.SandboxOff}} {
			t.Run(name+"/"+c.label, func(t *testing.T) {
				// Arrange
				model := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("answer")}, llmtest.Turn{Chunks: llmtest.Text("A title")})
				opts := harness.Options{Config: featureConfig(t, nil), Models: llmtest.Factory{M: model}, Features: features()}
				if c.backend != nil {
					opts.Backend = c.backend
				}
				h, err := harness.New(opts)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				go func() { _ = h.Run(ctx) }()
				th, _ := h.NewThread(ctx, "alice", "")

				// Act
				run, err := h.Send(ctx, "alice", th.ID, "hi", harness.RunOptions{Model: "gpt-test", Sandbox: c.mode})
				if err != nil {
					t.Fatal(err)
				}
				var types []string
				for ev := range h.Events(ctx, run.ID, 0) {
					types = append(types, ev.Type)
				}

				// Assert
				if len(types) == 0 || types[len(types)-1] != agui.RunFinished {
					t.Fatalf("events %v", types)
				}
				if c.backend != nil && len(c.backend.Created()) != 0 {
					t.Fatal("a sandbox was created")
				}
				var system strings.Builder
				for _, m := range model.Calls()[0].Messages {
					system.WriteString(m.Content)
				}
				for _, leak := range []string{"private Linux sandbox", "Local Git Checkpoints", "project_instructions"} {
					if strings.Contains(system.String(), leak) {
						t.Fatalf("%q in the prompt of a run without sandbox", leak)
					}
				}
			})
		}
	}
}
