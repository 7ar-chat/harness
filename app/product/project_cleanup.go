package product

import (
	"context"
	"errors"
	"log/slog"

	"github.com/7ar-chat/harness/core/executors"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/workflows"
)

// cancelProjectRuns stops the workflow runs still active in a project that is being deleted, so none keeps starting
// tasks in it (the factory's own tasks are cancelled by their runs).
func (s *server) cancelProjectRuns(ctx context.Context, projectID string) {
	if s.opt.Workflows == nil || s.opt.WorkflowEngine == nil {
		return
	}
	runs, err := s.opt.Workflows.ListRuns(ctx, workflows.RunFilter{ProjectID: projectID, States: []workflows.RunState{workflows.RunRunning, workflows.RunPaused}, Limit: 500})
	if err != nil {
		slog.Warn("project delete: listing the workflow runs failed", "project", projectID, "error", err)
		return
	}
	for _, r := range runs {
		if _, err := s.opt.WorkflowEngine.Cancel(ctx, r.ID, "project deleted"); err != nil && !errors.Is(err, workflows.ErrState) && !errors.Is(err, workflows.ErrNotFound) {
			slog.Warn("project delete: cancelling a workflow run failed", "project", projectID, "run", r.ID, "error", err)
		}
	}
}

// cancelProjectTasks cancels the factory tasks of a project that is being deleted and are not finished, so no run keeps
// working (and spending) in a project that is going away. Cancelling a running task also cancels its run, which
// stops the agent; its thread's sandbox is released when the thread is deleted.
func (s *server) cancelProjectTasks(ctx context.Context, projectID string) {
	f := s.factory()
	if f == nil || f.Store == nil || f.Tasks == nil {
		return
	}
	live := []factory.State{factory.StatePending, factory.StateReady, factory.StateRunning, factory.StateReview, factory.StateBlocked}
	list, err := f.Store.ListTasks(ctx, factory.Filter{ProjectID: projectID, States: live, Limit: 100000})
	if err != nil {
		slog.Warn("project delete: listing the tasks failed", "project", projectID, "error", err)
		return
	}
	for _, t := range list {
		if err := f.Tasks.Cancel(ctx, t.ID); err != nil && !errors.Is(err, factory.ErrConflict) && !errors.Is(err, factory.ErrNotFound) {
			slog.Warn("project delete: cancelling a task failed", "project", projectID, "task", t.ID, "error", err)
		}
	}
}

// dropProjectTasks removes the factory rows of a deleted project: the threads its tasks worked in (which kills their
// sandboxes and drops their documents, as deleting a thread does anywhere), then the tasks, their events and learning
// records, their thread info and the project's triggers.
func (s *server) dropProjectTasks(ctx context.Context, projectID string) {
	f := s.factory()
	if f == nil || f.Store == nil {
		return
	}
	list, err := f.Store.ListTasks(ctx, factory.Filter{ProjectID: projectID, Limit: 100000})
	if err != nil {
		slog.Warn("project delete: listing the tasks failed", "project", projectID, "error", err)
	}
	for _, t := range list {
		if t.ThreadID == "" {
			continue
		}
		// Most of these threads went with the project's threads already; one that is still there is deleted, one that no
		// store could delete still loses its documents.
		if _, err := s.H.DeleteThread(ctx, t.Owner, t.ThreadID); err != nil {
			s.H.ThreadDeleted(ctx, t.ThreadID)
		}
	}
	if err := f.Store.DeleteProject(ctx, projectID); err != nil {
		slog.Warn("project delete: deleting the factory rows failed", "project", projectID, "error", err)
	}
}

// dropProjectRecords removes what a deleted project owned in the stores of the product: its requirements, its
// executors, its workflows and runs. (Roles, installs and schedules are dropped by deleteProject itself.) The
// project is already gone, so a failure is logged and leaves rows nobody can reach.
func (s *server) dropProjectRecords(ctx context.Context, projectID string) {
	if rs := s.opt.Requirements; rs != nil {
		for {
			list, err := rs.List(ctx, requirements.Filter{ProjectID: projectID, Limit: 500})
			if err != nil {
				slog.Warn("project delete: listing the requirements failed", "project", projectID, "error", err)
				break
			}
			deleted := 0
			for _, r := range list {
				if err := rs.Delete(ctx, r.ID); err != nil {
					slog.Warn("project delete: deleting a requirement failed", "project", projectID, "requirement", r.ID, "error", err)
					continue
				}
				deleted++
			}
			if len(list) < 500 || deleted == 0 {
				break
			}
		}
	}
	if a := s.opt.Executors; a != nil && a.defs != nil {
		if err := a.defs.DeleteProject(ctx, projectID); err != nil && !errors.Is(err, executors.ErrNotFound) {
			slog.Warn("project delete: deleting the executors failed", "project", projectID, "error", err)
		}
	}
	if ws := s.opt.Workflows; ws != nil {
		if err := ws.DeleteProject(ctx, projectID); err != nil && !errors.Is(err, workflows.ErrNotFound) {
			slog.Warn("project delete: deleting the workflows failed", "project", projectID, "error", err)
		}
	}
}
