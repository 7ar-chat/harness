package forge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/engine/tools"
)

// ToolsOptions configures the agent tools of the forge.
type ToolsOptions struct {
	Forge factory.Forge
	// Store tells which thread works on which task, repository and branch.
	Store factory.Store
	// Controller moves the task to review when the agent opens its pull request.
	Controller factory.Controller
	// Push publishes the thread's branch from the host (see Checkout.Push);
	// protected are the branches it must refuse. open_pr calls it before it
	// opens or updates the pull request. Nil leaves pushing to the agent, for
	// hosts whose sandbox holds its own push credentials.
	Push func(ctx context.Context, threadID string, protected []string) (PushResult, error)
}

const (
	maxToolText = 6000
	maxComments = 30
	maxCommentC = 1200
)

type toolProvider struct{ ToolsOptions }

// NewTools returns the provider of open_pr, pr_status, ci_log and
// pr_comments. Only a thread that works on a factory task with a repository
// gets them.
func NewTools(o ToolsOptions) tools.Provider { return &toolProvider{o} }

// Name implements tools.Provider.
func (p *toolProvider) Name() string { return "forge" }

type readOnlyTool struct{ ai.InvokableTool }

func (readOnlyTool) ReadOnly() bool { return true }

type openPRInput struct {
	Title string `json:"title" jsonschema_description:"Pull request title: one line saying what the change does"`
	Body  string `json:"body" jsonschema_description:"Pull request description: what changed and why, how it was verified"`
	Draft bool   `json:"draft,omitempty" jsonschema_description:"Open as a draft pull request"`
}

type ciLogInput struct {
	Check string `json:"check,omitempty" jsonschema_description:"Name of the check; empty picks the first failing one"`
}

type none struct{}

// Tools implements tools.Provider.
func (p *toolProvider) Tools(ctx context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	if p.Forge == nil || p.Store == nil || rc.ThreadID == "" {
		return nil, nil, nil
	}
	ti, err := p.Store.GetThreadInfo(ctx, rc.ThreadID)
	if err != nil {
		if errors.Is(err, factory.ErrNotFound) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("forge tools: thread info of %s: %w", rc.ThreadID, err)
	}
	if ti.TaskID == "" || ti.Repo == "" {
		return nil, nil, nil
	}
	open, err := ai.InferTool("open_pr", openPRDesc, func(ctx context.Context, in openPRInput) (string, error) {
		return p.openPR(ctx, rc.ThreadID, in)
	})
	if err != nil {
		return nil, nil, err
	}
	status, err := ai.InferTool("pr_status", "Show the state of your pull request: open, merged or closed, whether it can be merged, and each CI check with its result.", func(ctx context.Context, _ none) (string, error) {
		return p.prStatus(ctx, rc.ThreadID)
	})
	if err != nil {
		return nil, nil, err
	}
	log, err := ai.InferTool("ci_log", "Show the end of the log of a failing CI check of your pull request (the first failing one, or the one named), to fix what it reports.", func(ctx context.Context, in ciLogInput) (string, error) {
		return p.ciLog(ctx, rc.ThreadID, in.Check)
	})
	if err != nil {
		return nil, nil, err
	}
	comments, err := ai.InferTool("pr_comments", "List the comments and reviews on your pull request (reviewers' requests, inline comments with file and line).", func(ctx context.Context, _ none) (string, error) {
		return p.prComments(ctx, rc.ThreadID)
	})
	if err != nil {
		return nil, nil, err
	}
	return []ai.BaseTool{open, readOnlyTool{status}, readOnlyTool{log}, readOnlyTool{comments}}, nil, nil
}

const openPRDesc = `Commit your work, then call open_pr: it pushes your branch for you (you have no push credentials) and opens or updates the pull request. Calling it again after more commits pushes them.
Calling it again for the same branch returns the pull request that is already open. It refuses a protected branch or the base branch as head: work on your own branch.
After it opens, CI and a reviewer judge the change; a failure or a review comes back to you as a new attempt.`

// scope is what a tool call works on, resolved afresh at every call (the
// task's pull request may have changed since the run began).
type scope struct {
	ti   *factory.ThreadInfo
	task *factory.Task
}

func (p *toolProvider) scope(ctx context.Context, threadID string) (scope, error) {
	ti, err := p.Store.GetThreadInfo(ctx, threadID)
	if err != nil {
		return scope{}, tools.Errorf("this thread is not working on a repository task")
	}
	if ti.TaskID == "" || ti.Repo == "" {
		return scope{}, tools.Errorf("this thread is not working on a repository task")
	}
	t, err := p.Store.GetTask(ctx, ti.TaskID)
	if err != nil {
		return scope{}, fmt.Errorf("load task %s: %w", ti.TaskID, err)
	}
	return scope{ti: ti, task: t}, nil
}

