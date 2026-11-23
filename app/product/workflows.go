package product

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/roles"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/workflows"
)

// Workflows: the procedures (SOPs) of a project as a versioned graph of steps (an agent, a command, a human gate,
// another workflow), and the runs that carry one out as factory tasks. Served when Options.Workflows is set; the
// routes that start or steer a run need Options.WorkflowEngine (the factory on) and answer 501 without it.
//
//	GET    /api/workflow-templates                                   → [WorkflowTemplate]   (the four builtin templates, read-only)
//	GET    /api/projects/{pid}/workflows                             → [Workflow]
//	POST   /api/projects/{pid}/workflows {name,description?,inputs?,nodes}   → 201 Workflow  (member; a draft; 400 with the issues when the graph is invalid)
//	POST   /api/projects/{pid}/workflows/validate {…}                → {issues}             (member; nothing is stored)
//	POST   /api/projects/{pid}/workflows/from-template {template,name?} → 201 Workflow      (member; copies a builtin template)
//	GET    /api/projects/{pid}/workflows/{wid}                       → Workflow
//	PUT    /api/projects/{pid}/workflows/{wid} {…,version?}          → Workflow             (member; back to draft; a stale version is 409)
//	DELETE /api/projects/{pid}/workflows/{wid}                       → 204                  (member)
//	POST   /api/projects/{pid}/workflows/{wid}/publish {version?}    → Workflow             (member; validates again)
//	POST   /api/projects/{pid}/workflows/{wid}/duplicate {name?}     → 201 Workflow         (member)
//	GET    /api/projects/{pid}/workflows/{wid}/history               → [WorkflowVersion]    (the last 10 versions, newest first)
//	POST   /api/projects/{pid}/workflows/{wid}/restore {version}     → Workflow             (member; the old version becomes a new draft)
//	GET    /api/projects/{pid}/workflow-runs?workflowId=&state=      → [WorkflowRun]
//	POST   /api/projects/{pid}/workflow-runs {workflowId,inputs?,requirementId?} → 201 WorkflowRun  (member)
//	GET    /api/projects/{pid}/workflow-runs/{rid}                   → WorkflowRunDetail
//	POST   /api/workflow-runs/{id}/pause|resume|cancel               → WorkflowRun          (member)
//	POST   /api/workflow-runs/{id}/rerun {nodeId,feedback?}          → WorkflowRun          (member; the node and everything after it runs again)
//	POST   /api/workflow-runs/{id}/retry {nodeId}                    → WorkflowRun          (member)
//	POST   /api/workflow-runs/{id}/takeover {nodeId,result,outputs?} → WorkflowRun          (member; the step is done by a person)
//	POST   /api/workflow-runs/{id}/nodes/{nid}/approve {comment?}    → WorkflowRun          (an approver of the gate, or a project owner)
//	POST   /api/workflow-runs/{id}/nodes/{nid}/send-back {feedback}  → WorkflowRun          (the same)
//	POST   /api/workflow-runs/{id}/nodes/{nid}/add-context {text}    → WorkflowRun          (the same)
//
// A gate that waits for the caller is also listed in GET /api/needs-review (see inbox.go). The feed carries
// workflow.changed (a workflow was saved or published), workflow.run (a run or step moved) and workflow.gate (a
// gate waits for a person).

// WorkflowRunDetail is a run with who may decide its waiting gates.
type WorkflowRunDetail struct {
	Run   workflows.Run      `json:"run"`
	Gates []WorkflowGateView `json:"gates"`
	// CanSteer is whether the caller may pause, rerun or take over (a project member).
	CanSteer bool `json:"canSteer"`
}

// WorkflowGateView is a waiting gate and whether the caller may decide it.
type WorkflowGateView struct {
	NodeID    string   `json:"nodeId"`
	Title     string   `json:"title"`
	Approvers []string `json:"approvers"`
	MayDecide bool     `json:"mayDecide"`
}

// WorkflowIssues is the body of a validation answer.
type WorkflowIssues struct {
	Issues []workflows.Issue `json:"issues"`
}

type workflowBody struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Inputs      []workflows.Input `json:"inputs"`
	Nodes       []workflows.Node  `json:"nodes"`
	// Version is the version the editor started from; 0 skips the check.
	Version int `json:"version"`
}

