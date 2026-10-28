package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/store"
)

// Deliverable verification (harness v3 §4): after a tool call changed files
// under /workspace/outputs/, the matching checker runs and its summary is
// appended to the tool result (CUSTOM work.verification); a run that wants to
// stop with an unverified deliverable is reminded once; answers carry
// metadata.verification.

// OutputsDir holds the deliverables.
const OutputsDir = "/workspace/outputs/"

// CustomVerification is the CUSTOM event of a checker result (and of an
// answer's verification summary).
const CustomVerification = "work.verification"

// CheckResult is one checker result.
type CheckResult struct {
	Path    string `json:"path"`
	Checker string `json:"checker"`
	OK      bool   `json:"ok"`
	// Failed: the checker could not run (exit 1, timeout, skill missing): the
	// file is unverified, not known bad.
	Failed   bool   `json:"failed,omitempty"`
	Summary  string `json:"summary"`
	NextStep string `json:"nextStep,omitempty"`
}

// Checker runs the checker of a deliverable (verify.Verifier).
type Checker interface {
	// CheckerFor names the checker of a file ("" when its type has none).
	CheckerFor(path string) string
	// Check runs it in the run's sandbox.
	Check(ctx context.Context, run *store.Run, path string) CheckResult
}

// CheckCommander is optionally implemented by a Checker: the shell command
// that checks a file (named in reminders, so the model can run it itself).
type CheckCommander interface {
	CheckCommand(path string) string
}

// CheckCompleter is optionally implemented by a Checker whose own check does
// more than the file's checker command (CheckCommand): Complete returns the
// runtime's result for p given base, the result of that command run by the
// model on p's final content (final review of round 3, F1: a deck's baseline
// and a document's page citations are still checked). A Checker without it
// takes base as its result.
type CheckCompleter interface {
	Complete(ctx context.Context, run *store.Run, path string, base CheckResult) CheckResult
}

// maxChecksPerCall bounds the checkers run after one tool call (and before
// one model call, for the files changed past that bound).
const maxChecksPerCall = 3

// visualTypes need a view_file of their pages, for vision models.
var visualTypes = map[string]bool{".docx": true, ".pptx": true, ".pdf": true}

// deliverable is the verification state of one output file in the current turn.
type deliverable struct {
	changed int // sequence of the last change (0: not changed in this turn)
	checked int // sequence of the latest check result
	viewed  int // sequence of the last view_file whose pages the model saw
	// viewFailed: sequence of the last view_file that could not render it, and why.
	viewFailed int
	viewError  string
	last       CheckResult
	hasLast    bool
	// runtime: the latest result is the runtime's own check (it wins over a
	// passing model-run one).
	runtime bool
	// pending: changed past maxChecksPerCall in one call; checked before the
	// next model call (origin: that call's id).
	pending bool
	origin  string
}

// verdict of a deliverable after its last change: checked and passing,
// checked and failing, or no usable result (the checker could not run or none ran).
func (d *deliverable) verdict() (checked, ok bool) {
	if !d.hasLast || d.checked < d.changed || d.last.Failed {
		return false, false
	}
	return true, d.last.OK
}

// pendingView is a view_file whose page images were queued for the next model
// call: it counts as a view (at seq) only once the model was sent all of them.
type pendingView struct {
	path   string
	seq    int
	images []string
}

type verifyState struct {
	seq      int
	files    map[string]*deliverable
	views    []pendingView
	reminded bool
}

func (v *verifyState) file(p string) *deliverable {
	if v.files == nil {
		v.files = map[string]*deliverable{}
	}
	d := v.files[p]
	if d == nil {
		d = &deliverable{}
		v.files[p] = d
	}
	return d
}

func (v *verifyState) next() int { v.seq++; return v.seq }

func (v *verifyState) changed(p, change string) {
	if change == store.ChangeDeleted {
		delete(v.files, p)
		return
	}
	d := v.file(p)
	d.changed, d.pending = v.next(), false
}

// result records a checker result: the latest one decides, except that a
// passing model-run result never overrides the runtime's own verdict on the
// same content.
func (v *verifyState) result(r CheckResult, runtime bool) {
	d := v.file(r.Path)
	if !runtime && r.OK && d.runtime && d.checked > d.changed && !d.last.Failed {
		return
	}
	d.checked = v.next()
	d.last, d.hasLast, d.runtime = r, true, runtime
	d.pending = false
}

// viewed records a view of p seen by the model (seq: when it was made).
func (v *verifyState) viewed(p string, seq int) {
	if d, ok := v.files[p]; ok && seq > d.viewed {
		d.viewed = seq
	}
}

