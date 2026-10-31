//go:build unix

package harness_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
)

// writesFileModel writes content to a.txt on the first call of a run, then answers "done".
func writesFileModel(content string) *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("title")}
		}
		if c.Messages[len(c.Messages)-1].Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("done")}
		}
		args := `{"path":"/workspace/a.txt","content":"` + content + `\n"}`
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "w1", "write_file", args), "tool_calls")}}
	})
}

// localRepoHarness is a harness on the local backend over a git workspace, with the permission layer in yolo mode
// (it snapshots the working tree before every run, which is what Undo restores).
func localRepoHarness(t *testing.T, model *llmtest.Model, mod func(*harness.Options)) (*harness.Harness, string) {
	t.Helper()
	repo := gitRepo(t)
	env := map[string]string{
		"SANDBOX_BACKEND": "local", "LOCAL_WORKSPACE": repo, "LOCAL_STATE_DIR": t.TempDir(),
		"AGENT_PERMISSIONS": "yolo", "RUNTIME_SHUTDOWN_GRACE_SECONDS": "1",
	}
	h := buildFacade(t, env, model, func(o *harness.Options) {
		o.Backend = nil
		o.Features = []harness.Feature{harness.Sandbox(), harness.Plan(), harness.Permissions(false)}
		if mod != nil {
			mod(o)
		}
	})
	return h, repo
}

func TestUndoRestoresTheWorkspaceFilesAndRedoWritesThemAgain(t *testing.T) {
	// Arrange: the run rewrites a.txt from v1 to v2
	h, repo := localRepoHarness(t, writesFileModel("v2"), nil)
	ctx := startFacade(t, h)
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "change a.txt", harness.RunOptions{})
	if got, _ := readFile(repo, "a.txt"); got != "v2" {
		t.Fatalf("the run did not write the file: %q", got)
	}

	// Act
	undone, err := h.Undo(ctx, "alice", th.ID)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readFile(repo, "a.txt"); got != "v1" {
		t.Fatalf("a.txt after undo = %q, want v1", got)
	}
	if !slices.Equal(undone.Files, []string{"a.txt"}) || undone.Prompt != "change a.txt" {
		t.Fatalf("undo step %+v", undone)
	}

	// Act
	redone, err := h.Redo(ctx, "alice", th.ID)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readFile(repo, "a.txt"); got != "v2" {
		t.Fatalf("a.txt after redo = %q, want v2", got)
	}
	if !slices.Equal(redone.Files, []string{"a.txt"}) {
		t.Fatalf("redo step %+v", redone)
	}
	if msgs := messagesOf(ctx, t, h, th.ID); len(msgs) == 0 || msgs[0].Content != "change a.txt" {
		t.Fatalf("messages after redo %q", contents(msgs))
	}
}

func TestUndoThatCannotEditTheHistoryPutsTheFilesBack(t *testing.T) {
	// Arrange
	boom := errors.New("store down")
	st := &flakyStore{Store: memstore.New()}
	h, repo := localRepoHarness(t, writesFileModel("v2"), func(o *harness.Options) { o.Store = st })
	ctx := startFacade(t, h)
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "change a.txt", harness.RunOptions{})
	st.supersedeErr = boom

	// Act
	_, err := h.Undo(ctx, "alice", th.ID)

	// Assert: the messages stayed, so the files must not be left restored
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if got, _ := readFile(repo, "a.txt"); got != "v2" {
		t.Fatalf("a.txt = %q: a failed undo must leave the files as the run wrote them", got)
	}
	if msgs := messagesOf(ctx, t, h, th.ID); len(msgs) == 0 {
		t.Fatal("the messages were removed")
	}
}

func TestUndoOfAWorkspaceWithoutASnapshotStillRemovesTheMessages(t *testing.T) {
	// Arrange: the local workspace defaults to the current directory, which is not a repository
	cwd := t.TempDir()
	t.Chdir(cwd)
	h := buildFacade(t, map[string]string{"SANDBOX_BACKEND": "local", "LOCAL_STATE_DIR": t.TempDir(), "RUNTIME_SHUTDOWN_GRACE_SECONDS": "1"},
		echoModel(), func(o *harness.Options) { o.Backend = nil })
	ctx := startFacade(t, h)
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "hello", harness.RunOptions{})

	// Act
	step, err := h.Undo(ctx, "alice", th.ID)

	// Assert
	if err != nil || step.Prompt != "hello" || len(step.Files) != 0 {
		t.Fatalf("undo %+v %v", step, err)
	}
	if msgs := messagesOf(ctx, t, h, th.ID); len(msgs) != 0 {
		t.Fatalf("messages %q", contents(msgs))
	}
}