type workflowRunBody struct {
	WorkflowID    string            `json:"workflowId"`
	Inputs        map[string]string `json:"inputs"`
	RequirementID string            `json:"requirementId"`
}

type nodeBody struct {
	NodeID   string            `json:"nodeId"`
	Feedback string            `json:"feedback"`
	Result   string            `json:"result"`
	Outputs  map[string]string `json:"outputs"`
	Comment  string            `json:"comment"`
	Text     string            `json:"text"`
}

func (s *server) routeWorkflows() {
	if s.opt.Workflows == nil {
		return
	}
	if eng := s.opt.WorkflowEngine; eng != nil {
		eng.Observe(s.workflowChanged)
	}
	s.Route("GET /workflow-templates", s.listWorkflowTemplates)
	s.Route("GET /projects/{pid}/workflows", s.listWorkflows)
	s.Route("POST /projects/{pid}/workflows", s.createWorkflow)
	s.Route("POST /projects/{pid}/workflows/validate", s.validateWorkflow)
	s.Route("POST /projects/{pid}/workflows/from-template", s.workflowFromTemplate)
	s.Route("GET /projects/{pid}/workflows/{wid}", s.getWorkflow)
	s.Route("PUT /projects/{pid}/workflows/{wid}", s.putWorkflow)
	s.Route("DELETE /projects/{pid}/workflows/{wid}", s.deleteWorkflow)
	s.Route("POST /projects/{pid}/workflows/{wid}/publish", s.publishWorkflow)
	s.Route("POST /projects/{pid}/workflows/{wid}/duplicate", s.duplicateWorkflow)
	s.Route("GET /projects/{pid}/workflows/{wid}/history", s.workflowHistory)
	s.Route("POST /projects/{pid}/workflows/{wid}/restore", s.restoreWorkflow)
	s.Route("GET /projects/{pid}/workflow-runs", s.listWorkflowRuns)
	s.Route("POST /projects/{pid}/workflow-runs", s.createWorkflowRun)
	s.Route("GET /projects/{pid}/workflow-runs/{rid}", s.getWorkflowRun)
	for _, a := range []string{"pause", "resume", "cancel", "rerun", "retry", "takeover"} {
		s.Route("POST /workflow-runs/{id}/"+a, s.runAction(a))
	}
	for _, a := range []string{"approve", "send-back", "add-context"} {
		s.Route("POST /workflow-runs/{id}/nodes/{nid}/"+a, s.gateAction(a))
	}
}

func workflowFail(w http.ResponseWriter, err error) {
	var inv *workflows.InvalidError
	switch {
	case errors.As(err, &inv):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"code": "invalid_workflow", "message": inv.Error()[len(workflows.ErrInvalid.Error())+2:], "issues": inv.Issues}})
	case errors.Is(err, workflows.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
	case errors.Is(err, workflows.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "A workflow with that name already exists in this project.")
	case errors.Is(err, workflows.ErrStale):
		writeError(w, http.StatusConflict, "stale", "The workflow was changed by someone else: reload it and apply your change again.")
	case errors.Is(err, workflows.ErrState):
		writeError(w, http.StatusConflict, "state", strings.TrimPrefix(err.Error(), workflows.ErrState.Error()+": "))
	case errors.Is(err, workflows.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "You are not an approver of this gate.")
	case errors.Is(err, workflows.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), workflows.ErrInvalid.Error()+": "))
	default:
		fail(w, err)
	}
}

// engine answers 501 when the factory (so the engine) is not on.
func (s *server) workflowEngine(w http.ResponseWriter) (workflows.Engine, bool) {
	if s.opt.WorkflowEngine == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Workflow runs need the agent factory (FACTORY=1).")
		return nil, false
	}
	return s.opt.WorkflowEngine, true
}

// roleExists tells the validation whether a step's role resolves for the project.
func (s *server) roleExists(ctx context.Context, login, projectID string) func(string) bool {
	return func(name string) bool {
		if s.roles != nil {
			_, err := s.roles.Resolve(ctx, roles.Query{Login: login, ProjectID: projectID}, name)
			return err == nil
		}
		_, ok := builtinRoles()[name]
		return ok
	}
}

