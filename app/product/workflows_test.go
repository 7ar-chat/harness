package product

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/adapters/store/workflowstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/workflows"
	wfengine "github.com/7ar-chat/harness/features/workflows"
)

// wfTasks is a task service whose tasks finish when the test says so.
type wfTasks struct {
	mu    sync.Mutex
	tasks map[string]*factory.Task
	n     int
}

func (f *wfTasks) Create(_ context.Context, t factory.Task) (*factory.Task, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.tasks {
		if x.Source.Key == t.Source.Key {
			c := *x
			return &c, false, nil
		}
	}
	f.n++
	t.ID, t.State = fmt.Sprintf("t%d", f.n), factory.StateReady
	f.tasks[t.ID] = &t
	c := t
	return &c, true, nil
}

func (f *wfTasks) Get(_ context.Context, id string) (*factory.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return nil, factory.ErrNotFound
	}
	c := *t
	return &c, nil
}

func (f *wfTasks) Cancel(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tasks[id]; ok && !t.State.Terminal() {
		t.State = factory.StateCancelled
	}
	return nil
}

func (f *wfTasks) finish(node, result string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tasks {
		for _, l := range t.Labels {
			if l == factory.LabelWorkflowNode+node && t.State == factory.StateReady {
				t.State, t.Result = factory.StateDone, result
			}
		}
	}
}

type wfRig struct {
	*rig
	tasks   *wfTasks
	eng     *wfengine.Engine
	store   *workflowstore.Mem
	project string
}

