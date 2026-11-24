package insights_test

import (
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/insights"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/workflows"
)

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func person(r insights.Result, login string) insights.Person {
	for _, p := range r.People {
		if p.Login == login {
			return p
		}
	}
	return insights.Person{}
}

func TestApprovalLatencyPerPersonAndKind(t *testing.T) {
	// Arrange: ann decides three tool approvals (10m, 30m, 20m), bob a gate (4h) and a blocked task (10h); an older tool
	// approval has no request time and a decision outside the range is ignored.
	min := func(m int) time.Duration { return time.Duration(m) * time.Minute }
	blocked := merged("b", 0, 20, 1, state(at(4), factory.StateBlocked, "ready to merge"), state(at(14), factory.StateReady, ""))
	run := workflows.Run{ID: "run1", WorkflowName: "ship", CreatedAt: at(1), State: workflows.RunDone,
		Graph: []workflows.Node{{ID: "ok", Kind: workflows.KindGate}},
		Steps: []workflows.NodeRun{{NodeID: "ok", StartedAt: at(30), FinishedAt: at(34)}}}
	in := insights.Input{From: from, To: to, Now: now, Tasks: []insights.TaskInput{blocked}, Runs: []workflows.Run{run},
		Audit: []store.AuditEntry{
			{Actor: "ann", Action: "run.resume", At: at(40), Detail: map[string]any{"decision": "approve", "requestedAt": rfc(at(40).Add(-min(10)))}},
			{Actor: "ann", Action: "run.resume", At: at(41), Detail: map[string]any{"decision": "deny", "requestedAt": rfc(at(41).Add(-min(30)))}},
			{Actor: "ann", Action: "run.resume", At: at(42), Detail: map[string]any{"decision": "approve", "requestedAt": rfc(at(42).Add(-min(20)))}},
			{Actor: "ann", Action: "run.resume", At: at(43), Detail: map[string]any{"decision": "approve"}}, // before requests were recorded
			{Actor: "ann", Action: "run.resume", At: from.AddDate(0, 0, -3), Detail: map[string]any{"decision": "approve", "requestedAt": rfc(from.AddDate(0, 0, -3).Add(-time.Hour))}},
			{Actor: "bob", Action: "task.resume", At: at(14).Add(5 * time.Second), Detail: map[string]any{"task": "b"}},
			{Actor: "bob", Action: "workflow.gate.approve", At: at(34).Add(2 * time.Second), Detail: map[string]any{"run": "run1", "node": "ok"}}, // request time from the gate step
			{Actor: "supervisor", Action: "run.resume", At: at(44), Detail: map[string]any{"decision": "approve", "requestedAt": rfc(at(43))}},
		}}

	// Act
	r := insights.Compute(in)

	// Assert
	ann, bob := person(r, "ann"), person(r, "bob")
	if ann.Waits.Tools.N != 3 || ann.Waits.Tools.MedianSec != 20*60 || ann.Waits.Tools.TotalSec != 60*60 || ann.Latency.N != 3 || ann.Latency.MedianSec != 20*60 {
		t.Fatalf("ann = %+v", ann)
	}
	if ann.Waits.Tasks.N != 0 || ann.Waits.Gates.N != 0 {
		t.Fatalf("ann waits = %+v", ann.Waits)
	}
	if bob.Waits.Tasks.N != 1 || bob.Waits.Tasks.MedianSec != 10*3600 || bob.Waits.Gates.N != 1 || bob.Waits.Gates.MedianSec != 4*3600+2 || bob.Latency.N != 2 {
		t.Fatalf("bob = %+v", bob)
	}
	if bob.Waits.Tools.N != 0 {
		t.Fatalf("bob has tool waits: %+v", bob.Waits.Tools)
	}
	w := r.Waiting
	if w.Tools.N != 3 || w.Tasks.N != 1 || w.Tasks.TotalSec != 10*3600 || w.Gates.N != 1 || w.Gates.TotalSec != 4*3600 {
		t.Fatalf("waiting = %+v", w)
	}
}

func TestApprovalLatencyIsUnknownNotZeroWithoutRequestTimes(t *testing.T) {
	in := insights.Input{From: from, To: to, Now: now, Audit: []store.AuditEntry{
		{Actor: "ann", Action: "run.resume", At: at(2), Detail: map[string]any{"decision": "approve"}},
		{Actor: "ann", Action: "task.resume", At: at(3), Detail: map[string]any{"task": "gone"}},
		{Actor: "ann", Action: "workflow.gate.approve", At: at(4), Detail: map[string]any{"run": "x", "node": "y"}},
	}}
	r := insights.Compute(in)
	p := person(r, "ann")
	if p.Approvals != 1 || p.Latency.N != 0 || p.Waits != (insights.Waits{}) {
		t.Fatalf("ann = %+v", p)
	}
	if r.Waiting != (insights.Waits{}) {
		t.Fatalf("waiting = %+v", r.Waiting)
	}
}
