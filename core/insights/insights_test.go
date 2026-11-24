package insights_test

import (
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/insights"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/usage"
	"github.com/7ar-chat/harness/core/workflows"
)

var (
	from = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to   = from.AddDate(0, 0, 30)
	now  = to
)

func at(h int) time.Time { return from.Add(time.Duration(h) * time.Hour) }

func ev(kind string, when time.Time, data map[string]any) factory.TaskEvent {
	return factory.TaskEvent{Kind: kind, At: when, Data: data}
}

func state(when time.Time, s factory.State, text string) factory.TaskEvent {
	return factory.TaskEvent{Kind: factory.EventState, At: when, Text: text, Data: map[string]any{"to": string(s)}}
}

// merged builds a merged task that started at hour h0 and finished at h1, with extra history.
func merged(id string, h0, h1 int, spent float64, extra ...factory.TaskEvent) insights.TaskInput {
	evs := append([]factory.TaskEvent{ev(factory.EventRun, at(h0), nil)}, extra...)
	return insights.TaskInput{
		Task: factory.Task{ID: id, Title: "task " + id, State: factory.StateDone, Attempts: 1, PR: factory.PullRequest{Number: 1, URL: "https://x/pr/1"},
			CreatedAt: at(h0), FinishedAt: at(h1), Spent: factory.Spend{USD: spent, Tokens: 1000, Runs: 1}},
		Events: evs,
	}
}

func TestDeliveryAndCycle(t *testing.T) {
	// Arrange: one clean merge (2h), one merge after a red CI and a 10h wait on a human, one failed, one cancelled, one blocked.
	in := insights.Input{From: from, To: to, Now: now, HasLedger: true}
	in.Tasks = []insights.TaskInput{
		merged("a", 0, 2, 1.0, ev(factory.EventCI, at(1), map[string]any{"state": "passed"})),
		merged("b", 0, 20, 3.0,
			ev(factory.EventCI, at(2), map[string]any{"state": "failed"}),
			state(at(4), factory.StateBlocked, "ready to merge"), state(at(14), factory.StateReview, ""),
			ev(factory.EventReview, at(15), map[string]any{"verdict": "changes"})),
		{Task: factory.Task{ID: "c", State: factory.StateFailed, CreatedAt: at(1), FinishedAt: at(3)}},
		{Task: factory.Task{ID: "d", State: factory.StateCancelled, CreatedAt: at(1), FinishedAt: at(3)}},
		{Task: factory.Task{ID: "e", State: factory.StateBlocked, CreatedAt: at(5), UpdatedAt: at(5), BlockedReason: "needs you"},
			Events: []factory.TaskEvent{state(to.Add(-48*time.Hour), factory.StateBlocked, "needs you")}},
		{Task: factory.Task{ID: "sub", Source: factory.Source{Kind: factory.SourceDelegate}, State: factory.StateDone, CreatedAt: at(1), FinishedAt: at(2)}},
	}
	in.UsageTotal = usage.UsageTotals{CostUSD: 8, Runs: 4}

	// Act
	r := insights.Compute(in)

	// Assert
	d := r.Delivery
	if d.Started != 5 || d.Merged != 2 || d.Failed != 1 || d.Cancelled != 1 || d.Blocked != 1 {
		t.Fatalf("delivery = %+v", d)
	}
	if d.MergeRate != 0.5 || d.FirstPassRate != 0.5 {
		t.Fatalf("rates = %v %v", d.MergeRate, d.FirstPassRate)
	}
	if d.CycleMedianSec != 2*3600 || d.CycleP90Sec != 20*3600 {
		t.Fatalf("cycle = %v %v", d.CycleMedianSec, d.CycleP90Sec)
	}
	if d.ReworkPerPR != 1 || d.ReworkSpread[0] != 1 || d.ReworkSpread[2] != 1 || d.CI.Failed != 1 || d.CI.Passed != 1 {
		t.Fatalf("rework = %+v", d)
	}
	if d.WaitingSec != 10*3600 || d.WorkingSec != 12*3600 {
		t.Fatalf("working/waiting = %v %v", d.WorkingSec, d.WaitingSec)
	}
	if d.NeededHumanOverDay != 1 {
		t.Fatalf("over a day = %d", d.NeededHumanOverDay) // the blocked one has waited 48h
	}
	if r.Cost.PerMergedUSD != 4 || r.Cost.TotalUSD != 8 {
		t.Fatalf("cost = %+v", r.Cost)
	}
	if len(r.Attention.BlockedLongest) != 1 || r.Attention.BlockedLongest[0].ID != "e" || r.Attention.BlockedLongest[0].Value != 48*3600 {
		t.Fatalf("blocked = %+v", r.Attention.BlockedLongest)
	}
	if len(r.Attention.MostExpensive) != 2 || r.Attention.MostExpensive[0].ID != "b" {
		t.Fatalf("expensive = %+v", r.Attention.MostExpensive)
	}
	if len(r.Attention.MostReworked) != 1 || r.Attention.MostReworked[0].ID != "b" {
		t.Fatalf("reworked = %+v", r.Attention.MostReworked)
	}
	if !strings.Contains(r.Summary, "merged 2 PRs (50% first-pass)") || !strings.Contains(r.Summary, "$4.00 per PR") || !strings.Contains(r.Summary, "1 task needed a human for more than a day") {
		t.Fatalf("summary = %q", r.Summary)
	}
	if r.Empty {
		t.Fatal("not empty")
	}
}

