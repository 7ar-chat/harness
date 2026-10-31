package agent

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// verificationLists sorts the deliverables changed in this turn: verified
// (latest check after the last change passed, and, for documents and a vision
// model, viewed since), failed (latest check after the last change failed:
// path, checker, summary) and unverified (never checked, the checker could
// not run, or not viewed: path, reason). Files without a checker or a view
// requirement are in none.
func (st *runState) verificationLists() (verified []string, failed, unverified []map[string]any) {
	v := &st.v3.verify
	names := make([]string, 0, len(v.files))
	for p := range v.files {
		names = append(names, p)
	}
	sort.Strings(names)
	c := st.a.Harness.Checker
	vision := st.a.Harness.Vision != nil && st.a.Harness.Vision(st.in.Run.Model)
	for _, p := range names {
		d := v.files[p]
		if d.changed == 0 {
			continue
		}
		hasChecker := c != nil && c.CheckerFor(p) != ""
		checked, ok := d.verdict()
		if hasChecker && checked && !ok {
			failed = append(failed, map[string]any{"path": p, "checker": d.last.Checker, "summary": d.last.Summary})
			continue
		}
		var reasons []string
		if hasChecker && !checked {
			switch {
			case d.pending:
				reasons = append(reasons, "its checker has not run yet"+st.checkHint(p))
			case d.hasLast && d.checked > d.changed && d.last.Failed:
				reasons = append(reasons, "its checker could not run ("+d.last.Summary+")"+st.checkHint(p))
			default:
				reasons = append(reasons, "no checker ran after its last change"+st.checkHint(p))
			}
		}
		needsView := vision && visualTypes[strings.ToLower(path.Ext(p))]
		if needsView && d.viewed < d.changed {
			if d.viewFailed > d.changed {
				reasons = append(reasons, RenderFailedPrefix+d.viewError)
			} else {
				reasons = append(reasons, "its rendered pages were not viewed (view_file) after its last change")
			}
		}
		switch {
		case len(reasons) > 0:
			unverified = append(unverified, map[string]any{"path": p, "reason": strings.Join(reasons, "; ")})
		case hasChecker || needsView:
			verified = append(verified, p)
		}
	}
	return verified, failed, unverified
}

// verification lists the deliverables that are not verified (failing ones
// first, with their reason) and the verified ones.
func (st *runState) verification() (verified []string, unverified []map[string]any) {
	verified, failed, unverified := st.verificationLists()
	out := make([]map[string]any, 0, len(failed)+len(unverified))
	for _, f := range failed {
		out = append(out, map[string]any{"path": f["path"], "reason": "its checker failed (" + fmt.Sprint(f["summary"]) + ")"})
	}
	return verified, append(out, unverified...)
}

// verificationReminder refuses the first stop with unverified deliverables.
func (st *runState) verificationReminder() string {
	v := &st.v3.verify
	if v.reminded {
		return ""
	}
	_, all := st.verification()
	var unverified []map[string]any
	for _, u := range all {
		// A file view_file could not render cannot be fixed by viewing it again (A5).
		if !strings.HasPrefix(fmt.Sprint(u["reason"]), RenderFailedPrefix) {
			unverified = append(unverified, u)
		}
	}
	if len(unverified) == 0 {
		return ""
	}
	v.reminded = true
	var b strings.Builder
	b.WriteString("Before you finish: these deliverables changed in this task are not verified yet:")
	for _, u := range unverified {
		fmt.Fprintf(&b, "\n- %s: %s", u["path"], u["reason"])
	}
	b.WriteString("\nFix and re-check them (run the skill's checker, look at the pages with view_file). If you cannot, say plainly in your answer which file could not be verified and why.")
	return b.String()
}

// answerVerification is the metadata.verification of an answer of a run that
// changed deliverables (nil otherwise).
func (st *runState) answerVerification() map[string]any {
	changed := false
	for _, d := range st.v3.verify.files {
		if d.changed > 0 {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	verified, failed, unverified := st.verificationLists()
	ver := make([]any, 0, len(verified)) // JSON shapes, as reloaded from the store
	for _, p := range verified {
		ver = append(ver, p)
	}
	fl := make([]any, 0, len(failed))
	for _, f := range failed {
		fl = append(fl, f)
	}
	un := make([]any, 0, len(unverified))
	for _, u := range unverified {
		un = append(un, u)
	}
	return map[string]any{"verified": ver, "failed": fl, "unverified": un}
}

// answerMetadata is the metadata of an assistant message: metadata.verification
// on answers (no tool calls) of a run that changed deliverables.
func (st *runState) answerMetadata(msg *ai.Message) map[string]any {
	if msg == nil || len(msg.ToolCalls) > 0 {
		return nil
	}
	if v := st.answerVerification(); v != nil {
		return map[string]any{"verification": v}
	}
	return nil
}

// emitAnswerVerification announces an answer's verification summary live.
func (st *runState) emitAnswerVerification(ctx context.Context, row *store.Message) error {
	if row == nil || row.Metadata == nil {
		return nil
	}
	v, ok := row.Metadata["verification"].(map[string]any)
	verified, _ := v["verified"].([]any)
	failed, _ := v["failed"].([]any)
	unverified, _ := v["unverified"].([]any)
	if !ok || len(verified)+len(failed)+len(unverified) == 0 {
		return nil // nothing checkable changed: no badge to show
	}
	return st.emit(ctx, agui.CustomEvent(CustomVerification, map[string]any{"messageId": row.ID, "verified": v["verified"], "failed": v["failed"],
		"unverified": v["unverified"]}))
}