func (s *server) workflowEnv(ctx context.Context, login, projectID string) workflows.Env {
	return workflows.Env{
		RoleExists: s.roleExists(ctx, login, projectID),
		Workflow: func(id string) (*workflows.Workflow, bool) {
			w, err := s.opt.Workflows.Get(ctx, id)
			if err != nil || w.ProjectID != projectID {
				return nil, false
			}
			return w, true
		},
	}
}

func (s *server) workflowOf(w http.ResponseWriter, r *http.Request, pid string) (*workflows.Workflow, bool) {
	wf, err := s.opt.Workflows.Get(r.Context(), r.PathValue("wid"))
	if errors.Is(err, workflows.ErrNotFound) || err == nil && wf.ProjectID != pid {
		s.FailAccess(w, errNotFound)
		return nil, false
	}
	if err != nil {
		workflowFail(w, err)
		return nil, false
	}
	return wf, true
}

func (s *server) workflowAudit(c Caller, pid, action string, wf *workflows.Workflow) {
	s.note(inProject(c, pid), "", action, map[string]any{"workflow": wf.ID, "name": wf.Name, "version": wf.Version},
		FeedEvent{Type: "workflow.changed", Title: wf.Name})
}

func (s *server) listWorkflowTemplates(w http.ResponseWriter, _ *http.Request, _ Caller) {
	writeJSON(w, http.StatusOK, workflows.Templates())
}

