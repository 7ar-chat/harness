package projectdocs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/features/projectdocs"
	"github.com/7ar-chat/harness/prompts"
)

func manager(t *testing.T, files map[string]string) (sandbox.Manager, string) {
	t.Helper()
	be := sandboxtest.NewBackend()
	be.OnCreate = func(s *sandboxtest.Sandbox) {
		for p, c := range files {
			s.Put(p, c)
		}
	}
	st := memstore.New()
	th, err := st.CreateThread(context.Background(), "u", "")
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.NewManager(be, st, nil, sandbox.ManagerOptions{MaxPerUser: 5}), th.ID
}

func input(thread, variant string) agent.Input {
	return agent.Input{Run: &store.Run{ThreadID: thread, UserLogin: "u"}, Sandbox: true, Variant: variant}
}

func TestSectionRendersAgentsMdForTheCodingVariantOnly(t *testing.T) {
	// Arrange
	mgr, th := manager(t, map[string]string{"/workspace/AGENTS.md": "Run `make test`.\n"})
	sec := projectdocs.Section(mgr)

	// Act
	coding := sec(context.Background(), input(th, prompts.Coding))
	other := sec(context.Background(), input(th, prompts.Default))

	// Assert
	if !strings.Contains(coding, "Run `make test`.") || !strings.Contains(coding, `file="AGENTS.md"`) {
		t.Fatalf("coding section %q", coding)
	}
	if other != "" {
		t.Fatalf("default variant got %q", other)
	}
}

func TestSectionFallsBackToClaudeMdAndSkipsWhenAbsent(t *testing.T) {
	// Arrange
	m1, th1 := manager(t, map[string]string{"/workspace/CLAUDE.md": "use tabs"})
	m2, th2 := manager(t, nil)
	withClaude, empty := projectdocs.Section(m1), projectdocs.Section(m2)

	// Act
	a := withClaude(context.Background(), input(th1, prompts.Coding))
	b := empty(context.Background(), input(th2, prompts.Coding))

	// Assert
	if !strings.Contains(a, `file="CLAUDE.md"`) || !strings.Contains(a, "use tabs") {
		t.Fatalf("got %q", a)
	}
	if b != "" {
		t.Fatalf("got %q", b)
	}
}
