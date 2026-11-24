// Package insights turns a project's tasks, requirements, workflow runs, audit trail and usage ledger into a handful
// of plain answers: what the agents delivered, what it cost, how much of the time a person was needed. It is pure
// computation over values the caller loads (no stores, no clock, no I/O), so every number is testable with fixtures.
//
// Definitions the numbers rest on (they are also said in the UI and the report):
//
//   - Started: tasks created in the range (delegated sub-tasks are not counted).
//   - Merged: tasks that ended "done" with a pull request, finished in the range. Done without a pull request counts
//     as Completed. Failed, Cancelled: finished in the range. Blocked: waiting for a person right now.
//   - Cycle time: first run to finish, for merged tasks.
//   - Waiting on a human: the time a task spent in state "blocked" (an approval, a question, "ready to merge").
//   - Rework round: a red CI run or a review that asked for changes on the task's pull request.
//   - First pass: merged with no rework round and a single attempt.
//   - Days are cut in the caller's time zone.
package insights

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/usage"
	"github.com/7ar-chat/harness/core/workflows"
)

// MaxDays bounds a range (the day charts get one point per day).
const MaxDays = 366

// TaskInput is a task with its history.
type TaskInput struct {
	Task   factory.Task
	Events []factory.TaskEvent
}

// Input is everything Compute reads. Usage rows are the ledger's reports for the same range and project.
type Input struct {
	// From is inclusive, To exclusive; Now is the clock (open waits end at min(Now, To)).
	From, To, Now time.Time
	// Loc is the time zone of days and weeks (nil: UTC).
	Loc          *time.Location
	Tasks        []TaskInput
	Requirements []requirements.Requirement
	Runs         []workflows.Run
	Audit        []store.AuditEntry

	UsageTotal    usage.UsageTotals
	UsageByModel  []usage.UsageRow
	UsageByUser   []usage.UsageRow
	UsageDayModel []usage.UsageRow // key "day|model"
	// HasLedger says the usage figures come from the ledger; without it cost is summed from the tasks.
	HasLedger bool
}

