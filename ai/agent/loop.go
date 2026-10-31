package agent

import (
	"context"
	"errors"

	"github.com/GhaziBenDahmane/harness/ai"
)

// ErrStepLimit is returned by Loop when the step budget ended without a policy finishing the run.
var ErrStepLimit = errors.New("agent: step limit reached")

// Step is one iteration of the loop, shared by the loop and its policy.
type Step struct {
	// N is the step number, from 1. Last is set on the final step the budget allows.
	N    int
	Last bool
	// Messages and Options are the request of the model call, as returned by Policy.Request.
	Messages []*ai.Message
	Options  []ai.Option
}

// Action says what the loop does after a model answer.
type Action int

const (
	// Finish ends the loop: the run is complete.
	Finish Action = iota
	// Continue starts another step. The policy has already changed whatever the next request is built from.
	Continue
	// RunTools runs the answer's tool calls through Policy.Tools, then continues (or finishes, when Tools says stop).
	RunTools
)

// Policy is everything that varies between agents. The loop owns the shape of a run (steps, the budget, the order
// of the phases, a retry after a context overflow, stopping on cancellation); a policy owns what a step means: where
// the conversation lives, how a request is built, what is recorded, which answers end the run and how tools run.
//
// The phases of a step, in order:
//
//	BeginStep -> Request -> Call [-> Recover -> Request -> Call] -> Replied -> Next [-> Tools] -> EndStep
//
// EndStep runs for every step whose BeginStep ran, also when a later phase failed.
type Policy interface {
	// BeginStep prepares the step: steering, compaction, notes. An error ends the run.
	BeginStep(ctx context.Context, s *Step) error
	// Request builds the model call for the step from the policy's conversation.
	Request(ctx context.Context, s *Step) ([]*ai.Message, []ai.Option)
	// Call makes the model call of s (s.Messages, s.Options) and returns the assembled answer. A cancelled call
	// returns the part it has with ctx.Err(); a call that failed returns a nil message.
	Call(ctx context.Context, s *Step) (*ai.Message, error)
	// Recover is asked once per step when Call failed with no answer and ctx is alive: it may change the conversation
	// so that the same step can be tried again (compact after a context overflow). retry reports whether it did.
	Recover(ctx context.Context, s *Step, err error) (retry bool, rerr error)
	// Replied records the outcome of Call: the usage, the answer in the conversation, persistence. reply is nil when
	// the call produced nothing; callErr is Call's error. A non-nil result ends the run (and wins over callErr).
	Replied(ctx context.Context, s *Step, reply *ai.Message, callErr error) error
	// Next decides what follows an answer. It is only asked when the call succeeded.
	Next(ctx context.Context, s *Step, reply *ai.Message) (Action, error)
	// Tools runs the tool calls of the answer after Next returned RunTools. stop ends the run once they ran (a tool
	// asked for the user).
	Tools(ctx context.Context, s *Step, calls []ai.ToolCall) (stop bool, err error)
	// EndStep closes the step: end events, flushing. It must not fail the run.
	EndStep(ctx context.Context, s *Step)
}

// Base implements the phases a policy often leaves alone. Embed it and write the rest.
type Base struct{}

// BeginStep does nothing.
func (Base) BeginStep(context.Context, *Step) error { return nil }

// Recover never retries.
func (Base) Recover(context.Context, *Step, error) (bool, error) { return false, nil }

// Replied records nothing.
func (Base) Replied(context.Context, *Step, *ai.Message, error) error { return nil }

// Next finishes on an answer without tool calls and on the last step (whatever it asked for), else runs the tools.
func (Base) Next(_ context.Context, s *Step, reply *ai.Message) (Action, error) {
	if len(reply.ToolCalls) == 0 || s.Last {
		return Finish, nil
	}
	return RunTools, nil
}

// EndStep does nothing.
func (Base) EndStep(context.Context, *Step) {}

// Loop runs the policy for at most maxSteps steps (at least one) and returns the number of steps it began. It
// returns nil when a step finished the run, ErrStepLimit when the budget ended first, ctx.Err() when the context
// ended between steps, or the error a phase returned.
func Loop(ctx context.Context, maxSteps int, p Policy) (int, error) {
	maxSteps = max(maxSteps, 1)
	for n := 1; n <= maxSteps; n++ {
		if err := ctx.Err(); err != nil {
			return n - 1, err
		}
		done, err := runStep(ctx, p, &Step{N: n, Last: n == maxSteps})
		if err != nil {
			return n, err
		}
		if done {
			return n, nil
		}
	}
	return maxSteps, ErrStepLimit
}

func runStep(ctx context.Context, p Policy, s *Step) (done bool, err error) {
	berr := p.BeginStep(ctx, s)
	defer p.EndStep(ctx, s)
	if berr != nil {
		return false, berr
	}
	s.Messages, s.Options = p.Request(ctx, s)
	reply, callErr := p.Call(ctx, s)
	if reply == nil && callErr != nil && ctx.Err() == nil {
		retry, rerr := p.Recover(ctx, s, callErr)
		if rerr != nil {
			return false, rerr
		}
		if retry {
			s.Messages, s.Options = p.Request(ctx, s)
			reply, callErr = p.Call(ctx, s)
		}
	}
	if rerr := p.Replied(ctx, s, reply, callErr); rerr != nil {
		return false, rerr
	}
	if callErr != nil {
		return false, callErr
	}
	action, err := p.Next(ctx, s, reply)
	if err != nil {
		return false, err
	}
	switch action {
	case Finish:
		return true, nil
	case RunTools:
		return p.Tools(ctx, s, reply.ToolCalls)
	}
	return false, nil
}