func (p *toolProvider) openPR(ctx context.Context, threadID string, in openPRInput) (string, error) {
	s, err := p.scope(ctx, threadID)
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return "", tools.Errorf("title must not be empty")
	}
	head := s.ti.Branch
	if head == "" {
		head = s.task.Branch
	}
	if head == "" {
		return "", tools.Errorf("this thread has no working branch")
	}
	base := s.ti.BaseBranch
	if base == "" {
		base = s.task.BaseBranch
	}
	if base == "" {
		base, err = p.Forge.DefaultBranch(ctx, s.ti.Repo)
		if err != nil {
			return "", tools.Errorf("cannot tell the base branch: %v", err)
		}
	}
	if head == base {
		return "", tools.Errorf("refusing to open a pull request from %s into itself: commit on your own branch", head)
	}
	protected, err := p.Forge.ProtectedBranches(ctx, s.ti.Repo)
	if err != nil {
		return "", tools.Errorf("cannot check which branches are protected: %v", err)
	}
	if slices.Contains(protected, head) {
		return "", tools.Errorf("refusing to open a pull request from the protected branch %s: switch to your own branch (git checkout -b ...)", head)
	}
	var pushed string
	if p.Push != nil {
		res, err := p.Push(ctx, threadID, protected)
		if err != nil {
			return "", tools.Errorf("pushing your branch failed: %s", explain(err))
		}
		pushed = describePush(res, base)
	}
	// Idempotent: the task already has a pull request for this branch.
	// A retried attempt calls open_pr again after committing fixes: it is pushed
	// and the task goes back to review.
	if n := s.task.PR.Number; n > 0 {
		if pr, err := p.Forge.GetPR(ctx, s.ti.Repo, n); err == nil && pr.Head == head && pr.State == "open" {
			if err := p.toReview(ctx, s.task.ID, pr); err != nil {
				return "", err
			}
			return pushed + "Pull request already open: " + describePR(pr), nil
		}
	}
	pr, err := p.Forge.OpenPR(ctx, factory.OpenPR{Repo: s.ti.Repo, Title: title, Body: in.Body, Head: head, Base: base, Draft: in.Draft})
	if err != nil {
		if existing := p.existing(ctx, s.ti.Repo, head, base, err); existing != nil {
			pr = existing
		} else {
			return "", tools.Errorf("opening the pull request failed: %s", explain(err))
		}
	}
	if err := p.toReview(ctx, s.task.ID, pr); err != nil {
		return "", err
	}
	return pushed + "Pull request opened: " + describePR(pr), nil
}

// describePush is the line open_pr reports before the pull request: what was
// pushed, and the uncommitted work that was not.
func describePush(r PushResult, base string) string {
	s := fmt.Sprintf("Pushed %s (%d commit(s) ahead of %s)\n", r.Branch, r.Ahead, base)
	if r.Dirty > 0 {
		s += fmt.Sprintf("Warning: %d uncommitted change(s) were not pushed\n", r.Dirty)
	}
	return s
}

func (p *toolProvider) toReview(ctx context.Context, taskID string, pr *factory.PRInfo) error {
	if p.Controller == nil {
		return nil
	}
	if err := p.Controller.Review(ctx, taskID, factory.PullRequest{Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadSHA}); err != nil {
		return fmt.Errorf("move task %s to review: %w", taskID, err)
	}
	return nil
}

// existing recovers from "a pull request already exists" (422): the one for the branch.
func (p *toolProvider) existing(ctx context.Context, repo, head, base string, err error) *factory.PRInfo {
	if !errors.Is(err, ErrUnprocessable) {
		return nil
	}
	f, ok := p.Forge.(interface {
		FindOpenPR(ctx context.Context, repo, head, base string) (*factory.PRInfo, error)
	})
	if !ok {
		return nil
	}
	pr, ferr := f.FindOpenPR(ctx, repo, head, base)
	if ferr != nil {
		return nil
	}
	return pr
}

func describePR(pr *factory.PRInfo) string {
	s := fmt.Sprintf("#%d %s (%s -> %s)", pr.Number, pr.URL, pr.Head, pr.Base)
	if pr.Draft {
		s += " [draft]"
	}
	return s
}

// explain is an error for the model: GitHub's own words, no token.
func explain(err error) string { return Redact(err.Error()) }

func (p *toolProvider) pr(ctx context.Context, s scope) (*factory.PRInfo, error) {
	if s.task.PR.Number == 0 {
		return nil, tools.Errorf("no pull request yet: commit your work and call open_pr")
	}
	pr, err := p.Forge.GetPR(ctx, s.ti.Repo, s.task.PR.Number)
	if err != nil {
		return nil, tools.Errorf("reading pull request #%d failed: %s", s.task.PR.Number, explain(err))
	}
	return pr, nil
}

