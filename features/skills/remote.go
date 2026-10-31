package skills

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/7ar-chat/harness/core/layout"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
)

// Project skills of a remote sandbox: the sandbox keeps its own repository checkout in /workspace, so its
// .har/skills, .agents/skills and .claude/skills folders are read through the sandbox API (never the host's
// disk) and merged over the global registry for that thread's runs (Installer).

// Bounds of the discovery of one sandbox.
const (
	// maxRemoteSkills caps the skill folders read from one sandbox.
	maxRemoteSkills = 64
	// maxRemoteSkillBytes caps the content of one project skill (the packages limits allow far more; a
	// repository's own skill is documentation and small scripts).
	maxRemoteSkillBytes = 8 << 20
	// readBatch is the number of files fetched by one read command.
	readBatch = 64
	// listDepth reaches every file of a package: the folder, its sub-folders up to MaxDirectoryDepth, the files.
	listDepth = MaxDirectoryDepth + 2
)

// RemoteDirs lists the sandbox directories skills are read from, lowest priority first (the order of Dirs).
func RemoteDirs() []string {
	var out []string
	for _, d := range []string{layout.DirName, agentsDir, claudeDir} {
		out = append(out, path.Join(sandbox.Workspace, d, layout.SkillsFolder))
	}
	return out
}

// LoadSandbox reads the project skills of the sandbox's /workspace into a new registry (source "project": they
// are repository text, so they are fenced like the local backend's project skills). A missing directory adds
// nothing. Symbolic links are not followed (sandbox.ReadRegularFiles reads regular files reached without
// one), the package limits apply (build validates every package) and a skill over maxRemoteSkillBytes, or one
// that is invalid, is logged and skipped. Only a sandbox found gone is an error.
func LoadSandbox(ctx context.Context, sb sandbox.Sandbox) (*Registry, error) {
	reg := &Registry{byName: map[string]*entry{}}
	for _, root := range RemoteDirs() {
		infos, err := sb.List(ctx, root, listDepth)
		if err != nil {
			if sandbox.IsGone(err) {
				return nil, err
			}
			continue // no such directory (or unreadable): nothing to add
		}
		for _, name := range skillFolders(root, infos) {
			files, err := readSkill(ctx, sb, root, name, infos)
			if err != nil {
				if sandbox.IsGone(err) {
					return nil, err
				}
				slog.Warn("skills: reading a sandbox project skill failed", "dir", root+"/"+name, "error", err)
				continue
			}
			pkg, issues := build(files)
			if len(issues) > 0 {
				slog.Error("skills: invalid sandbox project skill skipped", "dir", root+"/"+name, "error", invalidIssues(issues))
				continue
			}
			reg.put(SourceProject, name, pkg)
		}
	}
	return reg, nil
}

// hiddenPart reports a path element LoadFS leaves out of a package.
func hiddenPart(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, ".") || part == "__pycache__" {
			return true
		}
	}
	return strings.HasSuffix(p, ".pyc")
}

// relTo returns p relative to root (infos may hold absolute or root-relative paths).
func relTo(root, p string) string {
	if !strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	return strings.TrimPrefix(p, root+"/")
}

