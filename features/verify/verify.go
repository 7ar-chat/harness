// Package verify runs the deliverable checkers of harness v3 §4: after a
// tool call changed a file under /workspace/outputs/, the skill checker that
// matches its extension runs in the thread's sandbox (≤ 60 s) and its result
// becomes an agent.CheckResult (appended to the tool result, CUSTOM
// work.verification, the verification StopGuard and the goal judge).
package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/tools/sandboxtools"
	"github.com/GhaziBenDahmane/harness/features/goals"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/store"
)

// Checker is the command checking one file type; {file} in Command is
// replaced by the shell-quoted path. The command prints the checker contract
// JSON {ok, file, checks: [{name, ok, detail}], next_step?} and exits 0 (ok),
// 2 (not ok) or 1 (usage error, missing file, crash: could not run).
type Checker struct {
	Name    string
	Skill   string // installed before the command runs ("" none)
	Command string
	// Baseline is appended to Command when the deliverable has an original
	// (review round 2, N8: the deck the user gave, see Verifier.baseline);
	// {baseline} is replaced by the shell-quoted path of that original.
	Baseline string
}

// ArtifactSource reads a thread's artifacts (artifacts.Service): the
// originals of deliverables (baselines) and the uploaded PDFs citations refer to.
type ArtifactSource interface {
	ListArtifacts(ctx context.Context, threadID string) ([]*store.Artifact, error)
	ListArtifactVersions(ctx context.Context, artifactID string) ([]store.ArtifactVersion, error)
	Content(ctx context.Context, a *store.Artifact, version int) ([]byte, *store.ArtifactVersion, error)
}

// Options configures the verifier.
type Options struct {
	// Checkers by lower-case extension (".docx"); nil: DefaultCheckers.
	Checkers map[string]Checker
	// Timeout bounds one checker run (default 60 s).
	Timeout time.Duration
	// Sandbox gives the thread's live sandbox (Get: never creates one).
	Sandbox sandbox.Manager
	// EnsureSkill installs a skill into the sandbox before its checker runs.
	EnsureSkill func(ctx context.Context, run *store.Run, sb sandbox.Sandbox, skill string) error
	// Messages reads a thread's messages (Deliverables).
	Messages interface {
		ListMessages(ctx context.Context, threadID string) ([]*store.Message, error)
	}
	// Artifacts finds originals (Checker.Baseline) and PDF sources (Citations); nil: neither.
	Artifacts ArtifactSource
	// Citations checks the page citations "(p. N)" of .docx/.pptx/.pdf
	// deliverables against the thread's uploaded PDFs ({sources}, {file});
	// its result is merged into the format checker's (review round 2, A9).
	// nil: DefaultCitations when Checkers is nil too, else none.
	Citations *Checker
}

// citationTypes are the deliverables whose page citations are checked.
var citationTypes = map[string]bool{".docx": true, ".pptx": true, ".pdf": true}

// maxCitationSources bounds the PDFs given to the citation checker.
const maxCitationSources = 5

// BaselinesDir receives the originals written for a checker (Checker.Baseline).
const BaselinesDir = sandbox.ScratchDir + "/baselines"

// ValidatorScript validates the files no other skill covers: Python compiles,
// JSON and CSV parse (checker contract, check names py_compile / json / csv).
// It ships with the xlsx skill, so the model can run it itself.
const ValidatorScript = "/skills/xlsx/scripts/validate_file.py"

