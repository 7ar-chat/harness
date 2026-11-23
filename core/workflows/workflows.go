// Package workflows is the vocabulary of workflows: the procedures (SOPs) of a project as a versioned graph of
// steps, and the runs that carry one out. A step is an agent (a role and a prompt), a script (a command), a human
// gate, another workflow, or a build or preview. A run turns each step into a factory task, in dependency order.
// The package holds the types, the validation of a graph, the templating of prompts, the builtin templates and
// the ports (Store, Engine); features/workflows implements the engine, adapters/store/workflowstore the
// persistence and the product serves them.
package workflows

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

// Kind is what a node does.
type Kind string

// Node kinds. Build and Preview run like a script (a command that must exit 0); they differ only in what they
// say about the step.
const (
	KindAgent       Kind = "agent"
	KindScript      Kind = "script"
	KindGate        Kind = "gate"
	KindSubworkflow Kind = "subworkflow"
	KindBuild       Kind = "build"
	KindPreview     Kind = "preview"
)

// Valid reports whether k is a kind of node.
func (k Kind) Valid() bool {
	switch k {
	case KindAgent, KindScript, KindGate, KindSubworkflow, KindBuild, KindPreview:
		return true
	}
	return false
}

// Command reports whether the node runs a shell command.
func (k Kind) Command() bool { return k == KindScript || k == KindBuild || k == KindPreview }

// Failure actions (OnFailure.Action).
const (
	// FailStop fails the run (the default).
	FailStop = "fail"
	// FailContinue lets the steps that depend on the node go on.
	FailContinue = "continue"
	// FailRework sends the run back to OnFailure.Target with the failure text, MaxRounds times.
	FailRework = "rework"
)

// Workflow states.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
)

// Limits.
const (
	MaxNodes   = 60
	MaxPrompt  = 20000
	MaxName    = 80
	MaxHistory = 10
	// DefaultRounds is the number of rework rounds of an edge that names none.
	DefaultRounds = 3
	// MaxDepth bounds the nesting of subworkflows.
	MaxDepth = 4
)

// Errors of the ports.
var (
	ErrNotFound = errors.New("workflows: not found")
	// ErrConflict: the name is taken in the project.
	ErrConflict = errors.New("workflows: name already in use")
	// ErrStale: the workflow or run changed since the version the caller read.
	ErrStale = errors.New("workflows: changed meanwhile")
	// ErrInvalid wraps a rejected input; a graph problem is an *InvalidError carrying the issues.
	ErrInvalid = errors.New("workflows: invalid")
	// ErrState: the action does not apply in the state the run or node is in.
	ErrState = errors.New("workflows: not possible in this state")
	// ErrForbidden: the caller may not decide this gate.
	ErrForbidden = errors.New("workflows: not allowed")
)

// Pos is where the designer draws a node.
type Pos struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// OnFailure says what happens when a node fails (and, for a gate, where a send-back goes).
type OnFailure struct {
	// Action is FailStop (also ""), FailContinue or FailRework.
	Action string `json:"action,omitempty"`
	// Target is the earlier node a rework goes back to.
	Target string `json:"target,omitempty"`
	// MaxRounds caps how many times the failure is sent back (0: DefaultRounds).
	MaxRounds int `json:"maxRounds,omitempty"`
}

