package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

// EmptyAnswerNudge is given (once per run) when the model stops without text or tool calls.
const EmptyAnswerNudge = "You stopped without answering. Continue the task, or give the user a short summary of what you did and found."

// LoopGuardResult replaces the 3rd consecutive identical tool call (%s: the tool name).
const LoopGuardResult = "Not executed: you called %s with identical arguments 3 times in a row. Do not repeat it; change your approach (inspect the error, read the file, try different arguments) or explain to the user why you are blocked."

// AlternationGuardResult replaces the 6th call of an A-B-A-B-A-B alternation (%s, %s: the tools).
const AlternationGuardResult = "Not executed: your last calls alternate between the same two calls (%s and %s) without progress. Stop and change your approach, or explain to the user what blocks you."

// RepeatedErrorNote is appended to a tool result whose error already occurred twice in this task.
const RepeatedErrorNote = "\n\n[note] This is the 3rd time this error occurred in this task: %s\nDo not retry the same way: read the error, inspect the inputs (read_file, search_files) and change the approach."

// RepeatedResultNote is appended to a call that returned the same result for
// the repeatedResultNote-th time in this task (%d: the count): a re-reading
// cycle the consecutive guards do not see (round 3, A2).
const RepeatedResultNote = "\n\n[note] You already ran this exact call %d times in this task and got the same result each time. Do not run it again: use the result you already have (keep notes in a file under /workspace/.work/ if you need them later), or change your approach."

// ConsecutiveErrorsNote is appended after failedCallsNote failed tool calls in a row.
const ConsecutiveErrorsNote = "\n\n[note] %d tool calls in a row have failed. Step back: re-read the errors, check your assumptions and try a different approach. If you need a decision or information only the user has, ask with ask_user."

// TooManyErrorsNote ends a run stuck on failing calls: the next answer is forced without tools.
const TooManyErrorsNote = "Too many tool calls in a row failed (%d). Do not call tools now: tell the user what you tried, what failed and what you need to continue."

const (
	maxStopReminders = 2
	// failedCallsNote and failedCallsStop are the consecutive failed calls
	// that get a note, and that stop tool use for a final answer.
	failedCallsNote = 5
	failedCallsStop = 8
	// sameErrorNote: the occurrences of one error that get a note.
	sameErrorNote = 3
	// reminderEvery: steps between budget and plan reminders.
	reminderEvery = 8
	// repeatedResultNote: the identical results of one call that get a note.
	repeatedResultNote = 3
)

// guardState is the per-run state of the loop and stuck guards, the nudge and the reminders.
type guardState struct {
	guards []tools.StopGuard
	// recent are the signatures of the last executed tool calls (at most 5).
	recent    []string
	nudged    bool
	reminders int
	// failedInRow counts consecutive failed tool calls; errors counts each
	// error text of the turn; forceAnswer makes the next step answer without tools.
	failedInRow int
	errors      map[string]int
	forceAnswer bool
	// plan is the turn's last plan (budget reminders).
	plan []store.PlanItem
	// turn are the earlier calls of the turn (resumed run), for TurnObserver tools.
	turn []tools.TurnCall
	// polling are the job_output signatures whose last result was a running
	// job: polling it again is progress, never a loop.
	polling map[string]bool
	// results counts, per call signature and result, the identical results of the turn.
	results map[[32]byte]int
	// pushed is set once the run was told to write a partial deliverable;
	// called counts the run's tool calls.
	pushed bool
	called int
	// asksFile: the turn is meant to produce a file (the model's text or a
	// call's arguments name /workspace/outputs/); inPlace: it changed an
	// existing workspace file outside /workspace/outputs/ (F3).
	asksFile bool
	inPlace  bool
}

// stopGuards lists the run's StopGuard tools in tool-name order.
func stopGuards(toolMap map[string]tool.BaseTool) []tools.StopGuard {
	names := make([]string, 0, len(toolMap))
	for n := range toolMap {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []tools.StopGuard
	for _, n := range names {
		if g, ok := toolMap[n].(tools.StopGuard); ok {
			out = append(out, g)
		}
	}
	return out
}

// callSignature identifies a call by tool name and canonical arguments (sorted keys, no spacing).
func callSignature(tc schema.ToolCall) string {
	args := strings.TrimSpace(tc.Function.Arguments)
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		if b, err := json.Marshal(v); err == nil {
			args = string(b)
		}
	}
	return tc.Function.Name + "\x00" + args
}