// DefaultCheckers are the built-in skills' checkers (developer D's contract)
// and the generic validators.
func DefaultCheckers() map[string]Checker {
	skill := func(name, skill, script, args string) Checker {
		return Checker{Name: name, Skill: skill, Command: "python3 /skills/" + skill + "/scripts/" + script + " " + args}
	}
	generic := Checker{Name: "validate", Skill: "xlsx", Command: "python3 " + ValidatorScript + " {file}"}
	deck := skill("fit_text", "pptx", "fit_text.py", "{file}")
	deck.Baseline = "--baseline {baseline}" // only the overlaps the agent added to an uploaded deck fail (N8)
	return map[string]Checker{
		".docx": skill("inspect_docx", "docx", "inspect_docx.py", "--check {file}"),
		".pptx": deck,
		".xlsx": skill("recalc", "xlsx", "recalc.py", "--check-only {file}"),
		".xlsm": skill("recalc", "xlsx", "recalc.py", "--check-only {file}"),
		".pdf":  skill("extract", "pdf", "extract.py", "--check {file}"),
		".png":  skill("make_chart", "charts", "make_chart.py", "--check {file}"),
		".jpg":  skill("make_chart", "charts", "make_chart.py", "--check {file}"),
		".jpeg": skill("make_chart", "charts", "make_chart.py", "--check {file}"),
		".svg":  skill("make_chart", "charts", "make_chart.py", "--check {file}"),
		".py":   generic,
		".json": generic,
		".csv":  generic,
	}
}

// DefaultCitations is the pdf skill's citation checker (check_citations.py SOURCE.pdf... FILE).
func DefaultCitations() *Checker {
	return &Checker{Name: "check_citations", Skill: "pdf", Command: "python3 /skills/pdf/scripts/check_citations.py {sources} {file}"}
}

// Verifier implements agent.Checker.
type Verifier struct{ o Options }

var (
	_ agent.Checker           = (*Verifier)(nil)
	_ agent.CheckCommander    = (*Verifier)(nil)
	_ agent.CheckCompleter    = (*Verifier)(nil)
	_ agent.DeliverableSource = (*Verifier)(nil)
)

// Deliverables implements agent.DeliverableSource (post-compaction
// rehydration): the deliverables changed in the run's turn with their last
// checker result, from the stored tool messages.
func (v *Verifier) Deliverables(ctx context.Context, run *store.Run) []agent.Deliverable {
	if v.o.Messages == nil {
		return nil
	}
	msgs, err := v.o.Messages.ListMessages(ctx, run.ThreadID)
	if err != nil {
		return nil
	}
	return agent.TurnDeliverables(msgs)
}

// New returns a verifier.
func New(o Options) *Verifier {
	if o.Checkers == nil {
		o.Checkers = DefaultCheckers()
		if o.Citations == nil {
			o.Citations = DefaultCitations()
		}
	}
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	return &Verifier{o: o}
}

func (v *Verifier) checker(p string) (Checker, bool) {
	c, ok := v.o.Checkers[strings.ToLower(path.Ext(p))]
	return c, ok
}

// CheckerFor implements agent.Checker.
func (v *Verifier) CheckerFor(p string) string {
	if c, ok := v.checker(p); ok {
		if c.Name == "validate" {
			return genericName(p)
		}
		return c.Name
	}
	return ""
}

func genericName(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".py":
		return "py_compile"
	case ".json":
		return "json"
	}
	return "csv"
}

// CheckCommand implements agent.CheckCommander: the command that checks p
// ("" when its type has no checker).
func (v *Verifier) CheckCommand(p string) string {
	c, ok := v.checker(p)
	if !ok {
		return ""
	}
	return strings.ReplaceAll(c.Command, "{file}", shellQuote(p))
}

// shellQuote quotes s for bash.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Check implements agent.Checker: the file type's checker (with the
// deliverable's original as baseline when it has one), then, for documents
// citing pages of an uploaded PDF, the citation checker (one merged result).
func (v *Verifier) Check(ctx context.Context, run *store.Run, p string) agent.CheckResult {
	return v.check(ctx, run, p, nil)
}

