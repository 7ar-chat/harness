package budget

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

// topTasks is how many of the most expensive tasks a Report lists.
const topTasks = 5

// Report is the spend of a project's factory work over a period, for the UI and CLI.
type Report struct {
	ProjectID string
	Since     time.Time
	// Total is everything spent since Since; Total.Unpriced counts the runs
	// of models without a known price, whose USD is 0 rather than free.
	Total factory.Spend
	// ByState counts the tasks active since Since by state.
	ByState map[factory.State]int
	// Top are the most expensive of those tasks, dearest first.
	Top []TaskSpend
	// ByDay and ByModel break the project's spend since Since down (filled by the host from its usage ledger; empty
	// without one). Days are UTC dates, oldest first; models dearest first.
	ByDay   []Breakdown
	ByModel []Breakdown
}

// Breakdown is the spend of one day or one model, cache reads and writes apart.
type Breakdown struct {
	Key              string
	USD              float64
	Runs             int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	Unpriced         int64
}

// TaskSpend is one task of a Report.
type TaskSpend struct {
	ID    string
	Title string
	State factory.State
	Spent factory.Spend
}

// Summary reports the spend of the project's tasks ("" project: all) created
// or updated since the given time.
func Summary(ctx context.Context, store factory.Store, projectID string, since time.Time) (Report, error) {
	total, err := store.SpendSince(ctx, projectID, since)
	if err != nil {
		return Report{}, fmt.Errorf("budget: total spend: %w", err)
	}
	tasks, err := store.ListTasks(ctx, factory.Filter{ProjectID: projectID, Limit: 1 << 20})
	if err != nil {
		return Report{}, fmt.Errorf("budget: listing tasks: %w", err)
	}
	r := Report{ProjectID: projectID, Since: since, Total: total, ByState: map[factory.State]int{}}
	for _, t := range tasks {
		if t.UpdatedAt.Before(since) && t.CreatedAt.Before(since) {
			continue
		}
		r.ByState[t.State]++
		if t.Spent.USD > 0 || t.Spent.Tokens > 0 {
			r.Top = append(r.Top, TaskSpend{ID: t.ID, Title: t.Title, State: t.State, Spent: t.Spent})
		}
	}
	sort.Slice(r.Top, func(i, j int) bool {
		a, b := r.Top[i], r.Top[j]
		if a.Spent.USD != b.Spent.USD {
			return a.Spent.USD > b.Spent.USD
		}
		if a.Spent.Tokens != b.Spent.Tokens {
			return a.Spent.Tokens > b.Spent.Tokens
		}
		return a.ID < b.ID
	})
	if len(r.Top) > topTasks {
		r.Top = r.Top[:topTasks]
	}
	return r, nil
}
