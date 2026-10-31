package approvals_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/approvals"
)

// recordingStore keeps the approvals the gate creates (err: creating fails) and answers modes like liveModes.
type recordingStore struct {
	liveModes
	created []*store.Approval
	err     error
}

func (s *recordingStore) CreateApproval(_ context.Context, a *store.Approval) error {
	if s.err != nil {
		return s.err
	}
	a.ID = "ap-1"
	s.created = append(s.created, a)
	return nil
}

func toolCall(id, name, args string) ai.ToolCall {
	return ai.ToolCall{ID: id, Function: ai.FunctionCall{Name: name, Arguments: args}}
}

func TestRegisterRiskyMakesAHostToolNeedApproval(t *testing.T) {
	// A host's own tool registered as risky is gated like execute_command, whatever the spelling, and nothing else changes.
	// Arrange
	approvals.RegisterRisky("Host-Deploy_Cov")
	gate := &approvals.Gate{}
	run := interactiveRun()

	// Act
	spelled := approvals.IsRisky("host_deploy_cov")
	viaMCP := approvals.IsRisky("srv__hostDeployCov")
	asked := gate.NeedsApproval(context.Background(), run, "host_deploy_cov", `{}`)
	askedAutomatic := gate.NeedsApproval(context.Background(), &store.Run{InteractionMode: store.InteractionAutomatic}, "host_deploy_cov", `{}`)
	other := gate.NeedsApproval(context.Background(), run, "host_status_cov", `{}`)

	// Assert
	if !spelled || !viaMCP || !asked || askedAutomatic || other {
		t.Fatalf("spelled %v via MCP %v asked %v asked in automatic %v other %v", spelled, viaMCP, asked, askedAutomatic, other)
	}
}

func TestTrustMCPToolIsAlwaysAskedWhileTheUserCanAnswer(t *testing.T) {
	// The approval tool of a project's MCP server asks in interactive runs even when its kind was approved for the run,
	// and is refused where nobody can answer.
	// Arrange
	const tool = approvals.TrustMCPToolPrefix + "files"
	gate := &approvals.Gate{}
	interactive := &store.Run{InteractionMode: store.InteractionInteractive, ApprovedTools: []string{approvals.Kind(tool)}}
	automatic := &store.Run{InteractionMode: store.InteractionAutomatic}

	// Act
	asked := gate.NeedsApproval(context.Background(), interactive, tool, `{}`)
	askedAutomatic := gate.NeedsApproval(context.Background(), automatic, tool, `{}`)
	risky := gate.Risky(context.Background(), automatic, tool, `{}`)
	riskyApproved := gate.Risky(context.Background(), interactive, tool, `{}`)
	deniedAutomatic := gate.Denial(context.Background(), automatic, tool, `{}`)
	deniedInteractive := gate.Denial(context.Background(), interactive, tool, `{}`)
	otherDenied := gate.Denial(context.Background(), automatic, "execute_command", `{"command":"ls"}`)

	// Assert
	if !asked || askedAutomatic || !risky || !riskyApproved {
		t.Fatalf("asked %v asked in automatic %v risky %v risky when approved %v", asked, askedAutomatic, risky, riskyApproved)
	}
	if deniedAutomatic != approvals.TrustMCPDeniedResult || deniedInteractive != "" || otherDenied != "" {
		t.Fatalf("denial automatic %q interactive %q other tool %q", deniedAutomatic, deniedInteractive, otherDenied)
	}
}

func TestInteractiveReadsTheLiveMode(t *testing.T) {
	// Interactive follows the mode switched during the run and counts an unreadable mode as interactive.
	// Arrange
	live := liveModes{modes: map[string]string{"a": store.InteractionInteractive, "b": store.InteractionAutomatic}}
	gate := &approvals.Gate{Store: live}
	broken := &approvals.Gate{Store: liveModes{err: errors.New("down")}}

	// Act
	switchedOn := gate.Interactive(context.Background(), &store.Run{ID: "a", InteractionMode: store.InteractionAutomatic})
	switchedOff := gate.Interactive(context.Background(), &store.Run{ID: "b", InteractionMode: store.InteractionInteractive})
	unreadable := broken.Interactive(context.Background(), &store.Run{ID: "c", InteractionMode: store.InteractionAutomatic})

	// Assert
	if !switchedOn || switchedOff || !unreadable {
		t.Fatalf("switched on %v switched off %v unreadable %v", switchedOn, switchedOff, unreadable)
	}
}