// Complete implements agent.CheckCompleter (final review of round 3, F1): base
// is the result of p's checker command (CheckCommand) run by the model on p's
// final content. It stands for the format check unless p has a baseline (the
// command did not pass it: the checker runs again with it); the citation
// check still runs. A file type with neither keeps base (no sandbox call).
func (v *Verifier) Complete(ctx context.Context, run *store.Run, p string, base agent.CheckResult) agent.CheckResult {
	c, ok := v.checker(p)
	baseline := c.Baseline != "" && v.o.Artifacts != nil
	citations := v.o.Citations != nil && v.o.Artifacts != nil && citationTypes[strings.ToLower(path.Ext(p))]
	if !ok || base.Failed || !baseline && !citations {
		return base
	}
	return v.check(ctx, run, p, &base)
}

// check is Check; a non-nil base is the format checker's result on the
// current content without a baseline (Complete).
func (v *Verifier) check(ctx context.Context, run *store.Run, p string, base *agent.CheckResult) agent.CheckResult {
	c, ok := v.checker(p)
	name := v.CheckerFor(p)
	failed := func(format string, args ...any) agent.CheckResult {
		return agent.CheckResult{Path: p, Checker: name, Failed: true, Summary: name + ": " + fmt.Sprintf(format, args...)}
	}
	if !ok {
		return failed("no checker for this file type")
	}
	if v.o.Sandbox == nil || run.Sandbox == store.SandboxOff {
		return failed("could not run (no sandbox)")
	}
	sb, err := v.o.Sandbox.Get(ctx, run.ThreadID)
	if err != nil {
		if errors.Is(err, sandbox.ErrNoSandbox) {
			return failed("could not run (the sandbox is gone)")
		}
		return failed("could not run (sandbox unavailable)")
	}
	if c.Skill != "" && v.o.EnsureSkill != nil {
		if err := v.o.EnsureSkill(ctx, run, sb, c.Skill); err != nil {
			return failed("could not run (installing the %s skill failed)", c.Skill)
		}
	}
	var arts []*store.Artifact
	if v.o.Artifacts != nil {
		arts, _ = v.o.Artifacts.ListArtifacts(ctx, run.ThreadID)
	}
	cmd := strings.ReplaceAll(c.Command, "{file}", shellQuote(p))
	withBaseline := false
	if c.Baseline != "" {
		if b := v.baseline(ctx, run, sb, p, arts); b != "" {
			cmd += " " + strings.ReplaceAll(c.Baseline, "{baseline}", shellQuote(b))
			withBaseline = true
		}
	}
	var r agent.CheckResult
	if base != nil && !withBaseline {
		r = *base
	} else {
		r, _ = v.run(ctx, sb, name, cmd)
	}
	r.Path = p
	if r.Failed {
		return r
	}
	return v.withCitations(ctx, run, sb, p, r, arts)
}

// run executes one checker command and reads its result (and stdout).
func (v *Verifier) run(ctx context.Context, sb sandbox.Sandbox, name, command string) (agent.CheckResult, string) {
	failed := func(format string, args ...any) agent.CheckResult {
		return agent.CheckResult{Checker: name, Failed: true, Summary: name + ": " + fmt.Sprintf(format, args...)}
	}
	var stdout, stderr strings.Builder
	cctx, cancel := context.WithTimeout(ctx, v.o.Timeout+5*time.Second)
	defer cancel()
	res, err := sb.Exec(cctx, sandbox.ExecRequest{Command: command, Cwd: sandbox.Workspace, Timeout: v.o.Timeout,
		OnStdout: func(t string) {
			if stdout.Len() < 1<<20 {
				stdout.WriteString(t)
			}
		},
		OnStderr: func(t string) {
			if stderr.Len() < 4000 {
				stderr.WriteString(t)
			}
		}})
	if err != nil {
		if cctx.Err() != nil && ctx.Err() == nil {
			return failed("timed out after %s", v.o.Timeout), ""
		}
		return failed("could not run (%s)", truncate(err.Error(), 200)), ""
	}
	r, found := agent.ParseCheckerOutput(name, stdout.String(), res.ExitCode)
	if !found {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		if res.ExitCode == 1 {
			return failed("could not run (exit 1) %s", tail), ""
		}
		return failed("gave no result (exit %d) %s", res.ExitCode, tail), ""
	}
	return r, stdout.String()
}

