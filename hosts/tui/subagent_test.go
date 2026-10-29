package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

func subEv(id, phase string, extra map[string]any) agui.Event {
	v := map[string]any{"toolCallId": id, "phase": phase, "agent": "explore"}
	for k, x := range extra {
		v[k] = x
	}
	return agui.SubagentEvent(v)
}

func subTool(id, name, args, result string, isErr bool) agui.Event {
	return subEv(id, agui.SubagentTool, map[string]any{"tool": map[string]any{"name": name, "args": args, "result": result, "isError": isErr}})
}

func usage(in, out int) map[string]any { return map[string]any{"in": in, "out": out, "cached": 0} }

// exploreRun feeds the start of an explore call and seven tool calls of its sub-agent.
func exploreRun(t *testing.T, m *Model) {
	t.Helper()
	startRun(t, m, "where is Handle?")
	feed(t, m, toolEvents("c1", "explore", `{"task":"where is Handle defined and who calls it?"}`, "x", false)[:4]...)
	feed(t, m, subEv("c1", agui.SubagentStart, map[string]any{"task": "where is Handle defined and who calls it?"}))
	for i := 1; i <= 7; i++ {
		feed(t, m, subEv("c1", agui.SubagentStep, map[string]any{"step": i, "usage": usage(400*i, 30*i)}))
		switch {
		case i == 3:
			feed(t, m, subTool("c1", "read_file", "pkg/a.go:10-40", "package pkg", false))
		case i == 5:
			feed(t, m, subTool("c1", "read_file", "missing.go", "error: no such file", true))
		default:
			feed(t, m, subTool("c1", "search_files", fmt.Sprintf("%q", fmt.Sprintf("foo%d", i)), "3 matches", false))
		}
	}
}

func TestSubagentEventDecodesIntoItsFields(t *testing.T) {
	// Arrange
	raw, _ := json.Marshal(subEv("c9", agui.SubagentTool, map[string]any{"step": 4, "model": "cheap", "usage": usage(100, 20),
		"tool": map[string]any{"name": "list_files", "args": "pkg", "result": "a.go", "isError": true}}))

	// Act
	e := Decode(client.Event{Type: agui.Custom, Data: raw}).SubagentValue()

	// Assert
	want := SubEvent{ToolCallID: "c9", Phase: "tool", Agent: "explore", Model: "cheap", Step: 4, Usage: Usage{In: 100, Out: 20},
		Tool: SubTool{Name: "list_files", Args: "pkg", Result: "a.go", Err: true}}
	if e != want {
		t.Fatalf("got %+v want %+v", e, want)
	}
}

func TestSubagentEventWithMissingFieldsDecodesToZeroValues(t *testing.T) {
	// Arrange
	raw := json.RawMessage(`{"type":"CUSTOM","name":"work.subagent","value":{"phase":7}}`)

	// Act
	e := Decode(client.Event{Data: raw}).SubagentValue()

	// Assert
	if e != (SubEvent{}) {
		t.Fatalf("got %+v", e)
	}
}

func TestExploreBlockShowsTheLastToolCallsOfTheRunningSubagent(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})

	// Act
	exploreRun(t, m)

	// Assert
	golden(t, "subagent_running", m.Transcript())
}

func TestExploreBlockShowsAllLinesAndTheFullReportWhenExpanded(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	exploreRun(t, m)
	feed(t, m, subEv("c1", agui.SubagentEnd, map[string]any{"reportChars": 80, "usage": usage(2800, 210)}),
		agui.New(agui.ToolCallResult, map[string]any{"toolCallId": "c1", "role": "tool", "content": "Handle is in pkg/a.go:3.\n\nCallers:\n- cmd/main.go:12\n- cmd/other.go:40\n- pkg/b.go:7"}))

	// Act
	collapsed := m.Transcript()
	m.expanded = true

	// Assert
	golden(t, "subagent_done", collapsed)
	golden(t, "subagent_expanded", m.Transcript())
}

func TestSubagentEventForAnUnknownToolCallIsIgnored(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	startRun(t, m, "hi")

	// Act
	feed(t, m, subTool("nope", "read_file", "a.go", "ok", false), agui.CustomEvent("work.from_the_future", map[string]any{"x": 1}))

	// Assert
	if got := m.Transcript(); strings.Contains(got, "read_file") || len(m.blocks) != 1 {
		t.Fatalf("transcript %q", got)
	}
}

