package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

// Lazy skills: a sandbox gets only the skills its run needs (needed), each
// with its declared (Package.Requires) and implicit (Package.Uses)
// dependencies:
//   - the attachments requested for the run (mentions, selected versions, a
//     creation session's skill-creator; not the defaults);
//   - the skills loaded or used in the run and the thread's history
//     (load_skill, files written naming /skills/<name>/, failed commands'
//     install notes);
//   - before a command, the known skills it names (/skills/<name>/), directly
//     or in the /workspace scripts it runs, and the run's index for a bare
//     /skills (ls /skills).

// LoadSkillTool is the name of the tool that loads a skill (skilltools).
const LoadSkillTool = "load_skill"

// Bounds of the lazy installation.
const (
	// maxIndexInstall caps the skills a bare /skills reference installs.
	maxIndexInstall = 8
	// maxScripts and maxScriptBytes bound the scripts read before a command.
	maxScripts     = 4
	maxScriptBytes = 1 << 20
)

// runState is what the installer keeps per run, for an hour after its last
// use: its plan (the attachments are fixed when the run starts), the packages
// it resolved, the skills it needs, the ones of the thread's history (read
// once per run) and the scripts it scanned.
type runState struct {
	plan     *RunPlan
	resolved map[string]installItem
	needed   []string
	history  []string
	readHist bool
	scanned  map[string]scan
	at       time.Time
}

// scan is a scanned file: its stamp ("size mtime", "size -" when written by
// the run) and the skill names it references.
type scan struct {
	stamp string
	refs  []string
}

// state returns the run's state, created when missing and refreshed (expired
// ones are dropped). Callers hold i.mu.
func (i *Installer) state(runID string) *runState {
	if i.runs == nil {
		i.runs = map[string]*runState{}
	}
	now := time.Now()
	for id, r := range i.runs {
		if now.Sub(r.at) > time.Hour {
			delete(i.runs, id)
		}
	}
	r, ok := i.runs[runID]
	if !ok {
		r = &runState{resolved: map[string]installItem{}, scanned: map[string]scan{}}
		i.runs[runID] = r
	}
	r.at = now
	return r
}

// Plan returns the run's skill plan, cached per run (the attachments are
// fixed when the run starts).
func (i *Installer) Plan(ctx context.Context, rc tools.RunContext) (RunPlan, error) {
	i.mu.Lock()
	if r := i.state(rc.RunID); r.plan != nil {
		plan := *r.plan
		i.mu.Unlock()
		return plan, nil
	}
	i.mu.Unlock()
	plan, err := i.Registry.Plan(ctx, &store.Run{ID: rc.RunID, ThreadID: rc.ThreadID, UserLogin: rc.Login})
	if err != nil {
		return RunPlan{}, err
	}
	i.mu.Lock()
	i.state(rc.RunID).plan = &plan
	i.mu.Unlock()
	return plan, nil
}

// MarkNeeded records that the run needs the skills (loaded with load_skill,
// or named by a file it wrote, or installed after a failed command): they
// are installed in the thread's sandbox from then on (at its creation or
// before the next command). Later runs find them in the thread's history.
func (i *Installer) MarkNeeded(rc tools.RunContext, names ...string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	r := i.state(rc.RunID)
	for _, name := range names {
		if !slices.Contains(r.needed, name) {
			r.needed = append(r.needed, name)
		}
	}
}