// tracked resolves the file a model-run checker reported against the turn's
// deliverables: absolute paths as they are, relative ones under /workspace,
// else the one deliverable ending with that relative path ("" when none).
func (v *verifyState) tracked(file string) string {
	file = strings.TrimSpace(file)
	if file == "" {
		return ""
	}
	if strings.HasPrefix(file, "/") {
		p := path.Clean(file)
		if isDeliverable(p) {
			return p
		}
		return ""
	}
	rel := path.Clean(file)
	if p := path.Clean("/workspace/" + rel); isDeliverable(p) {
		if _, ok := v.files[p]; ok {
			return p
		}
	}
	match := ""
	for p, d := range v.files {
		if d.changed > 0 && strings.HasSuffix(p, "/"+rel) {
			if match != "" {
				return "" // ambiguous
			}
			match = p
		}
	}
	return match
}

func isDeliverable(p string) bool { return strings.HasPrefix(p, OutputsDir) }

// changedOutputs are the deliverables a tool call created or changed (artifact snapshot metadata).
func changedOutputs(metadata map[string]any) (changed []string, all [][2]string) {
	items, _ := metadata["artifacts"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		p, _ := m["path"].(string)
		change, _ := m["change"].(string)
		if !isDeliverable(p) {
			continue
		}
		all = append(all, [2]string{p, change})
		if change != store.ChangeDeleted {
			changed = append(changed, p)
		}
	}
	return changed, all
}

// checkerOutput is the checker contract's stdout (also when the model runs a checker itself).
type checkerOutput struct {
	OK     *bool  `json:"ok"`
	File   string `json:"file"`
	Checks []struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	} `json:"checks"`
	NextStep string `json:"next_step"`
}

func summarize(checker string, out checkerOutput) (bool, string) {
	ok := out.OK != nil && *out.OK
	var failing []string
	for _, c := range out.Checks {
		if !c.OK {
			d := strings.Join(strings.Fields(c.Detail), " ")
			if d != "" {
				failing = append(failing, c.Name+": "+d)
			} else {
				failing = append(failing, c.Name)
			}
		}
	}
	var b strings.Builder
	if ok {
		fmt.Fprintf(&b, "%s: ok (%d checks passed)", checker, len(out.Checks))
	} else {
		fmt.Fprintf(&b, "%s: NOT OK", checker)
		if len(failing) > 0 {
			b.WriteString(" — " + strings.Join(failing, "; "))
		}
	}
	return ok, truncate(b.String(), 700)
}

// ParseCheckerOutput reads a checker run: the last checker-contract JSON
// object on stdout ({ok, file, checks, next_step?}) and the exit code (0 ok,
// 2 not ok, 1 or other: the checker could not run). found is false when
// stdout holds no contract object.
func ParseCheckerOutput(checker, stdout string, exitCode int) (r CheckResult, found bool) {
	r, _, found = parseChecker(checker, stdout, exitCode)
	return r, found
}

// parseChecker is ParseCheckerOutput with the reported file and the number of checks.
func parseChecker(checker, stdout string, exitCode int) (r CheckResult, out checkerOutput, found bool) {
	text := strings.TrimSpace(stdout)
	for i := strings.LastIndex(text, "{"); i >= 0; {
		out = checkerOutput{}
		if json.Unmarshal([]byte(text[i:]), &out) == nil && out.OK != nil && out.Checks != nil {
			ok, summary := summarize(checker, out)
			r = CheckResult{Path: path.Clean(out.File), Checker: checker, OK: ok && exitCode == 0, Summary: summary, NextStep: out.NextStep}
			if exitCode != 0 && exitCode != 2 {
				r.OK, r.Failed, r.Summary = false, true, fmt.Sprintf("%s could not run (exit %d)", checker, exitCode)
			}
			return r, out, true
		}
		if i == 0 {
			break
		}
		i = strings.LastIndex(text[:i], "{")
	}
	return CheckResult{}, checkerOutput{}, false
}