func TestEmptyRange(t *testing.T) {
	r := insights.Compute(insights.Input{From: from, To: to, Now: now})
	if !r.Empty || r.Delivery.Merged != 0 || r.Delivery.MergeRate != 0 || r.Cost.PerMergedUSD != 0 {
		t.Fatalf("empty = %+v", r)
	}
	if len(r.Cost.ByDay) != 30 || len(r.Flow.Weeks) == 0 || !strings.Contains(r.Summary, "Insights appear after the first merged task") {
		t.Fatalf("shape: days=%d weeks=%d summary=%q", len(r.Cost.ByDay), len(r.Flow.Weeks), r.Summary)
	}
	if r.Tasks == nil || r.People == nil || r.Attention.Slowest == nil {
		t.Fatal("lists must be empty arrays, not null")
	}
}

func TestTasksOutsideTheRangeAreLeftOut(t *testing.T) {
	old := merged("old", -100, -90, 5)
	old.Task.CreatedAt = from.AddDate(0, 0, -40)
	old.Task.FinishedAt = from.AddDate(0, 0, -39)
	r := insights.Compute(insights.Input{From: from, To: to, Now: now, Tasks: []insights.TaskInput{old}})
	if r.Delivery.Merged != 0 || r.Delivery.Started != 0 || len(r.Tasks) != 0 {
		t.Fatalf("%+v", r.Delivery)
	}
}

func TestCostByDayUsesTheTimeZoneAndFillsQuietDays(t *testing.T) {
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	in := insights.Input{From: from, To: from.AddDate(0, 0, 3), Now: now, Loc: tokyo, HasLedger: true,
		UsageDayModel: []usage.UsageRow{{Key: "2026-11-02|m1", UsageTotals: usage.UsageTotals{CostUSD: 1}}, {Key: "2026-11-02|m2", UsageTotals: usage.UsageTotals{CostUSD: 2}}},
		UsageByModel: []usage.UsageRow{
			{Key: "m1", UsageTotals: usage.UsageTotals{CostUSD: 1, CacheReadTokens: 50, CacheWriteTokens: 5}},
			{Key: "m2", UsageTotals: usage.UsageTotals{CostUSD: 2, Unpriced: 1}}}}
	r := insights.Compute(in)
	if len(r.Cost.ByDay) != 4 { // Tokyo: Nov 1 09:00 to Nov 4 09:00 touches four dates
		t.Fatalf("days = %+v", r.Cost.ByDay)
	}
	var found bool
	for _, d := range r.Cost.ByDay {
		if d.Day == "2026-11-02" {
			found = true
			if d.USD != 3 || d.ByModel["m2"] != 2 {
				t.Fatalf("day = %+v", d)
			}
		} else if d.USD != 0 {
			t.Fatalf("quiet day = %+v", d)
		}
	}
	if !found {
		t.Fatal("day missing")
	}
	if r.Cost.Models[0].Model != "m2" || !r.Cost.Models[0].Unpriced || r.Cost.Models[1].CacheReadTokens != 50 {
		t.Fatalf("models = %+v", r.Cost.Models)
	}
}

func TestUsageGroupsCutDaysInTheZone(t *testing.T) {
	// 23:30 UTC on Nov 1 is already Nov 2 in Tokyo.
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	e := usage.Entry{RunID: "r", Model: "m", CostUSD: 1, At: time.Date(2026, 11, 1, 23, 30, 0, 0, time.UTC)}
	utc := usage.Aggregate([]usage.Entry{e}, usage.Query{Group: usage.GroupDayModel})
	jp := usage.Aggregate([]usage.Entry{e}, usage.Query{Group: usage.GroupDayModel, Loc: tokyo})
	if utc.Rows[0].Key != "2026-11-01|m" || jp.Rows[0].Key != "2026-11-02|m" {
		t.Fatalf("%v %v", utc.Rows, jp.Rows)
	}
}

func TestUnpricedIsFlaggedInTheSummary(t *testing.T) {
	in := insights.Input{From: from, To: to, Now: now, HasLedger: true, UsageTotal: usage.UsageTotals{CostUSD: 2, Runs: 2, Unpriced: 1},
		Tasks: []insights.TaskInput{merged("a", 0, 1, 2)}}
	r := insights.Compute(in)
	if r.Cost.UnpricedRuns != 1 || !strings.Contains(r.Summary, "lower bound") {
		t.Fatalf("%q", r.Summary)
	}
}

