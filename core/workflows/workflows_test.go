package workflows_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/workflows"
)

func agent(id string, deps ...string) workflows.Node {
	return workflows.Node{ID: id, Kind: workflows.KindAgent, Prompt: "do " + id, DependsOn: deps}
}

func wf(ns ...workflows.Node) workflows.Workflow {
	return workflows.Workflow{Name: "w", Nodes: ns}
}

func messages(is []workflows.Issue) string {
	var out []string
	for _, i := range is {
		out = append(out, i.NodeID+": "+i.Message)
	}
	return strings.Join(out, "\n")
}

func TestValidateAcceptsABuiltinTemplate(t *testing.T) {
	env := workflows.Env{RoleExists: func(string) bool { return true }}
	for _, tpl := range workflows.Templates() {
		w := workflows.FromTemplate(tpl, "p1")
		if is := w.Validate(env); len(is) > 0 {
			t.Errorf("template %s: %s", tpl.ID, messages(is))
		}
		if w.Status != workflows.StatusDraft || w.ProjectID != "p1" || w.Template != tpl.ID {
			t.Errorf("template %s copy = %+v", tpl.ID, w)
		}
	}
}

func TestValidateReportsEachProblem(t *testing.T) {
	cases := []struct {
		name string
		w    workflows.Workflow
		want string
	}{
		{"cycle", wf(agent("a", "b"), agent("b", "a")), "cycle"},
		{"missing dependency", wf(agent("a", "ghost")), "does not exist"},
		{"self dependency", wf(agent("a", "a")), "itself"},
		{"duplicate id", wf(agent("a"), agent("a")), "two steps"},
		{"bad id", wf(agent("A b")), "lower case slug"},
		{"no prompt", wf(workflows.Node{ID: "a", Kind: workflows.KindAgent}), "needs a prompt"},
		{"no command", wf(workflows.Node{ID: "a", Kind: workflows.KindScript}), "needs a command"},
		{"gate without approvers", wf(agent("a"), workflows.Node{ID: "g", Kind: workflows.KindGate, DependsOn: []string{"a"}}), "at least one approver"},
		{"gate with nothing to review", wf(workflows.Node{ID: "g", Kind: workflows.KindGate, Approvers: []string{"role:member"}}), "nothing to review"},
		{"unknown kind", wf(workflows.Node{ID: "a", Kind: "weird"}), "unknown kind"},
		{"unknown input", wf(workflows.Node{ID: "a", Kind: workflows.KindAgent, Prompt: "{{inputs.x}}"}), "no such input"},
		{"reference to a later step", wf(workflows.Node{ID: "a", Kind: workflows.KindAgent, Prompt: "{{nodes.b.result}}"}, agent("b")), "does not run before"},
		{"rework target missing", wf(agent("a"), workflows.Node{ID: "b", Kind: workflows.KindAgent, Prompt: "x", DependsOn: []string{"a"},
			OnFailure: &workflows.OnFailure{Action: workflows.FailRework, Target: "zzz"}}), "does not exist"},
		{"rework target not earlier", wf(agent("a"), agent("b"), workflows.Node{ID: "c", Kind: workflows.KindAgent, Prompt: "x", DependsOn: []string{"a"},
			OnFailure: &workflows.OnFailure{Action: workflows.FailRework, Target: "b"}}), "must be a step this one depends on"},
		{"no steps", wf(), "no steps"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			is := c.w.Validate(workflows.Env{})
			// Assert
			if !strings.Contains(messages(is), c.want) {
				t.Fatalf("issues = %q, want one containing %q", messages(is), c.want)
			}
			if err := c.w.Check(workflows.Env{}); !errors.Is(err, workflows.ErrInvalid) {
				t.Fatalf("Check = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestValidateChecksRolesAndSubworkflows(t *testing.T) {
	// Arrange
	env := workflows.Env{
		RoleExists: func(n string) bool { return n == "coder" },
		Workflow: func(id string) (*workflows.Workflow, bool) {
			switch id {
			case "pub":
				return &workflows.Workflow{ID: id, Name: "pub", Status: workflows.StatusPublished}, true
			case "draft":
				return &workflows.Workflow{ID: id, Name: "draft", Status: workflows.StatusDraft}, true
			}
			return nil, false
		},
	}
	w := wf(
		workflows.Node{ID: "a", Kind: workflows.KindAgent, Prompt: "x", Role: "nobody"},
		workflows.Node{ID: "b", Kind: workflows.KindSubworkflow, WorkflowID: "draft"},
		workflows.Node{ID: "c", Kind: workflows.KindSubworkflow, WorkflowID: "gone"},
		workflows.Node{ID: "d", Kind: workflows.KindSubworkflow, WorkflowID: "pub"},
	)

	// Act
	got := messages(w.Validate(env))

	// Assert
	for _, want := range []string{`the role "nobody" does not exist`, "is not published", "the workflow does not exist"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "d:") {
		t.Errorf("a published subworkflow was rejected:\n%s", got)
	}
}

func TestGraphHelpers(t *testing.T) {
	ns := []workflows.Node{agent("a"), agent("b", "a"), agent("c", "b"), agent("d", "a")}
	if got := strings.Join(workflows.Ancestors(ns, "c"), ","); got != "b,a" {
		t.Errorf("ancestors of c = %s", got)
	}
	down := workflows.Downstream(ns, "a")
	if len(down) != 3 {
		t.Errorf("downstream of a = %v", down)
	}
	if got := workflows.Downstream(ns, "c"); len(got) != 0 {
		t.Errorf("downstream of c = %v", got)
	}
}

func TestRenderFillsKnownVariablesAndBlanksTheRest(t *testing.T) {
	got := workflows.Render("a {{ inputs.x }} b {{nodes.p.result}} c {{nope}}", func(n string) (string, bool) {
		if n == "inputs.x" {
			return "X", true
		}
		return "", false
	})
	if got != "a X b  c " {
		t.Fatalf("rendered %q", got)
	}
	if refs := workflows.Refs("{{a.b}} {{a.b}} {{c}}"); len(refs) != 2 {
		t.Fatalf("refs = %v", refs)
	}
}

func TestMayDecide(t *testing.T) {
	n := workflows.Node{Approvers: []string{"alice", "role:owner"}}
	if !n.MayDecide("alice", "viewer") || !n.MayDecide("bob", "owner") || n.MayDecide("bob", "member") {
		t.Fatal("approver matching is wrong")
	}
	m := workflows.Node{Approvers: []string{"role:member"}}
	if !m.MayDecide("x", "member") || !m.MayDecide("x", "owner") || m.MayDecide("x", "viewer") {
		t.Fatal("role:member must accept members and owners only")
	}
}
