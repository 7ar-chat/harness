package workflows

import (
	"context"
	"fmt"
	"strings"
	"time"

	core "github.com/7ar-chat/harness/core/workflows"
)

// Start implements core.Engine.
func (e *Engine) Start(ctx context.Context, req core.StartRequest) (*core.Run, error) {
	return e.start1(ctx, req, nil)
}

// start1 creates a run of a published workflow and starts its first steps. parent is the running parent of a
// subworkflow (its in-memory state, which may not be saved yet).
func (e *Engine) start1(ctx context.Context, req core.StartRequest, parent *core.Run) (*core.Run, error) {
	w, err := e.o.Store.Get(ctx, req.WorkflowID)
	if err != nil {
		return nil, err
	}
	if w.ProjectID != req.ProjectID {
		return nil, core.ErrNotFound
	}
	if w.Status != core.StatusPublished {
		return nil, fmt.Errorf("%w: workflow %q is a draft: publish it first", core.ErrState, w.Name)
	}
	if parent != nil {
		if err := e.checkNesting(ctx, parent, w.ID); err != nil {
			return nil, err
		}
	}
	inputs := map[string]string{}
	for _, in := range w.Inputs {
		v := strings.TrimSpace(req.Inputs[in.Name])
		if v == "" {
			v = in.Default
		}
		if v == "" && in.Required {
			return nil, fmt.Errorf("%w: input %q is required", core.ErrInvalid, in.Name)
		}
		inputs[in.Name] = v
	}
	r := core.Run{
		ProjectID: w.ProjectID, WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: w.Version,
		Graph: w.Nodes, Inputs: inputs, RequirementID: req.RequirementID, Owner: req.Owner,
		Repo: req.Repo, BaseBranch: req.BaseBranch, State: core.RunRunning,
		ParentRunID: req.ParentRunID, ParentNodeID: req.ParentNodeID, ParentRound: req.ParentRound,
	}
	for _, n := range w.Nodes {
		r.Steps = append(r.Steps, core.NodeRun{NodeID: n.ID, State: core.NodePending})
	}
	r.Events = []core.RunEvent{{At: e.o.Now().UTC(), Kind: "started", Actor: req.Owner, Text: "started " + w.Name}}
	created, err := e.o.Store.CreateRun(ctx, r)
	if err != nil {
		return nil, err
	}
	if created.Repo != "" {
		// The steps that use the repository share one branch, so each builds on the last.
		created.Branch = "workflow/" + prefix(created.ID, 8)
		if created, err = e.o.Store.UpdateRun(ctx, *created); err != nil {
			return nil, err
		}
	}
	e.emit([]core.Change{{Run: *created, Kind: "started"}})
	return e.Advance(ctx, created.ID)
}

func prefix(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// checkNesting refuses a subworkflow that would recurse or nest too deep.
func (e *Engine) checkNesting(ctx context.Context, parent *core.Run, workflowID string) error {
	depth, cur := 1, parent
	for {
		if cur.WorkflowID == workflowID {
			return fmt.Errorf("%w: a workflow cannot run itself, even through another", core.ErrInvalid)
		}
		if cur.ParentRunID == "" {
			return nil
		}
		depth++
		if depth > core.MaxDepth {
			return fmt.Errorf("%w: subworkflows nest at most %d deep", core.ErrInvalid, core.MaxDepth)
		}
		next, err := e.o.Store.GetRun(ctx, cur.ParentRunID)
		if err != nil {
			return err
		}
		cur = next
	}
}

func stepOf(r *core.Run, id string) (*core.NodeRun, error) {
	s := r.Step(id)
	if s == nil {
		return nil, fmt.Errorf("%w: the run has no step %q", core.ErrNotFound, id)
	}
	return s, nil
}

func stateErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", core.ErrState, fmt.Sprintf(format, a...))
}

// Pause implements core.Engine: no step starts until Resume; the ones running go on.
func (e *Engine) Pause(ctx context.Context, runID, actor string) (*core.Run, error) {
	return e.mutate(ctx, runID, func(p *pass) error {
		if p.r.State != core.RunRunning {
			return stateErr("the run is %s", p.r.State)
		}
		p.r.State = core.RunPaused
		p.log("control", "", actor, "paused")
		p.note("control", "")
		return nil
	})
}

// Resume implements core.Engine.
func (e *Engine) Resume(ctx context.Context, runID, actor string) (*core.Run, error) {
	return e.mutate(ctx, runID, func(p *pass) error {
		if p.r.State != core.RunPaused {
			return stateErr("the run is %s", p.r.State)
		}
		p.r.State = core.RunRunning
		p.log("control", "", actor, "resumed")
		p.note("control", "")
		return nil
	})
}

// Cancel implements core.Engine.
func (e *Engine) Cancel(ctx context.Context, runID, actor string) (*core.Run, error) {
	return e.cancelRun(ctx, runID, actor)
}

func (e *Engine) cancelRun(ctx context.Context, runID, actor string) (*core.Run, error) {
	return e.mutate(ctx, runID, func(p *pass) error {
		r := p.r
		if r.State.Terminal() {
			return stateErr("the run is already %s", r.State)
		}
		for i := range r.Steps {
			s := &r.Steps[i]
			switch s.State {
			case core.NodeRunning, core.NodeWaiting:
				e.cancelStep(p, s)
				s.State, s.FinishedAt = core.NodeCancelled, e.o.Now().UTC()
			case core.NodePending:
				s.State = core.NodeCancelled
			}
		}
		r.State, r.FinishedAt = core.RunCancelled, e.o.Now().UTC()
		p.log("control", "", actor, "cancelled")
		p.note("control", "")
		return nil
	})
}