func (p *toolProvider) prStatus(ctx context.Context, threadID string) (string, error) {
	s, err := p.scope(ctx, threadID)
	if err != nil {
		return "", err
	}
	pr, err := p.pr(ctx, s)
	if err != nil {
		return "", err
	}
	checks, err := p.Forge.Checks(ctx, s.ti.Repo, pr.HeadSHA)
	if err != nil {
		return "", tools.Errorf("reading the checks failed: %s", explain(err))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Pull request %s\nState: %s", describePR(pr), pr.State)
	switch {
	case pr.State != "open":
	case pr.Mergeable == nil:
		b.WriteString("; mergeability not computed yet")
	case *pr.Mergeable:
		b.WriteString("; mergeable")
	default:
		fmt.Fprintf(&b, "; NOT mergeable (conflicts with %s: rebase onto origin/%s)", pr.Base, pr.Base)
	}
	b.WriteString("\n")
	b.WriteString(summarizeChecks(checks))
	return b.String(), nil
}

// summarizeChecks is "Checks: 2 passed, 1 failed, 0 pending" and a line per check that is not green.
func summarizeChecks(checks []factory.Check) string {
	if len(checks) == 0 {
		return "Checks: none reported"
	}
	var passed, failed, pending int
	var lines []string
	for _, c := range checks {
		switch c.State {
		case factory.CheckPassed:
			passed++
			continue
		case factory.CheckFailed:
			failed++
		default:
			pending++
		}
		line := fmt.Sprintf("- %s: %s", c.Name, c.State)
		if c.Detail != "" {
			line += " (" + clipRunes(c.Detail, 160) + ")"
		}
		lines = append(lines, line)
	}
	s := fmt.Sprintf("Checks: %d passed, %d failed, %d pending", passed, failed, pending)
	if len(lines) > 0 {
		s += "\n" + strings.Join(lines, "\n")
	}
	return s
}

func (p *toolProvider) ciLog(ctx context.Context, threadID, name string) (string, error) {
	s, err := p.scope(ctx, threadID)
	if err != nil {
		return "", err
	}
	pr, err := p.pr(ctx, s)
	if err != nil {
		return "", err
	}
	checks, err := p.Forge.Checks(ctx, s.ti.Repo, pr.HeadSHA)
	if err != nil {
		return "", tools.Errorf("reading the checks failed: %s", explain(err))
	}
	var pick *factory.Check
	name = strings.TrimSpace(name)
	for i := range checks {
		c := &checks[i]
		if name != "" && strings.EqualFold(c.Name, name) || name == "" && c.State == factory.CheckFailed {
			pick = c
			break
		}
	}
	switch {
	case pick == nil && name != "":
		return "", tools.Errorf("no check named %q; checks: %s", name, checkNames(checks))
	case pick == nil:
		return "No check is failing.\n" + summarizeChecks(checks), nil
	case pick.State != factory.CheckFailed:
		return fmt.Sprintf("Check %q is %s, not failed.", pick.Name, pick.State), nil
	}
	log, err := p.Forge.FailureLog(ctx, s.ti.Repo, *pick)
	if err != nil {
		return "", tools.Errorf("reading the log of %q failed: %s", pick.Name, explain(err))
	}
	if strings.TrimSpace(log) == "" {
		msg := fmt.Sprintf("Check %q failed but its log is not available", pick.Name)
		if pick.Detail != "" {
			msg += ": " + pick.Detail
		}
		if pick.URL != "" {
			msg += " (" + pick.URL + ")"
		}
		return msg, nil
	}
	return fmt.Sprintf("Log of the failed check %q (end):\n%s", pick.Name, tailRunes(log, maxToolText)), nil
}

func checkNames(checks []factory.Check) string {
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

func (p *toolProvider) prComments(ctx context.Context, threadID string) (string, error) {
	s, err := p.scope(ctx, threadID)
	if err != nil {
		return "", err
	}
	if s.task.PR.Number == 0 {
		return "", tools.Errorf("no pull request yet: commit your work and call open_pr")
	}
	cs, err := p.Forge.Comments(ctx, s.ti.Repo, s.task.PR.Number)
	if err != nil {
		return "", tools.Errorf("reading the comments failed: %s", explain(err))
	}
	return renderComments(cs, maxComments), nil
}

// renderComments lists the newest max comments, oldest first.
func renderComments(cs []factory.Comment, max int) string {
	if len(cs) == 0 {
		return "No comments."
	}
	var b strings.Builder
	if len(cs) > max {
		fmt.Fprintf(&b, "(%d older comments omitted)\n", len(cs)-max)
		cs = cs[len(cs)-max:]
	}
	for i, c := range cs {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s", c.Author)
		switch c.Review {
		case "approved":
			b.WriteString(" approved")
		case "changes_requested":
			b.WriteString(" requested changes")
		}
		if c.Path != "" {
			fmt.Fprintf(&b, " on %s", c.Path)
			if c.Line > 0 {
				fmt.Fprintf(&b, ":%d", c.Line)
			}
		}
		b.WriteString(": ")
		b.WriteString(clipRunes(strings.TrimSpace(c.Body), maxCommentC))
	}
	return b.String()
}