// withCitations merges the citation checker's verdict into r when the
// deliverable is a document, the thread has uploaded PDFs and the document
// cites pages (review round 2, A9). A citation check that cannot run leaves
// the verdict as is (its summary says so).
func (v *Verifier) withCitations(ctx context.Context, run *store.Run, sb sandbox.Sandbox, p string, r agent.CheckResult,
	arts []*store.Artifact) agent.CheckResult {
	c := v.o.Citations
	if c == nil || !citationTypes[strings.ToLower(path.Ext(p))] {
		return r
	}
	var sources []string
	for _, a := range arts {
		if a.Kind != store.ArtifactUpload || a.Deleted || a.Path == p || strings.ToLower(path.Ext(a.Path)) != ".pdf" {
			continue
		}
		if _, err := sb.Stat(ctx, a.Path); err == nil {
			sources = append(sources, a.Path)
		}
	}
	if len(sources) == 0 {
		return r
	}
	sort.Strings(sources)
	if len(sources) > maxCitationSources {
		sources = sources[:maxCitationSources]
	}
	if c.Skill != "" && v.o.EnsureSkill != nil {
		if err := v.o.EnsureSkill(ctx, run, sb, c.Skill); err != nil {
			r.Summary += "; citations could not be checked (installing the " + c.Skill + " skill failed)"
			return r
		}
	}
	quoted := make([]string, len(sources))
	for i, s := range sources {
		quoted[i] = shellQuote(s)
	}
	cmd := strings.ReplaceAll(strings.ReplaceAll(c.Command, "{sources}", strings.Join(quoted, " ")), "{file}", shellQuote(p))
	cr, stdout := v.run(ctx, sb, c.Name, cmd)
	if cr.Failed {
		r.Summary += "; citations could not be checked (" + truncate(strings.TrimPrefix(cr.Summary, c.Name+": "), 200) + ")"
		return r
	}
	if !cites(stdout) {
		return r
	}
	if !cr.OK {
		if r.OK || r.NextStep == "" {
			r.NextStep = cr.NextStep
		} else if cr.NextStep != "" {
			r.NextStep += "; then " + cr.NextStep
		}
	}
	r.OK = r.OK && cr.OK
	r.Summary += "; " + cr.Summary
	return r
}

// cites reports whether the citation checker found page citations (its
// "citations" list; a result without the list counts as citing).
func cites(stdout string) bool {
	text := strings.TrimSpace(stdout)
	for i := strings.LastIndex(text, "{"); i >= 0; i = strings.LastIndex(text[:i], "{") {
		var out struct {
			Checks    []json.RawMessage  `json:"checks"`
			Citations *[]json.RawMessage `json:"citations"`
		}
		if json.Unmarshal([]byte(text[i:]), &out) == nil && out.Checks != nil {
			return out.Citations == nil || len(*out.Citations) > 0
		}
		if i == 0 {
			break
		}
	}
	return true
}

