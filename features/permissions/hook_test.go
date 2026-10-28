package permissions

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/approvals"
	"github.com/GhaziBenDahmane/harness/store"
)

type fakeStore struct{ created []*store.Approval }

func (f *fakeStore) CreateApproval(_ context.Context, a *store.Approval) error {
	a.ID = "ap1"
	f.created = append(f.created, a)
	return nil
}
func (f *fakeStore) RunInteractionMode(context.Context, string) (string, error) { return "", nil }

func newHook(mode Mode, rules ...Rule) (*Hook, *fakeStore) {
	fs := &fakeStore{}
	return &Hook{Policy: testPolicy(mode, rules...), Inner: &approvals.Gate{Store: fs}}, fs
}

func TestHookDecisionsPerMode(t *testing.T) {
	// ask: needs approval; deny: refused by Denial; allow: neither.
	type want struct{ needs, risky, denied bool }
	tests := []struct {
		mode      Mode
		tool, arg string
		want      want
	}{
		{ModeAsk, "execute_command", cmdArgs("go test ./..."), want{}},
		{ModeAsk, "execute_command", cmdArgs("git push"), want{needs: true, risky: true}},
		{ModeAsk, "execute_command", cmdArgs("rm -rf /"), want{denied: true}},
		{ModeAsk, "write_file", pathArgs("a.go"), want{}},
		{ModeAsk, "write_file", pathArgs("/etc/hosts"), want{needs: true, risky: true}},
		{ModeAsk, "edit_file", pathArgs(".git/HEAD"), want{denied: true}},
		{ModeAutoEdit, "write_file", pathArgs("/etc/hosts"), want{}},
		{ModeAutoEdit, "execute_command", cmdArgs("git push"), want{needs: true, risky: true}},
		{ModeYolo, "execute_command", cmdArgs("git push"), want{}},
		{ModeYolo, "execute_command", cmdArgs("rm -rf ~"), want{denied: true}},
		{ModeYoloUnsafe, "execute_command", cmdArgs("rm -rf ~"), want{}},
		{ModeReadOnly, "execute_command", cmdArgs("ls"), want{}},
		{ModeReadOnly, "execute_command", cmdArgs("touch x"), want{denied: true}},
		{ModeReadOnly, "write_file", pathArgs("a.go"), want{denied: true}},
		// Tools the policy has no opinion on follow the interactive gate (automatic run: not asked).
		{ModeAsk, "read_file", `{"path":"x"}`, want{}},
		{ModeAsk, "list_files", `{}`, want{}},
	}
	run := &store.Run{ID: "r1", ThreadID: "t1"}
	for _, tt := range tests {
		t.Run(string(tt.mode)+" "+tt.tool+" "+tt.arg, func(t *testing.T) {
			h, _ := newHook(tt.mode)
			got := want{
				needs:  h.NeedsApproval(context.Background(), run, tt.tool, tt.arg),
				risky:  h.Risky(context.Background(), run, tt.tool, tt.arg),
				denied: h.Denial(context.Background(), run, tt.tool, tt.arg) != "",
			}
			if got != tt.want {
				t.Fatalf("%s: got %+v, want %+v", tt.arg, got, tt.want)
			}
		})
	}
}

func TestHookDefersUnjudgedToolsToTheInteractiveGate(t *testing.T) {
	h, _ := newHook(ModeAsk)
	interactive := &store.Run{ID: "r1", InteractionMode: store.InteractionInteractive}
	gate := &approvals.Gate{}
	h.Inner = gate
	// delete_* tools are risky for the gate in an interactive run, and the hook has no rule for them.
	if !h.NeedsApproval(context.Background(), interactive, "delete_thing", `{"path":"/x"}`) {
		t.Fatal("unjudged risky tool was not deferred to the gate")
	}
	if h.NeedsApproval(context.Background(), &store.Run{ID: "r2"}, "delete_thing", `{"path":"/x"}`) {
		t.Fatal("automatic runs must not ask for unjudged tools")
	}
}

func TestHookApprovedEditKindIsNotAskedAgainButCommandsAre(t *testing.T) {
	h, _ := newHook(ModeAsk)
	run := &store.Run{ID: "r1", ApprovedTools: []string{"writefile", "executecommand"}}
	if h.NeedsApproval(context.Background(), run, "write_file", pathArgs("/etc/hosts")) {
		t.Error("an edit kind approved for the run was asked again")
	}
	if !h.NeedsApproval(context.Background(), run, "execute_command", cmdArgs("git push")) {
		t.Error("approving the command kind must not cover every later command")
	}
}

func TestHookPauseRecordsTheApproval(t *testing.T) {
	h, fs := newHook(ModeAsk)
	tc := schema.ToolCall{ID: "call1", Function: schema.FunctionCall{Name: "execute_command", Arguments: cmdArgs("git push")}}

	intr, err := h.Pause(context.Background(), &store.Run{ID: "r1", ThreadID: "t1"}, tc)

	if err != nil || intr == nil || intr.Approval == nil || len(fs.created) != 1 {
		t.Fatalf("interrupt %+v err %v created %d", intr, err, len(fs.created))
	}
	if a := fs.created[0]; a.ToolName != "execute_command" || a.ToolCallID != "call1" || !strings.Contains(a.ToolInputPreview, "git push") {
		t.Fatalf("approval = %+v", a)
	}
}

func TestDeniedMessageIsClear(t *testing.T) {
	msg := DeniedMessage(Decision{Action: Deny, Reason: "writing .env is blocked"})
	for _, part := range []string{"permission denied", "writing .env is blocked", "was not run", "do not retry", ".har/permissions.yml"} {
		if !strings.Contains(msg, part) {
			t.Errorf("message %q lacks %q", msg, part)
		}
	}
}
