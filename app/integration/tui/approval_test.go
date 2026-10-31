//go:build unix

package tui_integration_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/hosts/tui"
)

// approvalRig is a local-sandbox rig whose model asks to run `bash mk.sh`, a
// command that needs approval; mk.sh leaves ran.txt in the workspace.
func approvalRig(t *testing.T, interactive bool) (*toolRig, string) {
	t.Helper()
	r := newToolRig(t, interactive, true, scriptedTool("execute_command", `{"command":"bash mk.sh"}`, "Script finished"))
	ws := os.Getenv("LOCAL_WORKSPACE")
	if err := os.WriteFile(filepath.Join(ws, "mk.sh"), []byte("echo ran > ran.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return r, ws
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// finishOf returns the RUN_FINISHED event of decoded events.
func finishOf(t *testing.T, evs []tui.Ev) tui.Ev {
	t.Helper()
	for _, ev := range evs {
		if ev.Type == agui.RunFinished {
			return ev
		}
	}
	t.Fatalf("no RUN_FINISHED in %d events", len(evs))
	return tui.Ev{}
}

// askForApproval runs the prompt and returns the approval interrupt it ends with.
func askForApproval(t *testing.T, r *toolRig, ws string) agui.Interrupt {
	t.Helper()
	runID, err := r.be.Send(r.ctx, "run the script")
	if err != nil {
		t.Fatal(err)
	}
	fin := finishOf(t, r.drain(runID))
	if fin.Outcome != "interrupt" || len(fin.Interrupts) != 1 {
		t.Fatalf("outcome %q interrupts %v", fin.Outcome, fin.Interrupts)
	}
	in := fin.Interrupts[0]
	if in.Reason != "tool_approval" || in.ID == "" {
		t.Fatalf("interrupt %+v", in)
	}
	if fileExists(filepath.Join(ws, "ran.txt")) {
		t.Fatal("the command ran before the approval")
	}
	return in
}

func TestClientBackendApprovalApproveRunsTheCommandAndFinishes(t *testing.T) {
	// Arrange
	r, ws := approvalRig(t, true)
	in := askForApproval(t, r, ws)

	// Act
	resumed, err := r.be.Resume(r.ctx, tui.ResumeRequest{InterruptID: in.ID, Reason: in.Reason, Decision: tui.DecisionApprove})
	if err != nil {
		t.Fatal(err)
	}
	evs := r.drain(resumed)

	// Assert
	fin := finishOf(t, evs)
	if fin.Outcome != "success" {
		t.Fatalf("outcome %q", fin.Outcome)
	}
	var text string
	for _, ev := range evs {
		text += ev.Delta
	}
	if !strings.Contains(text, "Script finished") {
		t.Fatalf("text %q", text)
	}
	got, err := os.ReadFile(filepath.Join(ws, "ran.txt"))
	if err != nil || string(got) != "ran\n" {
		t.Fatalf("ran.txt %q, %v", got, err)
	}
}

func TestClientBackendApprovalDenyKeepsTheCommandFromRunning(t *testing.T) {
	// Arrange
	r, ws := approvalRig(t, true)
	in := askForApproval(t, r, ws)

	// Act
	resumed, err := r.be.Resume(r.ctx, tui.ResumeRequest{InterruptID: in.ID, Reason: in.Reason, Decision: tui.DecisionDeny})
	if err != nil {
		t.Fatal(err)
	}
	evs := r.drain(resumed)

	// Assert
	fin := finishOf(t, evs)
	if fin.Outcome != "success" {
		t.Fatalf("outcome %q", fin.Outcome)
	}
	if fileExists(filepath.Join(ws, "ran.txt")) {
		t.Fatal("the denied command ran")
	}
	// The denial reaches the model as the tool's result.
	denied := false
	for _, ev := range evs {
		if ev.Type == agui.ToolCallResult && strings.Contains(ev.Content, "denied") {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("no tool result reports the denial: %+v", evs)
	}
}

func TestPrintModeExitsWithTwoWhenACommandNeedsApproval(t *testing.T) {
	// Arrange
	r, ws := approvalRig(t, false)
	var stdout, stderr bytes.Buffer

	// Act
	code := tui.RunPrint(r.ctx, r.be, "run the script", tui.PrintOptions{Stdout: &stdout, Stderr: &stderr})

	// Assert
	if code != tui.ExitInterrupted {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "waiting for input (tool_approval)") || !strings.Contains(stderr.String(), "execute_command") {
		t.Fatalf("stderr %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout %q", stdout.String())
	}
	if fileExists(filepath.Join(ws, "ran.txt")) {
		t.Fatal("the command ran without approval")
	}
}