// Node is one step of a workflow.
type Node struct {
	ID        string   `json:"id"`
	Kind      Kind     `json:"kind"`
	Title     string   `json:"title,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
	Pos       Pos      `json:"pos"`

	// Agent: Role is a role of the registry or a builtin one ("" is coder); Prompt is the task the agent gets,
	// with {{inputs.name}}, {{nodes.id.result}}, {{nodes.id.outputs.name}}, {{requirement.title}} and
	// {{requirement.body}} filled in. Model, Policy and Budget override the defaults of the task.
	Role   string         `json:"role,omitempty"`
	Prompt string         `json:"prompt,omitempty"`
	Model  string         `json:"model,omitempty"`
	Policy string         `json:"policy,omitempty"`
	Budget factory.Budget `json:"budget,omitzero"`
	// Outputs are the artifacts the step declares: the agent ends its report with a "### name" section for each.
	Outputs []string `json:"outputs,omitempty"`
	// UseRepo works in the run's repository on the run's branch. WaitMerge makes the step wait until the pull
	// request is merged; by default it is done once the pull request is open.
	UseRepo   bool `json:"useRepo,omitempty"`
	WaitMerge bool `json:"waitMerge,omitempty"`

	// Script, build, preview: Command succeeds when it exits 0.
	Command string `json:"command,omitempty"`

	// Gate: Approvers are logins or "role:member" / "role:owner" (any project member of that level).
	Approvers []string `json:"approvers,omitempty"`

	// Subworkflow: the workflow to run (of the same project) and its inputs (templated like a prompt).
	WorkflowID string            `json:"workflowId,omitempty"`
	Inputs     map[string]string `json:"inputs,omitempty"`

	OnFailure *OnFailure `json:"onFailure,omitempty"`
}

// MayDecide reports whether login, who has projectRole in the project, is an approver of the gate.
func (n Node) MayDecide(login, projectRole string) bool {
	for _, a := range n.Approvers {
		switch {
		case a == login:
			return true
		case strings.HasPrefix(a, "role:"):
			want := strings.TrimPrefix(a, "role:")
			if want == projectRole || want == "member" && projectRole == "owner" {
				return true
			}
		}
	}
	return false
}

// Input is a value a run is started with.
type Input struct {
	Name     string `json:"name"`
	Label    string `json:"label,omitempty"`
	Required bool   `json:"required,omitempty"`
	Default  string `json:"default,omitempty"`
	// Long asks for a text area.
	Long bool `json:"long,omitempty"`
}

// Workflow is a procedure of a project.
type Workflow struct {
	ID          string  `json:"id"`
	ProjectID   string  `json:"projectId"`
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Inputs      []Input `json:"inputs,omitempty"`
	Nodes       []Node  `json:"nodes"`
	// Status is StatusDraft or StatusPublished; only a published workflow can be run, and a change makes it a
	// draft again until it is published.
	Status string `json:"status"`
	// Version starts at 1 and grows with every change; PublishedVersion is the version last published (0: never).
	Version          int `json:"version"`
	PublishedVersion int `json:"publishedVersion"`
	// Template is the builtin template the workflow was copied from.
	Template  string    `json:"template,omitempty"`
	CreatedBy string    `json:"createdBy,omitempty"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Node returns the node called id.
func (w Workflow) Node(id string) (Node, bool) { return findNode(w.Nodes, id) }

func findNode(ns []Node, id string) (Node, bool) {
	for _, n := range ns {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// Version is a past state of a workflow, kept when it was replaced.
type Version struct {
	Version   int       `json:"version"`
	Workflow  Workflow  `json:"workflow"`
	ChangedBy string    `json:"changedBy,omitempty"`
	At        time.Time `json:"at"`
}

// RunState is where a run is.
type RunState string

// Run states.
const (
	RunRunning   RunState = "running"
	RunPaused    RunState = "paused"
	RunDone      RunState = "done"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"
)

// Terminal reports whether the run is over.
func (s RunState) Terminal() bool { return s == RunDone || s == RunFailed || s == RunCancelled }

// NodeState is where a step of a run is.
type NodeState string

// Step states. Waiting is a gate that needs a decision.
const (
	NodePending   NodeState = "pending"
	NodeRunning   NodeState = "running"
	NodeWaiting   NodeState = "waiting"
	NodeDone      NodeState = "done"
	NodeFailed    NodeState = "failed"
	NodeSkipped   NodeState = "skipped"
	NodeCancelled NodeState = "cancelled"
)

// Settled reports whether the step will not change by itself.
func (s NodeState) Settled() bool {
	return s == NodeDone || s == NodeFailed || s == NodeSkipped || s == NodeCancelled
}

// NodeRun is the state of one node in a run.
type NodeRun struct {
	NodeID string    `json:"nodeId"`
	State  NodeState `json:"state"`
	// Round counts how many times the step was started (a rework or a rerun starts it again).
	Round int `json:"round"`
	// TaskID is the factory task of the current round; ChildRunID the run of a subworkflow.
	TaskID     string `json:"taskId,omitempty"`
	ChildRunID string `json:"childRunId,omitempty"`
	// TaskState is the state of the task as last seen (a blocked task shows its reason in Note).
	TaskState string            `json:"taskState,omitempty"`
	Result    string            `json:"result,omitempty"`
	Error     string            `json:"error,omitempty"`
	Outputs   map[string]string `json:"outputs,omitempty"`
	PRURL     string            `json:"prUrl,omitempty"`
	// Feedback is what the next start of the step must address (a failure sent back, a reviewer's remarks).
	Feedback string `json:"feedback,omitempty"`
	// Context are the notes people added to a gate.
	Context []string `json:"context,omitempty"`
	// Manual: a person took the step over and gave its result.
	Manual     bool      `json:"manual,omitempty"`
	Note       string    `json:"note,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// RunEvent is one line of a run's log.
type RunEvent struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	NodeID string    `json:"nodeId,omitempty"`
	Actor  string    `json:"actor,omitempty"`
	Text   string    `json:"text"`
}

// MaxEvents bounds a run's log; the oldest lines are dropped.
const MaxEvents = 300

// Run is one execution of a workflow.
type Run struct {
	ID              string `json:"id"`
	ProjectID       string `json:"projectId"`
	WorkflowID      string `json:"workflowId"`
	WorkflowName    string `json:"workflowName"`
	WorkflowVersion int    `json:"workflowVersion"`
	// Graph is the workflow as it was when the run started: later edits do not change a run.
	Graph         []Node            `json:"graph"`
	Inputs        map[string]string `json:"inputs,omitempty"`
	RequirementID string            `json:"requirementId,omitempty"`
	Owner         string            `json:"owner"`
	Repo          string            `json:"repo,omitempty"`
	BaseBranch    string            `json:"baseBranch,omitempty"`
	// Branch is shared by the steps that use the repository.
	Branch string   `json:"branch,omitempty"`
	State  RunState `json:"state"`
	Error  string   `json:"error,omitempty"`
	// Steps has one entry per node of Graph, in its order.
	Steps []NodeRun `json:"steps"`
	// Rework counts the rework rounds used per failing node.
	Rework map[string]int `json:"rework,omitempty"`
	Events []RunEvent     `json:"events,omitempty"`
	// ParentRunID and ParentNodeID say which step of which run started this one (a subworkflow).
	ParentRunID  string `json:"parentRunId,omitempty"`
	ParentNodeID string `json:"parentNodeId,omitempty"`
	ParentRound  int    `json:"parentRound,omitempty"`
	// Version is the optimistic-lock counter of the stored run.
	Version    int       `json:"version"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// Step returns the state of the node id.
func (r *Run) Step(id string) *NodeRun {
	for i := range r.Steps {
		if r.Steps[i].NodeID == id {
			return &r.Steps[i]
		}
	}
	return nil
}

// Node returns the node id of the run's graph.
func (r *Run) Node(id string) (Node, bool) { return findNode(r.Graph, id) }

// RunFilter selects runs (all fields optional, ANDed).
type RunFilter struct {
	ProjectID   string
	WorkflowID  string
	ParentRunID string
	States      []RunState
	Limit       int // 0: 100
}

// Store persists workflows (with the last MaxHistory versions of each) and runs. Safe for concurrent use.
type Store interface {
	// Create adds w (the ID is set when empty) at version 1. ErrConflict when the name is taken in the project.
	Create(ctx context.Context, w Workflow, actor string) (*Workflow, error)
	// Update replaces the content of workflow w.ID: the previous state goes to the history, the version grows.
	// A non-zero w.Version must be the current one (ErrStale). Status and PublishedVersion are taken from w.
	Update(ctx context.Context, w Workflow, actor string) (*Workflow, error)
	Get(ctx context.Context, id string) (*Workflow, error)
	List(ctx context.Context, projectID string) ([]Workflow, error)
	// History returns the past versions, newest first (ErrNotFound for an unknown workflow).
	History(ctx context.Context, id string) ([]Version, error)
	Delete(ctx context.Context, id string) error
	// DeleteProject removes the workflows and runs of a project (a deleted project).
	DeleteProject(ctx context.Context, projectID string) error

	// CreateRun stores r (the ID is set when empty) at version 1.
	CreateRun(ctx context.Context, r Run) (*Run, error)
	GetRun(ctx context.Context, id string) (*Run, error)
	// ListRuns returns the runs newest first.
	ListRuns(ctx context.Context, f RunFilter) ([]Run, error)
	// UpdateRun replaces the run when r.Version is the stored one (ErrStale otherwise) and bumps it.
	UpdateRun(ctx context.Context, r Run) (*Run, error)
}

// StartRequest is what starts a run.
type StartRequest struct {
	ProjectID     string
	WorkflowID    string
	Inputs        map[string]string
	RequirementID string
	Owner         string
	Repo          string
	BaseBranch    string
	// ParentRunID, ParentNodeID and ParentRound are set by the engine for a subworkflow.
	ParentRunID  string
	ParentNodeID string
	ParentRound  int
}

// Change is what an Engine reports to its observers.
type Change struct {
	Run    Run
	Kind   string // started, step, gate, finished, control
	NodeID string
}

// Engine drives runs. The product serves it over HTTP; the host builds it (features/workflows).
type Engine interface {
	Start(ctx context.Context, req StartRequest) (*Run, error)
	Pause(ctx context.Context, runID, actor string) (*Run, error)
	Resume(ctx context.Context, runID, actor string) (*Run, error)
	Cancel(ctx context.Context, runID, actor string) (*Run, error)
	// Rerun starts the run again from a node: it and everything downstream of it is invalidated.
	Rerun(ctx context.Context, runID, nodeID, actor, feedback string) (*Run, error)
	// Retry starts a failed or cancelled step again.
	Retry(ctx context.Context, runID, nodeID, actor string) (*Run, error)
	// Takeover completes a step by hand with the person's result.
	Takeover(ctx context.Context, runID, nodeID, actor, result string, outputs map[string]string) (*Run, error)
	// Approve, SendBack and AddContext are the decisions on a gate. The caller has checked who may decide.
	Approve(ctx context.Context, runID, nodeID, actor, comment string) (*Run, error)
	SendBack(ctx context.Context, runID, nodeID, actor, feedback string) (*Run, error)
	AddContext(ctx context.Context, runID, nodeID, actor, text string) (*Run, error)
	// TaskMoved is the factory's OnTaskState hook: it advances the run a task belongs to.
	TaskMoved(t factory.Task)
	// Observe registers a function told of every change (it must not block).
	Observe(fn func(Change))
}

// ---- templating ----

var refRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.\-]+)\s*\}\}`)

// Refs returns the variables a text refers to ("inputs.name", "nodes.plan.result").
func Refs(s string) []string {
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// Render replaces every {{variable}} of s with lookup's value; an unknown variable becomes empty.
func Render(s string, lookup func(name string) (string, bool)) string {
	return refRe.ReplaceAllStringFunc(s, func(m string) string {
		v, _ := lookup(refRe.FindStringSubmatch(m)[1])
		return v
	})
}

// ---- graph ----

var idRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ValidID reports whether s can be a node id.
func ValidID(s string) bool { return idRe.MatchString(s) }

// Ancestors returns every node id the node id depends on, directly or not.
func Ancestors(ns []Node, id string) []string {
	by := map[string]Node{}
	for _, n := range ns {
		by[n.ID] = n
	}
	var out []string
	seen := map[string]bool{}
	var walk func(string)
	walk = func(x string) {
		for _, d := range by[x].DependsOn {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
				walk(d)
			}
		}
	}
	walk(id)
	return out
}

// Downstream returns the ids of the nodes that depend on id, directly or not.
func Downstream(ns []Node, id string) []string {
	var out []string
	for _, n := range ns {
		if n.ID != id && slices.Contains(Ancestors(ns, n.ID), id) {
			out = append(out, n.ID)
		}
	}
	return out
}

// Issue is one problem of a graph.
type Issue struct {
	NodeID  string `json:"nodeId,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// InvalidError is a rejected workflow with its issues; errors.Is(err, ErrInvalid) is true.