// skillFolders returns the names of the folders of root that hold a SKILL.md, sorted, at most maxRemoteSkills.
func skillFolders(root string, infos []sandbox.FileInfo) []string {
	seen := map[string]bool{}
	for _, fi := range infos {
		if fi.Type != "file" {
			continue
		}
		name, inner, ok := strings.Cut(relTo(root, fi.Path), "/")
		if ok && inner == "SKILL.md" && !strings.HasPrefix(name, ".") {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > maxRemoteSkills {
		slog.Warn("skills: too many sandbox project skills, the rest are ignored", "dir", root, "limit", maxRemoteSkills)
		names = names[:maxRemoteSkills]
	}
	return names
}

// readSkill fetches the files of root/name listed in infos. It fails for a skill over the entry or size limits
// and for a file cut by the read budget; files that are symbolic links (or under one) are simply absent, so a
// package whose SKILL.md is a link fails validation.
func readSkill(ctx context.Context, sb sandbox.Sandbox, root, name string, infos []sandbox.FileInfo) ([]File, error) {
	prefix := name + "/"
	var paths []string
	sizes := map[string]int64{}
	var total int64
	for _, fi := range infos {
		rel := relTo(root, fi.Path)
		if fi.Type != "file" || !strings.HasPrefix(rel, prefix) || hiddenPart(rel[len(prefix):]) {
			continue
		}
		abs := path.Join(root, rel)
		if abs != path.Clean(abs) || strings.ContainsAny(abs, "\x00\n") {
			continue
		}
		paths = append(paths, abs)
		sizes[abs] = fi.Size
		total += fi.Size
	}
	switch {
	case len(paths) > MaxEntries:
		return nil, fmt.Errorf("%d files (limit %d)", len(paths), MaxEntries)
	case total > maxRemoteSkillBytes:
		return nil, fmt.Errorf("%d bytes (limit %d)", total, int64(maxRemoteSkillBytes))
	}
	sort.Strings(paths)
	var files []File
	for start := 0; start < len(paths); start += readBatch {
		batch := paths[start:min(start+readBatch, len(paths))]
		var want int64
		for _, p := range batch {
			want += sizes[p]
		}
		got, err := sandbox.ReadRegularFiles(ctx, sb, batch, nil, want+1)
		if err != nil {
			return nil, err
		}
		for _, f := range got {
			size, _ := strconv.ParseInt(strings.Fields(f.Stamp)[0], 10, 64)
			if int64(len(f.Data)) != size {
				return nil, fmt.Errorf("%s changed while it was read", f.Path)
			}
			files = append(files, File{Path: strings.TrimPrefix(f.Path, root+"/"+prefix), Data: f.Data})
		}
	}
	return files, nil
}

// registry returns the registry of the run: the global one, with the project skills of the run's sandbox once
// they were read (discoverProject).
func (i *Installer) registry(rc tools.RunContext) *Registry {
	i.mu.Lock()
	defer i.mu.Unlock()
	if m := i.state(rc.RunID).merged; m != nil {
		return m
	}
	return i.Registry
}

// ByName returns the skill with that name as the run sees it: a project skill read from the run's sandbox
// replaces a global skill of the same name.
func (i *Installer) ByName(ctx context.Context, rc tools.RunContext, name string) (Skill, *Package, error) {
	return i.registry(rc).ByName(ctx, name)
}

// discoverProject reads the project skills of the sandbox into the run's registry (SandboxProject). It reads
// once per run, so once per sandbox acquisition of the run: the sandbox of a thread outlives its runs and the
// agent may add a skill folder between them, which the next run finds. It drops the run's cached plan and
// resolved packages, which did not know the new skills. Only a sandbox found gone is an error.
func (i *Installer) discoverProject(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox) error {
	if !i.SandboxProject {
		return nil
	}
	i.mu.Lock()
	st := i.state(rc.RunID)
	i.mu.Unlock()
	st.disc.Lock()
	defer st.disc.Unlock()
	i.mu.Lock()
	done := st.discovered
	i.mu.Unlock()
	if done {
		return nil
	}
	overlay, err := LoadSandbox(ctx, sb)
	if err != nil {
		return err
	}
	i.mu.Lock()
	st.discovered, st.merged, st.plan = true, i.Registry.merged(overlay), nil
	clear(st.resolved)
	i.mu.Unlock()
	if n := len(overlay.byName); n > 0 {
		slog.Info("skills: project skills read from the sandbox", "run_id", rc.RunID, "sandbox_id", sb.ID(), "skills", n)
	}
	return nil
}

// PromptSection is Registry.PromptSection for the run's registry: with SandboxProject it also lists the
// project skills of the thread's sandbox when one is already running (the prompt never starts a sandbox). A
// sandbox the run starts later is announced in the run by StepNote. It records what the prompt listed, so
// StepNote never lists a skill twice. It is an agent.PromptSection.
func (i *Installer) PromptSection(ctx context.Context, in agent.Input) string {
	run := in.Run
	rc := tools.RunContext{ThreadID: run.ThreadID, RunID: run.ID, Login: run.UserLogin, Sandbox: run.Sandbox}
	if i.SandboxProject && in.Sandbox && i.Sandboxes != nil && i.Sandboxes.Enabled() {
		if sb, err := i.Sandboxes.Get(ctx, run.ThreadID); err == nil {
			if err := i.discoverProject(ctx, rc, sb); err != nil {
				slog.Warn("skills: reading the sandbox's project skills failed", "run_id", run.ID, "error", err)
			}
		}
	}
	plan, err := i.Plan(ctx, rc)
	if err != nil {
		slog.Warn("skills: resolving run skills failed", "run_id", run.ID, "error", err)
		return ""
	}
	section := promptOf(ctx, plan, in)
	if section != "" {
		i.mu.Lock()
		i.state(rc.RunID).announced = announcedBy(plan)
		i.mu.Unlock()
	}
	return section
}

// announcedBy maps the skills a plan's index lists to their revision.
func announcedBy(plan RunPlan) map[string]string {
	out := map[string]string{}
	for _, s := range listed(plan) {
		out[s.Name] = s.Revision
	}
	return out
}

// ProjectSkillsNote introduces the skills StepNote announces.
const ProjectSkillsNote = "The sandbox started during this run holds project skills that were not in the skills index of your instructions. Call load_skill with one of these names when a request matches it."

// StepNote tells the model, before its next request, about the project skills of the run's sandbox that the
// prompt could not list because the sandbox had not started when the run began: once discovery (the sandbox's
// first acquisition of the run) found project skills the prompt did not list (by name and revision), it
// returns them in the index format, project text fenced as data (Fence, UserSkillNote), and then nothing for
// the rest of the run. It returns "" without SandboxProject, before discovery, when the prompt listed every
// project skill (a sandbox already running), and when the sandbox has none. It never starts a sandbox. It is an
// agent.StepNote.
func (i *Installer) StepNote(ctx context.Context, in agent.Input) string {
	if !i.SandboxProject || !in.Sandbox || in.Run == nil {
		return ""
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	st := i.state(in.Run.ID)
	if !st.discovered || st.merged == nil {
		return ""
	}
	var b strings.Builder
	for _, s := range st.merged.List(ctx) {
		if s.Source != SourceProject {
			continue
		}
		if rev, seen := st.announced[s.Name]; seen && rev == s.Revision {
			continue
		}
		if st.announced == nil {
			st.announced = map[string]string{}
		}
		st.announced[s.Name] = s.Revision
		line, _ := indexLine(s)
		b.WriteString(line + "\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return "<skills>\n" + ProjectSkillsNote + "\n" + b.String() + UserSkillNote + "\n</skills>"
}
