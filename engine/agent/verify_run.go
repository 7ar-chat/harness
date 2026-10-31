package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/agui"
)

// verificationItem is the JSON form of a check result in a tool result and
// its metadata (modelRun only in the metadata: the order it is replayed in).
func verificationItem(r CheckResult, meta, modelRun bool) map[string]any {
	item := map[string]any{"path": r.Path, "checker": r.Checker, "ok": r.OK, "summary": r.Summary}
	if !r.OK && r.NextStep != "" {
		item["nextStep"] = truncate(r.NextStep, 400)
	}
	if meta {
		item["failed"] = r.Failed
		if modelRun {
			item["modelRun"] = true
		}
	}
	return item
}

// pendingNote tells the model which changed files are checked later.
const pendingNote = "not checked yet (more than %d files changed in one call): checked before your next step"

// withVerification adds the check results to a tool result: a "verification"
// field (and "verificationPending", the files checked later) of a JSON-object
// result, else "[verification] path — summary" lines.
func withVerification(content string, results []CheckResult, pending []any) string {
	if isJSONObject(content) {
		out := strings.TrimSpace(content)
		ok := true
		if len(results) > 0 {
			items := make([]any, 0, len(results))
			for _, r := range results {
				items = append(items, verificationItem(r, false, false))
			}
			out, ok = addJSONField(out, "verification", items)
		}
		if ok && len(pending) > 0 {
			out, ok = addJSONField(out, "verificationPending", map[string]any{"paths": pending, "note": fmt.Sprintf(pendingNote, maxChecksPerCall)})
		}
		if ok {
			return out
		}
	}
	var b strings.Builder
	b.WriteString(content)
	for _, r := range results {
		b.WriteString("\n\n[verification] " + r.Path + " — " + r.Summary)
		if !r.OK && r.NextStep != "" {
			b.WriteString("\nNext step: " + truncate(r.NextStep, 400))
		}
	}
	if len(pending) > 0 {
		names := make([]string, 0, len(pending))
		for _, p := range pending {
			names = append(names, fmt.Sprint(p))
		}
		b.WriteString("\n\n[verification] " + strings.Join(names, ", ") + " — " + fmt.Sprintf(pendingNote, maxChecksPerCall))
	}
	return b.String()
}

// verifyAfterTool records the call's deliverable changes, runs their checkers
// (at most maxChecksPerCall; the others are checked before the next model
// call), notes view_file calls and checkers the model ran itself, and returns
// the result with the checker results added and the tool message metadata
// with metadata.verification.
func (st *runState) verifyAfterTool(ctx context.Context, tc ai.ToolCall, content string, failed bool, metadata map[string]any, em *toolEmitter) (string, map[string]any) {
	v := &st.v3.verify
	var results []CheckResult
	var modelRun []bool
	var final *CheckResult // the file's own check, run last by the command (A3)
	if !failed {
		// A checker the model ran is older than the files the same command
		// changed, unless the command's last step is that file's own checker
		// command: then it checked the final content (round 3, A3).
		if r, ok := v.modelRanChecker(tc, content); ok {
			if st.checkedLast(tc, r.Path) {
				final = &r
			} else {
				v.result(r, false)
				results, modelRun = append(results, r), append(modelRun, true)
			}
		}
		if tc.Function.Name == "view_file" {
			st.noteView(tc.Function.Arguments, content)
		}
	} else if tc.Function.Name == "view_file" {
		content += st.noteViewFailed(tc.Function.Arguments, content)
	}
	changed, all := changedOutputs(metadata)
	for _, c := range all {
		v.changed(c[0], c[1])
	}
	st.g.noteInPlace(metadata)
	if final != nil {
		if c := st.a.Harness.Checker; c != nil {
			final.Checker = c.CheckerFor(final.Path)
			if cc, ok := c.(CheckCompleter); ok && ctx.Err() == nil {
				*final = st.safeComplete(ctx, cc, *final)
			}
		}
		v.result(*final, true) // the runtime's own check, its format part run by the command
		results, modelRun = append(results, *final), append(modelRun, false)
	}
	if c := st.a.Harness.Checker; c != nil && ctx.Err() == nil {
		n := 0
		for _, p := range changed {
			if c.CheckerFor(p) == "" || (final != nil && final.Path == p) {
				continue
			}
			if n == maxChecksPerCall {
				d := v.file(p)
				d.pending, d.origin = true, tc.ID
				continue
			}
			n++
			r := st.safeCheck(ctx, c, p)
			v.result(r, true)
			results, modelRun = append(results, r), append(modelRun, false)
		}
	}
	var pending []any
	for _, p := range changed {
		if d := v.files[p]; d != nil && d.pending {
			pending = append(pending, p)
		}
	}
	if len(results) == 0 && len(pending) == 0 {
		return content, metadata
	}
	items := make([]any, 0, len(results))
	for i, r := range results {
		items = append(items, verificationItem(r, true, modelRun[i]))
		em.EmitAfterResult(agui.CustomEvent(CustomVerification, map[string]any{"toolCallId": tc.ID, "path": r.Path, "checker": r.Checker,
			"ok": r.OK, "summary": r.Summary}))
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["verification"] = items
	if len(pending) > 0 {
		metadata["verificationPending"] = pending
	}
	return withVerification(content, results, pending), metadata
}

// noteView records a successful view_file: it counts once its pages were sent.
func (st *runState) noteView(arguments, content string) {
	v := &st.v3.verify
	var args struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return
	}
	var res struct {
		Pages []struct {
			ImagePath string `json:"imagePath"`
		} `json:"pages"`
	}
	_ = json.Unmarshal([]byte(content), &res)
	pv := pendingView{path: absPath(args.Path), seq: v.next()}
	for _, pg := range res.Pages {
		if pg.ImagePath != "" {
			pv.images = append(pv.images, pg.ImagePath)
		}
	}
	if len(pv.images) > 0 {
		v.views = append(v.views, pv)
	}
}

