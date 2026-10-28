//go:build unix

package clienttest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// approvalRig is a rig on the local sandbox whose model asks to run `bash mk.sh`, a command that needs
// approval, and a thread whose first run ended waiting for it.
func approvalRig(t *testing.T, mk Factory) (r *rig, threadID, interruptID, workspace string) {
	t.Helper()
	r = newRig(t, mk, rigOpts{local: true, model: scripted("execute_command", `{"command":"bash mk.sh"}`)})
	workspace = os.Getenv("LOCAL_WORKSPACE")
	if err := os.WriteFile(filepath.Join(workspace, "mk.sh"), []byte("echo ran > ran.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	threadID = newThread(t, r, r.alice)
	run := mustSend(t, r, r.alice, threadID, "run the script", client.SendOptions{Interactive: true})
	outcome, interrupts := finished(t, stream(t, r, r.alice, run.ID, 0))
	th := waitIdle(t, r, r.alice, threadID)
	if outcome != "interrupt" || len(interrupts) != 1 || interrupts[0].Reason != client.ReasonToolApproval {
		t.Fatalf("outcome %q interrupts %+v", outcome, interrupts)
	}
	if th.PendingApproval == nil || th.PendingApproval["toolName"] != "execute_command" || th.PendingApproval["id"] != interrupts[0].ID {
		t.Fatalf("pendingApproval = %v", th.PendingApproval)
	}
	return r, threadID, interrupts[0].ID, workspace
}

func testApproval(t *testing.T, mk Factory) {
	t.Run("approve runs the command", func(t *testing.T) {
		// Arrange
		r, id, interrupt, ws := approvalRig(t, mk)
		if _, err := os.Stat(filepath.Join(ws, "ran.txt")); err == nil {
			t.Fatal("the command ran before the approval")
		}

		// Act
		_, stranger := r.bob.Resume(r.ctx, id, client.ResumeRequest{InterruptID: interrupt, Decision: client.DecisionApprove})
		run, err := r.alice.Resume(r.ctx, id, client.ResumeRequest{InterruptID: interrupt, Reason: client.ReasonToolApproval, Decision: client.DecisionApprove, Always: true})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		outcome, _ := finished(t, stream(t, r, r.alice, run.ID, 0))
		th := waitIdle(t, r, r.alice, id)

		// Assert
		wantErr(t, "another user answering", stranger, client.ErrNotFound)
		if _, err := os.Stat(filepath.Join(ws, "ran.txt")); err != nil {
			t.Fatal("the approved command did not run")
		}
		if outcome == "interrupt" || th.PendingApproval != nil {
			t.Fatalf("approval still pending: %q %v", outcome, th.PendingApproval)
		}
	})
	t.Run("deny keeps the command from running", func(t *testing.T) {
		// Arrange
		r, id, interrupt, ws := approvalRig(t, mk)

		// Act: the reason is inferred from the decision
		run, err := r.alice.Resume(r.ctx, id, client.ResumeRequest{InterruptID: interrupt, Decision: client.DecisionDeny})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		finished(t, stream(t, r, r.alice, run.ID, 0))

		// Assert
		if _, err := os.Stat(filepath.Join(ws, "ran.txt")); err == nil {
			t.Fatal("the denied command ran")
		}
	})
}

func testFiles(t *testing.T, mk Factory) {
	// Arrange: the model writes out.md in the thread's workspace.
	r := newRig(t, mk, rigOpts{local: true, model: scripted("write_file", `{"path":"out.md","content":"hello files"}`)})
	id := newThread(t, r, r.alice)
	run := mustSend(t, r, r.alice, id, "write it", client.SendOptions{})
	finished(t, stream(t, r, r.alice, run.ID, 0))
	waitIdle(t, r, r.alice, id)

	// Act
	files, err := r.alice.ListFiles(r.ctx, id, "")
	dl, dlErr := r.alice.DownloadFile(r.ctx, id, "out.md")
	_, missing := r.alice.DownloadFile(r.ctx, id, "nope.md")
	_, escape := r.alice.ListFiles(r.ctx, id, "../../etc")
	_, escapeDL := r.alice.DownloadFile(r.ctx, id, "/etc/passwd")
	_, dir := r.alice.DownloadFile(r.ctx, id, "")

	// Assert
	var found *client.File
	for i := range files {
		if files[i].Name == "out.md" {
			found = &files[i]
		}
	}
	if err != nil || found == nil || found.Type != "file" || found.Size != int64(len("hello files")) || found.Path != "/workspace/out.md" {
		t.Fatalf("ListFiles = %+v, %v", files, err)
	}
	if dlErr != nil || string(dl.Data) != "hello files" || dl.Name != "out.md" {
		t.Fatalf("DownloadFile = %+v, %v", dl, dlErr)
	}
	wantErr(t, "a missing file", missing, client.ErrNotFound)
	wantErr(t, "a path outside the workspace", escape, client.ErrInvalidPath)
	wantErr(t, "a download outside the workspace", escapeDL, client.ErrInvalidPath)
	wantErr(t, "downloading the workspace", dir, client.ErrInvalidPath)
}