// baseline is the original of deliverable p, written under BaselinesDir
// ("" when it has none; review round 2, N8), from the artifact store:
//  1. the latest version of p the user gave (no agent run: uploaded there);
//  2. else an upload of the same name, as uploaded;
//  3. else the upload p's first version is a copy of (cp, then edits).
func (v *Verifier) baseline(ctx context.Context, run *store.Run, sb sandbox.Sandbox, p string, arts []*store.Artifact) string {
	if v.o.Artifacts == nil {
		return ""
	}
	versions := func(a *store.Artifact) []store.ArtifactVersion {
		vs, err := v.o.Artifacts.ListArtifactVersions(ctx, a.ID)
		if err != nil {
			return nil
		}
		sort.Slice(vs, func(i, j int) bool { return vs[i].Version < vs[j].Version })
		return vs
	}
	userVersion := func(vs []store.ArtifactVersion) (store.ArtifactVersion, bool) {
		for i := len(vs) - 1; i >= 0; i-- {
			if vs[i].RunID == "" {
				return vs[i], true
			}
		}
		return store.ArtifactVersion{}, false
	}
	var self *store.Artifact
	var uploads []*store.Artifact
	for _, a := range arts {
		switch {
		case a.Deleted:
		case a.Path == p:
			self = a
		case a.Kind == store.ArtifactUpload:
			uploads = append(uploads, a)
		}
	}
	sort.SliceStable(uploads, func(i, j int) bool { return uploads[i].UpdatedAt.After(uploads[j].UpdatedAt) })
	var pick *store.Artifact
	var ver store.ArtifactVersion
	var own []store.ArtifactVersion
	if self != nil {
		own = versions(self)
		if uv, ok := userVersion(own); ok {
			pick, ver = self, uv
		}
	}
	if pick == nil {
		for _, u := range uploads {
			if path.Base(u.Path) != path.Base(p) {
				continue
			}
			vs := versions(u)
			if uv, ok := userVersion(vs); ok {
				pick, ver = u, uv
			} else if len(vs) > 0 {
				pick, ver = u, vs[0]
			}
			break
		}
	}
	if pick == nil && len(own) > 0 {
		for _, u := range uploads {
			for _, uv := range versions(u) {
				if uv.RunID == "" && uv.SHA256 == own[0].SHA256 {
					pick, ver = self, own[0]
				}
			}
			if pick != nil {
				break
			}
		}
	}
	if pick == nil {
		return ""
	}
	sum := ver.SHA256
	if len(sum) > 16 {
		sum = sum[:16]
	}
	dst := BaselinesDir + "/" + unsafeChars.Replace(sum) + strings.ToLower(path.Ext(p))
	if _, err := sb.Stat(ctx, dst); err == nil {
		return dst
	}
	data, _, err := v.o.Artifacts.Content(ctx, pick, ver.Version)
	if err != nil {
		return ""
	}
	if err := sb.MkdirAll(ctx, BaselinesDir); err != nil {
		return ""
	}
	if err := sb.WriteFile(ctx, dst, data); err != nil {
		return ""
	}
	return dst
}

// unsafeChars keeps a stored hash a safe file name.
var unsafeChars = strings.NewReplacer("/", "_", "\\", "_", "..", "_", " ", "_")

// EvidenceChars bounds the text excerpt of a deliverable given to the goal judge.
const EvidenceChars = 2000

// maxEvidence bounds the deliverables given to the goal judge.
const maxEvidence = 8

// Evidence implements the goal judge's evidence (goals.Hook.Evidence): the
// deliverables of every turn since the goal was set (g; nil: the run's turn)
// with their latest checker result and the start of their extracted text
// (the live sandbox only: none is created for it).
func (v *Verifier) Evidence(ctx context.Context, run *store.Run, g *store.Goal) []goals.Evidence {
	var ds []agent.Deliverable
	if g == nil {
		ds = v.Deliverables(ctx, run)
	} else if v.o.Messages != nil {
		if msgs, err := v.o.Messages.ListMessages(ctx, run.ThreadID); err == nil {
			ds = agent.DeliverablesSince(msgs, g.CreatedAt)
		}
	}
	if len(ds) == 0 {
		return nil
	}
	if len(ds) > maxEvidence {
		ds = ds[len(ds)-maxEvidence:]
	}
	var sb sandbox.Sandbox
	if v.o.Sandbox != nil {
		sb, _ = v.o.Sandbox.Get(ctx, run.ThreadID)
	}
	out := make([]goals.Evidence, 0, len(ds))
	for _, d := range ds {
		e := goals.Evidence{Path: d.Path, Checker: d.Checker, OK: d.OK, Summary: d.Summary}
		if sb != nil {
			if text, err := sandboxtools.ExtractText(ctx, sb, d.Path, EvidenceChars); err == nil {
				e.Text = text
			}
		}
		out = append(out, e)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
