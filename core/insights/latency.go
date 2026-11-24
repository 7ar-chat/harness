package insights

import (
	"sort"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

// Approval latency: how long a request waited for a person before that person decided it, per person and kind.
//
//   - tool: a risky tool call waiting in a session. The decision is the audit entry "run.resume" (approve or deny); the
//     request time is recorded in its detail as requestedAt (the approval's creation time). Decisions made before the
//     product recorded it have no request time and are left out ("n/a"), not guessed.
//   - task: a task blocked for a person (an escalation, a policy question, "ready to merge"). The decision is the audit
//     entry "task.resume"; the request is the moment the task became blocked, found in its history, so this works for
//     old tasks too.
//   - gate: a workflow gate. The decision is the audit entry "workflow.gate.approve" or "workflow.gate.send-back"; the
//     request time is recorded as requestedAt, else taken from the gate step when the step finished with the decision.

// Latency is a count and the median and total waiting time of requests a person decided (N 0: unknown, shown as n/a).
type Latency struct {
	N         int     `json:"n"`
	MedianSec float64 `json:"medianSec"`
	TotalSec  float64 `json:"totalSec"`
}

// Waits splits waiting on a person by what was waited for.
type Waits struct {
	Tools Latency `json:"tools"`
	Tasks Latency `json:"tasks"`
	Gates Latency `json:"gates"`
}

func latencyOf(sec []float64) Latency {
	l := Latency{N: len(sec), MedianSec: Percentile(sec, 0.5)}
	for _, s := range sec {
		l.TotalSec += s
	}
	return l
}

// waitSample is one decided request.
type waitSample struct {
	login string
	kind  string // tool, task or gate
	sec   float64
}

type span struct{ from, to time.Time }

// blockSpans lists the closed blocked intervals of a task, from its history.
func blockSpans(ti TaskInput) []span {
	evs := append([]factory.TaskEvent(nil), ti.Events...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.Before(evs[j].At) })
	var out []span
	var from time.Time
	for _, e := range evs {
		if e.Kind != factory.EventState {
			continue
		}
		to, _ := e.Data["to"].(string)
		switch {
		case to == string(factory.StateBlocked):
			if from.IsZero() {
				from = e.At
			}
		case !from.IsZero():
			out = append(out, span{from, e.At})
			from = time.Time{}
		}
	}
	return out
}

func detailTime(d map[string]any, key string) time.Time {
	s, _ := d[key].(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// matchSlack is how far an audit entry may be from the event it belongs to (the entry is written just after the action).
const matchSlack = 30 * time.Second

// waitSamples collects the decided requests whose decision falls in the range.
func waitSamples(in Input, inRange func(time.Time) bool) []waitSample {
	spans := map[string][]span{}
	for _, ti := range in.Tasks {
		if ss := blockSpans(ti); len(ss) > 0 {
			spans[ti.Task.ID] = ss
		}
	}
	steps := map[string]map[string][]stepTimes{}
	for _, r := range in.Runs {
		for _, s := range r.Steps {
			if steps[r.ID] == nil {
				steps[r.ID] = map[string][]stepTimes{}
			}
			steps[r.ID][s.NodeID] = append(steps[r.ID][s.NodeID], stepTimes{s.StartedAt, s.FinishedAt})
		}
	}
	var out []waitSample
	add := func(login, kind string, from, to time.Time) {
		if sec := to.Sub(from).Seconds(); !from.IsZero() && sec >= 0 {
			out = append(out, waitSample{login, kind, sec})
		}
	}
	for _, a := range in.Audit {
		if !inRange(a.At) || a.Actor == "" || a.Actor == "factory" || a.Actor == "supervisor" {
			continue
		}
		switch a.Action {
		case "run.resume":
			if dec, _ := a.Detail["decision"].(string); dec == "approve" || dec == "deny" || dec == "allow" {
				add(a.Actor, "tool", detailTime(a.Detail, "requestedAt"), a.At)
			}
		case "task.resume":
			id, _ := a.Detail["task"].(string)
			var best span
			for _, sp := range spans[id] {
				d := a.At.Sub(sp.to)
				if d < -matchSlack || d > matchSlack {
					continue
				}
				if best.to.IsZero() || abs(a.At.Sub(sp.to)) < abs(a.At.Sub(best.to)) {
					best = sp
				}
			}
			if !best.to.IsZero() {
				add(a.Actor, "task", best.from, best.to)
			}
		case "workflow.gate.approve", "workflow.gate.send-back":
			from := detailTime(a.Detail, "requestedAt")
			if from.IsZero() {
				run, _ := a.Detail["run"].(string)
				node, _ := a.Detail["node"].(string)
				for _, st := range steps[run][node] {
					if !st.started.IsZero() && !st.finished.IsZero() && !st.started.After(a.At) && abs(a.At.Sub(st.finished)) <= matchSlack {
						from = st.started
					}
				}
			}
			add(a.Actor, "gate", from, a.At)
		}
	}
	return out
}

type stepTimes struct{ started, finished time.Time }

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// personLatencies fills the latency of every person in m from the samples.
func personLatencies(samples []waitSample, get func(login string) *Person) {
	by := map[string]map[string][]float64{}
	for _, s := range samples {
		if by[s.login] == nil {
			by[s.login] = map[string][]float64{}
		}
		by[s.login][s.kind] = append(by[s.login][s.kind], s.sec)
	}
	for login, kinds := range by {
		p := get(login)
		var all []float64
		for _, v := range kinds {
			all = append(all, v...)
		}
		p.Latency = latencyOf(all)
		p.Waits = Waits{Tools: latencyOf(kinds["tool"]), Tasks: latencyOf(kinds["task"]), Gates: latencyOf(kinds["gate"])}
	}
}

// waiting is the breakdown behind the headline "time waiting on a human": every decided tool approval, every blocked
// period of a task that ended in the range, and every workflow gate wait.
func waiting(in Input, samples []waitSample, gates WorkflowStats, inRange func(time.Time) bool) Waits {
	var tools, tasks []float64
	for _, s := range samples {
		if s.kind == "tool" {
			tools = append(tools, s.sec)
		}
	}
	for _, ti := range in.Tasks {
		if ti.Task.Source.Kind == factory.SourceDelegate {
			continue
		}
		for _, sp := range blockSpans(ti) {
			if inRange(sp.to) {
				tasks = append(tasks, sp.to.Sub(sp.from).Seconds())
			}
		}
	}
	return Waits{Tools: latencyOf(tools), Tasks: latencyOf(tasks),
		Gates: Latency{N: gates.GateWaits, MedianSec: gates.GateWaitMedianSec, TotalSec: gates.GateWaitTotalSec}}
}