type InvalidError struct{ Issues []Issue }

func (e *InvalidError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, is := range e.Issues {
		if is.NodeID != "" {
			parts[i] = is.NodeID + ": " + is.Message
		} else {
			parts[i] = is.Message
		}
	}
	return ErrInvalid.Error() + ": " + strings.Join(parts, "; ")
}

// Is implements errors.Is.
func (e *InvalidError) Is(t error) bool { return t == ErrInvalid }

// Env is what Validate checks references against; a nil function accepts everything.
type Env struct {
	// RoleExists reports whether a role name resolves (a builtin one, or one of the project).
	RoleExists func(name string) bool
	// Workflow returns another workflow of the project.
	Workflow func(id string) (*Workflow, bool)
}

// Validate normalizes w (trims text, drops empty entries) and returns its problems; none means it can be stored
// and published.
func (w *Workflow) Validate(env Env) []Issue {
	var out []Issue
	add := func(node, field, msg string) { out = append(out, Issue{NodeID: node, Field: field, Message: msg}) }
	w.Name = strings.TrimSpace(w.Name)
	w.Description = strings.TrimSpace(w.Description)
	switch {
	case w.Name == "":
		add("", "name", "the workflow needs a name")
	case len([]rune(w.Name)) > MaxName:
		add("", "name", "the name is too long")
	}
	if len(w.Nodes) == 0 {
		add("", "nodes", "the workflow has no steps")
	}
	if len(w.Nodes) > MaxNodes {
		add("", "nodes", fmt.Sprintf("a workflow has at most %d steps", MaxNodes))
		return out
	}
	inputs := map[string]bool{}
	for i := range w.Inputs {
		in := &w.Inputs[i]
		in.Name = strings.TrimSpace(in.Name)
		if !ValidID(in.Name) {
			add("", "inputs", fmt.Sprintf("input name %q must be a lower case slug", in.Name))
		}
		inputs[in.Name] = true
	}
	ids := map[string]int{}
	for _, n := range w.Nodes {
		ids[n.ID]++
	}
	for i := range w.Nodes {
		n := &w.Nodes[i]
		n.Title = strings.TrimSpace(n.Title)
		n.Prompt = strings.TrimSpace(n.Prompt)
		n.Command = strings.TrimSpace(n.Command)
		n.Role = strings.TrimSpace(n.Role)
		n.DependsOn = cleanList(n.DependsOn)
		n.Approvers = cleanList(n.Approvers)
		n.Outputs = cleanList(n.Outputs)
		if !ValidID(n.ID) {
			add(n.ID, "id", "the id must be a lower case slug of at most 32 characters")
			continue
		}
		if ids[n.ID] > 1 {
			add(n.ID, "id", "two steps have this id")
		}
		if !n.Kind.Valid() {
			add(n.ID, "kind", fmt.Sprintf("unknown kind %q", n.Kind))
			continue
		}
		for _, d := range n.DependsOn {
			if d == n.ID {
				add(n.ID, "dependsOn", "a step cannot depend on itself")
			} else if ids[d] == 0 {
				add(n.ID, "dependsOn", fmt.Sprintf("depends on %q, which does not exist", d))
			}
		}
		switch {
		case n.Kind == KindAgent:
			if n.Prompt == "" {
				add(n.ID, "prompt", "the agent needs a prompt")
			}
			if len([]rune(n.Prompt)) > MaxPrompt {
				add(n.ID, "prompt", "the prompt is too long")
			}
			if n.Role != "" && env.RoleExists != nil && !env.RoleExists(n.Role) {
				add(n.ID, "role", fmt.Sprintf("the role %q does not exist", n.Role))
			}
			for _, o := range n.Outputs {
				if !ValidID(o) {
					add(n.ID, "outputs", fmt.Sprintf("output name %q must be a lower case slug", o))
				}
			}
		case n.Kind.Command():
			if n.Command == "" {
				add(n.ID, "command", "the step needs a command")
			}
		case n.Kind == KindGate:
			if len(n.Approvers) == 0 {
				add(n.ID, "approvers", "a gate needs at least one approver (a login, role:member or role:owner)")
			}
			if len(n.DependsOn) == 0 {
				add(n.ID, "dependsOn", "a gate must follow a step: it has nothing to review")
			}
		case n.Kind == KindSubworkflow:
			switch {
			case n.WorkflowID == "":
				add(n.ID, "workflowId", "choose the workflow to run")
			case n.WorkflowID == w.ID && w.ID != "":
				add(n.ID, "workflowId", "a workflow cannot run itself")
			case env.Workflow != nil:
				if sub, ok := env.Workflow(n.WorkflowID); !ok {
					add(n.ID, "workflowId", "the workflow does not exist")
				} else if sub.Status != StatusPublished {
					add(n.ID, "workflowId", fmt.Sprintf("workflow %q is not published", sub.Name))
				}
			}
		}
		if f := n.OnFailure; f != nil {
			switch f.Action {
			case "", FailStop, FailContinue:
			case FailRework:
				if f.Target == "" && n.Kind != KindGate {
					add(n.ID, "onFailure", "a rework edge needs the step to go back to")
				}
			default:
				add(n.ID, "onFailure", fmt.Sprintf("unknown failure action %q", f.Action))
			}
			if f.MaxRounds < 0 || f.MaxRounds > 20 {
				add(n.ID, "onFailure", "max rounds must be between 0 and 20")
			}
			if f.Target != "" && ids[f.Target] == 0 {
				add(n.ID, "onFailure", fmt.Sprintf("the rework target %q does not exist", f.Target))
			}
		}
		for _, field := range []struct{ name, text string }{{"prompt", n.Prompt}, {"command", n.Command}} {
			for _, ref := range Refs(field.text) {
				if msg := checkRef(ref, w, n, inputs, ids); msg != "" {
					add(n.ID, field.name, msg)
				}
			}
		}
	}
	if cyc := cycle(w.Nodes); cyc != "" {
		add("", "dependsOn", "the steps form a cycle: "+cyc)
	} else if len(out) == 0 {
		// A rework target must come earlier (an ancestor), or the run could not reach the failing node again.
		for _, n := range w.Nodes {
			if f := n.OnFailure; f != nil && f.Action == FailRework && f.Target != "" &&
				!slices.Contains(Ancestors(w.Nodes, n.ID), f.Target) {
				add(n.ID, "onFailure", fmt.Sprintf("the rework target %q must be a step this one depends on", f.Target))
			}
		}
	}
	return out
}

