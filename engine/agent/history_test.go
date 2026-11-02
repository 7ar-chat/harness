package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/store"
)

func assistantRow(id, content string, pos int64, calls ...store.ToolCall) *store.Message {
	return &store.Message{ID: id, Role: store.RoleAssistant, Content: content, Position: pos, ToolCalls: calls}
}

func callOf(id, name string) store.ToolCall {
	return store.ToolCall{ID: id, Type: "function", Function: store.ToolCallFunction{Name: name, Arguments: "{}"}}
}

func TestBuildHistoryAnswersEveryCallExactlyOnce(t *testing.T) {
	// Arrange: c2 has no stored result (the run died), c3's result is stored under a later position, c1's id is reused by a later assistant message.
	msgs := []*store.Message{
		userRow("u", "go", time.Time{}),
		assistantRow("a1", "", 2, callOf("c1", "read_file"), callOf("c2", "write_file")),
		{ID: "t1", Role: store.RoleTool, ToolCallID: "c1", Content: "first result", Position: 3},
		assistantRow("a2", "", 4, callOf("c3", "list_files")),
		{ID: "t3", Role: store.RoleTool, ToolCallID: "c3", Content: "late result", Position: 9},
		{ID: "orphan", Role: store.RoleTool, ToolCallID: "nope", Content: "orphan", Position: 10},
		assistantRow("a3", "", 11, callOf("c1", "read_file")), // reused id: dropped
		assistantRow("a4", "empty", 12),
		assistantRow("a5", "", 13),
	}

	// Act
	h := BuildHistory(msgs)

	// Assert
	var shape []string
	for _, m := range h {
		switch m.Role {
		case ai.RoleAssistant:
			var ids []string
			for _, c := range m.ToolCalls {
				ids = append(ids, c.ID)
			}
			shape = append(shape, "assistant"+fmt.Sprint(ids))
		case ai.RoleTool:
			shape = append(shape, "tool:"+m.ToolCallID+"="+m.Content)
		default:
			shape = append(shape, string(m.Role))
		}
	}
	want := "[user assistant[c1 c2] tool:c1=first result tool:c2=" + interruptedResult + " assistant[c3] tool:c3=late result assistant[]]"
	if fmt.Sprint(shape) != want {
		t.Fatalf("shape:\n got %v\nwant %s", shape, want)
	}
}

func TestBuildHistoryCutsOldToolOutputsButNotTheLastTwoAssistantTurns(t *testing.T) {
	// Arrange
	long := strings.Repeat("x", 10000) + "TAIL"
	msgs := []*store.Message{userRow("u", "go", time.Time{})}
	for i := range 4 {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs, assistantRow("a"+id, "", int64(2+2*i), callOf(id, "read_file")),
			&store.Message{ID: "t" + id, Role: store.RoleTool, ToolCallID: id, Content: long, Position: int64(3 + 2*i)})
	}

	// Act
	h := BuildHistory(msgs)

	// Assert
	var lens []int
	for _, m := range h {
		if m.Role == ai.RoleTool {
			lens = append(lens, len(m.Content))
		}
	}
	if len(lens) != 4 || lens[0] > 2500 || lens[1] > 2500 || lens[2] != len(long) || lens[3] != len(long) {
		t.Fatalf("tool result sizes %v", lens)
	}
	if !strings.HasSuffix(h[2].Content, "TAIL") {
		t.Error("the end of a cut output (exit code, stderr) was lost")
	}
}

func TestBuildHistoryGivesReasoningToTheAssistantMessageOfItsRunOnlyWhenAsked(t *testing.T) {
	// Arrange
	msgs := []*store.Message{
		userRow("u", "go", time.Time{}),
		{ID: "r1", RunID: "run-1", Role: store.RoleReasoning, Content: "thinking ", Position: 2},
		{ID: "r2", RunID: "run-1", Role: store.RoleReasoning, Content: "more", Position: 3},
		{ID: "a1", RunID: "run-1", Role: store.RoleAssistant, Content: "answer", Position: 4},
		{ID: "a2", RunID: "run-1", Role: store.RoleAssistant, Content: "second answer", Position: 5},
	}

	// Act
	with := BuildHistoryWith(msgs, HistoryOptions{Reasoning: true})
	without := BuildHistory(msgs)

	// Assert
	if with[1].ReasoningContent != "thinking more" || with[2].ReasoningContent != "" {
		t.Errorf("with: %q / %q", with[1].ReasoningContent, with[2].ReasoningContent)
	}
	if without[1].ReasoningContent != "" {
		t.Errorf("reasoning passed back without being asked: %q", without[1].ReasoningContent)
	}
}

func TestBuildHistoryMarksStepsAndRequestsAndKeepsStorePositions(t *testing.T) {
	// Arrange
	steer := &store.Message{ID: "s", Role: store.RoleUser, Content: "also B", Position: 5, Metadata: map[string]any{"steer": true}}

	// Act
	h := BuildHistory([]*store.Message{userRow("u", "go", time.Time{}), steer})

	// Assert
	if h[1].Content != SteerPrefix+"also B" {
		t.Errorf("steer %q", h[1].Content)
	}
}

func TestAuthorTagNamesWhoSpoke(t *testing.T) {
	for name, tc := range map[string]struct{ login, kind, onBehalf, want string }{
		"user":                  {"alice", "", "", "[alice] "},
		"supervisor for a user": {"sup", store.AuthorSupervisor, "bob", "[supervisor, for bob] "},
		"supervisor":            {"sup", store.AuthorSupervisor, "", "[supervisor] "},
		"unknown":               {"", "", "", ""},
	} {
		if got := AuthorTag(tc.login, tc.kind, tc.onBehalf); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestTagsAppearWhenASupervisorJoinsASingleUserThread(t *testing.T) {
	// Arrange
	sup := &store.Message{ID: "s", Role: store.RoleUser, Content: "stop", Position: 2, AuthorKind: store.AuthorSupervisor, OnBehalfOf: "alice"}

	// Act
	h := BuildHistory([]*store.Message{userMsg(1, "hello", "alice"), sup})

	// Assert
	if h[0].Content != "[alice] hello" || h[1].Content != "[supervisor, for alice] stop" {
		t.Fatalf("%q / %q", h[0].Content, h[1].Content)
	}
}
