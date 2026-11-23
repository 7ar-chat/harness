// Package usage is the ledger of what runs consume: one entry per model of a finished run (tokens by kind, the price at
// the time, who, which project, which kind of work), the monthly spend caps, and the store port. The ledger is written
// when a run ends (an end-of-run hook, app/usagehook) and read by the Usage page and by the cap check when a run starts.
package usage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Sources of a run (Entry.Source).
const (
	// SourceSession is an interactive conversation.
	SourceSession = "session"
	// SourceTask is a factory task.
	SourceTask = "task"
	// SourceSchedule is a scheduled agent task.
	SourceSchedule = "schedule"
)

// Groupings of Query.Group.
const (
	GroupDay    = "day"
	GroupModel  = "model"
	GroupThread = "thread"
	GroupUser   = "user"
)

// Scopes of a cap.
const (
	ScopeProject = "project"
	ScopeUser    = "user"
)

// WarnAt is the share of a cap from which the page and the API warn.
const WarnAt = 0.8

var (
	// ErrInvalid wraps a refused value.
	ErrInvalid = errors.New("usage: invalid")
	// ErrCapReached is what a CapError matches with errors.Is: a run may not start because a monthly cap is used up.
	ErrCapReached = errors.New("spend cap reached")
)

// CapError says which cap stopped a run.
type CapError struct {
	// Scope is ScopeProject or ScopeUser; ID the project id or the login.
	Scope, ID string
	CapUSD    float64
	SpentUSD  float64
}

func (e *CapError) Error() string {
	who := "Your monthly spend cap"
	if e.Scope == ScopeProject {
		who = "This project's monthly spend cap"
	}
	return fmt.Sprintf("%s ($%.2f) is used up: $%.2f spent this month. An owner can raise it on the Usage page.", who, e.CapUSD, e.SpentUSD)
}

// Is makes errors.Is(err, ErrCapReached) true.
func (e *CapError) Is(target error) bool { return target == ErrCapReached }

// Entry is the usage of one model in one finished run.
type Entry struct {
	RunID    string `json:"runId"`
	ThreadID string `json:"threadId"`
	// ProjectID is empty for a personal conversation.
	ProjectID string `json:"projectId,omitempty"`
	// User is who the work is booked to: the person who started the conversation.
	User   string `json:"user"`
	Source string `json:"source"`
	// SourceID is the schedule id or the task id (empty for a session).
	SourceID         string  `json:"sourceId,omitempty"`
	Model            string  `json:"model"`
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	CostUSD          float64 `json:"costUsd"`
	// Unpriced is true when the model has no price (CostUSD is then 0, not free).
	Unpriced bool      `json:"unpriced,omitempty"`
	At       time.Time `json:"at"`
}

// Query selects entries.
type Query struct {
	// ProjectID restricts to a project; User to a person (both empty: everything).
	ProjectID string
	User      string
	// From is inclusive, To exclusive; a zero bound is open.
	From, To time.Time
	// Group is one of the Group constants ("": a single total).
	Group string
	// Limit bounds the rows of a thread grouping (0: 50).
	Limit int
}

// Matches reports whether e is selected by q's filters.
func (q Query) Matches(e Entry) bool {
	if q.ProjectID != "" && e.ProjectID != q.ProjectID {
		return false
	}
	if q.User != "" && e.User != q.User {
		return false
	}
	if !q.From.IsZero() && e.At.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && !e.At.Before(q.To) {
		return false
	}
	return true
}

// UsageTotals is a sum of entries.
type UsageTotals struct {
	Runs             int64   `json:"runs"`
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	CostUSD          float64 `json:"costUsd"`
	// Unpriced counts the entries of models without a price: CostUSD leaves them out.
	Unpriced int64 `json:"unpriced"`
}

func (t *UsageTotals) add(e Entry) {
	t.InputTokens += e.InputTokens
	t.OutputTokens += e.OutputTokens
	t.CacheReadTokens += e.CacheReadTokens
	t.CacheWriteTokens += e.CacheWriteTokens
	t.CostUSD += e.CostUSD
	if e.Unpriced {
		t.Unpriced++
	}
}

// UsageRow is a total with its group key (a day "2026-11-03", a model, a thread id or a login).
type UsageRow struct {
	Key string `json:"key"`
	UsageTotals
}

// Report is the answer to a Query.
type Report struct {
	Total UsageTotals `json:"total"`
	// BySource splits the total between the kinds of work (session, task, schedule).
	BySource map[string]UsageTotals `json:"bySource"`
	// Rows are the groups: by day for a day grouping, else the costliest first.
	Rows []UsageRow `json:"rows"`
}

