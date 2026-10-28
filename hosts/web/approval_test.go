//go:build unix

package httpserver_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// approvalRig is a rig on the local backend whose model asks to run `bash mk.sh`,
// a command that needs approval.
func approvalRig(t *testing.T) (*rig, string, string) {
	t.Helper()
	r := newRig(t, rigOpts{local: true, model: scripted("execute_command", `{"command":"bash mk.sh"}`)})
	ws := os.Getenv("LOCAL_WORKSPACE")
	if err := os.WriteFile(filepath.Join(ws, "mk.sh"), []byte("echo ran > ran.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := r.newThread("alice")
	evs := r.stream(r.startRun("alice", id, "run the script"), "alice")
	out := finished(t, evs)
	r.waitIdle(id, "alice")
	intr := out["outcome"].(map[string]any)["interrupts"].([]any)[0].(map[string]any)
	if intr["reason"] != "tool_approval" {
		t.Fatalf("interrupt = %v", intr)
	}
	return r, id, ws
}

func TestApprovalRoundTripApprovesAndRunsTheCommand(t *testing.T) {
	// Arrange
	r, id, ws := approvalRig(t)

	// Act
	th := r.json("GET", "/api/threads/"+id, "alice", "", 200)
	pending := th["pendingApproval"].(map[string]any)
	if _, err := os.Stat(filepath.Join(ws, "ran.txt")); err == nil {
		t.Fatal("the command ran before the approval")
	}
	resumed := r.json("POST", "/api/threads/"+id+"/resume", "alice",
		`{"interruptId":`+strconv.Quote(pending["id"].(string))+`,"decision":"approve","always":true}`, 202)
	final := finished(t, r.stream(resumed["id"].(string), "alice"))
	r.waitIdle(id, "alice")
	after := r.json("GET", "/api/threads/"+id, "alice", "", 200)

	// Assert
	if pending["toolName"] != "execute_command" {
		t.Fatalf("pendingApproval = %v", pending)
	}
	if _, err := os.Stat(filepath.Join(ws, "ran.txt")); err != nil {
		t.Fatal("the approved command did not run")
	}
	if final["outcome"].(map[string]any)["type"] == "interrupt" || after["pendingApproval"] != nil {
		t.Fatalf("approval still pending: %v / %v", final["outcome"], after["pendingApproval"])
	}
}

func TestApprovalDenialKeepsTheCommandFromRunning(t *testing.T) {
	r, id, ws := approvalRig(t)
	pending := r.json("GET", "/api/threads/"+id, "alice", "", 200)["pendingApproval"].(map[string]any)

	resumed := r.json("POST", "/api/threads/"+id+"/resume", "alice",
		`{"interruptId":`+strconv.Quote(pending["id"].(string))+`,"decision":"deny"}`, 202)
	finished(t, r.stream(resumed["id"].(string), "alice"))

	if _, err := os.Stat(filepath.Join(ws, "ran.txt")); err == nil {
		t.Fatal("the denied command ran")
	}
}

func TestAnotherUserCannotAnswerTheApproval(t *testing.T) {
	r, id, ws := approvalRig(t)
	pending := r.json("GET", "/api/threads/"+id, "alice", "", 200)["pendingApproval"].(map[string]any)

	resp := r.do("POST", "/api/threads/"+id+"/resume", "mallory",
		`{"interruptId":`+strconv.Quote(pending["id"].(string))+`,"decision":"approve"}`)

	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(ws, "ran.txt")); err == nil {
		t.Fatal("the command ran")
	}
}