// neededNames are the skills needed by this run and the thread's history.
func (i *Installer) neededNames(ctx context.Context, rc tools.RunContext) []string {
	i.mu.Lock()
	read := i.state(rc.RunID).readHist
	i.mu.Unlock()
	if !read && i.Messages != nil && rc.ThreadID != "" {
		msgs, err := i.Messages.ListMessages(ctx, rc.ThreadID)
		if err != nil {
			slog.Warn("skills: reading the skills used in the thread failed", "run_id", rc.RunID, "error", err)
		} else {
			hist := LoadedIn(msgs)
			i.mu.Lock()
			r := i.state(rc.RunID)
			r.history, r.readHist = hist, true
			i.mu.Unlock()
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	r := i.state(rc.RunID)
	return append(append([]string(nil), r.needed...), r.history...)
}

// installNote is the note of a skill installed after a failed command.
var installNote = regexp.MustCompile(`Skills? ([a-z0-9][a-z0-9, -]*?) (?:was|were) installed; run the command again\.`)

// LoadedIn returns the skills a history loaded or used, in order, each once:
// successful load_skill calls, /skills/<name>/ paths in the content of
// successful write_file and edit_file calls, and the "Skill <name> was
// installed; run the command again." notes of tool results. Names are not
// checked against the catalog.
func LoadedIn(msgs []*store.Message) []string {
	failed := map[string]bool{}
	for _, m := range msgs {
		if m.Role == store.RoleTool && m.Error != "" {
			failed[m.ToolCallID] = true
		}
	}
	var out []string
	add := func(names ...string) {
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	for _, m := range msgs {
		switch m.Role {
		case store.RoleAssistant:
			for _, tc := range m.ToolCalls {
				if failed[tc.ID] {
					continue
				}
				var in struct {
					Name    string `json:"name"`
					Content string `json:"content"`
					NewText string `json:"newText"`
				}
				switch tc.Function.Name {
				case LoadSkillTool, "write_file", "edit_file":
					if json.Unmarshal([]byte(tc.Function.Arguments), &in) != nil {
						continue
					}
				default:
					continue
				}
				if tc.Function.Name == LoadSkillTool {
					add(in.Name)
					continue
				}
				names, _ := SkillPaths(in.Content + "\n" + in.NewText)
				add(names...)
			}
		case store.RoleTool:
			for _, m := range installNote.FindAllStringSubmatch(m.Content, -1) {
				add(strings.Split(m[1], ",")...)
			}
		}
	}
	return out
}

// SkillPaths returns the skill names a text names as /skills/<name>/ or as
// /skills/<name> ending a word, in order, each once, and whether it names the
// bare skills folder as a path argument (/skills or /skills/ followed by the
// end, a space, a shell separator or a glob: ls /skills, find /skills/ …). A
// path only counts at the start of a word (/home/skills/x does not).
func SkillPaths(text string) (names []string, root bool) {
	prefix := Root + "/"
	for i := 0; ; {
		k := strings.Index(text[i:], Root)
		if k < 0 {
			break
		}
		at := i + k
		i = at + len(Root)
		if at > 0 && wordByte(text[at-1]) {
			continue
		}
		if !strings.HasPrefix(text[at:], prefix) {
			if i == len(text) || bareEnd(text[i]) {
				root = true
			}
			continue
		}
		i = at + len(prefix)
		end := i
		for end < len(text) && slugByte(text[end]) {
			end++
		}
		name := text[i:end]
		if name == "" {
			if i == len(text) || bareEnd(text[i]) || text[i] == '*' {
				root = true
			}
			continue
		}
		if end < len(text) && wordByte(text[end]) && text[end] != '/' {
			continue
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names, root
}

// Referenced returns the skills of known whose folder a text names (SkillPaths).
func Referenced(text string, known []string) []string {
	names, _ := SkillPaths(text)
	var out []string
	for _, n := range names {
		if slices.Contains(known, n) {
			out = append(out, n)
		}
	}
	return out
}

// slugByte: a character of a skill name.
func slugByte(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' }

// wordByte: a character that continues a path word (a skill path is only
// recognized between non-word characters such as spaces, quotes, = or ;).
func wordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.-~/", c) >= 0
}

// bareEnd: what may follow a bare /skills path argument.
func bareEnd(c byte) bool { return strings.IndexByte(" \t\n;&|)", c) >= 0 }

var (
	// An interpreter running a file: python3 [flags] path, node path, bash path
	// (also inside a quoted bash -lc "…").
	interpreted = regexp.MustCompile(`(?:^|[\s;&|('"])(python3?|node|bash|sh)((?:\s+-\S*)*)\s+([^\s;&|()<>]+)`)
	// A file executed directly at the start of a command: ./path, /workspace/path.
	executed = regexp.MustCompile(`(?:^|[;&|(\n'"]|\bthen|\bdo)\s*((?:\./|/workspace/)[^\s;&|()<>'"]+)`)
)

// runsNoFile reports interpreter flags that run a module or inline code
// instead of a file (python -m / -c, also -mpip; node -e / -p; bash -c, -lc).
func runsNoFile(interp, flags string) bool {
	for _, f := range strings.Fields(flags) {
		if strings.HasPrefix(f, "--") {
			if interp == "node" && (f == "--eval" || f == "--print") {
				return true
			}
			continue
		}
		letters := strings.TrimPrefix(f, "-")
		switch interp {
		case "python", "python3":
			if strings.HasPrefix(letters, "m") || strings.HasPrefix(letters, "c") {
				return true
			}
		case "node":
			if strings.ContainsAny(letters, "ep") {
				return true
			}
		default:
			if strings.Contains(letters, "c") {
				return true
			}
		}
	}
	return false
}

// ScriptPaths returns the /workspace files a command runs (python3 x.py,
// node x.js, bash x.sh, ./x, also in a quoted bash -lc "…"; relative paths
// are under /workspace), at most 4, each once. Module and inline-code runs
// (python3 -m / -c, node -e, bash -c) name no file.
func ScriptPaths(command string) []string {
	var raw []string
	// Each match resumes right after its interpreter: the inner command of
	// bash -lc "python3 x.py" is found too.
	for pos := 0; pos < len(command); {
		loc := interpreted.FindStringSubmatchIndex(command[pos:])
		if loc == nil {
			break
		}
		m := func(g int) string { return command[pos+loc[2*g] : pos+loc[2*g+1]] }
		if !runsNoFile(m(1), m(2)) {
			raw = append(raw, m(3))
		}
		pos += loc[3]
	}
	for _, m := range executed.FindAllStringSubmatch(command, -1) {
		raw = append(raw, m[1])
	}
	var out []string
	for _, p := range raw {
		p = strings.Trim(p, `'"`)
		if p == "" || strings.HasPrefix(p, "-") || strings.ContainsAny(p, "$`*?\\") {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = sandbox.Workspace + "/" + p
		}
		p = path.Clean(p)
		if !strings.HasPrefix(p, sandbox.Workspace+"/") || slices.Contains(out, p) {
			continue
		}
		if out = append(out, p); len(out) == maxScripts {
			break
		}
	}
	return out
}

// needed returns the skills the run needs in its sandbox before command
// ("" at the sandbox's acquisition; extra: skill names found in the scripts
// it runs), with their dependencies.
func (i *Installer) needed(ctx context.Context, rc tools.RunContext, command string, extra []string) ([]installItem, error) {
	plan, err := i.Plan(ctx, rc)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, a := range plan.Attached {
		if a.Explicit || a.Reason != AttachDefault {
			names = append(names, a.Skill.Name)
		}
	}
	names = append(names, i.neededNames(ctx, rc)...)
	refs, root := SkillPaths(command)
	names = append(append(names, refs...), extra...)
	if root {
		index := plan.Names()
		names = append(names, index[:min(len(index), maxIndexInstall)]...)
	}
	return i.resolve(ctx, rc, names, nil, i.planLookup(plan)), nil
}

// lookupFunc finds a skill's package for the run.
type lookupFunc func(ctx context.Context, rc tools.RunContext, name string) (installItem, bool)

// resolve returns the package of each named skill and of its dependencies
// (declared and implicit), each once and skipping the names of skip; names
// lookup does not know are skipped.
func (i *Installer) resolve(ctx context.Context, rc tools.RunContext, names, skip []string, lookup lookupFunc) []installItem {
	var out []installItem
	seen := map[string]bool{}
	for _, n := range skip {
		seen[n] = true
	}
	queue := append([]string(nil), names...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		it, ok := lookup(ctx, rc, name)
		if !ok {
			continue
		}
		out = append(out, it)
		queue = append(append(queue, it.pkg.Requires...), it.pkg.Uses...)
	}
	return out
}

// cached returns the run's cached package of key, else finds and caches it.
func (i *Installer) cached(rc tools.RunContext, key string, find func() (installItem, bool)) (installItem, bool) {
	i.mu.Lock()
	it, ok := i.state(rc.RunID).resolved[key]
	i.mu.Unlock()
	if ok {
		return it, true
	}
	if it, ok = find(); !ok {
		return it, false
	}
	i.mu.Lock()
	i.state(rc.RunID).resolved[key] = it
	i.mu.Unlock()
	return it, true
}

// planLookup finds the skills the run's plan offers (the attached version,
// else the effective one).
func (i *Installer) planLookup(plan RunPlan) lookupFunc {
	return func(ctx context.Context, rc tools.RunContext, name string) (installItem, bool) {
		return i.cached(rc, name, func() (installItem, bool) {
			s, att, found := plan.Find(name)
			if !found {
				return installItem{}, false
			}
			if att != nil {
				return installItem{s, att.Package}, true
			}
			s, pkg, err := i.Registry.ByName(ctx, name)
			if err != nil {
				slog.Warn("skills: skill unavailable", "run_id", rc.RunID, "skill", name, "error", err)
				return installItem{}, false
			}
			return installItem{s, pkg}, true
		})
	}
}

// registryLookup finds any skill of the user's catalog, disabled ones
// included (the runtime's checkers).
func (i *Installer) registryLookup(ctx context.Context, rc tools.RunContext, name string) (installItem, bool) {
	return i.cached(rc, "\x00registry\x00"+name, func() (installItem, bool) {
		s, pkg, err := i.Registry.ByName(ctx, name)
		if err != nil {
			return installItem{}, false
		}
		return installItem{s, pkg}, true
	})
}

// EnsureRunSkills installs the skills the run needs (needed, without a
// command): the sandbox tools' OnAcquire hook, so a sandbox created after a
// load_skill holds that skill before its first command.
func (i *Installer) EnsureRunSkills(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox) {
	items, err := i.needed(ctx, rc, "", nil)
	if err != nil {
		slog.Warn("skills: resolving run skills failed", "run_id", rc.RunID, "error", err)
		return
	}
	if err := i.ensure(ctx, sb, items); err != nil {
		slog.Warn("skills: installing run skills failed", "run_id", rc.RunID, "error", err)
	}
}

// EnsureCommandSkills installs, before a command (foreground or background
// job) runs, the skills the run needs and the ones the command names
// (/skills/<name>/), directly or in the /workspace scripts it runs
// (ScriptPaths): a script of a skill that was never loaded still finds its
// files. The sandbox tools' BeforeCommand hook.
func (i *Installer) EnsureCommandSkills(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox, command string) error {
	extra, err := i.scriptRefs(ctx, rc, sb, ScriptPaths(command))
	if err != nil {
		return err
	}
	items, err := i.needed(ctx, rc, command, extra)
	if err != nil {
		return fmt.Errorf("resolving run skills: %w", err)
	}
	return i.ensure(ctx, sb, items)
}

// scriptRefs returns the skill names the scripts reference, read in one
// command (sandbox.ReadRegularFiles: regular files only, no symlink
// followed, maxScriptBytes in all), each file downloaded again only when its
// stamp changed (or never, when the run wrote it). Only a sandbox found gone
// is an error.
func (i *Installer) scriptRefs(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	known := make([]string, len(paths))
	i.mu.Lock()
	r := i.state(rc.RunID)
	for k, p := range paths {
		known[k] = r.scanned[p].stamp
	}
	i.mu.Unlock()
	files, err := sandbox.ReadRegularFiles(ctx, sb, paths, known, maxScriptBytes)
	if err != nil {
		if sandbox.IsGone(err) {
			return nil, err
		}
		slog.Warn("skills: reading the scripts of a command failed", "run_id", rc.RunID, "error", err)
		return nil, nil
	}
	var refs []string
	i.mu.Lock()
	defer i.mu.Unlock()
	r = i.state(rc.RunID)
	found := map[string]bool{}
	for _, f := range files {
		found[f.Path] = true
		sc := r.scanned[f.Path]
		if !f.Unchanged {
			sc.refs, _ = SkillPaths(string(f.Data))
		}
		sc.stamp = f.Stamp
		r.scanned[f.Path] = sc
		refs = append(refs, sc.refs...)
	}
	for _, p := range paths {
		if !found[p] {
			delete(r.scanned, p)
		}
	}
	return refs, nil
}

// NoteContent is the write_file / edit_file hook: the known skills a written
// file names (/skills/<name>/, e.g. a model-written script importing
// another skill's module) are needed by the run and installed now, with
// their dependencies; the file counts as scanned (a command running it does
// not download it while its size is unchanged).
func (i *Installer) NoteContent(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox, p string, content []byte) error {
	names, _ := SkillPaths(string(content))
	i.mu.Lock()
	i.state(rc.RunID).scanned[p] = scan{stamp: fmt.Sprintf("%d -", len(content)), refs: names}
	i.mu.Unlock()
	if len(names) == 0 {
		return nil
	}
	plan, err := i.Plan(ctx, rc)
	if err != nil {
		return fmt.Errorf("resolving run skills: %w", err)
	}
	refs := Referenced(string(content), plan.Names())
	if len(refs) == 0 {
		return nil
	}
	i.MarkNeeded(rc, refs...)
	return i.ensure(ctx, sb, i.resolve(ctx, rc, refs, nil, i.planLookup(plan)))
}

var (
	// Signs that a command failed on a missing file or module.
	missingSigns = []string{"No such file or directory", "can't open file", "No module named", "ModuleNotFoundError",
		"ImportError", "Cannot find module", "not found"}
	// A Python module the command could not import: its top-level name.
	missingModule = regexp.MustCompile(`(?:No module named|cannot import name '\w+' from) '([A-Za-z_]\w*)`)
)

// AfterFailedCommand is the safety net of a command that failed without a
// skill (the sandbox tools' AfterFailedCommand hook): when its output shows a
// missing file or module and names a skill not yet installed (a
// /skills/<name>/ path, or a Python module that only that skill's scripts/
// holds, e.g. a module → its skill), the skill is installed (with its
// dependencies) and recorded as needed. It returns the note for the tool
// result ("Skill <name> was installed; run the command again."; "" when
// nothing was installed); the command is never re-run (it may not be idempotent).
func (i *Installer) AfterFailedCommand(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox, output string) string {
	if !slices.ContainsFunc(missingSigns, func(s string) bool { return strings.Contains(output, s) }) {
		return ""
	}
	plan, err := i.Plan(ctx, rc)
	if err != nil {
		return ""
	}
	lookup := i.planLookup(plan)
	names := Referenced(output, plan.Names())
	for _, m := range missingModule.FindAllStringSubmatch(output, -1) {
		if name := i.moduleSkill(ctx, rc, plan, lookup, m[1]); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	var fresh []string
	for _, it := range i.resolve(ctx, rc, names, nil, lookup) {
		if slices.Contains(names, it.skill.Name) && !i.installed(ctx, sb, it.skill, it.pkg.Revision) {
			fresh = append(fresh, it.skill.Name)
		}
	}
	if len(fresh) == 0 {
		return ""
	}
	if err := i.ensure(ctx, sb, i.resolve(ctx, rc, fresh, nil, lookup)); err != nil {
		slog.Warn("skills: installing after a failed command failed", "run_id", rc.RunID, "skills", fresh, "error", err)
		return ""
	}
	i.MarkNeeded(rc, fresh...)
	if len(fresh) == 1 {
		return "Skill " + fresh[0] + " was installed; run the command again."
	}
	return "Skills " + strings.Join(fresh, ", ") + " were installed; run the command again."
}

// moduleSkill is the one skill of the plan whose scripts/ holds module.py ("" when none or several).
func (i *Installer) moduleSkill(ctx context.Context, rc tools.RunContext, plan RunPlan, lookup lookupFunc, module string) string {
	found := ""
	for _, name := range plan.Names() {
		it, ok := lookup(ctx, rc, name)
		if !ok {
			continue
		}
		for _, f := range it.pkg.Files {
			if f.Path == "scripts/"+module+".py" {
				if found != "" {
					return ""
				}
				found = name
			}
		}
	}
	return found
}

// InstallWithDependencies installs one skill and the skills it requires or
// uses (resolved in the run's plan): load_skill with a running sandbox.
func (i *Installer) InstallWithDependencies(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox, s Skill, pkg *Package) error {
	items := []installItem{{s, pkg}}
	if deps := append(append([]string(nil), pkg.Requires...), pkg.Uses...); len(deps) > 0 {
		plan, err := i.Plan(ctx, rc)
		if err != nil {
			return fmt.Errorf("resolving the dependencies of %s: %w", s.Name, err)
		}
		items = append(items, i.resolve(ctx, rc, deps, []string{s.Name}, i.planLookup(plan))...)
	}
	return i.ensure(ctx, sb, items)
}

// EnsureSkill installs a skill by name with its dependencies, all resolved in
// the user's catalog (disabled skills included) and cached per run, for the
// runtime's own checkers (verify.Options.EnsureSkill).
func (i *Installer) EnsureSkill(ctx context.Context, run *store.Run, sb sandbox.Sandbox, name string) error {
	rc := tools.RunContext{ThreadID: run.ThreadID, RunID: run.ID, Login: run.UserLogin, Sandbox: run.Sandbox}
	if _, ok := i.registryLookup(ctx, rc, name); !ok {
		return fmt.Errorf("skill %s: %w", name, ErrNotFound)
	}
	return i.ensure(ctx, sb, i.resolve(ctx, rc, []string{name}, nil, i.registryLookup))
}
