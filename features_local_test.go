//go:build unix

package harness_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/longtest"
)

// callingModel calls the tool named by the user's message (with {}), then answers with the tool result.
func callingModel() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		if last := c.Messages[len(c.Messages)-1]; last.Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("result: " + last.Content)}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", lastUserText(c), "{}"), "tool_calls")}}
	})
}

func TestLocalPermissionsAskOnFirstUseOfAStdioMCPServerToolOnly(t *testing.T) {
	// Arrange: one MCP server runs a local program (stdio), the other is remote
	ws := t.TempDir()
	env := map[string]string{
		"SANDBOX_BACKEND": "local", "LOCAL_WORKSPACE": ws, "LOCAL_STATE_DIR": t.TempDir(), "HOME": t.TempDir(),
		"HARNESS_MCP_STDIO": "1", "AGENT_PERMISSIONS": "",
		"MCP_SERVERS": `[{"name":"fs","command":"echo"},{"name":"web","url":"http://127.0.0.1:1/mcp"}]`,
	}
	var ran []string
	h, ctx := facade(t, env, callingModel(), func(o *harness.Options) {
		o.Backend = nil
		o.Features = []harness.Feature{harness.Sandbox(), harness.Plan(), harness.Permissions(false)}
		for _, name := range []string{"fs__read", "web__lookup"} {
			o.Tools = append(o.Tools, longtest.ExtraTool(name, func(context.Context, string) (string, error) {
				ran = append(ran, name)
				return name + " ok", nil
			}))
		}
	})
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	exchange(ctx, t, h, "alice", th.ID, "web__lookup", harness.RunOptions{})
	remoteRan := append([]string(nil), ran...)
	exchange(ctx, t, h, "alice", th.ID, "fs__read", harness.RunOptions{})
	got, _ := h.Store.GetThread(ctx, "alice", th.ID)

	// Assert
	if len(remoteRan) != 1 || remoteRan[0] != "web__lookup" {
		t.Fatalf("the remote server's tool must run without asking: %v", remoteRan)
	}
	if got.PendingApproval == nil || got.PendingApproval.ToolName != "fs__read" {
		t.Fatalf("the stdio server's tool must wait for approval: pending %+v", got.PendingApproval)
	}
	if len(ran) != 1 {
		t.Fatalf("the stdio tool ran before its approval: %v", ran)
	}
}

func TestLocalPermissionsKeepTheBeforeToolHooksOfOtherFeatures(t *testing.T) {
	// Arrange: another feature's hook sits next to the approvals gate that the permission layer wraps
	ran := false
	guard := harness.FeatureFunc("guard", func(b *harness.Builder) error {
		b.AddTools(longtest.ExtraTool("danger", func(context.Context, string) (string, error) { ran = true; return "boom", nil }))
		b.BeforeTool(denyTool{name: "danger"})
		return nil
	})
	env := map[string]string{"SANDBOX_BACKEND": "local", "LOCAL_WORKSPACE": t.TempDir(), "LOCAL_STATE_DIR": t.TempDir(), "AGENT_PERMISSIONS": "yolo"}
	h, ctx := facade(t, env, callingModel(), func(o *harness.Options) {
		o.Backend = nil
		o.Features = []harness.Feature{harness.Sandbox(), harness.Plan(), guard, harness.Permissions(false)}
	})
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	exchange(ctx, t, h, "alice", th.ID, "danger", harness.RunOptions{})

	// Assert
	msgs := messagesOf(ctx, t, h, th.ID)
	if ran || msgs[len(msgs)-1].Content != "result: Error: danger is forbidden here." {
		t.Fatalf("ran %v, last message %q", ran, msgs[len(msgs)-1].Content)
	}
}

func TestLocalBackendUsesTheCurrentDirectoryWhenNoWorkspaceIsConfigured(t *testing.T) {
	// Arrange
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	cfg := featureConfig(t, map[string]string{"SANDBOX_BACKEND": "local", "LOCAL_WORKSPACE": "", "LOCAL_STATE_DIR": t.TempDir()})
	backend, err := harness.NewSandboxBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := backend.Create(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	werr := inst.WriteFile(context.Background(), "/workspace/hello.txt", []byte("hi"))

	// Assert
	if werr != nil {
		t.Fatal(werr)
	}
	if got, ok := readFile(cwd, "hello.txt"); !ok || got != "hi" {
		t.Fatalf("hello.txt in the current directory: %q %v", got, ok)
	}
}

func TestProjectNotesOfAnotherFeatureAndTheProjectDocsAreBothAppendedToAFileRead(t *testing.T) {
	// Arrange: a feature earlier in the list already adds a note to every file tool result
	earlier := harness.FeatureFunc("earlier", func(b *harness.Builder) error {
		b.SandboxTools(func(o *sandboxtools.Options) {
			o.AfterPath = func(context.Context, tools.RunContext, sandbox.Sandbox, string) string { return "[NOTE-FROM-EARLIER]" }
		})
		return nil
	})
	backend := sandboxtest.NewBackend()
	backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		ctx := context.Background()
		_ = sb.WriteFile(ctx, "/workspace/pkg/AGENTS.md", []byte("Use tabs in pkg."))
		_ = sb.WriteFile(ctx, "/workspace/pkg/main.go", []byte("package pkg\n"))
	}
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("title")}
		}
		if last := c.Messages[len(c.Messages)-1]; last.Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("done")}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "r1", "read_file", `{"path":"/workspace/pkg/main.go"}`), "tool_calls")}}
	})
	h, ctx := facade(t, nil, model, func(o *harness.Options) {
		o.Backend = backend
		o.Features = []harness.Feature{harness.Sandbox(), earlier, harness.ProjectDocs()}
	})
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	exchange(ctx, t, h, "alice", th.ID, "read it", harness.RunOptions{})

	// Assert
	var result string
	for _, m := range messagesOf(ctx, t, h, th.ID) {
		if m.ToolCallID == "r1" {
			result = m.Content
		}
	}
	if !strings.Contains(result, "[NOTE-FROM-EARLIER]") || !strings.Contains(result, "Use tabs in pkg.") {
		t.Fatalf("read_file result %q: both notes must be appended", result)
	}
	if strings.Index(result, "[NOTE-FROM-EARLIER]") > strings.Index(result, "Use tabs in pkg.") {
		t.Fatalf("the earlier feature's note comes first: %q", result)
	}
}
