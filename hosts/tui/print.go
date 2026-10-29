package tui

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/GhaziBenDahmane/harness/engine/agui"
)

// Exit statuses of print mode.
const (
	ExitOK          = 0
	ExitFailed      = 1
	ExitInterrupted = 2 // the run ended waiting for an answer or approval
)

// PrintOptions configure RunPrint.
type PrintOptions struct {
	Stdout, Stderr io.Writer
	// JSON writes the AG-UI events as JSON lines to Stdout (nothing else).
	JSON bool
}

// RunPrint sends prompt, streams the answer to Stdout and the tool activity
// to Stderr, and returns the process exit status: 0 on success, 1 when the
// run failed, was cancelled or could not start, 2 when it ended waiting for
// the user (print mode cannot answer).
func RunPrint(ctx context.Context, b Backend, prompt string, o PrintOptions) int {
	runID, err := b.Send(ctx, prompt)
	if err != nil {
		fmt.Fprintln(o.Stderr, "harness: send:", err)
		return ExitFailed
	}
	p := &printer{o: o, tools: map[string]*printTool{}}
	for se := range b.Events(ctx, runID) {
		if o.JSON {
			fmt.Fprintln(o.Stdout, strings.TrimSpace(string(se.Data)))
			if code, done := p.finish(Decode(se), true); done {
				return code
			}
			continue
		}
		if code, done := p.handle(Decode(se)); done {
			return code
		}
	}
	if ctx.Err() != nil {
		return ExitFailed
	}
	fmt.Fprintln(o.Stderr, "harness: the event stream ended without a result")
	return ExitFailed
}

type printTool struct {
	name, args string
}

type printer struct {
	o        PrintOptions
	tools    map[string]*printTool
	midText  bool // an assistant message is open on stdout
	wroteAny bool
}

func (p *printer) handle(ev Ev) (int, bool) {
	switch ev.Type {
	case agui.TextMessageStart:
		if p.wroteAny && !p.midText {
			fmt.Fprintln(p.o.Stdout)
		}
		p.midText = true
	case agui.TextMessageContent:
		p.midText, p.wroteAny = true, true
		fmt.Fprint(p.o.Stdout, ev.Delta)
	case agui.TextMessageEnd:
		if p.midText {
			fmt.Fprintln(p.o.Stdout)
		}
		p.midText = false
	case agui.ToolCallStart:
		p.tools[ev.ToolCallID] = &printTool{name: ev.ToolName}
	case agui.ToolCallArgs:
		if t := p.tools[ev.ToolCallID]; t != nil {
			t.args += ev.Delta
		}
	case agui.ToolCallEnd:
		if t := p.tools[ev.ToolCallID]; t != nil {
			fmt.Fprintf(p.o.Stderr, "> %s %s\n", t.name, clip(ToolSummary(t.args), 120))
		}
	case agui.ToolCallResult:
		mark := "ok"
		if ev.IsError {
			mark = "error"
		}
		fmt.Fprintf(p.o.Stderr, "  %s: %s\n", mark, clip(oneLine(ev.Content), 160))
	case agui.Custom:
		switch ev.Name {
		case agui.CustomSteer:
			fmt.Fprintf(p.o.Stderr, "  steer: %v\n", ev.Value["text"])
		case agui.CustomSubagent:
			if e := ev.SubagentValue(); e.Phase == agui.SubagentTool {
				mark := ""
				if e.Tool.Err {
					mark = " (error)"
				}
				fmt.Fprintf(p.o.Stderr, "  ⎇ %s %s%s\n", e.Tool.Name, clip(e.Tool.Args, 120), mark)
			}
		}
	}
	return p.finish(ev, false)
}

// finish maps a terminal event to an exit status. quiet suppresses the
// diagnostics (JSON mode keeps stderr for errors only).
func (p *printer) finish(ev Ev, quiet bool) (int, bool) {
	switch ev.Type {
	case agui.RunError:
		fmt.Fprintf(p.o.Stderr, "harness: run failed: %s\n", ev.Message)
		return ExitFailed, true
	case agui.RunFinished:
		switch ev.Outcome {
		case "success", "":
			return ExitOK, true
		case "interrupt":
			for _, in := range ev.Interrupts {
				fmt.Fprintf(p.o.Stderr, "harness: waiting for input (%s): %s\n", in.Reason, in.Message)
			}
			return ExitInterrupted, true
		default:
			fmt.Fprintf(p.o.Stderr, "harness: run %s\n", ev.Outcome)
			return ExitFailed, true
		}
	}
	return 0, false
}