// Aggregate builds the report of entries already selected by q: the one implementation every store shares.
func Aggregate(entries []Entry, q Query) *Report {
	rep := &Report{BySource: map[string]UsageTotals{}, Rows: []UsageRow{}}
	seen := map[string]bool{}
	rows := map[string]*UsageRow{}
	for _, e := range entries {
		rep.Total.add(e)
		src := rep.BySource[e.Source]
		src.add(e)
		if k := "s|" + e.Source + "|" + e.RunID; !seen[k] {
			seen[k] = true
			src.Runs++
		}
		rep.BySource[e.Source] = src
		if k := "t|" + e.RunID; !seen[k] {
			seen[k] = true
			rep.Total.Runs++
		}
		if q.Group == "" {
			continue
		}
		key := groupKey(e, q.Group)
		r := rows[key]
		if r == nil {
			r = &UsageRow{Key: key}
			rows[key] = r
		}
		r.add(e)
		if k := "r|" + key + "|" + e.RunID; !seen[k] {
			seen[k] = true
			r.Runs++
		}
	}
	for _, r := range rows {
		rep.Rows = append(rep.Rows, *r)
	}
	sort.Slice(rep.Rows, func(i, j int) bool {
		a, b := rep.Rows[i], rep.Rows[j]
		switch {
		case q.Group == GroupDay:
			return a.Key < b.Key
		case a.CostUSD != b.CostUSD:
			return a.CostUSD > b.CostUSD
		case a.InputTokens+a.OutputTokens != b.InputTokens+b.OutputTokens:
			return a.InputTokens+a.OutputTokens > b.InputTokens+b.OutputTokens
		}
		return a.Key < b.Key
	})
	if q.Group == GroupThread {
		limit := q.Limit
		if limit <= 0 {
			limit = 50
		}
		if len(rep.Rows) > limit {
			rep.Rows = rep.Rows[:limit]
		}
	}
	return rep
}

func groupKey(e Entry, group string) string {
	switch group {
	case GroupDay:
		return e.At.UTC().Format("2006-01-02")
	case GroupModel:
		return e.Model
	case GroupThread:
		return e.ThreadID
	case GroupUser:
		return e.User
	}
	return ""
}

// ValidGroup reports whether g is a Group constant or empty.
func ValidGroup(g string) bool {
	switch g {
	case "", GroupDay, GroupModel, GroupThread, GroupUser:
		return true
	}
	return false
}

// Store persists the ledger and the caps.
type Store interface {
	// Record stores the entries of a run. A run is recorded once: entries of a run id already present are ignored.
	Record(ctx context.Context, entries []Entry) error
	// Report aggregates the entries matching q.
	Report(ctx context.Context, q Query) (*Report, error)
	// Spent is the cost of the entries of a project (projectID) or of a user across every project (user) since from.
	Spent(ctx context.Context, projectID, user string, from time.Time) (float64, error)
	// SetCap sets the monthly cap of a project (ScopeProject, the project id) or a user (ScopeUser, the login); 0 removes it.
	SetCap(ctx context.Context, scope, id string, usd float64) error
	// Cap returns the monthly cap (0: none).
	Cap(ctx context.Context, scope, id string) (float64, error)
}

// MonthStart is the first instant of t's calendar month, in UTC: the window the monthly caps count in.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Check returns a *CapError when the project's or the user's monthly cap is used up at now. projectID may be empty
// (a personal conversation: only the user's cap applies).
func Check(ctx context.Context, st Store, projectID, user string, now time.Time) error {
	from := MonthStart(now)
	if projectID != "" {
		if err := checkOne(ctx, st, ScopeProject, projectID, from); err != nil {
			return err
		}
	}
	if user != "" {
		return checkOne(ctx, st, ScopeUser, user, from)
	}
	return nil
}

func checkOne(ctx context.Context, st Store, scope, id string, from time.Time) error {
	limit, err := st.Cap(ctx, scope, id)
	if err != nil || limit <= 0 {
		return err
	}
	var spent float64
	if scope == ScopeProject {
		spent, err = st.Spent(ctx, id, "", from)
	} else {
		spent, err = st.Spent(ctx, "", id, from)
	}
	if err != nil {
		return err
	}
	if spent >= limit {
		return &CapError{Scope: scope, ID: id, CapUSD: limit, SpentUSD: spent}
	}
	return nil
}
