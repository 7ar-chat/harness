// Package requirements is the vocabulary of requirements tracking: what a project wants done (a requirement),
// followed from the idea to the merged pull request (requirement, then the sessions and factory tasks that work on
// it, then the PR, then merged). It holds types and the persistence port only; adapters/store/reqstore stores them
// and app/product serves them.
package requirements

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Status is where a requirement is in its life.
type Status string

// Requirement statuses, in the order of a normal life.
const (
	StatusBacklog    Status = "backlog"
	StatusReady      Status = "ready"
	StatusInProgress Status = "in_progress"
	StatusInReview   Status = "in_review"
	StatusDone       Status = "done"
	StatusRejected   Status = "rejected"
)

// Valid reports whether s is one of the statuses above.
func (s Status) Valid() bool {
	switch s {
	case StatusBacklog, StatusReady, StatusInProgress, StatusInReview, StatusDone, StatusRejected:
		return true
	}
	return false
}

// Closed reports whether no further work happens in s.
func (s Status) Closed() bool { return s == StatusDone || s == StatusRejected }

// Source kinds (Source.Kind).
const (
	SourceManual      = "manual"
	SourceGitHubIssue = "github_issue"
	SourceGitLabIssue = "gitlab_issue"
	SourceGiteaIssue  = "gitea_issue"
)

