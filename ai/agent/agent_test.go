package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/agent"
)

// script answers each call with the next reply and records what it was asked.
type script struct {
	replies []*ai.Message
	calls   [][]*ai.Message
	choices []*ai.ToolChoice
	err     error
}

func (s *script) Generate(_ context.Context, msgs []*ai.Message, opts ...ai.Option) (*ai.Message, error) {
	s.calls = append(s.calls, append([]*ai.Message(nil), msgs...))
	s.choices = append(s.choices, ai.Apply(opts...).ToolChoice)
	if s.err != nil {
		return nil, s.err
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}
func (s *script) Stream(context.Context, []*ai.Message, ...ai.Option) (*ai.StreamReader[*ai.Message], error) {
	return nil, errors.New("not used")
}
func (s *script) WithTools([]*ai.ToolInfo) (ai.Model, error) { return s, nil }

func call(id, name, args string) ai.ToolCall {
	return ai.ToolCall{ID: id, Type: "function", Function: ai.FunctionCall{Name: name, Arguments: args}}
}

type echo struct {
	in struct{}
	ro bool
	// active tracks how many calls run at once.
	active, peak *atomic.Int32
}

func (e echo) Info(context.Context) (*ai.ToolInfo, error) { return &ai.ToolInfo{Name: "echo"}, nil }
func (e echo) ReadOnly() bool                             { return e.ro }
func (e echo) InvokableRun(_ context.Context, args string) (string, error) {
	if e.active != nil {
		n := e.active.Add(1)
		for p := e.peak.Load(); n > p && !e.peak.CompareAndSwap(p, n); p = e.peak.Load() {
		}
		time.Sleep(30 * time.Millisecond)
		e.active.Add(-1)
	}
	if strings.Contains(args, "boom") {
		return "", errors.New("exploded")
	}
	return "got " + args, nil
}

func TestLoopRunsToolsAndStopsOnAPlainAnswer(t *testing.T) {
	m := &script{replies: []*ai.Message{
		ai.AssistantMessage("", []ai.ToolCall{call("1", "echo", `{"a":1}`), call("2", "nope", ``)}),
		ai.AssistantMessage("done", nil),
	}}
	var seen []string
	res, err := agent.Run(context.Background(), agent.Config{
		Model: m, Tools: []ai.InvokableTool{echo{}},
		OnTool: func(c ai.ToolCall, r string) { seen = append(seen, c.ID+":"+r) },
	}, []*ai.Message{ai.UserMessage("go")})

	if err != nil || res.Reply.Content != "done" || res.Steps != 2 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if got := strings.Join(seen, "|"); got != `1:got {"a":1}|2:error: unknown tool "nope"` {
		t.Fatalf("tool results %q", got)
	}
	// user, assistant(calls), tool, tool, assistant
	if len(res.Messages) != 5 || res.Messages[2].ToolCallID != "1" || res.Messages[2].ToolName != "echo" || res.Messages[3].ToolCallID != "2" {
		t.Fatalf("transcript %+v", res.Messages)
	}
}

func TestLastStepForbidsToolsAndGetsTheNudge(t *testing.T) {
	m := &script{replies: []*ai.Message{
		ai.AssistantMessage("", []ai.ToolCall{call("1", "echo", `{}`)}),
		ai.AssistantMessage("report", []ai.ToolCall{call("2", "echo", `{}`)}), // ignored: the budget is spent
	}}
	res, err := agent.Run(context.Background(), agent.Config{
		Model: m, Tools: []ai.InvokableTool{echo{}}, MaxSteps: 2, LimitNudge: "write the report now",
	}, []*ai.Message{ai.UserMessage("go")})

	if err != nil || res.Reply.Content != "report" || res.Steps != 2 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if m.choices[0] != nil || m.choices[1] == nil || *m.choices[1] != ai.ToolChoiceForbidden {
		t.Fatalf("tool choices %v", m.choices)
	}
	lastCall := m.calls[1]
	if got := lastCall[len(lastCall)-1]; got.Role != ai.RoleUser || got.Content != "write the report now" {
		t.Fatalf("the last request ends with %+v", got)
	}
}

func TestParallelRunsOnlyReadOnlyCallsTogether(t *testing.T) {
	run := func(readOnly, parallel bool) int32 {
		var active, peak atomic.Int32
		m := &script{replies: []*ai.Message{
			ai.AssistantMessage("", []ai.ToolCall{call("1", "echo", `{}`), call("2", "echo", `{}`), call("3", "echo", `{}`)}),
			ai.AssistantMessage("ok", nil),
		}}
		_, err := agent.Run(context.Background(), agent.Config{
			Model: m, Tools: []ai.InvokableTool{echo{ro: readOnly, active: &active, peak: &peak}}, Parallel: parallel,
		}, []*ai.Message{ai.UserMessage("go")})
		if err != nil {
			t.Fatal(err)
		}
		return peak.Load()
	}

	if got := run(true, true); got < 2 {
		t.Errorf("read-only calls with Parallel peaked at %d concurrent, want them to overlap", got)
	}
	if got := run(false, true); got != 1 {
		t.Errorf("mutating calls peaked at %d concurrent, want 1", got)
	}
	if got := run(true, false); got != 1 {
		t.Errorf("read-only calls without Parallel peaked at %d concurrent, want 1", got)
	}
}

func TestSteerMessagesJoinTheConversationBeforeTheCall(t *testing.T) {
	m := &script{replies: []*ai.Message{ai.AssistantMessage("ok", nil)}}
	_, err := agent.Run(context.Background(), agent.Config{
		Model: m,
		Steer: func(_ context.Context, step int) []*ai.Message {
			return []*ai.Message{ai.UserMessage("also: be brief")}
		},
	}, []*ai.Message{ai.UserMessage("go")})

	if err != nil || len(m.calls[0]) != 2 || m.calls[0][1].Content != "also: be brief" {
		t.Fatalf("first request %+v err %v", m.calls[0], err)
	}
}

func TestAFailedCallReturnsTheTranscriptAndTheError(t *testing.T) {
	boom := errors.New("provider down")
	res, err := agent.Run(context.Background(), agent.Config{Model: &script{err: boom}}, []*ai.Message{ai.UserMessage("go")})

	if !errors.Is(err, boom) || len(res.Messages) != 1 || res.Reply != nil {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestACancelledContextStopsBeforeTheNextCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &script{replies: []*ai.Message{ai.AssistantMessage("", []ai.ToolCall{call("1", "echo", `{}`)})}}
	_, err := agent.Run(ctx, agent.Config{
		Model: m, Tools: []ai.InvokableTool{echo{}},
		OnTool: func(ai.ToolCall, string) { cancel() },
	}, []*ai.Message{ai.UserMessage("go")})

	if !errors.Is(err, context.Canceled) || len(m.calls) != 1 {
		t.Fatalf("err %v after %d calls", err, len(m.calls))
	}
}
