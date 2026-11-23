package product

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/workflows"
)

// The "Needs review" inbox: everything that waits on the caller, across projects.
//
//	GET /api/needs-review → {tasks, threads, gates, count}
//
// tasks are the factory tasks of the caller's projects (member or owner) that are blocked, or in review with an
// open pull request; threads are the sessions waiting for an approval or an answer (from the overview). A
// server without the factory feature answers with the threads only.

// inboxTask is one task waiting on a human.
type inboxTask struct {
	// Kind is "blocked" (the task needs a decision) or "review" (its pull request waits for a merge).
	Kind        string       `json:"kind"`
	ProjectID   string       `json:"projectId"`
	ProjectName string       `json:"projectName"`
	Role        string       `json:"role"`
	Task        factory.Task `json:"task"`
	// Reason is why a blocked task stopped; LastEvent is the newest line of the task's history.
	Reason    string             `json:"reason,omitempty"`
	LastEvent *factory.TaskEvent `json:"lastEvent,omitempty"`
}

// inboxThread is one session waiting for an approval or an answer.
type inboxThread struct {
	// ProjectID is empty for a personal session.
	ProjectID   string        `json:"projectId,omitempty"`
	ProjectName string        `json:"projectName"`
	Thread      threadSummary `json:"thread"`
}

// inboxGate is a workflow gate that waits for a decision of the caller (an approver, or a project owner).
type inboxGate struct {
	ProjectID    string    `json:"projectId"`
	ProjectName  string    `json:"projectName"`
	RunID        string    `json:"runId"`
	WorkflowName string    `json:"workflowName"`
	NodeID       string    `json:"nodeId"`
	Title        string    `json:"title"`
	Approvers    []string  `json:"approvers"`
	Since        time.Time `json:"since,omitzero"`
	// Summary is what the step before the gate produced, to decide on.
	Summary string `json:"summary,omitempty"`
}

// inboxDoc is the answer of GET /needs-review.
type inboxDoc struct {
	Tasks   []inboxTask   `json:"tasks"`
	Threads []inboxThread `json:"threads"`
	// Gates are the workflow gates waiting for the caller.
	Gates []inboxGate `json:"gates"`
	Count int         `json:"count"`
}

func (s *server) routeInbox() {
	s.Route("GET /needs-review", s.needsReview)
}

func (s *server) needsReview(w http.ResponseWriter, r *http.Request, c Caller) {
	ov, err := s.overviewFor(r.Context(), c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	doc := inboxDoc{Tasks: []inboxTask{}, Threads: []inboxThread{}, Gates: []inboxGate{}}
	waiting := func(t threadSummary) bool {
		return t.Status == "waiting_approval" || t.Status == "waiting_input"
	}
	for _, t := range ov.Personal {
		if waiting(t) {
			doc.Threads = append(doc.Threads, inboxThread{ProjectName: "Personal", Thread: t})
		}
	}
	for _, p := range ov.Projects {
		if !store.RoleAtLeast(p.Role, store.RoleMember) {
			continue
		}
		for _, t := range p.Threads {
			if waiting(t) {
				doc.Threads = append(doc.Threads, inboxThread{ProjectID: p.ID, ProjectName: p.Name, Thread: t})
			}
		}
		if s.factory() != nil {
			doc.Tasks = append(doc.Tasks, s.inboxTasks(r.Context(), p)...)
		}
		if s.opt.Workflows != nil {
			doc.Gates = append(doc.Gates, s.inboxGates(r.Context(), p, c.Actor)...)
		}
	}
	sort.SliceStable(doc.Threads, func(i, j int) bool { return doc.Threads[i].Thread.UpdatedAt.After(doc.Threads[j].Thread.UpdatedAt) })
	sort.SliceStable(doc.Tasks, func(i, j int) bool {
		a, b := doc.Tasks[i], doc.Tasks[j]
		if a.Kind != b.Kind {
			return a.Kind == "blocked" // a stopped task before a pull request that only waits
		}
		return a.Task.UpdatedAt.After(b.Task.UpdatedAt)
	})
	sort.SliceStable(doc.Gates, func(i, j int) bool { return doc.Gates[i].Since.After(doc.Gates[j].Since) })
	doc.Count = len(doc.Tasks) + len(doc.Threads) + len(doc.Gates)
	writeJSON(w, http.StatusOK, doc)
}

// inboxGates are the waiting gates of the project's active runs that the actor may decide.
func (s *server) inboxGates(ctx context.Context, p overviewProject, actor string) []inboxGate {
	runs, err := s.opt.Workflows.ListRuns(ctx, workflows.RunFilter{ProjectID: p.ID, States: []workflows.RunState{workflows.RunRunning, workflows.RunPaused}, Limit: 100})
	if err != nil {
		slog.Warn("product: listing a project's workflow runs for the inbox failed", "project", p.ID, "error", err)
		return nil
	}
	var out []inboxGate
	for _, run := range runs {
		for _, st := range run.Steps {
			n, ok := run.Node(st.NodeID)
			if st.State != workflows.NodeWaiting || !ok || !mayDecide(n, actor, p.Role) {
				continue
			}
			summary := ""
			for _, d := range n.DependsOn {
				if ds := run.Step(d); ds != nil && ds.Result != "" {
					summary = ds.Result
					break
				}
			}
			if r := []rune(summary); len(r) > 1500 {
				summary = string(r[:1500]) + "..."
			}
			out = append(out, inboxGate{ProjectID: p.ID, ProjectName: p.Name, RunID: run.ID, WorkflowName: run.WorkflowName,
				NodeID: n.ID, Title: gateTitle(n), Approvers: nonNil(n.Approvers), Since: st.StartedAt, Summary: summary})
		}
	}
	return out
}

// inboxTasks are the blocked tasks and the tasks in review with an open pull request of one project.
func (s *server) inboxTasks(ctx context.Context, p overviewProject) []inboxTask {
	f := s.factory()
	ts, err := f.Tasks.List(ctx, factory.Filter{ProjectID: p.ID, States: []factory.State{factory.StateBlocked, factory.StateReview}, Limit: 200})
	if err != nil {
		// One project's failure must not hide the rest of the inbox.
		slog.Warn("product: listing a project's tasks for the inbox failed", "project", p.ID, "error", err)
		return nil
	}
	var out []inboxTask
	for _, t := range ts {
		it := inboxTask{ProjectID: p.ID, ProjectName: p.Name, Role: p.Role, Task: t}
		switch {
		case t.State == factory.StateBlocked:
			it.Kind, it.Reason = "blocked", t.BlockedReason
		case t.State == factory.StateReview && t.PR.Number > 0:
			it.Kind = "review"
		default:
			continue
		}
		if ev, err := f.Store.ListEvents(ctx, t.ID, 1); err == nil && len(ev) > 0 {
			it.LastEvent = &ev[len(ev)-1]
		}
		out = append(out, it)
	}
	return out
}