// RenderFailedPrefix starts the unverified reason of a deliverable whose
// view_file failed after its last change (round 3, A5): the StopGuard accepts
// the stop, the answer must say the file could not be viewed.
const RenderFailedPrefix = "render failed: "

// ViewFailedNote follows a failed view_file of a deliverable.
const ViewFailedNote = "\n\n[note] This file could not be rendered, so you cannot look at its pages. Do not say that it renders or looks right: say in your answer that you could not view it, and why."

// noteViewFailed records a view_file of a deliverable that could not render it
// and returns the note for its result ("" for other files).
func (st *runState) noteViewFailed(arguments, content string) string {
	v := &st.v3.verify
	var args struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return ""
	}
	d, ok := v.files[absPath(args.Path)]
	if !ok || d.changed == 0 {
		return ""
	}
	d.viewFailed = v.next()
	d.viewError = truncate(strings.Join(strings.Fields(strings.TrimPrefix(strings.TrimSpace(content), "Error: ")), " "), 300)
	return ViewFailedNote
}

// checkPending runs the checkers of the deliverables changed past the per-call
// bound (at most maxChecksPerCall per step), before the model call: their
// results go to the model as a reminder and to work.verification (with the
// id of the call that changed them).
func (st *runState) checkPending(ctx context.Context) error {
	v := &st.v3.verify
	c := st.a.Harness.Checker
	if c == nil || ctx.Err() != nil {
		return nil
	}
	var names []string
	for p, d := range v.files {
		if d.pending {
			names = append(names, p)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	var lines []string
	stored := map[string][]any{} // origin call → its deferred results
	defer func() { st.storeDeferred(ctx, stored) }()
	for i, p := range names {
		if i == maxChecksPerCall {
			lines = append(lines, fmt.Sprintf("%d more files are checked before your next step.", len(names)-i))
			break
		}
		origin := v.files[p].origin
		r := st.safeCheck(ctx, c, p)
		v.result(r, true)
		stored[origin] = append(stored[origin], verificationItem(r, true, false))
		line := "[verification] " + r.Path + " — " + r.Summary
		if !r.OK && r.NextStep != "" {
			line += "\nNext step: " + truncate(r.NextStep, 400)
		}
		lines = append(lines, line)
		if err := st.emit(ctx, agui.CustomEvent(CustomVerification, map[string]any{"toolCallId": origin, "path": r.Path, "checker": r.Checker,
			"ok": r.OK, "summary": r.Summary})); err != nil {
			return err
		}
	}
	st.history = append(st.history, ai.UserMessage("<system-reminder>\nChecks of files changed earlier (more than "+
		fmt.Sprint(maxChecksPerCall)+" in one call):\n"+strings.Join(lines, "\n")+"\n</system-reminder>"))
	return nil
}

// VerificationStore persists the checks run after their tool call
// (store.HarnessStore.AppendToolVerification), optionally implemented by Agent.Store.
type VerificationStore interface {
	AppendToolVerification(ctx context.Context, threadID, toolCallID string, items []any) error
}

// storeDeferred appends deferred check results to metadata.verification of
// their origin tool message (round 3, N2): reloads, resumed runs and the goal
// judge see them like the checks run in the call itself.
func (st *runState) storeDeferred(ctx context.Context, byOrigin map[string][]any) {
	vs, ok := st.a.Store.(VerificationStore)
	if !ok {
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	for origin, items := range byOrigin {
		if err := vs.AppendToolVerification(pctx, st.in.Run.ThreadID, origin, items); err != nil {
			slog.Error("agent: persisting deferred checks failed", "run_id", st.in.Run.ID, "tool_call_id", origin, "error", err)
		}
	}
}

// safeComplete is CheckCompleter.Complete; a panic leaves the file unverified.
func (st *runState) safeComplete(ctx context.Context, cc CheckCompleter, base CheckResult) (r CheckResult) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("agent: checker panicked", "run_id", st.in.Run.ID, "path", base.Path, "panic", rec)
			r = CheckResult{Path: base.Path, Checker: base.Checker, Failed: true, Summary: "the checker crashed"}
		}
	}()
	r = cc.Complete(ctx, st.in.Run, base.Path, base)
	r.Path = base.Path
	return r
}

func (st *runState) safeCheck(ctx context.Context, c Checker, p string) (r CheckResult) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("agent: checker panicked", "run_id", st.in.Run.ID, "path", p, "panic", rec)
			r = CheckResult{Path: p, Checker: c.CheckerFor(p), Failed: true, Summary: "the checker crashed"}
		}
	}()
	return c.Check(ctx, st.in.Run, p)
}

// checkHint names the command that checks p, for reminders ("" when unknown).
func (st *runState) checkHint(p string) string {
	if cc, ok := st.a.Harness.Checker.(CheckCommander); ok {
		if cmd := cc.CheckCommand(p); cmd != "" {
			return " (check it with: " + cmd + ")"
		}
	}
	return ""
}
