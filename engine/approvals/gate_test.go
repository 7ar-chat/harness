package approvals_test

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/approvals"
)

func TestGateAsksOnlyForRiskyActions(t *testing.T) {
	// Interactive runs ask for commands, writes/deletes/moves outside /workspace/outputs and /workspace/.work, and mutating MCP tools; never for reads or work-area writes.
	// Arrange
	gate := &approvals.Gate{}
	run := &store.Run{InteractionMode: store.InteractionInteractive}
	cases := []struct {
		tool, args string
		want       bool
	}{
		{"execute_command", `{"command":"ls"}`, true},
		{"write_file", `{"path":"/workspace/outputs/report.docx","content":"x"}`, false},
		{"write_file", `{"path":"outputs/deck.pptx","content":"x"}`, false},
		{"edit_file", `{"path":"/workspace/.work/tmp.py","old":"a","new":"b"}`, false},
		{"write_file", `{"path":"/workspace/report.md","content":"x"}`, true},
		{"write_file", `{"path":"/workspace/outputs/../setup.sh","content":"x"}`, true},
		{"write_file", `{"path":"/workspace/outputsX/a","content":"x"}`, true},
		{"edit_file", `{"path":"/etc/hosts"}`, true},
		{"write_file", `{"content":"no path"}`, true},
		{"delete_file", `{"path":"/workspace/.work/old.csv"}`, false},
		{"delete_file", `{"path":"/workspace/data.csv"}`, true},
		{"move_file", `{"source":"/workspace/outputs/a","destination":"/workspace/b"}`, true},
		{"move_file", `{"source":"/workspace/outputs/a","destination":"/workspace/outputs/b"}`, false},
		{"jira__create_issue", `{}`, true},
		{"slack__send_message", `{}`, true},
		{"github__update_pr", `{}`, true},
		{"drive__publishDoc", `{}`, true},
		{"cms__post", `{}`, true},
		{"fs__remove_dir", `{"path":"/workspace/outputs/x"}`, true},
		{"jira__get_issue", `{}`, false},
		{"confluence__list_pages", `{}`, false},
		{"read_file", `{"path":"/etc/passwd"}`, false},
		{"search_files", `{"pattern":"x"}`, false},
		{"view_file", `{"path":"/workspace/a.pdf"}`, false},
		{"update_plan", `{}`, false},
		{"ask_user", `{}`, false},
	}
	for _, tc := range cases {
		// Act
		got := gate.NeedsApproval(context.Background(), run, tc.tool, tc.args)

		// Assert
		if got != tc.want {
			t.Errorf("NeedsApproval(%s %s) = %v, want %v", tc.tool, tc.args, got, tc.want)
		}
	}
}

func TestGateNeverAsksInAutomaticMode(t *testing.T) {
	// Automatic (and unset) runs never wait for an approval.
	// Arrange
	gate := &approvals.Gate{}

	// Act
	automatic := gate.NeedsApproval(context.Background(), &store.Run{InteractionMode: store.InteractionAutomatic}, "execute_command", `{"command":"rm -rf /"}`)
	unset := gate.NeedsApproval(context.Background(), &store.Run{}, "jira__delete_issue", `{}`)

	// Assert
	if automatic || unset {
		t.Fatalf("automatic %v unset %v", automatic, unset)
	}
}

func TestGateHonoursToolKindsApprovedForTheRun(t *testing.T) {
	// A tool kind approved with scope "run" runs without asking again in that run chain; other risky kinds still ask.
	// Arrange
	gate := &approvals.Gate{}
	run := &store.Run{InteractionMode: store.InteractionInteractive, ApprovedTools: []string{approvals.Kind("execute_command")}}

	// Act
	command := gate.NeedsApproval(context.Background(), run, "execute_command", `{"command":"make"}`)
	write := gate.NeedsApproval(context.Background(), run, "write_file", `{"path":"/workspace/setup.sh","content":"x"}`)

	// Assert
	if command || !write {
		t.Fatalf("command %v write %v", command, write)
	}
}

