//go:build unix

package conversation_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
)

// approvalModel asks to run `bash mk.sh` for "run the script", answers "Script finished" once it has the tool
// result (whatever it says) and "Anything else?" to any other message.
func approvalModel() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if titleCall(c) {
			return titleTurn()
		}
		last := c.Messages[len(c.Messages)-1]
		switch {
		case last.Role == ai.RoleTool:
			return llmtest.Turn{Chunks: llmtest.Text("Script finished")}
		case last.Role == ai.RoleUser && last.Content == "run the script":
			return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "execute_command", `{"command":"bash mk.sh"}`), "tool_calls")}}
		}
		return llmtest.Turn{Chunks: llmtest.Text("Anything else?")}
	})
}

// approvalRig is a rig on the local sandbox (which asks before executing commands in interactive runs) whose first
// run, on a fresh thread of alice, ended waiting for the approval of `bash mk.sh`; mk.sh appends a line to ran.txt.
type approvalRig struct {
	*rig
	model     *llmtest.Model
	thread    string
	workspace string
	firstRun  string
	events    []event
}

func newApprovalRig(t *testing.T) *approvalRig {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	model := approvalModel()
	a := &approvalRig{rig: newRig(t, rigOpts{local: true, model: model}), model: model}
	a.workspace = os.Getenv("LOCAL_WORKSPACE")
	if err := os.WriteFile(filepath.Join(a.workspace, "mk.sh"), []byte("echo ran >> ran.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.thread = a.newThread("alice")
	a.firstRun = a.send("alice", a.thread, "run the script", true)
	a.events = a.replay(a.firstRun, "alice")
	a.waitIdle("alice", a.thread)
	return a
}

func (a *approvalRig) pending(user string) map[string]any {
	a.t.Helper()
	p, _ := a.thread0(user)["pendingApproval"].(map[string]any)
	return p
}

func (a *approvalRig) thread0(user string) map[string]any { return a.rig.thread(user, a.thread) }

// resume answers the pending approval as user with the decision and returns the status and body.
func (a *approvalRig) resume(user, interruptID, decision string) (int, map[string]any) {
	a.t.Helper()
	status, b := a.request("POST", "/api/threads/"+a.thread+"/resume", user, `{"interruptId":"`+interruptID+`","decision":"`+decision+`"}`)
	return status, decodeObject(a.t, b)
}

// ran returns the content of ran.txt ("" when the command never ran).
func (a *approvalRig) ran() string {
	b, err := os.ReadFile(filepath.Join(a.workspace, "ran.txt"))
	if err != nil && !os.IsNotExist(err) {
		a.t.Fatal(err)
	}
	return string(b)
}

// roles lists role:toolCallId-or-content of the transcript's messages.
func transcript(th map[string]any) []string {
	var out []string
	for _, m := range th["messages"].([]any) {
		m := m.(map[string]any)
		out = append(out, m["role"].(string)+":"+m["content"].(string))
	}
	return out
}

func TestRunNeedingApprovalEndsWithAnInterruptAndLeavesTheCommandUnrun(t *testing.T) {
	// Arrange / Act
	a := newApprovalRig(t)

	// Assert
	typ, interrupts := outcome(t, a.events)
	if typ != "interrupt" || len(interrupts) != 1 || interrupts[0]["reason"] != "tool_approval" {
		t.Fatalf("outcome %q interrupts %v, want one tool_approval interrupt", typ, interrupts)
	}
	p := a.pending("alice")
	if p == nil || p["toolName"] != "execute_command" || p["status"] != "pending" || p["id"] != interrupts[0]["id"] || p["toolCallId"] != "c1" {
		t.Fatalf("pendingApproval = %v, want the pending execute_command approval %v", p, interrupts[0]["id"])
	}
	run := a.run("alice", a.firstRun)
	if run["status"] != "completed" {
		t.Errorf("status of the interrupted run = %v: the API reports it completed (the wait is carried by the thread)", run["status"])
	}
	if th := a.thread0("alice"); th["activeRun"] != nil {
		t.Errorf("activeRun = %v while only an approval is pending", th["activeRun"])
	}
	if got := a.ran(); got != "" {
		t.Errorf("mk.sh ran before the approval: ran.txt = %q", got)
	}
}

func TestApprovingRunsTheCommandOnceInAContinuationRunAndClearsThePending(t *testing.T) {
	// Arrange
	a := newApprovalRig(t)
	p := a.pending("alice")

	// Act
	status, resumed := a.resume("alice", p["id"].(string), "approve")
	evs := a.replay(resumed["id"].(string), "alice")
	th := a.waitIdle("alice", a.thread)

	// Assert
	if status != 202 || resumed["id"] == a.firstRun || resumed["threadId"] != a.thread {
		t.Fatalf("resume = %d %v, want 202 and a new run %q of the thread", status, resumed, a.firstRun)
	}
	if typ, _ := outcome(t, evs); typ != "success" {
		t.Errorf("continuation outcome %q (%v), want success", typ, types(evs))
	}
	if got := a.ran(); got != "ran\n" {
		t.Errorf("ran.txt = %q, want exactly one run of the script", got)
	}
	if th["pendingApproval"] != nil {
		t.Errorf("pendingApproval = %v after the approval", th["pendingApproval"])
	}
	want := []string{"user:run the script", "assistant:", `tool:{"exitCode":0}`, "assistant:Script finished"}
	if got := transcript(th); !slices.Equal(got, want) {
		t.Errorf("transcript:\n got %q\nwant %q", got, want)
	}
	last := agentCalls(a.model)
	if h := history(last[len(last)-1]); !slices.Contains(h, `tool:{"exitCode":0}`) {
		t.Errorf("the model's last request %q lacks the command's result", h)
	}
	if run := a.run("alice", resumed["id"].(string)); run["status"] != "completed" {
		t.Errorf("continuation status = %v", run["status"])
	}
}

func TestDenyingKeepsTheCommandFromRunningAndTheThreadUsable(t *testing.T) {
	// Arrange
	a := newApprovalRig(t)
	p := a.pending("alice")

	// Act
	status, resumed := a.resume("alice", p["id"].(string), "deny")
	evs := a.replay(resumed["id"].(string), "alice")
	th := a.waitIdle("alice", a.thread)
	next := a.send("alice", a.thread, "thanks", true)
	nextEvs := a.replay(next, "alice")
	after := a.waitIdle("alice", a.thread)

	// Assert
	if status != 202 {
		t.Fatalf("resume status %d, want 202", status)
	}
	if typ, _ := outcome(t, evs); typ != "success" {
		t.Errorf("run after the denial: outcome %q, want it to complete gracefully", typ)
	}
	if text, _ := deltas(evs); text != "Script finished" {
		t.Errorf("model's final text after the denial = %q", text)
	}
	if got := a.ran(); got != "" {
		t.Errorf("the denied command ran: ran.txt = %q", got)
	}
	tr := transcript(th)
	if len(tr) != 4 || !strings.HasPrefix(tr[2], "tool:") || !strings.Contains(tr[2], "denied") {
		t.Errorf("transcript = %q, want the tool message to say the user denied the action", tr)
	}
	if th["pendingApproval"] != nil {
		t.Errorf("pendingApproval = %v after the denial", th["pendingApproval"])
	}
	if text, _ := deltas(nextEvs); text != "Anything else?" {
		t.Errorf("follow-up answer = %q", text)
	}
	if typ, _ := outcome(t, nextEvs); typ != "success" {
		t.Errorf("follow-up outcome %q", typ)
	}
	if n := len(transcript(after)); n != 6 {
		t.Errorf("transcript has %d messages after the follow-up, want 6", n)
	}
}

func TestAnotherUserCannotAnswerAnApprovalAndTheOwnerStillCan(t *testing.T) {
	// Arrange
	a := newApprovalRig(t)
	p := a.pending("alice")
	id := p["id"].(string)

	// Act
	status, _ := a.resume("mallory", id, "approve")
	read, _ := a.request("GET", "/api/threads/"+a.thread, "mallory", "")
	stillPending := a.pending("alice")
	ranBefore := a.ran()
	ownerStatus, resumed := a.resume("alice", id, "approve")
	a.replay(resumed["id"].(string), "alice")
	a.waitIdle("alice", a.thread)

	// Assert
	if status != 404 || read != 404 {
		t.Errorf("another user: resume %d, read %d, want 404 for both", status, read)
	}
	if stillPending == nil || stillPending["id"] != id || stillPending["status"] != "pending" {
		t.Errorf("pendingApproval after the foreign attempt = %v, want it still pending", stillPending)
	}
	if ranBefore != "" {
		t.Errorf("the command ran for a foreign approval: %q", ranBefore)
	}
	if ownerStatus != 202 || a.ran() != "ran\n" {
		t.Errorf("control: the owner's approval = %d, ran.txt = %q, want 202 and one run", ownerStatus, a.ran())
	}
}

func TestAnsweringTheSameApprovalTwiceIsRefusedAndRunsTheCommandOnce(t *testing.T) {
	// Arrange
	a := newApprovalRig(t)
	id := a.pending("alice")["id"].(string)

	// Act
	first, resumed := a.resume("alice", id, "approve")
	immediate, _ := a.resume("alice", id, "approve") // while the continuation may still run
	a.replay(resumed["id"].(string), "alice")
	a.waitIdle("alice", a.thread)
	late, lateBody := a.resume("alice", id, "approve") // once it is over
	a.waitIdle("alice", a.thread)

	// Assert
	if first != 202 {
		t.Fatalf("first answer: %d, want 202", first)
	}
	if immediate != 409 || late != 409 {
		t.Errorf("second answers: %d (immediate), %d (late: %v), want 409 conflict", immediate, late, lateBody)
	}
	if got := a.ran(); got != "ran\n" {
		t.Errorf("ran.txt = %q, want the script to have run exactly once", got)
	}
	if n := len(agentCalls(a.model)); n != 2 {
		t.Errorf("%d agent model calls, want 2 (the tool call, then the answer)", n)
	}
}