// modelRanChecker finds a checker result printed by a command the model ran.
// It counts only when strict: the command ran a script (.py or .sh), its
// stdout ends with the checker contract JSON with a non-empty checks list, and
// the reported file resolves to a deliverable of the turn.
func (v *verifyState) modelRanChecker(tc schema.ToolCall, content string) (CheckResult, bool) {
	if tc.Function.Name != "execute_command" {
		return CheckResult{}, false
	}
	var res struct {
		Stdout   string `json:"stdout"`
		ExitCode *int   `json:"exitCode"`
	}
	if json.Unmarshal([]byte(content), &res) != nil || res.ExitCode == nil {
		return CheckResult{}, false
	}
	name := ""
	var args struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(tc.Function.Arguments), &args) == nil {
		for _, f := range strings.Fields(args.Command) {
			f = strings.Trim(f, `"';&|()`)
			if ext := path.Ext(f); ext == ".py" || ext == ".sh" {
				name = strings.TrimSuffix(path.Base(f), ext)
			}
		}
	}
	if name == "" {
		return CheckResult{}, false // no checker script ran (e.g. cat of an old report)
	}
	r, out, ok := parseChecker(name, res.Stdout, *res.ExitCode)
	if !ok || len(out.Checks) == 0 {
		return CheckResult{}, false
	}
	if r.Path = v.tracked(out.File); r.Path == "" {
		return CheckResult{}, false
	}
	return r, true
}

// checkedLast reports whether a command's last step is exactly the checker
// command of p (CheckCommander; its words in any order, quotes removed, no
// pipe, redirection or background job) and nothing before it can change what
// that step runs (unsafeShell): the check saw p's final content.
func (st *runState) checkedLast(tc schema.ToolCall, p string) bool {
	cc, ok := st.a.Harness.Checker.(CheckCommander)
	if !ok || tc.Function.Name != "execute_command" {
		return false
	}
	want := cc.CheckCommand(p)
	var args struct {
		Command    string `json:"command"`
		Background bool   `json:"background"`
	}
	if want == "" || json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil || args.Background || unsafeShell(args.Command) {
		return false
	}
	for i, w := range shellWords(want) {
		// The interpreter and the checker script may not be written or
		// replaced by an earlier step (cp fake.py …/inspect_docx.py, a
		// …/python3 shim): the script is named once, the interpreter never as a path.
		if ext := path.Ext(w); (ext == ".py" || ext == ".sh") && strings.Count(args.Command, w) != 1 || i == 0 && strings.Contains(args.Command, "/"+w) {
			return false
		}
	}
	steps := strings.FieldsFunc(strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n").Replace(args.Command), func(r rune) bool { return r == '\n' })
	if len(steps) == 0 {
		return false
	}
	last := strings.TrimSpace(steps[len(steps)-1])
	if strings.ContainsAny(last, "|<>&`$") {
		return false
	}
	return sameWords(shellWords(last), shellWords(want))
}

// shellRedefines matches what can make a later step of a command run
// something other than its words say (final review of round 3, F5): a shell
// function or alias, a changed PATH (or another variable that decides what
// runs: PYTHON*, BASH_ENV, ENV, LD_*), a sourced file, eval, exec, a builtin
// switched off, a hashed command path, or a Python start-up hook.
var shellRedefines = regexp.MustCompile(`(?:^|[^\w./-])(?:function|alias|eval|exec|source|enable|hash|builtin|command|declare|typeset|trap|shopt)(?:$|[^\w./-])` +
	`|(?:^|[\s;&|(){}])\.\s` +
	`|\w\s*\(\s*\)\s*[{(]` +
	`|(?:^|[^\w])(?:PATH|PYTHON\w*|BASH_ENV|ENV|LD_\w+|SHELLOPTS|BASHOPTS|IFS)\+?=` +
	`|sitecustomize|usercustomize|\.pth\b`)

// unsafeShell reports a command whose checker step cannot be trusted (when
// in doubt, the runtime runs its own check).
func unsafeShell(command string) bool { return shellRedefines.MatchString(command) }

// shellWords splits a simple command into words, removing quotes.
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	quote, word := rune(0), false
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, word = r, true
		case r == ' ' || r == '\t':
			if word {
				out = append(out, cur.String())
				cur.Reset()
				word = false
			}
		default:
			cur.WriteRune(r)
			word = true
		}
	}
	if word {
		out = append(out, cur.String())
	}
	return out
}

