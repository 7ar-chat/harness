package approvals_test

import (
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/approvals"
)

func TestIsRiskyMirrorsWorksRule(t *testing.T) {
	// execute_command, write_file, edit_file and delete/remove/move/rename tools (MCP: the tool part) are risky; read-only tools are not.
	cases := map[string]bool{
		"execute_command": true, "write_file": true, "edit_file": true, "Write-File": true,
		"delete_file": true, "removeItem": true, "move_file": true, "rename": true, "Delete-Branch": true,
		"jira__delete_issue": true, "github__move_card": true, "drive__rename_file": true, "fs__remove-dir": true,
		"read_file": false, "list_files": false, "load_skill": false, "update_plan": false, "ask_user": false,
		"remember": false, "forget": false, "jira__list_issues": false, "jira__get_issue": false, "": false,
		"confluence__undelete_page": false, "srv__delete__all": true, "a__b__remove_x": true, "srv__get__all": false,
	}
	for name, want := range cases {
		// Act
		got := approvals.IsRisky(name)

		// Assert
		if got != want {
			t.Errorf("IsRisky(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestGatedOnlyInInteractiveMode(t *testing.T) {
	// The gate applies to interactive runs only; automatic (and unset) runs are never gated.
	cases := []struct {
		mode, tool string
		want       bool
	}{
		{store.InteractionInteractive, "write_file", true},
		{store.InteractionInteractive, "read_file", false},
		{store.InteractionAutomatic, "write_file", false},
		{"", "execute_command", false},
	}
	for _, tc := range cases {
		// Act
		got := approvals.Gated(tc.mode, tc.tool)

		// Assert
		if got != tc.want {
			t.Errorf("Gated(%q, %q) = %v, want %v", tc.mode, tc.tool, got, tc.want)
		}
	}
}

func TestPreviewIsCanonicalJSON(t *testing.T) {
	// Sorted keys, compact, no HTML escaping, numbers as written; empty is {} and invalid JSON is kept.
	cases := map[string]string{
		`{"path": "/a", "content": "<b>&</b>", "n": 1.0, "nested": {"z": 1, "a": [2, 1]}}`: `{"content":"<b>&</b>","n":1.0,"nested":{"a":[2,1],"z":1},"path":"/a"}`,
		``:                `{}`,
		`  {"é": "ü"}  `:  `{"é":"ü"}`,
		`{"command": "ls`: `{"command": "ls`,
		`{} {}`:           `{} {}`,
	}
	for in, want := range cases {
		// Act
		got := approvals.Preview(in)

		// Assert
		if got != want {
			t.Errorf("Preview(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPreviewMatchesLikeWork(t *testing.T) {
	// Empty expected matches, equal matches, a "..." suffix is a truncated prefix, anything else differs.
	cases := []struct {
		expected, current string
		want              bool
	}{
		{"", `{"a":1}`, true},
		{`{"a":1}`, ` {"a":1} `, true},
		{`{"a":"lon...`, `{"a":"long text"}`, true},
		{`{"a":1}`, `{"a":2}`, false},
		{`{"b":...`, `{"a":1}`, false},
	}
	for _, tc := range cases {
		// Act
		got := approvals.PreviewMatches(tc.expected, tc.current)

		// Assert
		if got != tc.want {
			t.Errorf("PreviewMatches(%q, %q) = %v, want %v", tc.expected, tc.current, got, tc.want)
		}
	}
}

func TestMatchesCallChecksIDToolAndArguments(t *testing.T) {
	// An approval matches only the same call id, tool and canonical arguments (key order does not matter).
	a := &store.Approval{ToolCallID: "c1", ToolName: "write_file", ToolInputPreview: approvals.Preview(`{"path":"/a","content":"x"}`)}
	call := func(id, name, args string) ai.ToolCall {
		return ai.ToolCall{ID: id, Function: ai.FunctionCall{Name: name, Arguments: args}}
	}
	cases := []struct {
		tc   ai.ToolCall
		want bool
	}{
		{call("c1", "write_file", `{"content":"x","path":"/a"}`), true},
		{call("c1", "write_file", `{"content":"y","path":"/a"}`), false},
		{call("c1", "edit_file", `{"content":"x","path":"/a"}`), false},
		{call("c2", "write_file", `{"content":"x","path":"/a"}`), false},
	}
	for i, tc := range cases {
		// Act
		got := approvals.MatchesCall(a, tc.tc)

		// Assert
		if got != tc.want {
			t.Errorf("case %d: MatchesCall = %v, want %v", i, got, tc.want)
		}
	}
}

func TestApprovalResultTexts(t *testing.T) {
	// Work's build_resume_packet instructions: deny, respond with details or none; approve has no text (the tool runs).
	cases := []struct {
		decision string
		resp     store.ApprovalResponse
		want     string
	}{
		{store.DecisionApprove, store.ApprovalResponse{}, ""},
		{store.DecisionDeny, store.ApprovalResponse{ResponseText: "ignored"}, "The user denied the pending action. Do not retry it. Choose a safe alternative and continue."},
		{store.DecisionRespond, store.ApprovalResponse{SelectedChoice: " A "}, "The user answered the pending question. Use this answer and continue: selected option: A."},
		{store.DecisionRespond, store.ApprovalResponse{}, "The user answered the pending question. Use this answer and continue: no extra details provided."},
	}
	for _, tc := range cases {
		// Act
		got := store.ApprovalResult(tc.decision, tc.resp)

		// Assert
		if got != tc.want {
			t.Errorf("ApprovalResult(%q) = %q, want %q", tc.decision, got, tc.want)
		}
	}
}