// looping records a call about to run and returns the refusal when it is the
// 3rd identical call in a row or completes an A-B-A-B-A-B alternation ("": run
// it). Polling a job that was still running (job_output) is never a loop.
func (g *guardState) looping(tc schema.ToolCall) string {
	if refusal := g.peekLoop(tc); refusal != "" {
		return refusal
	}
	g.push(callSignature(tc))
	return ""
}

// peekLoop is looping without recording the call (before an approval is asked:
// a call the guard would refuse is refused without asking the user).
func (g *guardState) peekLoop(tc schema.ToolCall) string {
	sig := callSignature(tc)
	if g.polling[sig] {
		return "" // waiting for a running background job
	}
	n := len(g.recent)
	if n >= 2 && g.recent[n-1] == sig && g.recent[n-2] == sig {
		return fmt.Sprintf(LoopGuardResult, tc.Function.Name)
	}
	if n >= 5 {
		a, b := g.recent[n-5], g.recent[n-4]
		if a != b && sig == b && g.recent[n-3] == a && g.recent[n-2] == b && g.recent[n-1] == a && !g.polling[a] {
			return fmt.Sprintf(AlternationGuardResult, strings.SplitN(a, "\x00", 2)[0], tc.Function.Name)
		}
	}
	return ""
}

func (g *guardState) push(sig string) {
	g.recent = append(g.recent[max(0, len(g.recent)-4):], sig)
}

// redirected forgets the recent calls: the model got new input (steer, nudge, reminder, note).
func (g *guardState) redirected() { g.recent = nil }

var digits = regexp.MustCompile(`[0-9]+`)

// errorKey normalizes an error text (numbers and spacing ignored).
func errorKey(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "Error: ")
	s = digits.ReplaceAllString(strings.Join(strings.Fields(s), " "), "#")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// observed records a call's outcome and returns the note to append to its result.
func (g *guardState) observed(tc schema.ToolCall, content string, failed bool) string {
	if tc.Function.Name == "job_output" {
		g.notePoll(callSignature(tc), content, failed)
	}
	if !failed {
		g.failedInRow = 0
		if tc.Function.Name == "update_plan" {
			if items := planOf(tc.Function.Arguments); items != nil {
				g.plan = items
			}
		}
		return g.repeated(tc, content)
	}
	g.failedInRow++
	if g.errors == nil {
		g.errors = map[string]int{}
	}
	key := errorKey(content)
	g.errors[key]++
	note := ""
	if g.errors[key] == sameErrorNote {
		note += fmt.Sprintf(RepeatedErrorNote, truncate(strings.TrimPrefix(strings.TrimSpace(content), "Error: "), 300))
	}
	switch {
	case g.failedInRow >= failedCallsStop:
		g.forceAnswer = true
	case g.failedInRow == failedCallsNote:
		note += fmt.Sprintf(ConsecutiveErrorsNote, g.failedInRow)
	}
	return note
}

// repeated counts the identical results of a call and returns the note of
// the repeatedResultNote-th one ("" otherwise; never for a running job's poll).
func (g *guardState) repeated(tc schema.ToolCall, content string) string {
	sig := callSignature(tc)
	if g.polling[sig] {
		return ""
	}
	if g.results == nil {
		g.results = map[[32]byte]int{}
	}
	key := sha256.Sum256([]byte(sig + "\x00" + content))
	g.results[key]++
	if n := g.results[key]; n == repeatedResultNote {
		return fmt.Sprintf(RepeatedResultNote, n)
	}
	return ""
}

// notePoll records whether a job_output call found its job still running.
func (g *guardState) notePoll(sig, content string, failed bool) {
	var res struct {
		Status string `json:"status"`
	}
	running := !failed && json.Unmarshal([]byte(content), &res) == nil && res.Status == "running"
	if !running {
		delete(g.polling, sig)
		return
	}
	if g.polling == nil {
		g.polling = map[string]bool{}
	}
	g.polling[sig] = true
}

// planOf parses update_plan arguments.
func planOf(args string) []store.PlanItem {
	var in struct {
		Items []store.PlanItem `json:"items"`
	}
	if json.Unmarshal([]byte(args), &in) != nil || in.Items == nil {
		return nil
	}
	return in.Items
}