func TestGateUsesTheConfiguredRiskyToolPatterns(t *testing.T) {
	// APPROVAL_RISKY_TOOL_PATTERNS replaces the default mutating verbs for MCP tools.
	// Arrange
	gate := &approvals.Gate{RiskyToolPattern: regexp.MustCompile("(?i)archive")}
	run := &store.Run{InteractionMode: store.InteractionInteractive}

	// Act
	archive := gate.NeedsApproval(context.Background(), run, "jira__archive_issue", `{}`)
	create := gate.NeedsApproval(context.Background(), run, "jira__create_issue", `{}`)

	// Assert
	if !archive || create {
		t.Fatalf("archive %v create %v", archive, create)
	}
}

func TestGateAsksWhenAPathKeyRepeatsOrDiffersInCase(t *testing.T) {
	// The tools decode arguments case-insensitively (last key wins): a repeated or case-variant path key always asks, for every gated tool (review round 1 P2 #1).
	// Arrange
	gate := &approvals.Gate{}
	run := &store.Run{InteractionMode: store.InteractionInteractive}
	cases := []struct{ tool, args string }{
		{"write_file", `{"path":"/workspace/outputs/ok.txt","Path":"/workspace/src/main.py","content":"x"}`},
		{"write_file", `{"Path":"/workspace/outputs/ok.txt","path":"/workspace/outputs/b.txt","content":"x"}`},
		{"write_file", `{"PATH":"/workspace/outputs/ok.txt","content":"x"}`},
		{"write_file", `{"path":"/workspace/outputs/a","path":"/workspace/src/main.py","content":"x"}`},
		{"delete_files", `{"path\u017f":["/workspace/src/main.py"],"paths":["/workspace/outputs/a"]}`},
		{"edit_file", `{"path":"/workspace/.work/a.py","pAth":"/workspace/src/main.py","oldText":"a","newText":"b"}`},
		{"delete_file", `{"path":"/workspace/outputs/a","PATH":"/workspace/data.csv"}`},
		{"move_file", `{"source":"/workspace/outputs/a","destination":"/workspace/outputs/b","Destination":"/workspace/src/b"}`},
		{"rename_file", `{"from":"/workspace/outputs/a","to":"/workspace/outputs/b","TO":"/etc/b"}`},
		{"fs__move_file", `{"source":"/workspace/outputs/a","destination":"/workspace/outputs/b"}`},
		{"fs__write_file", `{"path":"/workspace/outputs/a","Path":"/workspace/src/a"}`},
	}
	for _, tc := range cases {
		// Act
		got := gate.NeedsApproval(context.Background(), run, tc.tool, tc.args)

		// Assert
		if !got {
			t.Errorf("NeedsApproval(%s %s) = false, want true", tc.tool, tc.args)
		}
	}
}

// liveModes is the gate's store: the current interaction mode of each run (err: the read fails).
type liveModes struct {
	modes map[string]string
	err   error
}

func (s liveModes) CreateApproval(context.Context, *store.Approval) error { return nil }

func (s liveModes) RunInteractionMode(_ context.Context, runID string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.modes[runID], nil
}

func TestGateFollowsTheModeSwitchedDuringTheRun(t *testing.T) {
	// The gate reads the run's current mode: switching to interactive mid-run gates the next risky call, switching to automatic stops gating.
	// Arrange
	gate := &approvals.Gate{Store: liveModes{modes: map[string]string{"to-interactive": store.InteractionInteractive, "to-automatic": store.InteractionAutomatic, "to-unset": ""}}}
	startedAutomatic := &store.Run{ID: "to-interactive", InteractionMode: store.InteractionAutomatic}
	startedInteractive := &store.Run{ID: "to-automatic", InteractionMode: store.InteractionInteractive}
	startedInteractiveUnset := &store.Run{ID: "to-unset", InteractionMode: store.InteractionInteractive}

	// Act
	nowInteractive := gate.NeedsApproval(context.Background(), startedAutomatic, "execute_command", `{"command":"make"}`)
	nowAutomatic := gate.NeedsApproval(context.Background(), startedInteractive, "execute_command", `{"command":"make"}`)
	nowUnset := gate.NeedsApproval(context.Background(), startedInteractiveUnset, "execute_command", `{"command":"make"}`)

	// Assert
	if !nowInteractive || nowAutomatic || nowUnset {
		t.Fatalf("switched to interactive %v, to automatic %v, to unset %v", nowInteractive, nowAutomatic, nowUnset)
	}
	if startedAutomatic.InteractionMode != store.InteractionAutomatic || startedInteractive.InteractionMode != store.InteractionInteractive {
		t.Fatal("NeedsApproval changed the run")
	}
}