func checkRef(ref string, w *Workflow, n *Node, inputs map[string]bool, ids map[string]int) string {
	parts := strings.Split(ref, ".")
	switch parts[0] {
	case "inputs":
		if len(parts) != 2 || !inputs[parts[1]] {
			return fmt.Sprintf("{{%s}}: no such input", ref)
		}
	case "nodes":
		if len(parts) < 3 || ids[parts[1]] == 0 {
			return fmt.Sprintf("{{%s}}: no such step", ref)
		}
		if !slices.Contains(Ancestors(w.Nodes, n.ID), parts[1]) {
			return fmt.Sprintf("{{%s}}: step %q does not run before this one", ref, parts[1])
		}
	case "requirement":
		if len(parts) != 2 || parts[1] != "title" && parts[1] != "body" {
			return fmt.Sprintf("{{%s}}: use requirement.title or requirement.body", ref)
		}
	case "feedback", "branch", "repo":
	default:
		return fmt.Sprintf("{{%s}}: unknown variable", ref)
	}
	return ""
}

// cycle returns "a -> b -> a" when the graph has a cycle, else "".
func cycle(ns []Node) string {
	by := map[string]Node{}
	for _, n := range ns {
		by[n.ID] = n
	}
	state := map[string]int{}
	var stack []string
	var found string
	var visit func(string) bool
	visit = func(id string) bool {
		switch state[id] {
		case 1:
			i := slices.Index(stack, id)
			found = strings.Join(append(slices.Clone(stack[i:]), id), " -> ")
			return true
		case 2:
			return false
		}
		state[id] = 1
		stack = append(stack, id)
		for _, d := range by[id].DependsOn {
			if _, ok := by[d]; ok && visit(d) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return false
	}
	for _, n := range ns {
		if visit(n.ID) {
			return found
		}
	}
	return ""
}

func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// Check validates w and returns an *InvalidError when it has problems.
func (w *Workflow) Check(env Env) error {
	if is := w.Validate(env); len(is) > 0 {
		return &InvalidError{Issues: is}
	}
	return nil
}