// Origin says where a requirement came from. A GitHub issue is identified by Repo and Number, unique per project.
type Origin struct {
	Kind   string `json:"kind"`
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Key is the identity of the source inside a project ("" for a manual requirement, which has none).
func (s Origin) Key() string {
	if s.Repo == "" || s.Number <= 0 {
		return ""
	}
	var provider string
	switch s.Kind {
	case SourceGitHubIssue:
		provider = "github"
	case SourceGitLabIssue:
		provider = "gitlab"
	case SourceGiteaIssue:
		provider = "gitea"
	default:
		return ""
	}
	return provider + ":issue:" + strings.ToLower(s.Repo) + "#" + strconv.Itoa(s.Number)
}

// Issue is an issue of a code host, as an IssueSource lists it.
type Issue struct {
	Number int
	Title  string
	Body   string
	URL    string
	Author string
	Labels []string
}

// IssueSource lists the open issues of one repository of a code host (GitHub, GitLab, Gitea): the provider the
// requirements import reads. Source is the Origin.Kind the imported requirements get. The repository is the id the
// provider knows ("owner/name", or "host/group/project").
type IssueSource interface {
	Source() string
	OpenIssues(ctx context.Context, repo, label string, max int) ([]Issue, error)
}

// LinkedPR is the pull request that delivers the requirement. State is open, merged or closed.
type LinkedPR struct {
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

// Requirement is one thing a project wants done.
type Requirement struct {
	ID        string   `json:"id"`
	ProjectID string   `json:"projectId"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"` // markdown
	Status    Status   `json:"status"`
	Priority  int      `json:"priority,omitempty"` // higher first
	Labels    []string `json:"labels,omitempty"`
	Source    Origin   `json:"source"`
	Author    string   `json:"author,omitempty"`
	// ThreadIDs and TaskIDs are the sessions and factory tasks working on it, oldest first.
	ThreadIDs []string `json:"threadIds,omitempty"`
	TaskIDs   []string `json:"taskIds,omitempty"`
	PR        LinkedPR `json:"pr,omitzero"`
	// Flag says why work on an in-progress requirement needs a human (a failed or blocked task); empty when fine.
	Flag      string    `json:"flag,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Patch is a partial update. Nil fields are left alone.
type Patch struct {
	Title    *string
	Body     *string
	Status   *Status
	Priority *int
	Labels   *[]string
	PR       *LinkedPR
	Flag     *string
	// AddThreadID and AddTaskID link a session or a task (a link is added once).
	AddThreadID string
	AddTaskID   string
}

// Filter selects requirements (all fields optional, ANDed). Query matches the title and body, case-insensitively.
type Filter struct {
	ProjectID string
	Statuses  []Status
	Label     string
	Query     string
	ThreadID  string
	TaskID    string
	Limit     int // 0: 500
}

// DispatchBudget caps what a dispatched requirement may spend. A zero field is no cap on that axis.
type DispatchBudget struct {
	MaxUSD    float64 `json:"maxUsd,omitempty"`
	MaxTokens int64   `json:"maxTokens,omitempty"`
}

// Settings are the per-project settings of requirements tracking.
type Settings struct {
	ProjectID string `json:"projectId"`
	// AutoDispatch dispatches a requirement as soon as it becomes ready, with Policy and Budget. Off by default;
	// the server refuses to turn it on without a budget.
	AutoDispatch bool           `json:"autoDispatch"`
	Policy       string         `json:"policy,omitempty"`
	Budget       DispatchBudget `json:"budget,omitzero"`
	// CommentOnIssue posts on the source GitHub issue of a requirement when its pull request opens ("A pull request is
	// open: #142") and when it merges ("Fixed by #142"). Off by default: the GitHub App then needs the issues permission.
	CommentOnIssue bool `json:"commentOnIssue"`
}

// Errors of the port.
var (
	ErrNotFound = errors.New("requirements: not found")
	ErrInvalid  = errors.New("requirements: invalid")
)

// Store persists requirements and their settings. It is safe for concurrent use.
type Store interface {
	// Create stores r (a fresh ID, StatusBacklog and the clock when unset). When r has a source key and a
	// requirement of the project has it, that one is returned with created=false.
	Create(ctx context.Context, r Requirement) (*Requirement, bool, error)
	Get(ctx context.Context, id string) (*Requirement, error)
	Update(ctx context.Context, id string, p Patch) (*Requirement, error)
	Delete(ctx context.Context, id string) error
	// List returns the matches, highest priority first, then newest first.
	List(ctx context.Context, f Filter) ([]Requirement, error)

	// GetSettings returns the project's settings (AutoDispatch off when none were saved).
	GetSettings(ctx context.Context, projectID string) (Settings, error)
	PutSettings(ctx context.Context, s Settings) error
}

// Prepare validates r and fills the defaults of a new requirement (not its ID or times).
func Prepare(r Requirement) (Requirement, error) {
	r.Title = strings.TrimSpace(r.Title)
	switch {
	case r.ProjectID == "":
		return r, errors.Join(ErrInvalid, errors.New("a requirement belongs to a project"))
	case r.Title == "":
		return r, errors.Join(ErrInvalid, errors.New("a requirement needs a title"))
	}
	if r.Status == "" {
		r.Status = StatusBacklog
	}
	if !r.Status.Valid() {
		return r, errors.Join(ErrInvalid, errors.New("unknown status "+string(r.Status)))
	}
	if r.Source.Kind == "" {
		r.Source.Kind = SourceManual
	}
	r.Labels = CleanLabels(r.Labels)
	return r, nil
}

// CleanLabels trims, drops empty and duplicate labels, keeping the order.
func CleanLabels(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range in {
		l = strings.TrimSpace(l)
		if l == "" || seen[strings.ToLower(l)] {
			continue
		}
		seen[strings.ToLower(l)] = true
		out = append(out, l)
	}
	return out
}

// Apply applies p to r (the caller sets UpdatedAt). A patch with an invalid status or an empty title is rejected.
func Apply(r *Requirement, p Patch) error {
	if p.Title != nil && strings.TrimSpace(*p.Title) == "" {
		return errors.Join(ErrInvalid, errors.New("a requirement needs a title"))
	}
	if p.Status != nil && !p.Status.Valid() {
		return errors.Join(ErrInvalid, errors.New("unknown status "+string(*p.Status)))
	}
	if p.Title != nil {
		r.Title = strings.TrimSpace(*p.Title)
	}
	if p.Body != nil {
		r.Body = *p.Body
	}
	if p.Status != nil {
		r.Status = *p.Status
	}
	if p.Priority != nil {
		r.Priority = *p.Priority
	}
	if p.Labels != nil {
		r.Labels = CleanLabels(*p.Labels)
	}
	if p.PR != nil {
		r.PR = *p.PR
	}
	if p.Flag != nil {
		r.Flag = *p.Flag
	}
	if p.AddThreadID != "" && !contains(r.ThreadIDs, p.AddThreadID) {
		r.ThreadIDs = append(append([]string(nil), r.ThreadIDs...), p.AddThreadID)
	}
	if p.AddTaskID != "" && !contains(r.TaskIDs, p.AddTaskID) {
		r.TaskIDs = append(append([]string(nil), r.TaskIDs...), p.AddTaskID)
	}
	return nil
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// Match reports whether r satisfies f (Limit aside).
func (f Filter) Match(r Requirement) bool {
	if f.ProjectID != "" && r.ProjectID != f.ProjectID {
		return false
	}
	if len(f.Statuses) > 0 {
		ok := false
		for _, s := range f.Statuses {
			ok = ok || s == r.Status
		}
		if !ok {
			return false
		}
	}
	if f.Label != "" {
		ok := false
		for _, l := range r.Labels {
			ok = ok || strings.EqualFold(l, f.Label)
		}
		if !ok {
			return false
		}
	}
	if f.ThreadID != "" && !contains(r.ThreadIDs, f.ThreadID) {
		return false
	}
	if f.TaskID != "" && !contains(r.TaskIDs, f.TaskID) {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Query)); q != "" {
		if !strings.Contains(strings.ToLower(r.Title), q) && !strings.Contains(strings.ToLower(r.Body), q) {
			return false
		}
	}
	return true
}

// Less orders a list: priority first, then newest, then ID for a stable order.
func Less(a, b Requirement) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID < b.ID
}

// Clone copies the slices of r so a caller cannot change what a store holds.
func Clone(r Requirement) Requirement {
	r.Labels = append([]string(nil), r.Labels...)
	r.ThreadIDs = append([]string(nil), r.ThreadIDs...)
	r.TaskIDs = append([]string(nil), r.TaskIDs...)
	if len(r.Labels) == 0 {
		r.Labels = nil
	}
	if len(r.ThreadIDs) == 0 {
		r.ThreadIDs = nil
	}
	if len(r.TaskIDs) == 0 {
		r.TaskIDs = nil
	}
	return r
}