// newWfRig: alice owns the project, bob is a member, vic a viewer, carol is outside. withEngine false leaves the
// engine out (the factory is off).
func newWfRig(t *testing.T, withEngine bool) *wfRig {
	t.Helper()
	tm := teamstore.NewMem()
	for _, l := range []string{"alice", "bob", "vic", "carol"} {
		if _, err := tm.UpsertUser(context.Background(), store.User{Login: l, Name: l, Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	wr := &wfRig{store: workflowstore.NewMem(nil), tasks: &wfTasks{tasks: map[string]*factory.Task{}}}
	prod := Options{Team: tm, Workflows: wr.store}
	if withEngine {
		wr.eng = wfengine.New(wfengine.Options{Store: wr.store, Tasks: wr.tasks})
		prod.WorkflowEngine = wr.eng
	}
	model := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("ok")})
	wr.rig = newRig(t, rigOpts{team: true, model: model, prod: prod})
	wr.project = wr.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	wr.json("PUT", "/api/projects/"+wr.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	wr.json("PUT", "/api/projects/"+wr.project+"/members/vic", "alice", `{"role":"viewer"}`, 204)
	return wr
}

func (r *wfRig) base() string { return "/api/projects/" + r.project + "/workflows" }

func (r *wfRig) call(method, path, user, body string) (int, string) {
	r.t.Helper()
	resp := r.do(method, path, user, body)
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *wfRig) into(method, path, user, body string, want int, out any) {
	r.t.Helper()
	code, raw := r.call(method, path, user, body)
	if code != want {
		r.t.Fatalf("%s %s as %s: status %d, want %d: %s", method, path, user, code, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(raw), out); err != nil {
			r.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
}

const gatedBody = `{"name":"Delivery","description":"d","inputs":[{"name":"task","required":true}],"nodes":[
 {"id":"plan","kind":"agent","role":"planner","prompt":"plan {{inputs.task}}","pos":{"x":0,"y":0}},
 {"id":"review","kind":"gate","dependsOn":["plan"],"approvers":["role:member"],"pos":{"x":200,"y":0}},
 {"id":"build","kind":"agent","dependsOn":["review"],"prompt":"build {{nodes.review.result}}","pos":{"x":400,"y":0}}]}`

// withVersion adds the version the editor started from to a JSON body.
func withVersion(body string, v int) string {
	return strings.TrimSuffix(strings.TrimSpace(body), "}") + fmt.Sprintf(`,"version":%d}`, v)
}

func (r *wfRig) published(name string) workflows.Workflow {
	r.t.Helper()
	var w workflows.Workflow
	r.into("POST", r.base(), "bob", strings.Replace(gatedBody, "Delivery", name, 1), 201, &w)
	r.into("POST", r.base()+"/"+w.ID+"/publish", "bob", `{}`, 200, &w)
	return w
}

func TestWorkflowTemplatesAreBuiltinAndValid(t *testing.T) {
	r := newWfRig(t, true)
	var ts []workflows.Template
	r.into("GET", "/api/workflow-templates", "vic", "", 200, &ts)
	if len(ts) != 4 {
		t.Fatalf("templates = %d, want 4", len(ts))
	}
	var ids []string
	for _, x := range ts {
		ids = append(ids, x.ID)
	}
	if got := strings.Join(ids, ","); got != "backend-delivery,frontend-delivery,product-proposal,bug-fix" {
		t.Fatalf("template ids = %s", got)
	}
}

func TestWorkflowCrudAccessAndStaleVersion(t *testing.T) {
	// Arrange
	r := newWfRig(t, true)
	var w workflows.Workflow

	// Act and assert: a member creates a draft, a viewer cannot, an outsider sees nothing
	r.into("POST", r.base(), "vic", gatedBody, 403, nil)
	r.into("POST", r.base(), "carol", gatedBody, 404, nil)
	r.into("POST", r.base(), "bob", gatedBody, 201, &w)
	if w.Status != workflows.StatusDraft || w.Version != 1 || w.CreatedBy != "bob" || w.ProjectID != r.project {
		t.Fatalf("created = %+v", w)
	}
	r.into("POST", r.base(), "bob", gatedBody, 409, nil) // the name is taken
	var list []workflows.Workflow
	r.into("GET", r.base(), "vic", "", 200, &list)
	if len(list) != 1 {
		t.Fatalf("viewer's list = %d", len(list))
	}
	r.into("GET", r.base(), "carol", "", 404, nil)

	// Update bumps the version and keeps the old one; a stale editor gets 409
	var u workflows.Workflow
	r.into("PUT", r.base()+"/"+w.ID, "alice", withVersion(strings.Replace(gatedBody, `"d"`, `"edited"`, 1), 1), 200, &u)
	if u.Version != 2 || u.Description != "edited" || u.UpdatedBy != "alice" {
		t.Fatalf("updated = %+v", u)
	}
	r.into("PUT", r.base()+"/"+w.ID, "bob", withVersion(gatedBody, 1), 409, nil)

	// History and restore
	var hist []workflows.Version
	r.into("GET", r.base()+"/"+w.ID+"/history", "vic", "", 200, &hist)
	if len(hist) != 1 || hist[0].Version != 1 || hist[0].Workflow.Description != "d" {
		t.Fatalf("history = %+v", hist)
	}
	var rest workflows.Workflow
	r.into("POST", r.base()+"/"+w.ID+"/restore", "bob", `{"version":1}`, 200, &rest)
	if rest.Description != "d" || rest.Version != 3 || rest.Status != workflows.StatusDraft {
		t.Fatalf("restored = %+v", rest)
	}
	r.into("POST", r.base()+"/"+w.ID+"/restore", "bob", `{"version":99}`, 404, nil)

	// Duplicate and delete
	var dup workflows.Workflow
	r.into("POST", r.base()+"/"+w.ID+"/duplicate", "bob", `{}`, 201, &dup)
	if dup.ID == w.ID || dup.Name != "Delivery copy" || dup.Status != workflows.StatusDraft {
		t.Fatalf("duplicate = %+v", dup)
	}
	r.into("DELETE", r.base()+"/"+dup.ID, "vic", "", 403, nil)
	r.into("DELETE", r.base()+"/"+dup.ID, "bob", "", 204, nil)
	r.into("GET", r.base()+"/"+dup.ID, "bob", "", 404, nil)
}

func TestWorkflowSaveValidatesTheGraph(t *testing.T) {
	r := newWfRig(t, true)
	cases := []struct{ name, body, want string }{
		{"cycle", `{"name":"x","nodes":[{"id":"a","kind":"agent","prompt":"p","dependsOn":["b"]},{"id":"b","kind":"agent","prompt":"p","dependsOn":["a"]}]}`, "cycle"},
		{"missing ref", `{"name":"x","nodes":[{"id":"a","kind":"agent","prompt":"p","dependsOn":["zzz"]}]}`, "does not exist"},
		{"unknown role", `{"name":"x","nodes":[{"id":"a","kind":"agent","prompt":"p","role":"wizard"}]}`, `wizard`},
		{"gate without approvers", `{"name":"x","nodes":[{"id":"a","kind":"agent","prompt":"p"},{"id":"g","kind":"gate","dependsOn":["a"]}]}`, "at least one approver"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			code, raw := r.call("POST", r.base(), "bob", c.body)

			// Assert
			if code != 400 || !strings.Contains(raw, c.want) || !strings.Contains(raw, `"issues"`) || !strings.Contains(raw, "invalid_workflow") {
				t.Fatalf("status %d: %s", code, raw)
			}
			var iss WorkflowIssues
			r.into("POST", r.base()+"/validate", "bob", c.body, 200, &iss)
			if len(iss.Issues) == 0 {
				t.Fatal("validate reported nothing")
			}
		})
	}
	var ok WorkflowIssues
	r.into("POST", r.base()+"/validate", "bob", gatedBody, 200, &ok)
	if len(ok.Issues) != 0 {
		t.Fatalf("a valid graph has issues: %+v", ok.Issues)
	}
}

func TestWorkflowFromTemplatePublishAndRun(t *testing.T) {
	// Arrange
	r := newWfRig(t, true)

	// Act: copy a template (twice: the name is made unique), it is a draft that cannot run
	var w, w2 workflows.Workflow
	r.into("POST", r.base()+"/from-template", "bob", `{"template":"bug-fix"}`, 201, &w)
	r.into("POST", r.base()+"/from-template", "bob", `{"template":"bug-fix"}`, 201, &w2)
	r.into("POST", r.base()+"/from-template", "bob", `{"template":"nope"}`, 404, nil)
	if w.Template != "bug-fix" || w.Status != workflows.StatusDraft || w2.Name != "Bug fix (2)" {
		t.Fatalf("copies = %+v / %q", w, w2.Name)
	}
	r.into("POST", "/api/projects/"+r.project+"/workflow-runs", "bob", `{"workflowId":"`+w.ID+`","inputs":{"bug":"x"}}`, 409, nil)

	// Publish, then run
	var pub workflows.Workflow
	r.into("POST", r.base()+"/"+w.ID+"/publish", "vic", `{}`, 403, nil)
	r.into("POST", r.base()+"/"+w.ID+"/publish", "bob", `{}`, 200, &pub)
	if pub.Status != workflows.StatusPublished || pub.PublishedVersion != pub.Version {
		t.Fatalf("published = %+v", pub)
	}
	var run workflows.Run
	r.into("POST", "/api/projects/"+r.project+"/workflow-runs", "bob", `{"workflowId":"`+w.ID+`"}`, 400, nil) // the bug input is required
	r.into("POST", "/api/projects/"+r.project+"/workflow-runs", "vic", `{"workflowId":"`+w.ID+`","inputs":{"bug":"x"}}`, 403, nil)
	r.into("POST", "/api/projects/"+r.project+"/workflow-runs", "bob", `{"workflowId":"`+w.ID+`","inputs":{"bug":"it crashes"}}`, 201, &run)
	if run.State != workflows.RunRunning || run.Owner != "bob" || run.Step("reproduce").State != workflows.NodeRunning {
		t.Fatalf("run = %+v", run)
	}

	// Editing a published workflow makes it a draft again; the run keeps its snapshot
	r.into("PUT", r.base()+"/"+w.ID, "bob", `{"name":"Bug fix","nodes":[{"id":"a","kind":"agent","prompt":"x"}]}`, 200, &w)
	if w.Status != workflows.StatusDraft {
		t.Fatalf("edited = %+v", w)
	}
	var detail WorkflowRunDetail
	r.into("GET", "/api/projects/"+r.project+"/workflow-runs/"+run.ID, "vic", "", 200, &detail)
	if len(detail.Run.Graph) != 4 || detail.CanSteer {
		t.Fatalf("the run lost its snapshot or a viewer can steer: %+v", detail)
	}
	r.into("GET", "/api/projects/"+r.project+"/workflow-runs/"+run.ID, "carol", "", 404, nil)
	var runs []workflows.Run
	r.into("GET", "/api/projects/"+r.project+"/workflow-runs?state=running", "vic", "", 200, &runs)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
}

func TestWorkflowGateIsDecidedByApproversAndShownInTheInbox(t *testing.T) {
	// Arrange
	r := newWfRig(t, true)
	w := r.published("Gated")
	var run workflows.Run
	r.into("POST", "/api/projects/"+r.project+"/workflow-runs", "bob", `{"workflowId":"`+w.ID+`","inputs":{"task":"ship"}}`, 201, &run)
	r.tasks.finish("plan", "the plan")
	if _, err := r.eng.Advance(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}

	// Assert: the gate is in the inbox of members and owners, not of the viewer or the outsider
	inbox := func(user string) InboxDoc2 {
		var d InboxDoc2
		r.into("GET", "/api/needs-review", user, "", 200, &d)
		return d
	}
	for _, u := range []string{"alice", "bob"} {
		d := inbox(u)
		if len(d.Gates) != 1 || d.Count != 1 || d.Gates[0].RunID != run.ID || d.Gates[0].NodeID != "review" || d.Gates[0].Summary != "the plan" || d.Gates[0].WorkflowName != "Gated" {
			t.Fatalf("%s inbox = %+v", u, d)
		}
	}
	for _, u := range []string{"vic", "carol"} {
		if d := inbox(u); len(d.Gates) != 0 {
			t.Fatalf("%s sees a gate: %+v", u, d.Gates)
		}
	}
	var detail WorkflowRunDetail
	r.into("GET", "/api/projects/"+r.project+"/workflow-runs/"+run.ID, "bob", "", 200, &detail)
	if len(detail.Gates) != 1 || !detail.Gates[0].MayDecide || !detail.CanSteer {
		t.Fatalf("detail = %+v", detail)
	}
	r.into("GET", "/api/projects/"+r.project+"/workflow-runs/"+run.ID, "vic", "", 200, &detail)
	if detail.Gates[0].MayDecide {
		t.Fatal("a viewer may not decide")
	}

	// Act: a viewer is refused, an empty send-back is refused, context keeps it waiting, approval moves on
	base := "/api/workflow-runs/" + run.ID + "/nodes/review/"
	r.into("POST", base+"approve", "vic", `{}`, 403, nil)
	r.into("POST", base+"approve", "carol", `{}`, 404, nil)
	r.into("POST", "/api/workflow-runs/"+run.ID+"/nodes/plan/approve", "bob", `{}`, 404, nil) // not a gate
	r.into("POST", base+"send-back", "bob", `{}`, 400, nil)
	r.into("POST", base+"add-context", "bob", `{"text":"check the migration"}`, 200, &run)
	if run.Step("review").State != workflows.NodeWaiting || len(run.Step("review").Context) != 1 {
		t.Fatalf("gate = %+v", run.Step("review"))
	}
	r.into("POST", base+"approve", "bob", `{"comment":"ship it"}`, 200, &run)

	// Assert
	if run.Step("review").State != workflows.NodeDone || run.Step("build").State != workflows.NodeRunning {
		t.Fatalf("after approval: %+v", run.Steps)
	}
	if d := inbox("bob"); len(d.Gates) != 0 || d.Count != 0 {
		t.Fatalf("the approved gate is still in the inbox: %+v", d)
	}
	r.into("POST", base+"approve", "bob", `{}`, 409, nil)
}

func TestWorkflowGateSendBackReturnsTheRun(t *testing.T) {
	// Arrange
	r := newWfRig(t, true)
	w := r.published("Gated")
	var run workflows.Run
	r.into("POST", "/api/projects/"+r.project+"/workflow-runs", "bob", `{"workflowId":"`+w.ID+`","inputs":{"task":"ship"}}`, 201, &run)
	r.tasks.finish("plan", "weak plan")
	if _, err := r.eng.Advance(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}

	// Act
	r.into("POST", "/api/workflow-runs/"+run.ID+"/nodes/review/send-back", "alice", `{"feedback":"add a rollback plan"}`, 200, &run)

	// Assert
	if run.Step("plan").State != workflows.NodeRunning || run.Step("plan").Round != 2 || run.Step("review").State != workflows.NodePending {
		t.Fatalf("steps = %+v", run.Steps)
	}
}

func TestWorkflowRunControlsOverHTTP(t *testing.T) {
	// Arrange
	r := newWfRig(t, true)
	w := r.published("Gated")
	var run workflows.Run
	r.into("POST", "/api/projects/"+r.project+"/workflow-runs", "bob", `{"workflowId":"`+w.ID+`","inputs":{"task":"ship"}}`, 201, &run)
	ctl := func(user, action, body string, want int) {
		var out any = &run
		if want != 200 {
			out = nil
		}
		r.into("POST", "/api/workflow-runs/"+run.ID+"/"+action, user, body, want, out)
	}

	// Act and assert
	ctl("vic", "pause", `{}`, 403)
	ctl("carol", "pause", `{}`, 404)
	ctl("bob", "pause", `{}`, 200)
	if run.State != workflows.RunPaused {
		t.Fatalf("state = %s", run.State)
	}
	ctl("bob", "pause", `{}`, 409)
	ctl("bob", "resume", `{}`, 200)
	ctl("bob", "takeover", `{"nodeId":"plan","result":"I wrote the plan"}`, 200)
	if !run.Step("plan").Manual || run.Step("review").State != workflows.NodeWaiting {
		t.Fatalf("after takeover: %+v", run.Steps)
	}
	ctl("bob", "takeover", `{"nodeId":"plan","result":""}`, 409) // already done
	ctl("bob", "rerun", `{"nodeId":"plan","feedback":"again"}`, 200)
	if run.Step("plan").State != workflows.NodeRunning || run.Step("plan").Round != 2 || run.Step("review").State != workflows.NodePending {
		t.Fatalf("after rerun: %+v", run.Steps)
	}
	ctl("bob", "rerun", `{"nodeId":"ghost"}`, 404)
	ctl("bob", "retry", `{"nodeId":"plan"}`, 409) // it is running, not failed
	ctl("bob", "cancel", `{}`, 200)
	if run.State != workflows.RunCancelled {
		t.Fatalf("state = %s", run.State)
	}
}

func TestWorkflowRunsNeedTheEngine(t *testing.T) {
	r := newWfRig(t, false)
	var w workflows.Workflow
	r.into("POST", r.base(), "bob", gatedBody, 201, &w)
	r.into("POST", r.base()+"/"+w.ID+"/publish", "bob", `{}`, 200, &w)
	code, raw := r.call("POST", "/api/projects/"+r.project+"/workflow-runs", "bob", `{"workflowId":"`+w.ID+`","inputs":{"task":"x"}}`)
	if code != 501 {
		t.Fatalf("status %d: %s", code, raw)
	}
}

// InboxDoc2 is the part of the inbox answer the tests read.
type InboxDoc2 struct {
	Gates []inboxGate `json:"gates"`
	Count int         `json:"count"`
}