// Result is the whole answer.
type Result struct {
	ProjectID   string    `json:"projectId,omitempty"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	Days        int       `json:"days"`
	TZ          string    `json:"tz"`
	GeneratedAt time.Time `json:"generatedAt"`
	// Summary is the one-paragraph plain-language answer.
	Summary string `json:"summary"`
	// Empty is true when nothing was delivered or spent in the range.
	Empty     bool          `json:"empty"`
	Delivery  Delivery      `json:"delivery"`
	Flow      Flow          `json:"flow"`
	Cost      Cost          `json:"cost"`
	Quality   Quality       `json:"quality"`
	People    []Person      `json:"people"`
	Workflows WorkflowStats `json:"workflows"`
	Attention Attention     `json:"attention"`
	// Tasks is the table behind the numbers (one row per task that started or finished in the range).
	Tasks []TaskRow `json:"tasks"`
}

// Delivery answers "what did the agents deliver, and how fast".
type Delivery struct {
	Started   int `json:"started"`
	Merged    int `json:"merged"`
	Completed int `json:"completed"` // done without a pull request
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	// Blocked is how many tasks wait for a person right now.
	Blocked int `json:"blocked"`
	// MergeRate is merged / (merged + failed + cancelled), 0 when none finished.
	MergeRate float64 `json:"mergeRate"`
	// FirstPassRate is the share of merged tasks needing no rework and one attempt.
	FirstPassRate float64 `json:"firstPassRate"`
	// Cycle* are seconds from first run to merge.
	CycleMedianSec float64 `json:"cycleMedianSec"`
	CycleP90Sec    float64 `json:"cycleP90Sec"`
	// WaitMedianSec is the median time a task waited on a human (over tasks that waited at all).
	WaitMedianSec float64 `json:"waitMedianSec"`
	// NeededHumanOverDay counts tasks that waited a day or more.
	NeededHumanOverDay int `json:"neededHumanOverDay"`
	// ReworkPerPR is the mean rework rounds per merged task; ReworkSpread[i] counts merged tasks with i rounds (last: 3+).
	ReworkPerPR  float64   `json:"reworkPerPr"`
	ReworkSpread [4]int    `json:"reworkSpread"`
	CI           CountPair `json:"ci"`
	// WorkingSec and WaitingSec split the cycle time of merged tasks between agent work and waiting on a person.
	WorkingSec float64 `json:"workingSec"`
	WaitingSec float64 `json:"waitingSec"`
	// CycleBuckets is the cycle-time distribution.
	CycleBuckets []Bucket `json:"cycleBuckets"`
	ByRole       []Count  `json:"byRole"`
	ByExecutor   []Count  `json:"byExecutor"`
}

// CountPair is a pass and a fail count.
type CountPair struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

// Bucket is one bar of a distribution.
type Bucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Count is a named count.
type Count struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Flow is about requirements and throughput.
type Flow struct {
	RequirementsOpened int `json:"requirementsOpened"`
	RequirementsDone   int `json:"requirementsDone"`
	// Weeks is merged tasks per week (weeks start Monday, in the range's time zone).
	Weeks []WeekPoint `json:"weeks"`
}

// WeekPoint is one week of throughput.
type WeekPoint struct {
	Week    string `json:"week"` // the Monday, 2026-11-02
	Merged  int    `json:"merged"`
	Started int    `json:"started"`
}

// Cost is what the work cost.
type Cost struct {
	TotalUSD float64 `json:"totalUsd"`
	// PerMergedUSD is TotalUSD / merged PRs (0 when none merged).
	PerMergedUSD float64 `json:"perMergedUsd"`
	// UnpricedRuns counts runs of models without a price: their cost is missing from the total, not zero.
	UnpricedRuns int `json:"unpricedRuns"`
	Runs         int `json:"runs"`
	// Tokens by kind over the range.
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	// Models lists every model seen (stable order, dearest first): the stacks of ByDay refer to them.
	Models     []ModelCost `json:"models"`
	ByDay      []DayCost   `json:"byDay"`
	ByRole     []RoleCost  `json:"byRole"`
	ByExecutor []RoleCost  `json:"byExecutor"`
	TopTasks   []TaskRef   `json:"topTasks"`
}

// ModelCost is the usage of one model, cache reads and writes apart.
type ModelCost struct {
	Model            string  `json:"model"`
	Runs             int64   `json:"runs"`
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	CostUSD          float64 `json:"costUsd"`
	Unpriced         bool    `json:"unpriced,omitempty"`
}

// DayCost is a day's spend split by model.
type DayCost struct {
	Day     string             `json:"day"`
	USD     float64            `json:"usd"`
	ByModel map[string]float64 `json:"byModel,omitempty"`
}

// RoleCost is tasks and spend of a role or an executor.
type RoleCost struct {
	Name  string  `json:"name"`
	Tasks int     `json:"tasks"`
	USD   float64 `json:"usd"`
}

// TaskRef points at a task with the one number it is listed for.
type TaskRef struct {
	ID    string        `json:"id"`
	Title string        `json:"title"`
	State factory.State `json:"state"`
	// Value is the number it is listed for: dollars, seconds or rounds, by list.
	Value float64 `json:"value"`
	// Detail is a short plain-language reason ("blocked for 2d 4h: ready to merge").
	Detail string `json:"detail,omitempty"`
}

// Quality is about the gates in front of a merge.
type Quality struct {
	ReviewApproved int `json:"reviewApproved"`
	ReviewBlocked  int `json:"reviewBlocked"`
	// ReviewBlockRate is blocked / (approved + blocked).
	ReviewBlockRate float64 `json:"reviewBlockRate"`
	PolicyDenied    int     `json:"policyDenied"`
	PolicyAsked     int     `json:"policyAsked"`
	// LoopGuardTrips counts tasks stopped for repeating themselves (recorded as budget events).
	LoopGuardTrips int `json:"loopGuardTrips"`
	Escalations    int `json:"escalations"`
}

// Person is what one member did.
type Person struct {
	Login     string  `json:"login"`
	Sessions  int     `json:"sessions"`
	Runs      int64   `json:"runs"`
	CostUSD   float64 `json:"costUsd"`
	Approvals int     `json:"approvals"` // approvals decided (allowed or refused)
	Denied    int     `json:"denied"`
}

// WorkflowStats is about workflow runs started in the range.
type WorkflowStats struct {
	Runs        int     `json:"runs"`
	Done        int     `json:"done"`
	Failed      int     `json:"failed"`
	SuccessRate float64 `json:"successRate"`
	// SlowestNode is the step with the longest average duration (gates excluded).
	SlowestNode    string  `json:"slowestNode,omitempty"`
	SlowestNodeSec float64 `json:"slowestNodeSec,omitempty"`
	// GateWaits and GateWaitMedianSec say how long gates (human steps) waited.
	GateWaits         int     `json:"gateWaits"`
	GateWaitMedianSec float64 `json:"gateWaitMedianSec"`
	GateWaitTotalSec  float64 `json:"gateWaitTotalSec"`
}

// Attention lists the tasks worth a look.
type Attention struct {
	Slowest        []TaskRef `json:"slowest"`
	BlockedLongest []TaskRef `json:"blockedLongest"`
	MostExpensive  []TaskRef `json:"mostExpensive"`
	MostReworked   []TaskRef `json:"mostReworked"`
}

// TaskRow is one line of the task table.
type TaskRow struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	Role         string        `json:"role"`
	State        factory.State `json:"state"`
	Source       string        `json:"source"`
	CreatedAt    time.Time     `json:"createdAt"`
	FinishedAt   time.Time     `json:"finishedAt,omitzero"`
	CycleSec     float64       `json:"cycleSec"`
	HumanWaitSec float64       `json:"humanWaitSec"`
	Rework       int           `json:"rework"`
	Attempts     int           `json:"attempts"`
	CostUSD      float64       `json:"costUsd"`
	Tokens       int64         `json:"tokens"`
	Unpriced     bool          `json:"unpriced,omitempty"`
	PRURL        string        `json:"prUrl,omitempty"`
	Executors    string        `json:"executors,omitempty"`
}

// perTask is what Compute derives from one task's history.
type perTask struct {
	TaskInput
	firstRun   time.Time
	wait       time.Duration // total time blocked, up to the end of the range
	blockedNow time.Duration // how long the open block has lasted (0 when not blocked)
	blockWhy   string
	rework     int
	ciFailed   int
	ciPassed   int
	executors  []string
	execUSD    float64
}

const (
	roleDefault = "coder"
	noExecutor  = "built-in agent"
)

// Compute answers for in.
func Compute(in Input) Result {
	loc := in.Loc
	if loc == nil {
		loc = time.UTC
	}
	res := Result{From: in.From, To: in.To, TZ: loc.String(), GeneratedAt: in.Now.UTC()}
	res.Days = int(math.Ceil(in.To.Sub(in.From).Hours() / 24))
	res.People, res.Tasks = []Person{}, []TaskRow{}
	end := in.To
	if in.Now.Before(end) {
		end = in.Now
	}
	inRange := func(t time.Time) bool { return !t.IsZero() && !t.Before(in.From) && t.Before(in.To) }

	var (
		d          = &res.Delivery
		cycles     []float64
		waits      []float64
		reworkSum  int
		byRole     = map[string]*RoleCost{}
		byExec     = map[string]*RoleCost{}
		roleCount  = map[string]int{}
		execCount  = map[string]int{}
		weekStart  = func(t time.Time) string { return weekOf(t, loc) }
		weeks      = map[string]*WeekPoint{}
		taskCostIn float64
		unpricedT  int
		tasks      []perTask
	)
	for _, ti := range in.Tasks {
		t := ti.Task
		if t.Source.Kind == factory.SourceDelegate {
			continue
		}
		pt := analyze(ti, end)
		started := inRange(t.CreatedAt)
		finished := t.State.Terminal() && inRange(t.FinishedAt)
		if !started && !finished && !(t.State == factory.StateBlocked && !t.CreatedAt.After(in.To)) {
			continue
		}
		tasks = append(tasks, pt)
		role := t.Role
		if role == "" {
			role = roleDefault
		}
		if started {
			d.Started++
			roleCount[role]++
			for _, e := range executorNames(pt) {
				execCount[e]++
			}
			week(weeks, weekStart(t.CreatedAt)).Started++
		}
		if pt.wait > 0 {
			waits = append(waits, pt.wait.Seconds())
			if pt.wait >= 24*time.Hour {
				d.NeededHumanOverDay++
			}
		}
		if t.State == factory.StateBlocked {
			d.Blocked++
		}
		if finished || started {
			taskCostIn += t.Spent.USD
			if t.Spent.Unpriced > 0 {
				unpricedT++
			}
			rc := byRole[role]
			if rc == nil {
				rc = &RoleCost{Name: role}
				byRole[role] = rc
			}
			rc.Tasks++
			rc.USD += t.Spent.USD
			for _, e := range executorNames(pt) {
				ec := byExec[e]
				if ec == nil {
					ec = &RoleCost{Name: e}
					byExec[e] = ec
				}
				ec.Tasks++
			}
			if len(pt.executors) > 0 {
				share := pt.execUSD / float64(len(pt.executors))
				for _, e := range pt.executors {
					byExec[e].USD += share
				}
			}
		}
		if !finished {
			continue
		}
		switch t.State {
		case factory.StateFailed:
			d.Failed++
		case factory.StateCancelled:
			d.Cancelled++
		case factory.StateDone:
			if t.PR.Number == 0 && t.PR.URL == "" {
				d.Completed++
				continue
			}
			d.Merged++
			week(weeks, weekStart(t.FinishedAt)).Merged++
			cyc := t.FinishedAt.Sub(pt.firstRun)
			if cyc < 0 {
				cyc = 0
			}
			cycles = append(cycles, cyc.Seconds())
			wait := min(pt.wait, cyc)
			d.WaitingSec += wait.Seconds()
			d.WorkingSec += (cyc - wait).Seconds()
			reworkSum += pt.rework
			d.ReworkSpread[min(pt.rework, 3)]++
			d.CI.Passed += pt.ciPassed
			d.CI.Failed += pt.ciFailed
			if pt.rework == 0 && t.Attempts <= 1 {
				d.FirstPassRate++ // counted, turned into a share below
			}
		}
	}
	if fin := d.Merged + d.Failed + d.Cancelled; fin > 0 {
		d.MergeRate = float64(d.Merged) / float64(fin)
	}
	if d.Merged > 0 {
		d.FirstPassRate /= float64(d.Merged)
		d.ReworkPerPR = float64(reworkSum) / float64(d.Merged)
	}
	d.CycleMedianSec, d.CycleP90Sec = Percentile(cycles, 0.5), Percentile(cycles, 0.9)
	d.WaitMedianSec = Percentile(waits, 0.5)
	d.CycleBuckets = cycleBuckets(cycles)
	d.ByRole, d.ByExecutor = counts(roleCount), counts(execCount)

	res.Flow = flow(in, loc, inRange, weeks)
	res.Cost = cost(in, d.Merged, byRole, byExec, tasks, taskCostIn, unpricedT, loc)
	res.Quality = quality(tasks, inRange)
	res.People = people(in, inRange)
	res.Workflows = workflowStats(in.Runs, inRange)
	res.Attention = attention(tasks, end)
	res.Tasks = rows(tasks)
	res.Empty = d.Started == 0 && d.Merged == 0 && d.Failed == 0 && d.Cancelled == 0 && d.Blocked == 0 &&
		res.Cost.TotalUSD == 0 && res.Cost.Runs == 0 && res.Workflows.Runs == 0
	res.Summary = Summarize(res)
	return res
}

// analyze reads one task's history.
func analyze(ti TaskInput, end time.Time) perTask {
	pt := perTask{TaskInput: ti}
	evs := append([]factory.TaskEvent(nil), ti.Events...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.Before(evs[j].At) })
	var blockedAt time.Time
	seen := map[string]bool{}
	for _, e := range evs {
		switch e.Kind {
		case factory.EventRun:
			if pt.firstRun.IsZero() {
				pt.firstRun = e.At
			}
		case factory.EventState:
			to, _ := e.Data["to"].(string)
			if to == string(factory.StateRunning) && pt.firstRun.IsZero() {
				pt.firstRun = e.At
			}
			if to == string(factory.StateBlocked) {
				if blockedAt.IsZero() {
					blockedAt = e.At
					pt.blockWhy = e.Text
				}
			} else if !blockedAt.IsZero() {
				pt.wait += max(0, e.At.Sub(blockedAt))
				blockedAt = time.Time{}
			}
		case factory.EventCI:
			switch s, _ := e.Data["state"].(string); s {
			case "failed":
				pt.ciFailed++
				pt.rework++
			case "passed":
				pt.ciPassed++
			}
		case factory.EventReview:
			if v, _ := e.Data["verdict"].(string); v == "changes" {
				pt.rework++
			}
		case factory.EventExecutor:
			if name, _ := e.Data["executor"].(string); name != "" && !seen[name] {
				seen[name] = true
				pt.executors = append(pt.executors, name)
			}
			if usd, ok := e.Data["usd"].(float64); ok {
				pt.execUSD += usd
			}
		}
	}
	if pt.firstRun.IsZero() {
		pt.firstRun = ti.Task.CreatedAt
	}
	if !blockedAt.IsZero() {
		if ti.Task.State == factory.StateBlocked {
			pt.blockedNow = max(0, end.Sub(blockedAt))
			pt.wait += pt.blockedNow
		} else {
			pt.wait += max(0, end.Sub(blockedAt))
		}
	}
	if pt.blockedNow == 0 && ti.Task.State == factory.StateBlocked && !ti.Task.UpdatedAt.IsZero() {
		pt.blockedNow = max(0, end.Sub(ti.Task.UpdatedAt)) // the history had no block event
		pt.wait += pt.blockedNow
	}
	if pt.blockWhy == "" {
		pt.blockWhy = ti.Task.BlockedReason
	}
	sort.Strings(pt.executors)
	return pt
}

func executorNames(pt perTask) []string {
	if len(pt.executors) == 0 {
		return []string{noExecutor}
	}
	return pt.executors
}

func week(m map[string]*WeekPoint, key string) *WeekPoint {
	w := m[key]
	if w == nil {
		w = &WeekPoint{Week: key}
		m[key] = w
	}
	return w
}

// weekOf is the Monday (date) of the week t falls in, in loc.
func weekOf(t time.Time, loc *time.Location) string {
	t = t.In(loc)
	off := (int(t.Weekday()) + 6) % 7
	d := time.Date(t.Year(), t.Month(), t.Day()-off, 0, 0, 0, 0, loc)
	return d.Format("2006-01-02")
}

func flow(in Input, loc *time.Location, inRange func(time.Time) bool, weeks map[string]*WeekPoint) Flow {
	f := Flow{Weeks: []WeekPoint{}}
	for _, r := range in.Requirements {
		if inRange(r.CreatedAt) {
			f.RequirementsOpened++
		}
		if r.Status == requirements.StatusDone && inRange(r.UpdatedAt) {
			f.RequirementsDone++
		}
	}
	// One point per week of the range, including the empty ones.
	for w := parseDay(weekOf(in.From, loc), loc); w.Before(in.To); w = time.Date(w.Year(), w.Month(), w.Day()+7, 0, 0, 0, 0, loc) {
		key := w.Format("2006-01-02")
		if p := weeks[key]; p != nil {
			f.Weeks = append(f.Weeks, *p)
		} else {
			f.Weeks = append(f.Weeks, WeekPoint{Week: key})
		}
	}
	return f
}

func parseDay(s string, loc *time.Location) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", s, loc)
	return t
}

func cost(in Input, merged int, byRole, byExec map[string]*RoleCost, tasks []perTask, taskCost float64, unpricedTasks int, loc *time.Location) Cost {
	c := Cost{Models: []ModelCost{}, ByDay: []DayCost{}, ByRole: []RoleCost{}, ByExecutor: []RoleCost{}, TopTasks: []TaskRef{}}
	if in.HasLedger {
		u := in.UsageTotal
		c.TotalUSD, c.Runs, c.UnpricedRuns = u.CostUSD, int(u.Runs), int(u.Unpriced)
		c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheWriteTokens = u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens
	} else {
		c.TotalUSD, c.UnpricedRuns = taskCost, unpricedTasks
	}
	if merged > 0 {
		c.PerMergedUSD = c.TotalUSD / float64(merged)
	}
	for _, r := range in.UsageByModel {
		c.Models = append(c.Models, ModelCost{Model: r.Key, Runs: r.Runs, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			CacheReadTokens: r.CacheReadTokens, CacheWriteTokens: r.CacheWriteTokens, CostUSD: r.CostUSD, Unpriced: r.Unpriced > 0})
	}
	sort.SliceStable(c.Models, func(i, j int) bool {
		if c.Models[i].CostUSD != c.Models[j].CostUSD {
			return c.Models[i].CostUSD > c.Models[j].CostUSD
		}
		return c.Models[i].Model < c.Models[j].Model
	})
	days := map[string]*DayCost{}
	for _, r := range in.UsageDayModel {
		day, model, _ := strings.Cut(r.Key, "|")
		d := days[day]
		if d == nil {
			d = &DayCost{Day: day, ByModel: map[string]float64{}}
			days[day] = d
		}
		d.USD += r.CostUSD
		d.ByModel[model] += r.CostUSD
	}
	// A point for every day of the range, so a quiet day shows as zero rather than vanishing.
	for t := time.Date(in.From.In(loc).Year(), in.From.In(loc).Month(), in.From.In(loc).Day(), 0, 0, 0, 0, loc); t.Before(in.To) && len(c.ByDay) < MaxDays+1; t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc) {
		key := t.Format("2006-01-02")
		if d := days[key]; d != nil {
			c.ByDay = append(c.ByDay, *d)
			delete(days, key)
		} else {
			c.ByDay = append(c.ByDay, DayCost{Day: key})
		}
	}
	for _, v := range sortedRoleCost(byRole) {
		c.ByRole = append(c.ByRole, v)
	}
	for _, v := range sortedRoleCost(byExec) {
		c.ByExecutor = append(c.ByExecutor, v)
	}
	sorted := append([]perTask(nil), tasks...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Task.Spent.USD > sorted[j].Task.Spent.USD })
	for _, pt := range sorted {
		if pt.Task.Spent.USD <= 0 || len(c.TopTasks) == 5 {
			break
		}
		c.TopTasks = append(c.TopTasks, TaskRef{ID: pt.Task.ID, Title: pt.Task.Title, State: pt.Task.State, Value: pt.Task.Spent.USD})
	}
	return c
}

func sortedRoleCost(m map[string]*RoleCost) []RoleCost {
	out := make([]RoleCost, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tasks != out[j].Tasks {
			return out[i].Tasks > out[j].Tasks
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func counts(m map[string]int) []Count {
	out := make([]Count, 0, len(m))
	for k, v := range m {
		out = append(out, Count{Name: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func quality(tasks []perTask, inRange func(time.Time) bool) Quality {
	var q Quality
	loopy := map[string]bool{}
	for _, pt := range tasks {
		for _, e := range pt.Events {
			if !inRange(e.At) {
				continue
			}
			switch e.Kind {
			case factory.EventReview:
				switch v, _ := e.Data["verdict"].(string); v {
				case "approve":
					q.ReviewApproved++
				case "changes":
					q.ReviewBlocked++
				}
			case factory.EventPolicy:
				switch d, _ := e.Data["decision"].(string); d {
				case "deny", "denied":
					q.PolicyDenied++
				case "ask", "asked":
					q.PolicyAsked++
				}
			case factory.EventBudget:
				r, _ := e.Data["reason"].(string)
				r = strings.ToLower(r)
				if (strings.Contains(r, "repeat") || strings.Contains(r, "loop") || strings.Contains(r, "same way")) && !loopy[pt.Task.ID+e.Text] {
					loopy[pt.Task.ID+e.Text] = true
					q.LoopGuardTrips++
				}
			case factory.EventEscalated:
				q.Escalations++
			}
		}
	}
	if n := q.ReviewApproved + q.ReviewBlocked; n > 0 {
		q.ReviewBlockRate = float64(q.ReviewBlocked) / float64(n)
	}
	return q
}

func people(in Input, inRange func(time.Time) bool) []Person {
	m := map[string]*Person{}
	get := func(login string) *Person {
		p := m[login]
		if p == nil {
			p = &Person{Login: login}
			m[login] = p
		}
		return p
	}
	for _, r := range in.UsageByUser {
		if r.Key == "" {
			continue
		}
		p := get(r.Key)
		p.Runs, p.CostUSD = r.Runs, r.CostUSD
	}
	for _, a := range in.Audit {
		if !inRange(a.At) || a.Actor == "" || a.Actor == "factory" {
			continue
		}
		switch a.Action {
		case "thread.create":
			get(a.Actor).Sessions++
		case "run.resume", "approval.slack", "approval.telegram", "supervisor.approval":
			dec, _ := a.Detail["decision"].(string)
			if dec == "approve" || dec == "deny" || dec == "allow" {
				p := get(a.Actor)
				p.Approvals++
				if dec == "deny" {
					p.Denied++
				}
			}
		}
	}
	out := make([]Person, 0, len(m))
	for _, p := range m {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Runs != b.Runs {
			return a.Runs > b.Runs
		}
		return a.Login < b.Login
	})
	return out
}

func workflowStats(runs []workflows.Run, inRange func(time.Time) bool) WorkflowStats {
	var w WorkflowStats
	nodeSum := map[string]float64{}
	nodeN := map[string]int{}
	var gates []float64
	for _, r := range runs {
		if !inRange(r.CreatedAt) {
			continue
		}
		w.Runs++
		switch r.State {
		case workflows.RunDone:
			w.Done++
		case workflows.RunFailed:
			w.Failed++
		}
		kinds := map[string]workflows.Kind{}
		names := map[string]string{}
		for _, n := range r.Graph {
			kinds[n.ID] = n.Kind
			names[n.ID] = n.ID
		}
		for _, s := range r.Steps {
			if s.StartedAt.IsZero() || s.FinishedAt.IsZero() || s.FinishedAt.Before(s.StartedAt) {
				continue
			}
			sec := s.FinishedAt.Sub(s.StartedAt).Seconds()
			if kinds[s.NodeID] == workflows.KindGate {
				gates = append(gates, sec)
				w.GateWaitTotalSec += sec
				continue
			}
			key := r.WorkflowName + " / " + names[s.NodeID]
			nodeSum[key] += sec
			nodeN[key]++
		}
	}
	if fin := w.Done + w.Failed; fin > 0 {
		w.SuccessRate = float64(w.Done) / float64(fin)
	}
	keys := make([]string, 0, len(nodeSum))
	for k := range nodeSum {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if avg := nodeSum[k] / float64(nodeN[k]); avg > w.SlowestNodeSec {
			w.SlowestNode, w.SlowestNodeSec = k, avg
		}
	}
	w.GateWaits = len(gates)
	w.GateWaitMedianSec = Percentile(gates, 0.5)
	return w
}

func attention(tasks []perTask, end time.Time) Attention {
	a := Attention{Slowest: []TaskRef{}, BlockedLongest: []TaskRef{}, MostExpensive: []TaskRef{}, MostReworked: []TaskRef{}}
	top := func(refs []TaskRef) []TaskRef {
		sort.SliceStable(refs, func(i, j int) bool {
			if refs[i].Value != refs[j].Value {
				return refs[i].Value > refs[j].Value
			}
			return refs[i].ID < refs[j].ID
		})
		if len(refs) > 5 {
			refs = refs[:5]
		}
		if refs == nil {
			refs = []TaskRef{}
		}
		return refs
	}
	var slow, blocked, dear, rework []TaskRef
	for _, pt := range tasks {
		t := pt.Task
		ref := TaskRef{ID: t.ID, Title: t.Title, State: t.State}
		stop := t.FinishedAt
		if stop.IsZero() {
			stop = end
		}
		if sec := stop.Sub(pt.firstRun).Seconds(); sec > 0 && !pt.firstRun.IsZero() {
			r := ref
			r.Value, r.Detail = sec, "from first run to "+map[bool]string{true: "now", false: "finish"}[t.FinishedAt.IsZero()]
			slow = append(slow, r)
		}
		if t.State == factory.StateBlocked && pt.blockedNow > 0 {
			r := ref
			r.Value = pt.blockedNow.Seconds()
			r.Detail = "waiting " + Duration(pt.blockedNow.Seconds())
			if pt.blockWhy != "" {
				r.Detail += ": " + clip(pt.blockWhy, 120)
			}
			blocked = append(blocked, r)
		}
		if t.Spent.USD > 0 {
			r := ref
			r.Value = t.Spent.USD
			dear = append(dear, r)
		}
		if pt.rework > 0 {
			r := ref
			r.Value = float64(pt.rework)
			r.Detail = fmt.Sprintf("%d rework round(s)", pt.rework)
			rework = append(rework, r)
		}
	}
	a.Slowest, a.BlockedLongest, a.MostExpensive, a.MostReworked = top(slow), top(blocked), top(dear), top(rework)
	return a
}

func rows(tasks []perTask) []TaskRow {
	out := make([]TaskRow, 0, len(tasks))
	for _, pt := range tasks {
		t := pt.Task
		r := TaskRow{ID: t.ID, Title: t.Title, Role: t.Role, State: t.State, Source: t.Source.Kind, CreatedAt: t.CreatedAt, FinishedAt: t.FinishedAt,
			HumanWaitSec: pt.wait.Seconds(), Rework: pt.rework, Attempts: t.Attempts, CostUSD: t.Spent.USD, Tokens: t.Spent.Tokens,
			Unpriced: t.Spent.Unpriced > 0, PRURL: t.PR.URL, Executors: strings.Join(pt.executors, " ")}
		if r.Role == "" {
			r.Role = roleDefault
		}
		if !t.FinishedAt.IsZero() {
			r.CycleSec = max(0, t.FinishedAt.Sub(pt.firstRun).Seconds())
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Percentile is the nearest-rank percentile (p in 0..1) of v; 0 for no values. v is not modified.
func Percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	idx := int(math.Ceil(p*float64(len(s)))) - 1
	return s[min(max(idx, 0), len(s)-1)]
}

var bucketEdges = []struct {
	label string
	upto  float64 // seconds, exclusive
}{
	{"under 10 min", 600}, {"10 min to 1 h", 3600}, {"1 to 4 h", 4 * 3600}, {"4 to 24 h", 24 * 3600}, {"1 to 3 days", 3 * 24 * 3600}, {"over 3 days", math.Inf(1)},
}

func cycleBuckets(cycles []float64) []Bucket {
	out := make([]Bucket, len(bucketEdges))
	for i, e := range bucketEdges {
		out[i].Label = e.label
	}
	for _, c := range cycles {
		for i, e := range bucketEdges {
			if c < e.upto {
				out[i].Count++
				break
			}
		}
	}
	return out
}

// Duration formats seconds as "3h 10m", "2d 4h", "45s".
func Duration(sec float64) string {
	if sec < 1 {
		return "0s"
	}
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	}
	days, h := int(d.Hours())/24, int(d.Hours())%24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, h)
}

// Money formats dollars: "$41.20", with more digits under a cent.
func Money(usd float64) string {
	if usd > 0 && usd < 0.01 {
		return fmt.Sprintf("$%.4f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// Percent formats a 0..1 share as "82%".
func Percent(r float64) string { return fmt.Sprintf("%.0f%%", r*100) }

// Summarize is the plain-language paragraph at the top of the page and the report.
func Summarize(r Result) string {
	days := fmt.Sprintf("the last %d days", r.Days)
	if r.Days == 1 {
		days = "the last day"
	}
	d, c := r.Delivery, r.Cost
	if r.Empty {
		return fmt.Sprintf("In %s nothing was delivered yet. Insights appear after the first merged task.", days)
	}
	var b strings.Builder
	if d.Merged == 0 {
		fmt.Fprintf(&b, "In %s your agents started %d task%s and merged no pull requests", days, d.Started, plural(d.Started))
		if c.TotalUSD > 0 {
			fmt.Fprintf(&b, "; the work cost %s", Money(c.TotalUSD))
		}
	} else {
		fmt.Fprintf(&b, "In %s your agents merged %d PR%s (%s first-pass), cost %s (%s per PR), median cycle %s",
			days, d.Merged, plural(d.Merged), Percent(d.FirstPassRate), Money(c.TotalUSD), Money(c.PerMergedUSD), Duration(d.CycleMedianSec))
	}
	if d.NeededHumanOverDay > 0 {
		fmt.Fprintf(&b, "; %d task%s needed a human for more than a day", d.NeededHumanOverDay, plural(d.NeededHumanOverDay))
	}
	if d.Failed > 0 {
		fmt.Fprintf(&b, "; %d failed", d.Failed)
	}
	b.WriteString(".")
	if c.UnpricedRuns > 0 {
		fmt.Fprintf(&b, " %d run%s used a model without a known price, so the cost is a lower bound.", c.UnpricedRuns, plural(c.UnpricedRuns))
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