// seedGuards restores the guards from the turn's earlier calls (A-21): the
// recent signatures, consecutive failures, error counts and the plan.
func (st *runState) seedGuards(turn []*store.Message) {
	g := &st.g
	results := map[string]*store.Message{}
	for _, m := range turn {
		if m.Role == store.RoleTool {
			results[m.ToolCallID] = m
		}
	}
	for _, m := range turn {
		if m.Role == store.RoleTool {
			g.noteInPlace(m.Metadata)
		}
		if m.Role != store.RoleAssistant {
			continue
		}
		var calls []schema.ToolCall
		for _, c := range m.ToolCalls {
			calls = append(calls, schema.ToolCall{Function: schema.FunctionCall{Name: c.Function.Name, Arguments: c.Function.Arguments}})
		}
		g.noteFileIntent(m.Content, calls)
		for _, c := range m.ToolCalls {
			tc := schema.ToolCall{ID: c.ID, Function: schema.FunctionCall{Name: c.Function.Name, Arguments: c.Function.Arguments}}
			r := results[c.ID]
			if r == nil {
				continue // not run yet (the resumed call)
			}
			failed := r.Error != ""
			g.push(callSignature(tc))
			_ = g.observed(tc, r.Content, failed)
			g.turn = append(g.turn, tools.TurnCall{Name: c.Function.Name, Arguments: c.Function.Arguments, Result: r.Content, Failed: failed})
		}
	}
	g.forceAnswer = false // a new run (a user's decision) gets tools again
}

// observeTurn gives the turn's earlier calls to the tools that keep state across runs.
func (st *runState) observeTurn(toolMap map[string]tool.BaseTool) {
	if len(st.g.turn) == 0 {
		return
	}
	for _, t := range toolMap {
		if o, ok := t.(tools.TurnObserver); ok {
			func() {
				defer func() {
					if p := recover(); p != nil {
						slog.Error("agent: turn observer panicked", "run_id", st.in.Run.ID, "panic", p)
					}
				}()
				o.ObserveTurn(st.g.turn)
			}()
		}
	}
}

// PartialDeliverableReminder is given once, lateSteps() steps before the step
// limit, to a run with a sandbox that has not written any deliverable yet
// (%d: the steps left). Round 3 (A2).
const PartialDeliverableReminder = "<system-reminder>\nOnly %d steps left and no deliverable has been written yet. If the user asked for a file, write what you have to /workspace/outputs/ now (a partial deliverable, marked as partial, is better than none: a run that ends without the file it was asked for is reported as incomplete), then finish; otherwise answer with what you found.\n</system-reminder>"

// MsgStepLimitNoDeliverable is the RUN_ERROR max_steps of a run that used all
// its steps without producing the file it was meant to write (its last
// answer is kept).
const MsgStepLimitNoDeliverable = "The agent reached its step limit before producing a deliverable; its last answer is partial."

// minPushSteps: smaller step budgets get no partial-deliverable reminder.
const minPushSteps = 10

// lateSteps is when the partial-deliverable reminder comes: max(3, 10 % of the steps) before the limit.
func (st *runState) lateSteps() int { return max(3, st.a.MaxSteps/10) }

// wroteDeliverable reports whether the turn created or changed a file under /workspace/outputs/.
func (st *runState) wroteDeliverable() bool {
	for _, d := range st.v3.verify.files {
		if d.changed > 0 {
			return true
		}
	}
	return false
}

// produced reports whether the turn has a result file: a deliverable, or an
// existing workspace file changed in place (an upload edited where it is).
func (st *runState) produced() bool { return st.g.inPlace || st.wroteDeliverable() }

// missingFile reports a run that ran out of steps without the file it was
// meant to write (final review of round 3, F3): it called tools, produced no
// file, and its own text or a call's arguments named /workspace/outputs/ (a
// plan item, a write, a command that began writing there). A question or an
// analysis answered from what the tools returned is not one.
func (st *runState) missingFile() bool {
	return st.in.Sandbox && st.g.called > 0 && st.g.asksFile && !st.produced()
}

// outputsMention matches a path under /workspace/outputs/ (absolute, or
// relative to /workspace).
var outputsMention = regexp.MustCompile(`/workspace/outputs\b|(?:^|[^\w./-])outputs/`)