func sameWords(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return a[0] != "" && len(a) >= 2
}

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
func (st *runState) verifyAfterTool(ctx context.Context, tc schema.ToolCall, content string, failed bool, metadata map[string]any, em *toolEmitter) (string, map[string]any) {
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

// viewsSent counts the pending views whose page images were all sent to the
// model (sent: the image paths of the request); the others are dropped (their
// images are stubbed as not shown).
func (v *verifyState) viewsSent(sent map[string]bool) {
	for _, pv := range v.views {
		all := true
		for _, img := range pv.images {
			all = all && sent[img]
		}
		if all {
			v.viewed(pv.path, pv.seq)
		}
	}
	v.views = nil
}

func absPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/workspace/" + p
	}
	return path.Clean(p)
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
	st.history = append(st.history, schema.UserMessage("<system-reminder>\nChecks of files changed earlier (more than "+
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
func (st *runState) answerMetadata(msg *schema.Message) map[string]any {
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

// seedVerification rebuilds the turn's verification state from its stored
// messages (a resumed run continues the turn of an earlier one), in the order
// the run recorded it: per tool message the checks the model ran, its file
// changes, the runtime's checks, the files left to check. A view_file counts
// when a model call followed it (its pages were sent).
func (st *runState) seedVerification(turn []*store.Message) {
	v := &st.v3.verify
	calls := map[string]schema.ToolCall{}
	for i, m := range turn {
		switch m.Role {
		case store.RoleAssistant:
			for _, c := range m.ToolCalls {
				calls[c.ID] = schema.ToolCall{ID: c.ID, Function: schema.FunctionCall{Name: c.Function.Name, Arguments: c.Function.Arguments}}
			}
		case store.RoleTool:
			items, _ := m.Metadata["verification"].([]any)
			replay := func(modelRun bool) {
				for _, it := range items {
					r, _ := it.(map[string]any)
					p, _ := r["path"].(string)
					ok, _ := r["ok"].(bool)
					failed, _ := r["failed"].(bool)
					name, _ := r["checker"].(string)
					summary, _ := r["summary"].(string)
					next, _ := r["nextStep"].(string)
					mr, _ := r["modelRun"].(bool)
					if p != "" && mr == modelRun {
						v.result(CheckResult{Path: p, Checker: name, OK: ok, Failed: failed, Summary: summary, NextStep: next}, !modelRun)
					}
				}
			}
			replay(true)
			_, all := changedOutputs(m.Metadata)
			for _, c := range all {
				v.changed(c[0], c[1])
			}
			replay(false)
			pending, _ := m.Metadata["verificationPending"].([]any)
			for _, it := range pending {
				if p, _ := it.(string); p != "" {
					if d := v.files[p]; d != nil && d.checked < d.changed {
						d.pending, d.origin = true, m.ToolCallID
					}
				}
			}
			if tc, ok := calls[m.ToolCallID]; ok && tc.Function.Name == "view_file" && m.Error != "" {
				st.noteViewFailed(tc.Function.Arguments, m.Error) // A5: replayed for a resumed run
			}
			if tc, ok := calls[m.ToolCallID]; ok && tc.Function.Name == "view_file" && m.Error == "" && answeredAfter(turn[i+1:]) {
				var args struct {
					Path string `json:"path"`
				}
				if json.Unmarshal([]byte(tc.Function.Arguments), &args) == nil {
					v.viewed(absPath(args.Path), v.next())
				}
			}
		}
	}
}

// answeredAfter reports whether a model call followed (an assistant message).
func answeredAfter(rest []*store.Message) bool {
	for _, m := range rest {
		if m.Role == store.RoleAssistant {
			return true
		}
	}
	return false
}

var _ tools.ImageSink = (*toolEmitter)(nil)

// TurnDeliverables are the deliverables changed in the current turn of a
// thread's messages (the verification state a run rebuilds), sorted by path,
// with their last checker result after their last change.
func TurnDeliverables(msgs []*store.Message) []Deliverable {
	return deliverablesOf(turnMessages(msgs))
}

// DeliverablesSince are the deliverables changed in the turns that started at
// or after since (a goal's turns: since its creation), like TurnDeliverables;
// the current turn when no turn started since then.
func DeliverablesSince(msgs []*store.Message, since time.Time) []Deliverable {
	for i, m := range msgs {
		if m.Role == store.RoleUser && !m.IsSteer() && !m.CreatedAt.Before(since) {
			return deliverablesOf(msgs[i:])
		}
	}
	return TurnDeliverables(msgs)
}

func deliverablesOf(turns []*store.Message) []Deliverable {
	st := &runState{}
	st.seedVerification(turns)
	v := &st.v3.verify
	names := make([]string, 0, len(v.files))
	for p, d := range v.files {
		if d.changed > 0 {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	out := make([]Deliverable, 0, len(names))
	for _, p := range names {
		d := v.files[p]
		item := Deliverable{Path: p}
		if d.hasLast {
			item.Checker, item.Summary = d.last.Checker, d.last.Summary
			if checked, ok := d.verdict(); checked { // a checker that could not run gives no verdict
				item.OK = &ok
			}
		}
		out = append(out, item)
	}
	return out
}
