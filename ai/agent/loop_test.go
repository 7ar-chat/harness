package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/agent"
)

// trace is a Policy that logs each phase and answers from a script.
type trace struct {
	agent.Base
	log     []string
	replies []*ai.Message
	callErr []error // per Call: the error to return with a nil message
	recover bool
	next    func(s *agent.Step, reply *ai.Message) agent.Action
	calls   int
}

func (p *trace) add(s string) { p.log = append(p.log, s) }

func (p *trace) BeginStep(_ context.Context, s *agent.Step) error {
	p.add("begin" + string(rune('0'+s.N)))
	return nil
}
func (p *trace) Request(_ context.Context, s *agent.Step) ([]*ai.Message, []ai.Option) {
	p.add("request")
	return []*ai.Message{ai.UserMessage("q")}, nil
}
func (p *trace) Call(_ context.Context, s *agent.Step) (*ai.Message, error) {
	i := p.calls
	p.calls++
	p.add("call")
	if i < len(p.callErr) && p.callErr[i] != nil {
		return nil, p.callErr[i]
	}
	return p.replies[min(i, len(p.replies)-1)], nil
}
func (p *trace) Recover(_ context.Context, _ *agent.Step, _ error) (bool, error) {
	p.add("recover")
	return p.recover, nil
}
func (p *trace) Replied(_ context.Context, _ *agent.Step, reply *ai.Message, err error) error {
	p.add("replied")
	return nil
}
func (p *trace) Next(ctx context.Context, s *agent.Step, reply *ai.Message) (agent.Action, error) {
	p.add("next")
	if p.next != nil {
		return p.next(s, reply), nil
	}
	return p.Base.Next(ctx, s, reply)
}
func (p *trace) Tools(_ context.Context, _ *agent.Step, calls []ai.ToolCall) (bool, error) {
	p.add("tools")
	return false, nil
}
func (p *trace) EndStep(_ context.Context, _ *agent.Step) { p.add("end") }

func TestLoopRunsThePhasesOfAStepInOrderAndStopsOnAPlainAnswer(t *testing.T) {
	p := &trace{replies: []*ai.Message{
		ai.AssistantMessage("", []ai.ToolCall{call("1", "x", `{}`)}),
		ai.AssistantMessage("done", nil),
	}}

	n, err := agent.Loop(context.Background(), 5, p)

	want := "begin1 request call replied next tools end begin2 request call replied next end"
	if err != nil || n != 2 || strings.Join(p.log, " ") != want {
		t.Fatalf("n %d err %v\n got %s\nwant %s", n, err, strings.Join(p.log, " "), want)
	}
}

func TestLoopReportsTheStepLimitAndMarksTheLastStep(t *testing.T) {
	var last []bool
	p := &trace{replies: []*ai.Message{ai.AssistantMessage("", []ai.ToolCall{call("1", "x", `{}`)})}}
	p.next = func(s *agent.Step, _ *ai.Message) agent.Action { last = append(last, s.Last); return agent.Continue }

	n, err := agent.Loop(context.Background(), 3, p)

	if !errors.Is(err, agent.ErrStepLimit) || n != 3 || len(last) != 3 || last[0] || last[1] || !last[2] {
		t.Fatalf("n %d err %v last %v", n, err, last)
	}
}

func TestLoopAsksToRecoverOnceAndRebuildsTheRequest(t *testing.T) {
	boom := errors.New("context too long")
	p := &trace{recover: true, callErr: []error{boom}, replies: []*ai.Message{ai.AssistantMessage("ok", nil)}}

	_, err := agent.Loop(context.Background(), 2, p)

	want := "begin1 request call recover request call replied next end"
	if err != nil || strings.Join(p.log, " ") != want {
		t.Fatalf("err %v\n got %s\nwant %s", err, strings.Join(p.log, " "), want)
	}
}

func TestLoopDoesNotRecoverTwiceOrWhenTheCallProducedAnAnswer(t *testing.T) {
	boom := errors.New("still too long")
	p := &trace{recover: true, callErr: []error{boom, boom}, replies: []*ai.Message{ai.AssistantMessage("x", nil)}}

	_, err := agent.Loop(context.Background(), 2, p)

	if !errors.Is(err, boom) || strings.Count(strings.Join(p.log, " "), "recover") != 1 {
		t.Fatalf("err %v log %s, want one recovery and then the error", err, strings.Join(p.log, " "))
	}
}

func TestLoopDoesNotRecoverFromACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &trace{recover: true, callErr: []error{context.Canceled}}
	cancel()

	_, err := agent.Loop(ctx, 2, p)

	if !errors.Is(err, context.Canceled) || len(p.log) != 0 {
		t.Fatalf("err %v log %v, want nothing to run under a cancelled context", err, p.log)
	}
}

// failAt makes one phase fail.
type failAt struct {
	trace
	phase string
	err   error
}

func (p *failAt) BeginStep(ctx context.Context, s *agent.Step) error {
	_ = p.trace.BeginStep(ctx, s)
	if p.phase == "begin" {
		return p.err
	}
	return nil
}
func (p *failAt) Replied(ctx context.Context, s *agent.Step, r *ai.Message, e error) error {
	_ = p.trace.Replied(ctx, s, r, e)
	if p.phase == "replied" {
		return p.err
	}
	return nil
}

func TestEndStepRunsEvenWhenAPhaseFailsAndTheErrorIsReturned(t *testing.T) {
	for _, phase := range []string{"begin", "replied"} {
		boom := errors.New(phase + " failed")
		p := &failAt{trace: trace{replies: []*ai.Message{ai.AssistantMessage("x", nil)}}, phase: phase, err: boom}

		_, err := agent.Loop(context.Background(), 2, p)

		if !errors.Is(err, boom) || p.log[len(p.log)-1] != "end" {
			t.Errorf("%s: err %v log %v", phase, err, p.log)
		}
	}
}

func TestACallErrorIsRecordedBeforeItEndsTheRun(t *testing.T) {
	// Replied sees the failed call (so a policy can store a partial answer) and the call's error still wins.
	boom := errors.New("provider down")
	p := &trace{callErr: []error{boom}, replies: []*ai.Message{ai.AssistantMessage("x", nil)}}

	_, err := agent.Loop(context.Background(), 2, p)

	if !errors.Is(err, boom) || !strings.Contains(strings.Join(p.log, " "), "recover replied end") {
		t.Fatalf("err %v log %s", err, strings.Join(p.log, " "))
	}
}

func TestAPolicyCanChooseToContinueWithoutTools(t *testing.T) {
	p := &trace{replies: []*ai.Message{ai.AssistantMessage("", nil), ai.AssistantMessage("real answer", nil)}}
	p.next = func(s *agent.Step, reply *ai.Message) agent.Action {
		if reply.Content == "" {
			return agent.Continue // an empty answer: ask again
		}
		return agent.Finish
	}

	n, err := agent.Loop(context.Background(), 4, p)

	if err != nil || n != 2 || strings.Contains(strings.Join(p.log, " "), "tools") {
		t.Fatalf("n %d err %v log %v", n, err, p.log)
	}
}