func (s *server) listWorkflows(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	list, err := s.opt.Workflows.List(r.Context(), p.ID)
	if err != nil {
		workflowFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

func (s *server) getWorkflow(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	if wf, ok := s.workflowOf(w, r, p.ID); ok {
		writeJSON(w, http.StatusOK, wf)
	}
}

func (b workflowBody) workflow(projectID string) workflows.Workflow {
	return workflows.Workflow{ProjectID: projectID, Name: b.Name, Description: b.Description, Inputs: b.Inputs, Nodes: b.Nodes, Status: workflows.StatusDraft}
}

func (s *server) createWorkflow(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	var body workflowBody
	if !decode(w, r, &body) {
		return
	}
	wf := body.workflow(p.ID)
	if err := wf.Check(s.workflowEnv(r.Context(), c.Actor, p.ID)); err != nil {
		workflowFail(w, err)
		return
	}
	created, err := s.opt.Workflows.Create(r.Context(), wf, c.Actor)
	if err != nil {
		workflowFail(w, err)
		return
	}
	s.workflowAudit(c, p.ID, "workflow.create", created)
	writeJSON(w, http.StatusCreated, created)
}

func (s *server) validateWorkflow(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	var body workflowBody
	if !decode(w, r, &body) {
		return
	}
	wf := body.workflow(p.ID)
	if id := r.URL.Query().Get("id"); id != "" {
		wf.ID = id // so a workflow does not name itself as a subworkflow
	}
	issues := wf.Validate(s.workflowEnv(r.Context(), c.Actor, p.ID))
	writeJSON(w, http.StatusOK, WorkflowIssues{Issues: nonNil(issues)})
}

func (s *server) workflowFromTemplate(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	var body struct {
		Template string `json:"template"`
		Name     string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	t, found := workflows.TemplateByID(body.Template)
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "No such template.")
		return
	}
	wf := workflows.FromTemplate(t, p.ID)
	if n := strings.TrimSpace(body.Name); n != "" {
		wf.Name = n
	}
	created, err := s.createUniqueWorkflow(r.Context(), c.Actor, wf)
	if err != nil {
		workflowFail(w, err)
		return
	}
	s.workflowAudit(c, p.ID, "workflow.from_template", created)
	writeJSON(w, http.StatusCreated, created)
}

// createUniqueWorkflow stores wf, adding " (2)", " (3)"... to its name while the name is taken. The graph is
// checked first; a template copy that no longer passes (a role it names was removed) is refused with the issues.
func (s *server) createUniqueWorkflow(ctx context.Context, actor string, wf workflows.Workflow) (*workflows.Workflow, error) {
	if err := wf.Check(s.workflowEnv(ctx, actor, wf.ProjectID)); err != nil {
		return nil, err
	}
	base := wf.Name
	for i := 2; i < 50; i++ {
		created, err := s.opt.Workflows.Create(ctx, wf, actor)
		if !errors.Is(err, workflows.ErrConflict) {
			return created, err
		}
		wf.Name = fmt.Sprintf("%s (%d)", base, i)
	}
	return nil, workflows.ErrConflict
}

func (s *server) putWorkflow(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	cur, ok := s.workflowOf(w, r, p.ID)
	if !ok {
		return
	}
	var body workflowBody
	if !decode(w, r, &body) {
		return
	}
	next := body.workflow(p.ID)
	next.ID, next.Version, next.PublishedVersion = cur.ID, body.Version, cur.PublishedVersion
	if err := next.Check(s.workflowEnv(r.Context(), c.Actor, p.ID)); err != nil {
		workflowFail(w, err)
		return
	}
	updated, err := s.opt.Workflows.Update(r.Context(), next, c.Actor)
	if err != nil {
		workflowFail(w, err)
		return
	}
	s.workflowAudit(c, p.ID, "workflow.update", updated)
	writeJSON(w, http.StatusOK, updated)
}

func (s *server) deleteWorkflow(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	wf, ok := s.workflowOf(w, r, p.ID)
	if !ok {
		return
	}
	if err := s.opt.Workflows.Delete(r.Context(), wf.ID); err != nil {
		workflowFail(w, err)
		return
	}
	s.workflowAudit(c, p.ID, "workflow.delete", wf)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) publishWorkflow(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	cur, ok := s.workflowOf(w, r, p.ID)
	if !ok {
		return
	}
	var body struct {
		Version int `json:"version"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Version != 0 && body.Version != cur.Version {
		workflowFail(w, workflows.ErrStale)
		return
	}
	if err := cur.Check(s.workflowEnv(r.Context(), c.Actor, p.ID)); err != nil {
		workflowFail(w, err)
		return
	}
	next := *cur
	next.Status = workflows.StatusPublished
	next.PublishedVersion = cur.Version + 1 // the version this save creates
	updated, err := s.opt.Workflows.Update(r.Context(), next, c.Actor)
	if err != nil {
		workflowFail(w, err)
		return
	}
	s.workflowAudit(c, p.ID, "workflow.publish", updated)
	writeJSON(w, http.StatusOK, updated)
}

func (s *server) duplicateWorkflow(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	src, ok := s.workflowOf(w, r, p.ID)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	cp := *src
	cp.ID, cp.Status, cp.Version, cp.PublishedVersion = "", workflows.StatusDraft, 0, 0
	cp.Name = strings.TrimSpace(body.Name)
	if cp.Name == "" {
		cp.Name = src.Name + " copy"
	}
	created, err := s.createUniqueWorkflow(r.Context(), c.Actor, cp)
	if err != nil {
		workflowFail(w, err)
		return
	}
	s.workflowAudit(c, p.ID, "workflow.duplicate", created)
	writeJSON(w, http.StatusCreated, created)
}

func (s *server) workflowHistory(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	wf, ok := s.workflowOf(w, r, p.ID)
	if !ok {
		return
	}
	h, err := s.opt.Workflows.History(r.Context(), wf.ID)
	if err != nil {
		workflowFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(h))
}

func (s *server) restoreWorkflow(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	cur, ok := s.workflowOf(w, r, p.ID)
	if !ok {
		return
	}
	var body struct {
		Version int `json:"version"`
	}
	if !decode(w, r, &body) {
		return
	}
	h, err := s.opt.Workflows.History(r.Context(), cur.ID)
	if err != nil {
		workflowFail(w, err)
		return
	}
	i := slices.IndexFunc(h, func(v workflows.Version) bool { return v.Version == body.Version })
	if i < 0 {
		writeError(w, http.StatusNotFound, "not_found", "No such version.")
		return
	}
	old := h[i].Workflow
	next := *cur
	next.Name, next.Description, next.Inputs, next.Nodes = old.Name, old.Description, old.Inputs, old.Nodes
	next.Status, next.Version = workflows.StatusDraft, 0
	updated, err := s.opt.Workflows.Update(r.Context(), next, c.Actor)
	if err != nil {
		workflowFail(w, err)
		return
	}
	s.workflowAudit(c, p.ID, "workflow.restore", updated)
	writeJSON(w, http.StatusOK, updated)
}

// ---- runs ----

func (s *server) listWorkflowRuns(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := workflows.RunFilter{ProjectID: p.ID, WorkflowID: q.Get("workflowId"), Limit: min(limitParam(r, 50), 200)}
	for _, v := range strings.Split(q.Get("state"), ",") {
		if v = strings.TrimSpace(v); v != "" {
			f.States = append(f.States, workflows.RunState(v))
		}
	}
	list, err := s.opt.Workflows.ListRuns(r.Context(), f)
	if err != nil {
		workflowFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

func (s *server) createWorkflowRun(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	eng, ok := s.workflowEngine(w)
	if !ok {
		return
	}
	var body workflowRunBody
	if !decode(w, r, &body) {
		return
	}
	if body.RequirementID != "" && s.opt.Requirements != nil {
		if req, err := s.opt.Requirements.Get(r.Context(), body.RequirementID); err != nil || req.ProjectID != p.ID {
			writeError(w, http.StatusBadRequest, "invalid_request", "No such requirement in this project.")
			return
		}
	}
	run, err := eng.Start(r.Context(), workflows.StartRequest{
		ProjectID: p.ID, WorkflowID: body.WorkflowID, Inputs: body.Inputs, RequirementID: body.RequirementID,
		Owner: c.Actor, Repo: projectRepo(p), BaseBranch: p.DefaultBranch,
	})
	if err != nil {
		workflowFail(w, err)
		return
	}
	s.note(inProject(c, p.ID), "", "workflow.run", map[string]any{"run": run.ID, "workflow": run.WorkflowID}, FeedEvent{Type: "workflow.run", Title: run.WorkflowName, Status: string(run.State)})
	writeJSON(w, http.StatusCreated, run)
}

func (s *server) getWorkflowRun(w http.ResponseWriter, r *http.Request, c Caller) {
	p, role, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	run, err := s.opt.Workflows.GetRun(r.Context(), r.PathValue("rid"))
	if errors.Is(err, workflows.ErrNotFound) || err == nil && run.ProjectID != p.ID {
		s.FailAccess(w, errNotFound)
		return
	}
	if err != nil {
		workflowFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runDetail(*run, c.Actor, role))
}

func runDetail(run workflows.Run, login, role string) WorkflowRunDetail {
	d := WorkflowRunDetail{Run: run, Gates: []WorkflowGateView{}, CanSteer: store.RoleAtLeast(role, store.RoleMember)}
	for _, st := range run.Steps {
		if st.State != workflows.NodeWaiting {
			continue
		}
		n, _ := run.Node(st.NodeID)
		d.Gates = append(d.Gates, WorkflowGateView{NodeID: n.ID, Title: gateTitle(n), Approvers: nonNil(n.Approvers), MayDecide: mayDecide(n, login, role)})
	}
	return d
}

func gateTitle(n workflows.Node) string {
	if n.Title != "" {
		return n.Title
	}
	return n.ID
}

// mayDecide: an approver of the gate, or a project owner.
func mayDecide(n workflows.Node, login, role string) bool {
	return store.RoleAtLeast(role, store.RoleMember) && (n.MayDecide(login, role) || role == store.RoleOwner)
}

// runOf loads the run of the path and checks the caller's role in its project.
func (s *server) runOf(w http.ResponseWriter, r *http.Request, c Caller, need string) (*workflows.Run, string, bool) {
	run, err := s.opt.Workflows.GetRun(r.Context(), r.PathValue("id"))
	if errors.Is(err, workflows.ErrNotFound) {
		s.FailAccess(w, errNotFound)
		return nil, "", false
	}
	if err != nil {
		workflowFail(w, err)
		return nil, "", false
	}
	role, err := s.requireProject(r.Context(), run.ProjectID, c.Actor, need)
	if err != nil {
		s.FailAccess(w, err)
		return nil, "", false
	}
	return run, role, true
}

func (s *server) runAction(action string) handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) {
		run, _, ok := s.runOf(w, r, c, store.RoleMember)
		if !ok {
			return
		}
		eng, ok := s.workflowEngine(w)
		if !ok {
			return
		}
		var body nodeBody
		if !decode(w, r, &body) {
			return
		}
		ctx := r.Context()
		var out *workflows.Run
		var err error
		switch action {
		case "pause":
			out, err = eng.Pause(ctx, run.ID, c.Actor)
		case "resume":
			out, err = eng.Resume(ctx, run.ID, c.Actor)
		case "cancel":
			out, err = eng.Cancel(ctx, run.ID, c.Actor)
		case "rerun":
			out, err = eng.Rerun(ctx, run.ID, body.NodeID, c.Actor, body.Feedback)
		case "retry":
			out, err = eng.Retry(ctx, run.ID, body.NodeID, c.Actor)
		case "takeover":
			out, err = eng.Takeover(ctx, run.ID, body.NodeID, c.Actor, body.Result, body.Outputs)
		}
		if err != nil {
			workflowFail(w, err)
			return
		}
		s.note(inProject(c, run.ProjectID), "", "workflow.run."+action, map[string]any{"run": run.ID, "node": body.NodeID}, FeedEvent{Type: "workflow.run", Title: run.WorkflowName, Status: string(out.State)})
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *server) gateAction(action string) handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) {
		run, role, ok := s.runOf(w, r, c, store.RoleMember)
		if !ok {
			return
		}
		eng, ok := s.workflowEngine(w)
		if !ok {
			return
		}
		nid := r.PathValue("nid")
		n, found := run.Node(nid)
		if !found || n.Kind != workflows.KindGate {
			writeError(w, http.StatusNotFound, "not_found", "No such gate.")
			return
		}
		if !mayDecide(n, c.Actor, role) {
			workflowFail(w, workflows.ErrForbidden)
			return
		}
		var body nodeBody
		if !decode(w, r, &body) {
			return
		}
		ctx := r.Context()
		var out *workflows.Run
		var err error
		switch action {
		case "approve":
			out, err = eng.Approve(ctx, run.ID, nid, c.Actor, body.Comment)
		case "send-back":
			out, err = eng.SendBack(ctx, run.ID, nid, c.Actor, firstNonEmpty(body.Feedback, body.Comment))
		case "add-context":
			out, err = eng.AddContext(ctx, run.ID, nid, c.Actor, firstNonEmpty(body.Text, body.Comment))
		}
		if err != nil {
			workflowFail(w, err)
			return
		}
		s.note(inProject(c, run.ProjectID), "", "workflow.gate."+action, map[string]any{"run": run.ID, "node": nid}, FeedEvent{Type: "workflow.run", Title: run.WorkflowName, Status: string(out.State)})
		writeJSON(w, http.StatusOK, out)
	}
}

// workflowChanged is the engine's observer: the live feed tells the project's members a run or a step moved, and
// that a gate waits for them.
func (s *server) workflowChanged(c workflows.Change) {
	typ := "workflow.run"
	status := string(c.Run.State)
	if c.Kind == "gate" {
		typ = "workflow.gate"
		if st := c.Run.Step(c.NodeID); st != nil {
			status = string(st.State)
		}
	}
	title := c.Run.WorkflowName
	if n, ok := c.Run.Node(c.NodeID); ok && c.Kind == "gate" {
		title += ": " + gateTitle(n)
	}
	s.hub.publish(FeedEvent{Type: typ, ProjectID: c.Run.ProjectID, Title: title, Status: status, Actor: c.Run.Owner, owner: c.Run.Owner})
	if c.Kind == "finished" && c.Run.State == workflows.RunFailed {
		slog.Info("workflows: a run failed", "run", c.Run.ID, "error", c.Run.Error)
	}
}

// RequirementText adapts a requirements store to features/workflows' Options.Requirement.
func RequirementText(rs requirements.Store) func(ctx context.Context, projectID, id string) (string, string, bool) {
	return func(ctx context.Context, projectID, id string) (string, string, bool) {
		if rs == nil {
			return "", "", false
		}
		r, err := rs.Get(ctx, id)
		if err != nil || r.ProjectID != projectID {
			return "", "", false
		}
		return r.Title, r.Body, true
	}
}
