// Package agent is the tool-calling loop: ask the model, run the tools it calls, feed the results back,
// until it answers without a tool call or the step budget ends. It knows nothing of persistence, event
// transports or approvals; a caller observes the loop through the Config hooks and keeps whatever it
// needs from the returned transcript.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/7ar-chat/harness/ai"
)

// DefaultMaxSteps bounds a loop whose Config.MaxSteps is not set.
const DefaultMaxSteps = 20

// Config describes one loop.
type Config struct {
	// Model answers each step; it must already offer the tools of the loop (ai.Model.WithTools).
	Model ai.Model
	// Tools are the tools the loop may run, by the name in their ToolInfo.
	Tools []ai.InvokableTool
	// MaxSteps is the number of model calls allowed (0: DefaultMaxSteps). The last one is made with
	// tool calls forbidden and, when LimitNudge is set, after that message was appended.
	MaxSteps int
	// LimitNudge is the user message added before the last step.
	LimitNudge string
	// Options are passed to every model call.
	Options []ai.Option
	// Parallel runs consecutive calls of ReadOnly tools at the same time; by default every call runs alone, in
	// call order. MaxParallel bounds how many run at once (0: all of them).
	Parallel    bool
	MaxParallel int

	// Caller, when set, makes each model call as a stream under its watchdog and retry policy instead of one
	// Generate call; chunks are passed to OnChunk as they arrive and merged into the reply with ai.ConcatMessages.
	// A retried attempt delivers its chunks again: Caller.OnRetry tells the observer to discard the failed one.
	Caller *Caller
	// OnChunk receives the chunks of a streamed call (Caller set).
	OnChunk func(step int, chunk *ai.Message)

	// OnStep is called before each model call with the step number (from 1).
	OnStep func(step int)
	// OnReply is called with every answer of the model, also one that fails the loop's next stage.
	OnReply func(step int, reply *ai.Message)
	// OnTool is called after each tool call with its result text.
	OnTool func(call ai.ToolCall, result string)
	// Invoke runs one tool call and returns the text the model gets. Nil: the default runs the tool of that
	// name and reports failures as "error: ..." text, an unknown tool by name.
	Invoke func(ctx context.Context, call ai.ToolCall) string
	// Steer returns messages to add to the conversation before a model call (nil: none).
	Steer func(ctx context.Context, step int) []*ai.Message
}

// Result is a finished loop.
type Result struct {
	// Messages is the whole conversation: the input, every answer and every tool result.
	Messages []*ai.Message
	// Reply is the last answer of the model (no tool calls unless the step budget ended the loop).
	Reply *ai.Message
	// Steps is the number of model calls made.
	Steps int
}

// ErrNoModel is returned when Config.Model is nil.
var ErrNoModel = errors.New("agent: no model")

// ReadOnly marks a tool that has no side effects, so several calls of one step may run at the same time (Config.Parallel).
type ReadOnly interface{ ReadOnly() bool }

// Run executes the loop over the conversation msgs (not modified): the Loop with the policy Config describes. A
// failed model call returns the conversation so far with the error; a cancelled context returns ctx.Err().
func Run(ctx context.Context, cfg Config, msgs []*ai.Message) (Result, error) {
	if cfg.Model == nil {
		return Result{}, ErrNoModel
	}
	byName, err := index(ctx, cfg.Tools)
	if err != nil {
		return Result{}, err
	}
	invoke := cfg.Invoke
	if invoke == nil {
		invoke = func(ctx context.Context, call ai.ToolCall) string { return defaultInvoke(ctx, byName, call) }
	}
	steps := cfg.MaxSteps
	if steps <= 0 {
		steps = DefaultMaxSteps
	}
	p := &configPolicy{cfg: &cfg, byName: byName, invoke: invoke, conv: append([]*ai.Message(nil), msgs...)}
	n, err := Loop(ctx, steps, p)
	return Result{Messages: p.conv, Reply: p.reply, Steps: n}, err
}

// configPolicy is the Policy of Run: the conversation is a slice, the hooks are Config's.
type configPolicy struct {
	cfg    *Config
	byName map[string]ai.InvokableTool
	invoke func(context.Context, ai.ToolCall) string
	conv   []*ai.Message
	reply  *ai.Message
}

func (p *configPolicy) BeginStep(ctx context.Context, s *Step) error {
	if p.cfg.Steer != nil {
		p.conv = append(p.conv, p.cfg.Steer(ctx, s.N)...)
	}
	if s.Last && p.cfg.LimitNudge != "" {
		p.conv = append(p.conv, ai.UserMessage(p.cfg.LimitNudge))
	}
	if p.cfg.OnStep != nil {
		p.cfg.OnStep(s.N)
	}
	return nil
}