// reopen makes a finished run active again; steps that were skipped or cancelled with it go back to pending.
func (e *Engine) reopen(p *pass) {
	r := p.r
	if !r.State.Terminal() {
		return
	}
	r.State, r.Error = core.RunRunning, ""
	r.FinishedAt = time.Time{}
	for i := range r.Steps {
		if s := &r.Steps[i]; s.State == core.NodeSkipped || s.State == core.NodeCancelled {
			*s = core.NodeRun{NodeID: s.NodeID, State: core.NodePending, Round: s.Round}
		}
	}
}

// Rerun implements core.Engine.
func (e *Engine) Rerun(ctx context.Context, runID, nodeID, actor, feedback string) (*core.Run, error) {
	return e.mutate(ctx, runID, func(p *pass) error {
		if _, err := stepOf(p.r, nodeID); err != nil {
			return err
		}
		e.reopen(p)
		e.invalidate(p, nodeID, true)
		if feedback = strings.TrimSpace(feedback); feedback != "" {
			p.r.Step(nodeID).Feedback = feedback
		}
		p.log("control", nodeID, actor, "rerun from here")
		p.note("control", nodeID)
		return nil
	})
}

// Retry implements core.Engine.
func (e *Engine) Retry(ctx context.Context, runID, nodeID, actor string) (*core.Run, error) {
	return e.mutate(ctx, runID, func(p *pass) error {
		s, err := stepOf(p.r, nodeID)
		if err != nil {
			return err
		}
		if s.State != core.NodeFailed && s.State != core.NodeCancelled {
			return stateErr("step %s is %s: only a failed or cancelled step is retried", nodeID, s.State)
		}
		fb := s.Error
		e.reopen(p)
		e.invalidate(p, nodeID, true)
		p.r.Step(nodeID).Feedback = fb
		p.log("control", nodeID, actor, "retry")
		p.note("control", nodeID)
		return nil
	})
}

// Takeover implements core.Engine.
func (e *Engine) Takeover(ctx context.Context, runID, nodeID, actor, result string, outputs map[string]string) (*core.Run, error) {
	return e.mutate(ctx, runID, func(p *pass) error {
		s, err := stepOf(p.r, nodeID)
		if err != nil {
			return err
		}
		if s.State == core.NodeDone {
			return stateErr("step %s is %s", nodeID, s.State)
		}
		result = strings.TrimSpace(result)
		if result == "" {
			return fmt.Errorf("%w: say what the result is", core.ErrInvalid)
		}
		e.reopen(p)
		if s.State == core.NodeRunning || s.State == core.NodeWaiting {
			e.cancelStep(p, s)
		}
		s.State, s.Manual, s.Result, s.Outputs, s.Error = core.NodeDone, true, result, outputs, ""
		if s.StartedAt.IsZero() {
			s.StartedAt = e.o.Now().UTC()
		}
		s.FinishedAt = e.o.Now().UTC()
		s.Note = "done by " + actor
		p.log("control", nodeID, actor, "took over: "+firstLine(result))
		p.note("control", nodeID)
		return nil
	})
}

func waitingGate(p *pass, nodeID string) (*core.NodeRun, core.Node, error) {
	s, err := stepOf(p.r, nodeID)
	if err != nil {
		return nil, core.Node{}, err
	}
	n, _ := p.r.Node(nodeID)
	if n.Kind != core.KindGate {
		return nil, n, stateErr("step %s is not a gate", nodeID)
	}
	if s.State != core.NodeWaiting {
		return nil, n, stateErr("gate %s is %s, not waiting", nodeID, s.State)
	}
	if p.r.State.Terminal() {
		return nil, n, stateErr("the run is %s", p.r.State)
	}
	return s, n, nil
}

// Approve implements core.Engine.
func (e *Engine) Approve(ctx context.Context, runID, nodeID, actor, comment string) (*core.Run, error) {
	return e.mutate(ctx, runID, func(p *pass) error {
		s, _, err := waitingGate(p, nodeID)
		if err != nil {
			return err
		}
		comment = strings.TrimSpace(comment)
		s.State, s.Result, s.FinishedAt = core.NodeDone, comment, e.o.Now().UTC()
		s.Note = "approved by " + actor
		p.log("gate", nodeID, actor, "approved"+suffix(comment))
		p.note("gate", nodeID)
		return nil
	})
}

// SendBack implements core.Engine: the run goes back to the gate's target (or the step before it) with the
// feedback, and everything from there on runs again.
func (e *Engine) SendBack(ctx context.Context, runID, nodeID, actor, feedback string) (*core.Run, error) {
	return e.mutate(ctx, runID, func(p *pass) error {
		_, n, err := waitingGate(p, nodeID)
		if err != nil {
			return err
		}
		feedback = strings.TrimSpace(feedback)
		if feedback == "" {
			return fmt.Errorf("%w: say what must change", core.ErrInvalid)
		}
		p.log("gate", nodeID, actor, "sent back"+suffix(feedback))
		e.rework(p, n, "Sent back by "+actor+": "+feedback, "sent back")
		p.note("gate", nodeID)
		return nil
	})
}

// AddContext implements core.Engine: a note the steps after the gate are given; the gate keeps waiting.
func (e *Engine) AddContext(ctx context.Context, runID, nodeID, actor, text string) (*core.Run, error) {
	return e.mutate(ctx, runID, func(p *pass) error {
		s, _, err := waitingGate(p, nodeID)
		if err != nil {
			return err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return fmt.Errorf("%w: the note is empty", core.ErrInvalid)
		}
		s.Context = append(s.Context, actor+": "+text)
		p.log("gate", nodeID, actor, "added context: "+firstLine(text))
		p.note("gate", nodeID)
		return nil
	})
}

func suffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + firstLine(s)
}
