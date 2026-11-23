package workflows

import "slices"

// Template is a builtin workflow a project copies and then edits. Templates are code: read-only and never stored.
type Template struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Workflow    Workflow `json:"workflow"`
}

// Templates returns the builtin templates.
func Templates() []Template {
	return []Template{backendDelivery(), frontendDelivery(), productProposal(), bugFix()}
}

// TemplateByID returns the builtin template id.
func TemplateByID(id string) (Template, bool) {
	for _, t := range Templates() {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// FromTemplate returns a draft workflow for projectID that copies t.
func FromTemplate(t Template, projectID string) Workflow {
	w := t.Workflow
	w.ID, w.ProjectID, w.Status, w.Template = "", projectID, StatusDraft, t.ID
	w.Version, w.PublishedVersion = 0, 0
	w.Nodes = slices.Clone(w.Nodes)
	w.Inputs = slices.Clone(w.Inputs)
	return w
}

func at(col, row int) Pos { return Pos{X: float64(40 + col*240), Y: float64(40 + row*130)} }

func sendBack(target string) *OnFailure {
	return &OnFailure{Action: FailRework, Target: target, MaxRounds: DefaultRounds}
}

const reviewers = "role:member"

func backendDelivery() Template {
	return Template{ID: "backend-delivery", Name: "Backend delivery",
		Description: "Plan the change, implement it, run the tests, wait for a person to review, then land the pull request.",
		Workflow: Workflow{
			Name:        "Backend delivery",
			Description: "plan, implement, test, review gate, pull request",
			Inputs: []Input{
				{Name: "task", Label: "What to build", Required: true, Long: true},
				{Name: "test_command", Label: "Test command", Default: "make test"},
			},
			Nodes: []Node{
				{ID: "plan", Kind: KindAgent, Title: "Plan", Role: "planner", Pos: at(0, 0), Outputs: []string{"plan"},
					Prompt: "Read the repository and write an implementation plan for this task:\n\n{{inputs.task}}\n\nList the files to change, the steps in order, and how to verify them."},
				{ID: "implement", Kind: KindAgent, Title: "Implement", Role: "coder", DependsOn: []string{"plan"}, Pos: at(1, 0), UseRepo: true,
					Prompt:    "Implement the plan.\n\nTask: {{inputs.task}}\n\nPlan:\n{{nodes.plan.result}}\n\nCommit your work and open the pull request.",
					OnFailure: nil},
				{ID: "test", Kind: KindScript, Title: "Run the tests", DependsOn: []string{"implement"}, Pos: at(2, 0), UseRepo: true,
					Command: "{{inputs.test_command}}", OnFailure: sendBack("implement")},
				{ID: "review", Kind: KindGate, Title: "Review", DependsOn: []string{"test"}, Pos: at(3, 0), Approvers: []string{reviewers},
					OnFailure: sendBack("implement")},
				{ID: "pr", Kind: KindAgent, Title: "Land the pull request", Role: "coder", DependsOn: []string{"review"}, Pos: at(4, 0), UseRepo: true, WaitMerge: true,
					Prompt: "The change was approved. Make sure the pull request is up to date, its description explains what changed and why, and CI is green.\n\nReviewer notes:\n{{nodes.review.result}}"},
			},
		}}
}

func frontendDelivery() Template {
	return Template{ID: "frontend-delivery", Name: "Frontend delivery",
		Description: "Plan the change, implement it, build a preview, and hand it to a person for review.",
		Workflow: Workflow{
			Name:        "Frontend delivery",
			Description: "plan, implement, preview build, review gate",
			Inputs: []Input{
				{Name: "task", Label: "What to build", Required: true, Long: true},
				{Name: "build_command", Label: "Build command", Default: "npm ci && npm run build"},
			},
			Nodes: []Node{
				{ID: "plan", Kind: KindAgent, Title: "Plan", Role: "planner", Pos: at(0, 0), Outputs: []string{"plan"},
					Prompt: "Read the repository and plan this interface change:\n\n{{inputs.task}}\n\nName the components and pages to touch and how the result is verified in a browser."},
				{ID: "implement", Kind: KindAgent, Title: "Implement", Role: "coder", DependsOn: []string{"plan"}, Pos: at(1, 0), UseRepo: true,
					Prompt: "Implement the plan.\n\nTask: {{inputs.task}}\n\nPlan:\n{{nodes.plan.result}}\n\nCommit your work and open the pull request."},
				{ID: "build", Kind: KindBuild, Title: "Preview build", DependsOn: []string{"implement"}, Pos: at(2, 0), UseRepo: true,
					Command: "{{inputs.build_command}}", OnFailure: sendBack("implement")},
				{ID: "review", Kind: KindGate, Title: "Review the preview", DependsOn: []string{"build"}, Pos: at(3, 0), Approvers: []string{reviewers},
					OnFailure: sendBack("implement")},
			},
		}}
}

func productProposal() Template {
	return Template{ID: "product-proposal", Name: "Product proposal",
		Description: "Research a question, draft a proposal, and put it in front of a person before anything is built.",
		Workflow: Workflow{
			Name:        "Product proposal",
			Description: "research, draft, human gate",
			Inputs:      []Input{{Name: "topic", Label: "What to propose", Required: true, Long: true}},
			Nodes: []Node{
				{ID: "research", Kind: KindAgent, Title: "Research", Role: "explorer", Pos: at(0, 0), Outputs: []string{"findings"},
					Prompt: "Research this topic in the repository and its documents: what exists today, what is missing, what constrains a change.\n\n{{inputs.topic}}"},
				{ID: "draft", Kind: KindAgent, Title: "Draft the proposal", Role: "planner", DependsOn: []string{"research"}, Pos: at(1, 0), Outputs: []string{"proposal"},
					Prompt: "Write a one page proposal for: {{inputs.topic}}\n\nFindings:\n{{nodes.research.result}}\n\nGive the problem, the proposed change, the alternatives and the risks."},
				{ID: "approve", Kind: KindGate, Title: "Decision", DependsOn: []string{"draft"}, Pos: at(2, 0), Approvers: []string{"role:owner"},
					OnFailure: sendBack("draft")},
			},
		}}
}

func bugFix() Template {
	return Template{ID: "bug-fix", Name: "Bug fix",
		Description: "Reproduce the bug with a failing test, fix it, run the tests and open the pull request.",
		Workflow: Workflow{
			Name:        "Bug fix",
			Description: "reproduce, fix, test, pull request",
			Inputs: []Input{
				{Name: "bug", Label: "The bug (what happens, what should happen)", Required: true, Long: true},
				{Name: "test_command", Label: "Test command", Default: "make test"},
			},
			Nodes: []Node{
				{ID: "reproduce", Kind: KindAgent, Title: "Reproduce", Role: "tester", Pos: at(0, 0), UseRepo: true, Outputs: []string{"cause"},
					Prompt: "Reproduce this bug with a failing test and find its cause:\n\n{{inputs.bug}}\n\nCommit the failing test and open the pull request. Report the cause."},
				{ID: "fix", Kind: KindAgent, Title: "Fix", Role: "coder", DependsOn: []string{"reproduce"}, Pos: at(1, 0), UseRepo: true,
					Prompt: "Fix the bug so the failing test passes without breaking others.\n\nBug: {{inputs.bug}}\n\nCause found:\n{{nodes.reproduce.result}}\n\nCommit your work on the same pull request."},
				{ID: "test", Kind: KindScript, Title: "Run the tests", DependsOn: []string{"fix"}, Pos: at(2, 0), UseRepo: true,
					Command: "{{inputs.test_command}}", OnFailure: sendBack("fix")},
				{ID: "pr", Kind: KindAgent, Title: "Pull request", Role: "coder", DependsOn: []string{"test"}, Pos: at(3, 0), UseRepo: true, WaitMerge: true,
					Prompt: "Tests pass. Make sure the pull request describes the bug, the cause and the fix, and that CI is green."},
			},
		}}
}