func (p *configPolicy) Request(_ context.Context, s *Step) ([]*ai.Message, []ai.Option) {
	opts := append([]ai.Option(nil), p.cfg.Options...)
	if s.Last {
		opts = append(opts, ai.WithToolChoice(ai.ToolChoiceForbidden))
	}
	return p.conv, opts
}

func (p *configPolicy) Call(ctx context.Context, s *Step) (*ai.Message, error) {
	return p.cfg.reply(ctx, s.N, s.Messages, s.Options)
}

func (p *configPolicy) Recover(context.Context, *Step, error) (bool, error) { return false, nil }

func (p *configPolicy) Replied(_ context.Context, s *Step, reply *ai.Message, _ error) error {
	if reply != nil && p.cfg.OnReply != nil {
		p.cfg.OnReply(s.N, reply)
	}
	return nil
}

func (p *configPolicy) Next(_ context.Context, s *Step, reply *ai.Message) (Action, error) {
	p.reply = reply
	p.conv = append(p.conv, reply)
	if len(reply.ToolCalls) == 0 || s.Last {
		return Finish, nil
	}
	return RunTools, nil
}

func (p *configPolicy) Tools(ctx context.Context, _ *Step, calls []ai.ToolCall) (bool, error) {
	return false, runToolCalls(ctx, p.cfg, p.byName, p.invoke, calls, &p.conv)
}

func (p *configPolicy) EndStep(context.Context, *Step) {}

// reply makes the model call of a step: one Generate, or a stream through Config.Caller.
func (cfg *Config) reply(ctx context.Context, step int, conv []*ai.Message, opts []ai.Option) (*ai.Message, error) {
	if cfg.Caller == nil {
		return cfg.Model.Generate(ctx, conv, opts...)
	}
	stream, err := cfg.Caller.Call(ctx, cfg.Model, conv, opts, func() Attempt { return chunkObserver{step: step, on: cfg.OnChunk} })
	if err != nil {
		return nil, err
	}
	reply, err := ai.ConcatMessages(stream.Chunks)
	if err != nil {
		return nil, err
	}
	if reply.Role == "" {
		reply.Role = ai.RoleAssistant
	}
	return reply, nil
}

// chunkObserver is the Attempt of a streamed step: it forwards the chunks and has nothing to settle or undo.
type chunkObserver struct {
	step int
	on   func(step int, chunk *ai.Message)
}

func (o chunkObserver) Chunk(_ context.Context, c *ai.Message) error {
	if o.on != nil {
		o.on(o.step, c)
	}
	return nil
}
func (chunkObserver) Settle(context.Context, bool) error { return nil }
func (chunkObserver) Abort(context.Context, bool) error  { return nil }

func index(ctx context.Context, list []ai.InvokableTool) (map[string]ai.InvokableTool, error) {
	byName := make(map[string]ai.InvokableTool, len(list))
	for _, t := range list {
		info, err := t.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("agent: tool info: %w", err)
		}
		if info == nil || info.Name == "" {
			continue
		}
		byName[info.Name] = t
	}
	return byName, nil
}

// runToolCalls runs one step's calls and appends their results to conv in call order. With Config.Parallel,
// consecutive calls of ReadOnly tools run at the same time (at most MaxParallel), the rest one after the other.
func runToolCalls(ctx context.Context, cfg *Config, byName map[string]ai.InvokableTool,
	invoke func(context.Context, ai.ToolCall) string, calls []ai.ToolCall, conv *[]*ai.Message) error {
	record := func(call ai.ToolCall, result string) {
		if cfg.OnTool != nil {
			cfg.OnTool(call, result)
		}
		*conv = append(*conv, ai.ToolMessage(result, call.ID, ai.WithToolName(call.Function.Name)))
	}
	return Schedule(ctx, calls,
		func(_ context.Context, c ai.ToolCall) bool {
			ro, ok := byName[c.Function.Name].(ReadOnly)
			return cfg.Parallel && ok && ro.ReadOnly()
		},
		func(ctx context.Context, c ai.ToolCall) error {
			record(c, invoke(ctx, c))
			return nil
		},
		func(ctx context.Context, batch []ai.ToolCall) error {
			results := make([]string, len(batch))
			Parallel(len(batch), cfg.MaxParallel, func(k int) { results[k] = invoke(ctx, batch[k]) })
			for k, c := range batch {
				record(c, results[k])
			}
			return nil
		})
}

func defaultInvoke(ctx context.Context, byName map[string]ai.InvokableTool, call ai.ToolCall) string {
	t, ok := byName[call.Function.Name]
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", call.Function.Name)
	}
	args := call.Function.Arguments
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	out, err := t.InvokableRun(ctx, args)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}