func TestPauseRecordsAPendingApprovalAndReturnsItsInterrupt(t *testing.T) {
	// The approval keeps the canonical preview of the call and the interrupt carries the approval's ids.
	// Arrange
	st := &recordingStore{}
	gate := &approvals.Gate{Store: st}
	run := &store.Run{ID: "run-1", ThreadID: "thread-1"}

	// Act
	it, err := gate.Pause(context.Background(), run, toolCall("call-1", "write_file", `{"path":"/a","content":"x"}`))

	// Assert
	if err != nil || len(st.created) != 1 {
		t.Fatalf("err %v created %d", err, len(st.created))
	}
	a := st.created[0]
	if a.Kind != store.ApprovalKindRiskyTool || a.ThreadID != "thread-1" || a.RunID != "run-1" || a.ToolCallID != "call-1" ||
		a.ToolName != "write_file" || a.ToolInputPreview != `{"content":"x","path":"/a"}` {
		t.Fatalf("approval %+v", a)
	}
	if it.Reason != store.ReasonToolApproval || it.Question != "write_file" || it.ID != "ap-1" || it.ToolCallID != "call-1" || it.Approval != a || !it.AllowFreeText {
		t.Fatalf("interrupt %+v", it)
	}
	if !approvals.MatchesCall(a, toolCall("call-1", "write_file", `{"content":"x","path":"/a"}`)) {
		t.Fatal("the recorded approval does not match the call it was made for")
	}
}

func TestPauseFailsWhenTheApprovalCannotBeStored(t *testing.T) {
	// Fail closed: no interrupt is returned when the approval was not recorded.
	// Arrange
	gate := &approvals.Gate{Store: &recordingStore{err: errors.New("db down")}}

	// Act
	it, err := gate.Pause(context.Background(), &store.Run{ID: "r"}, toolCall("c", "execute_command", `{"command":"ls"}`))

	// Assert
	if err == nil || !strings.Contains(err.Error(), "db down") || it != nil {
		t.Fatalf("interrupt %v err %v", it, err)
	}
}

func TestPauseDescribesAnMCPServerInsteadOfEchoingArguments(t *testing.T) {
	// The approval of a project's MCP server shows the server description; when the describer knows no such server the call is refused rather than asked blindly.
	// Arrange
	const tool = approvals.TrustMCPToolPrefix + "files"
	st := &recordingStore{}
	described := &approvals.Gate{Store: st, Describe: func(name string) string {
		if name == tool {
			return "Run npx files-mcp"
		}
		return ""
	}}
	unknown := &approvals.Gate{Store: st, Describe: func(string) string { return "" }}
	run := &store.Run{ID: "r", ThreadID: "t"}

	// Act
	_, errDescribed := described.Pause(context.Background(), run, toolCall("c1", tool, `{"ignored":true}`))
	_, errUnknown := unknown.Pause(context.Background(), run, toolCall("c2", tool, `{}`))

	// Assert
	if errDescribed != nil || len(st.created) != 1 || st.created[0].ToolInputPreview != "Run npx files-mcp" {
		t.Fatalf("described: err %v created %+v", errDescribed, st.created)
	}
	if errUnknown == nil || len(st.created) != 1 {
		t.Fatalf("unknown server: err %v, %d approvals recorded; want a refusal and no new approval", errUnknown, len(st.created))
	}
	if !approvals.MatchesCall(st.created[0], toolCall("c1", tool, `{"other":1}`)) {
		t.Fatal("the MCP approval must match the call whatever its arguments")
	}
}

func TestPreviewOfValueThatCannotBeReencodedIsKept(t *testing.T) {
	// Arguments that are valid JSON but not an object are canonicalised too; trailing garbage keeps the text.
	// Act
	list := approvals.Preview(` [ 3 , "a<b" ] `)
	garbage := approvals.Preview(`{"a":1} x`)

	// Assert
	if list != `[3,"a<b"]` || garbage != `{"a":1} x` {
		t.Fatalf("list %q garbage %q", list, garbage)
	}
}