func TestGateKeepsTheRunsApprovedToolKindsAfterASwitchToInteractive(t *testing.T) {
	// A tool kind approved for the run is still not asked again when the live mode is interactive.
	// Arrange
	gate := &approvals.Gate{Store: liveModes{modes: map[string]string{"r": store.InteractionInteractive}}}
	run := &store.Run{ID: "r", InteractionMode: store.InteractionAutomatic, ApprovedTools: []string{approvals.Kind("execute_command")}}

	// Act
	command := gate.NeedsApproval(context.Background(), run, "execute_command", `{"command":"make"}`)
	write := gate.NeedsApproval(context.Background(), run, "write_file", `{"path":"/workspace/setup.sh","content":"x"}`)

	// Assert
	if command || !write {
		t.Fatalf("command %v write %v", command, write)
	}
}

func TestGateFailsClosedWhenTheLiveModeCannotBeRead(t *testing.T) {
	// When the current mode cannot be read the gate asks, even for a run that started automatic (it may have been switched to interactive).
	// Arrange
	gate := &approvals.Gate{Store: liveModes{err: errors.New("pool exhausted")}}

	// Act
	startedAutomatic := gate.NeedsApproval(context.Background(), &store.Run{ID: "a", InteractionMode: store.InteractionAutomatic}, "execute_command", `{"command":"make"}`)
	startedInteractive := gate.NeedsApproval(context.Background(), &store.Run{ID: "b", InteractionMode: store.InteractionInteractive}, "execute_command", `{"command":"make"}`)
	harmless := gate.NeedsApproval(context.Background(), &store.Run{ID: "c", InteractionMode: store.InteractionAutomatic}, "read_file", `{"path":"/etc/hosts"}`)

	// Assert
	if !startedAutomatic || !startedInteractive || harmless {
		t.Fatalf("started automatic %v, started interactive %v, read_file %v", startedAutomatic, startedInteractive, harmless)
	}
}

func TestGateKeepsTheStartModeWhenTheRunIsStopped(t *testing.T) {
	// A read that fails because the run was stopped (its context is cancelled) uses the start mode, so no stray approval card is recorded after Stop.
	// Arrange
	gate := &approvals.Gate{Store: liveModes{err: context.Canceled}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	startedAutomatic := gate.NeedsApproval(ctx, &store.Run{ID: "a", InteractionMode: store.InteractionAutomatic}, "execute_command", `{"command":"make"}`)
	startedInteractive := gate.NeedsApproval(ctx, &store.Run{ID: "b", InteractionMode: store.InteractionInteractive}, "execute_command", `{"command":"make"}`)

	// Assert
	if startedAutomatic || !startedInteractive {
		t.Fatalf("started automatic %v, started interactive %v", startedAutomatic, startedInteractive)
	}
}

func TestGateJudgesRiskWhateverTheLiveMode(t *testing.T) {
	// Risky applies the interactive rules even when the run is automatic now (calls behind a pending approval keep waiting); approved kinds and work-area writes are not risky.
	// Arrange
	gate := &approvals.Gate{Store: liveModes{modes: map[string]string{"r": store.InteractionAutomatic}}}
	run := &store.Run{ID: "r", InteractionMode: store.InteractionInteractive, ApprovedTools: []string{approvals.Kind("edit_file")}}

	// Act
	command := gate.Risky(context.Background(), run, "execute_command", `{"command":"make"}`)
	outside := gate.Risky(context.Background(), run, "write_file", `{"path":"/workspace/notes.md","content":"x"}`)
	workArea := gate.Risky(context.Background(), run, "write_file", `{"path":"/workspace/outputs/a.md","content":"x"}`)
	approved := gate.Risky(context.Background(), run, "edit_file", `{"path":"/workspace/a.py"}`)
	needs := gate.NeedsApproval(context.Background(), run, "execute_command", `{"command":"make"}`)

	// Assert
	if !command || !outside || workArea || approved || needs {
		t.Fatalf("command %v outside %v work area %v approved %v needs approval %v", command, outside, workArea, approved, needs)
	}
}