func TestSubagentLinesAreCappedPerRun(t *testing.T) {
	// Arrange
	s := &Subagent{}

	// Act
	for i := 0; i < subMaxLines+10; i++ {
		s.Apply(SubEvent{Phase: agui.SubagentTool, Tool: SubTool{Name: "list_files"}})
	}

	// Assert
	if len(s.Lines) != subMaxLines || s.Dropped != 10 {
		t.Fatalf("%d lines, %d dropped", len(s.Lines), s.Dropped)
	}
}

func TestChildrenOpensThePickerAndShowsTheSelectedTranscriptUntilEsc(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	exploreRun(t, m)
	feed(t, m, subEv("c1", agui.SubagentEnd, map[string]any{"reportChars": 26}),
		agui.New(agui.ToolCallResult, map[string]any{"toolCallId": "c1", "role": "tool", "content": "Handle is in pkg/a.go:3."}))

	// Act
	typeText(m, "/children")
	press(m, tea.KeyEnter)
	picker := m.View()
	press(m, tea.KeyEnter)
	pane := m.View()
	press(m, tea.KeyEsc)

	// Assert
	golden(t, "children_picker", picker[strings.Index(picker, "Sub-agents"):strings.Index(picker, "\n\n")])
	golden(t, "children_pane", pane)
	if m.sub.open || strings.Contains(m.View(), "Sub-agent transcript") {
		t.Fatal("esc did not return to the conversation")
	}
}

func TestChildrenOfASavedConversationShowsOnlyTheReport(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	tc := client.ToolCall{ID: "c1"}
	tc.Function.Name, tc.Function.Arguments = "explore", `{"task":"find Handle"}`
	m.loadThread(&client.Thread{Messages: []client.Message{
		{Role: client.RoleAssistant, ToolCalls: []client.ToolCall{tc}},
		{Role: client.RoleTool, ToolCallID: "c1", Content: "Handle is in pkg/a.go:3."},
	}})

	// Act
	typeText(m, "/children")
	press(m, tea.KeyEnter)
	press(m, tea.KeyEnter)

	// Assert
	golden(t, "children_report_only", m.View())
}

func TestChildrenWithoutSubagentsSaysSo(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})

	// Act
	typeText(m, "/children")
	press(m, tea.KeyEnter)

	// Assert
	if got := m.Transcript(); !strings.Contains(got, "no sub-agent ran") || m.picker != nil {
		t.Fatalf("transcript %q", got)
	}
}

// replayBackend streams fixed events.
type replayBackend struct {
	fakeBackend
	events []agui.Event
}

func (r *replayBackend) Events(context.Context, string) <-chan client.Event {
	ch := make(chan client.Event, len(r.events))
	for i, e := range r.events {
		b, _ := json.Marshal(e)
		ch <- client.Event{Type: e.Type(), Seq: int64(i + 1), Data: b}
	}
	close(ch)
	return ch
}

func TestPrintModeReportsSubagentToolsOnStderrOnly(t *testing.T) {
	// Arrange
	be := &replayBackend{events: append(toolEvents("c1", "explore", `{"task":"x"}`, "report", false)[:4],
		subEv("c1", agui.SubagentStart, map[string]any{"task": "x"}),
		subTool("c1", "search_files", `"foo"`, "3 matches", false),
		subTool("c1", "read_file", "gone.go", "error: missing", true),
		subEv("c1", agui.SubagentEnd, nil),
		agui.RunFinishedEvent("t", "r", "success", nil))}
	var stdout, stderr bytes.Buffer

	// Act
	code := RunPrint(context.Background(), be, "go", PrintOptions{Stdout: &stdout, Stderr: &stderr})

	// Assert
	if code != ExitOK || stdout.Len() != 0 {
		t.Fatalf("code %d stdout %q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "  ⎇ search_files \"foo\"\n") || !strings.Contains(stderr.String(), "  ⎇ read_file gone.go (error)\n") {
		t.Fatalf("stderr %q", stderr.String())
	}
}