// noteFileIntent records an assistant message that names /workspace/outputs/
// (in its text or in a tool call's arguments).
func (g *guardState) noteFileIntent(content string, calls []schema.ToolCall) {
	if g.asksFile {
		return
	}
	g.asksFile = outputsMention.MatchString(content)
	for _, tc := range calls {
		g.asksFile = g.asksFile || outputsMention.MatchString(tc.Function.Arguments)
	}
}

// noteInPlace records an existing workspace file outside /workspace/outputs/
// and /workspace/.work/ changed by a call (artifact snapshot metadata).
func (g *guardState) noteInPlace(metadata map[string]any) {
	items, _ := metadata["artifacts"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		p, _ := m["path"].(string)
		if change, _ := m["change"].(string); change == store.ChangeUpdated && !isDeliverable(p) && !strings.HasPrefix(p, "/workspace/.work/") {
			g.inPlace = true
		}
	}
}

// budgetReminder adds a reminder every reminderEvery steps of a run with
// tool calls: steps left, open plan items, deliverables not yet verified; and,
// lateSteps() before the limit, the push to write a partial deliverable.
func (st *runState) budgetReminder(step int) {
	if left := st.a.MaxSteps - step + 1; st.a.MaxSteps >= minPushSteps && st.in.Sandbox && !st.g.pushed && left == st.lateSteps() && !st.produced() {
		st.g.pushed = true
		st.history = append(st.history, schema.UserMessage(fmt.Sprintf(PartialDeliverableReminder, left)))
	}
	if step <= 1 || (step-1)%reminderEvery != 0 || st.a.MaxSteps <= 0 {
		return
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("Step %d of %d (%d left).", step, st.a.MaxSteps, st.a.MaxSteps-step+1))
	var open []string
	for _, it := range st.g.plan {
		if it.Status != store.PlanCompleted {
			open = append(open, fmt.Sprintf("%q (%s)", it.Title, it.Status))
		}
	}
	if len(open) > 0 {
		parts = append(parts, fmt.Sprintf("Plan: %d of %d items open: %s.", len(open), len(st.g.plan), strings.Join(open, ", ")))
	}
	if _, unverified := st.verification(); len(unverified) > 0 {
		var names []string
		for _, u := range unverified {
			names = append(names, fmt.Sprint(u["path"]))
		}
		parts = append(parts, "Deliverables not verified yet: "+strings.Join(names, ", ")+".")
	}
	st.history = append(st.history, schema.UserMessage("<system-reminder>\n"+strings.Join(parts, "\n")+"\n</system-reminder>"))
}

// stopReminder asks the StopGuard tools (and the verification guard) whether the model may stop.
func (st *runState) stopReminder(ctx context.Context) string {
	g := &st.g
	if g.reminders >= maxStopReminders {
		return ""
	}
	var parts []string
	if r := st.verificationReminder(); r != "" {
		parts = append(parts, r)
	}
	for _, sg := range g.guards {
		if r := strings.TrimSpace(safeReminder(st.in.Run.ID, ctx, sg)); r != "" {
			parts = append(parts, r)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	g.reminders++
	return "<system-reminder>\n" + strings.Join(parts, "\n\n") + "\n</system-reminder>"
}

func safeReminder(runID string, ctx context.Context, g tools.StopGuard) (out string) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("agent: stop guard panicked", "run_id", runID, "panic", p)
			out = ""
		}
	}()
	return g.StopReminder(ctx)
}

// continueAfterAnswer decides, before steers are closed, whether an answer
// without tool calls ends the run: an empty answer gets one nudge, unfinished
// work a StopGuard reminder, a dangling link the answer checker's note. The
// returned note is given to the model as an unpersisted user message.
func (st *runState) continueAfterAnswer(ctx context.Context, answer string) string {
	note := ""
	switch {
	case ctx.Err() != nil || st.h.interrupt != nil:
		return ""
	case strings.TrimSpace(answer) == "" && !st.g.nudged:
		st.g.nudged = true
		note = EmptyAnswerNudge
	case st.g.forceAnswer:
		return "" // stuck on failing calls: this answer ends the run
	default:
		if note = st.stopReminder(ctx); note == "" && !st.h.answerChecked {
			st.h.answerChecked = true
			note = st.checkAnswer(ctx)
		}
	}
	if note != "" {
		st.g.redirected()
	}
	return note
}