func TestWeeksStartOnMondayInTheZone(t *testing.T) {
	// 03:00 UTC on Monday Nov 2 is still Sunday evening in New York: that merge belongs to the week of Oct 26.
	ny, _ := time.LoadLocation("America/New_York")
	ti := merged("a", 0, 1, 0)
	ti.Task.FinishedAt = time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)
	r := insights.Compute(insights.Input{From: from, To: to, Now: now, Loc: ny, Tasks: []insights.TaskInput{ti}})
	got := map[string]int{}
	for _, w := range r.Flow.Weeks {
		got[w.Week] = w.Merged
	}
	if got["2026-10-26"] != 1 {
		t.Fatalf("weeks = %+v", r.Flow.Weeks)
	}
}

func TestRolesExecutorsQualityAndFlow(t *testing.T) {
	a := merged("a", 0, 2, 1,
		ev(factory.EventExecutor, at(1), map[string]any{"executor": "claude-code", "usd": 0.5}),
		ev(factory.EventReview, at(1), map[string]any{"verdict": "approve"}),
		ev(factory.EventPolicy, at(1), map[string]any{"decision": "deny"}),
		ev(factory.EventPolicy, at(1), map[string]any{"decision": "ask"}),
		ev(factory.EventBudget, at(1), map[string]any{"reason": "the same way three times"}))
	a.Task.Role = "reviewer"
	in := insights.Input{From: from, To: to, Now: now, Tasks: []insights.TaskInput{a, merged("b", 0, 1, 0)},
		Requirements: []requirements.Requirement{{CreatedAt: at(1), Status: requirements.StatusDone, UpdatedAt: at(2)}, {CreatedAt: at(3)}},
		Audit: []store.AuditEntry{
			{Actor: "ann", Action: "thread.create", At: at(1)},
			{Actor: "ann", Action: "run.resume", At: at(2), Detail: map[string]any{"decision": "approve"}},
			{Actor: "ann", Action: "run.resume", At: at(3), Detail: map[string]any{"decision": "deny"}},
			{Actor: "bob", Action: "run.resume", At: from.AddDate(0, 0, -5), Detail: map[string]any{"decision": "approve"}}},
		UsageByUser: []usage.UsageRow{{Key: "ann", UsageTotals: usage.UsageTotals{Runs: 3, CostUSD: 2}}}}
	r := insights.Compute(in)
	if len(r.Delivery.ByRole) != 2 {
		t.Fatalf("roles = %+v", r.Delivery.ByRole)
	}
	if len(r.Delivery.ByExecutor) != 2 || r.Cost.ByExecutor[0].Name == "" {
		t.Fatalf("executors = %+v / %+v", r.Delivery.ByExecutor, r.Cost.ByExecutor)
	}
	q := r.Quality
	if q.ReviewApproved != 1 || q.PolicyDenied != 1 || q.PolicyAsked != 1 || q.LoopGuardTrips != 1 {
		t.Fatalf("quality = %+v", q)
	}
	if r.Flow.RequirementsOpened != 2 || r.Flow.RequirementsDone != 1 {
		t.Fatalf("flow = %+v", r.Flow)
	}
	if len(r.People) != 1 || r.People[0].Sessions != 1 || r.People[0].Approvals != 2 || r.People[0].Denied != 1 || r.People[0].Runs != 3 {
		t.Fatalf("people = %+v", r.People)
	}
}

func TestWorkflows(t *testing.T) {
	run := workflows.Run{WorkflowName: "ship", CreatedAt: at(1), State: workflows.RunDone,
		Graph: []workflows.Node{{ID: "build", Kind: workflows.KindAgent}, {ID: "ok", Kind: workflows.KindGate}},
		Steps: []workflows.NodeRun{
			{NodeID: "build", StartedAt: at(1), FinishedAt: at(2)},
			{NodeID: "ok", StartedAt: at(2), FinishedAt: at(6)}}}
	failed := run
	failed.State = workflows.RunFailed
	r := insights.Compute(insights.Input{From: from, To: to, Now: now, Runs: []workflows.Run{run, failed}})
	w := r.Workflows
	if w.Runs != 2 || w.SuccessRate != 0.5 || w.SlowestNode != "ship / build" || w.SlowestNodeSec != 3600 || w.GateWaits != 2 || w.GateWaitMedianSec != 4*3600 {
		t.Fatalf("workflows = %+v", w)
	}
}

func TestFormatting(t *testing.T) {
	for in, want := range map[float64]string{0: "0s", 45: "45s", 600: "10m", 3*3600 + 600: "3h 10m", 2*86400 + 4*3600: "2d 4h", 86400: "1d"} {
		if got := insights.Duration(in); got != want {
			t.Errorf("Duration(%v) = %q, want %q", in, got, want)
		}
	}
	if insights.Money(0.004) != "$0.0040" || insights.Money(41.2) != "$41.20" {
		t.Error("money")
	}
	if insights.Percentile([]float64{4, 1, 3, 2}, 0.5) != 2 || insights.Percentile(nil, 0.9) != 0 {
		t.Error("percentile")
	}
}
