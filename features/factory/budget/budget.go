// Package budget meters and enforces what factory tasks spend. At the end of
// every run of a task thread it converts the run's token usage into money and
// adds it to the task; it blocks a task whose budget, the day's cap or its
// failure pattern says another attempt is not worth it; and during a run it
// stops tools once the task has no budget left, so the model can wrap up
// instead of being cut off.
package budget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/modelinfo"
)

// Defaults of Options.
const (
	DefaultWindow             = 24 * time.Hour
	DefaultRepeatLimit        = 3
	DefaultMaxToolCallsPerRun = 150
)

const (
	// maxRuns bounds the per-run state: runs that end are forgotten at once,
	// this only matters for runs whose end was never reported.
	maxRuns = 1024
	// maxSeen bounds the run ids remembered to make a repeated finish a no-op.
	maxSeen = 4096
	// refusalsBeforeCancel is how many refused calls of one run show the model
	// ignores the refusals; the run is then cancelled.
	refusalsBeforeCancel = 5
	// warnAt is the share of the budget at which the model is told to wrap up.
	warnAt = 0.8
	// persistTimeout bounds the store calls made after the run's own context
	// may already be cancelled.
	persistTimeout = 10 * time.Second
)

// Options configures a Guard.
type Options struct {
	Store      factory.Store
	Controller factory.Controller
	// Price returns the price of a model (modelinfo's Table.Lookup with the
	// provider hint bound). Nil, or a model it does not know: unpriced.
	Price func(model string) (modelinfo.Info, bool)
	// Daily caps the spend of all factory work (of each project when
	// PerProject) over Window. The zero value is no cap.
	Daily      factory.Budget
	PerProject bool
	// Window is the period Daily applies to (default 24h).
	Window time.Duration
	Now    func() time.Time
	// Cancel stops an in-flight run: used on a run that keeps calling tools
	// after being told the budget is gone. Nil: the refusals are all there is.
	Cancel func(ctx context.Context, runID string) error
	// Escalator is told when the controller could not block a task, so a
	// human still hears about it.
	Escalator factory.Escalator
	// RepeatLimit is the number of identical consecutive failures that block
	// a task (default 3).
	RepeatLimit int
	// MaxToolCallsPerRun caps the tool calls of one run of a task (default
	// 150; negative: no cap).
	MaxToolCallsPerRun int
	// Allow names tools that stay available when the budget is exhausted, on
	// top of the read-only ones the guard knows (the tools that report or
	// finish the work).
	Allow []string
}

// Guard enforces the budgets of factory tasks. It is safe for concurrent use.
type Guard struct {
	o     Options
	allow map[string]bool

	mu   sync.Mutex
	runs map[string]*runEntry
	seq  uint64
	seen map[string]struct{}
	ring []string
}

// runEntry is what a run in flight needs: its task (empty for an ordinary
// thread) and the counters of the mid-run checks.
type runEntry struct {
	taskID    string
	seq       uint64
	calls     int
	refusals  int
	noted     bool
	cancelled bool
}

// New returns a Guard. Store is required.
func New(o Options) *Guard {
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.RepeatLimit <= 0 {
		o.RepeatLimit = DefaultRepeatLimit
	}
	if o.MaxToolCallsPerRun == 0 {
		o.MaxToolCallsPerRun = DefaultMaxToolCallsPerRun
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	g := &Guard{o: o, allow: map[string]bool{}, runs: map[string]*runEntry{}, seen: map[string]struct{}{}}
	for n := range readOnlyTools {
		g.allow[n] = true
	}
	for _, n := range o.Allow {
		g.allow[n] = true
	}
	return g
}

// readOnlyTools are the tools that have no side effects or only touch the
// run's own plan, by name: the hooks see a tool's name, not the tool.
var readOnlyTools = map[string]bool{
	"read_file": true, "view_file": true, "search_files": true, "list_files": true,
	"job_output": true, "explore": true, "load_skill": true,
	"update_plan": true, "ask_user": true,
}

// Check tells the dispatcher whether another attempt of the task may start:
// its budget is not exhausted, the day's cap is not exhausted, and it is not
// failing the same way over and over.
func (g *Guard) Check(ctx context.Context, taskID string) (ok bool, reason string) {
	t, err := g.o.Store.GetTask(ctx, taskID)
	if err != nil {
		return false, fmt.Sprintf("loading task %s: %v", taskID, err)
	}
	if t.Budget.Over(t.Spent) {
		return false, budgetReason(t.Budget, t.Spent)
	}
	if factory.Repeating(t.Fingerprints, g.o.RepeatLimit) {
		return false, repeatReason(g.o.RepeatLimit, "")
	}
	spent, err := g.dailySpent(ctx, t.ProjectID)
	if err != nil {
		return false, err.Error()
	}
	if g.o.Daily.Over(spent) {
		return false, g.dailyReason(t.ProjectID, spent)
	}
	return true, ""
}

// dailySpent is what the project (all projects unless PerProject) spent in the
// window; zero when no daily cap is set.
func (g *Guard) dailySpent(ctx context.Context, projectID string) (factory.Spend, error) {
	if g.o.Daily == (factory.Budget{}) {
		return factory.Spend{}, nil
	}
	scope := ""
	if g.o.PerProject {
		scope = projectID
	}
	spent, err := g.o.Store.SpendSince(ctx, scope, g.o.Now().Add(-g.o.Window))
	if err != nil {
		return factory.Spend{}, fmt.Errorf("reading the spend of the last %s: %w", g.o.Window, err)
	}
	return spent, nil
}

func (g *Guard) dailyReason(projectID string, spent factory.Spend) string {
	where := "all projects"
	if g.o.PerProject {
		where = "project " + projectID
	}
	return fmt.Sprintf("daily budget exhausted: %s in the last %s (%s)", spendOf(g.o.Daily, spent), g.o.Window, where)
}

// block parks the task through the controller; when that is impossible a
// human is told directly.
func (g *Guard) block(ctx context.Context, t factory.Task, reason string) {
	var err error
	if g.o.Controller == nil {
		err = errors.New("no controller")
	} else {
		err = g.o.Controller.Block(ctx, t.ID, reason)
	}
	if err == nil {
		return
	}
	slog.Error("budget: blocking task failed", "task_id", t.ID, "reason", reason, "error", err)
	if g.o.Escalator != nil {
		g.o.Escalator.Escalate(ctx, t, reason)
	}
}

// budgetReason says which caps of b the spend s has reached.
func budgetReason(b factory.Budget, s factory.Spend) string {
	var parts []string
	if b.MaxUSD > 0 && s.USD >= b.MaxUSD {
		parts = append(parts, fmt.Sprintf("$%.2f spent of $%.2f", s.USD, b.MaxUSD))
	}
	if b.MaxTokens > 0 && s.Tokens >= b.MaxTokens {
		parts = append(parts, fmt.Sprintf("%d tokens used of %d", s.Tokens, b.MaxTokens))
	}
	return "budget exhausted: " + strings.Join(parts, ", ")
}

// spendOf describes the spend s against the caps of b that are set.
func spendOf(b factory.Budget, s factory.Spend) string {
	var parts []string
	if b.MaxUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f of $%.2f", s.USD, b.MaxUSD))
	}
	if b.MaxTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d tokens", s.Tokens, b.MaxTokens))
	}
	return strings.Join(parts, ", ")
}

func repeatReason(n int, errText string) string {
	r := fmt.Sprintf("failing the same way %d times", n)
	if errText = readable(errText); errText != "" {
		if rs := []rune(errText); len(rs) > 200 {
			errText = string(rs[:200]) + "…"
		}
		r += ": " + errText
	}
	return r
}

// readable is an error text on one line.
func readable(s string) string { return strings.Join(strings.Fields(s), " ") }

// fraction is the largest share of the capped axes of b that s has used.
func fraction(b factory.Budget, s factory.Spend) float64 {
	var f float64
	if b.MaxUSD > 0 {
		f = max(f, s.USD/b.MaxUSD)
	}
	if b.MaxTokens > 0 {
		f = max(f, float64(s.Tokens)/float64(b.MaxTokens))
	}
	return f
}
