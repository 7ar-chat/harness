package skills

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/syncx"
)

// Root is where skills are installed in the sandbox.
const Root = "/skills"

// writeConcurrency bounds the parallel file uploads of one installation.
const writeConcurrency = 8

// Bounds of the per-(sandbox, skill) install lock.
const (
	lockTTL  = 3 * time.Minute
	lockWait = 3 * time.Minute
	lockPoll = 50 * time.Millisecond
)

// InstallState is the state installers of several runtimes share: which
// revision of a skill each sandbox holds, and short locks serializing the
// installation of one skill in one sandbox. redisx.SkillInstalls implements it
// on Redis; nil means a single process (nothing shared).
type InstallState interface {
	// Revision returns the recorded revision of the skill in the sandbox ("" when none).
	Revision(ctx context.Context, sandboxID, skill string) (string, error)
	// Record stores skill -> revision for the sandbox, kept for ttl.
	Record(ctx context.Context, sandboxID string, revisions map[string]string, ttl time.Duration) error
	// TryLock takes key for ttl unless held (false: held by another).
	TryLock(ctx context.Context, key, token string, ttl time.Duration) (bool, error)
	// Unlock releases key while it still holds token.
	Unlock(ctx context.Context, key, token string) error
}

// Installer copies skills into sandboxes, once per (sandbox, skill revision).
// An installation counts once recorded in the InstallState (shared by every runtime) and
// its marker file /skills/<name>/.installed-<revision> (written last) is in
// the sandbox; each runtime then remembers it. Installations of one skill in
// one sandbox are serialized across runtimes (an InstallState lock). Which skills a
// run needs is decided lazily (lazy.go); loading a skill never starts a sandbox.
type Installer struct {
	Registry *Registry
	Shared   InstallState
	// Messages reads the thread's history: the skills loaded or used in its
	// earlier runs are installed again (nil: this run's only).
	Messages History

	// done remembers recent verified installations (sandboxID|name|revision),
	// bounded (syncx.DefaultLRUSize).
	done syncx.LRU[string, struct{}]
	mu   sync.Mutex
	runs map[string]*runState
	// per-sandbox install serialization in this process (entries freed on release)
	locks syncx.KeyedMutex
}

// History lists a thread's messages (store.Store implements it).
type History interface {
	ListMessages(ctx context.Context, threadID string) ([]*store.Message, error)
}

type installItem struct {
	skill Skill
	pkg   *Package
}

func lockKey(sandboxID, name string) string { return "work:skills:lock:" + sandboxID + ":" + name }

var hexRevision = regexp.MustCompile(`^[0-9a-f]{1,128}$`)

// marker is the file proving a complete installation of that revision in dir.
func marker(dir, rev string) string {
	if !hexRevision.MatchString(rev) {
		rev = "x"
	}
	return dir + "/.installed-" + rev
}

// installed reports a verified installation: known to this runtime, or
// recorded in the InstallState with its marker present in the sandbox (one Stat, then
// remembered).
func (i *Installer) installed(ctx context.Context, sb sandbox.Sandbox, s Skill, rev string) bool {
	key := sb.ID() + "|" + s.Name + "|" + rev
	if _, ok := i.done.Get(key); ok {
		return true
	}
	if i.Shared == nil {
		return false
	}
	got, err := i.Shared.Revision(ctx, sb.ID(), s.Name)
	if err != nil || got != rev {
		return false
	}
	if _, err := sb.Stat(ctx, marker(Root+"/"+s.Name, rev)); err != nil {
		return false
	}
	i.done.Add(key, struct{}{})
	return true
}

func (i *Installer) record(ctx context.Context, sandboxID string, fields map[string]string) {
	if i.Shared == nil || len(fields) == 0 {
		return
	}
	if err := i.Shared.Record(ctx, sandboxID, fields, 7*24*time.Hour); err != nil {
		slog.Warn("skills: recording installation failed", "sandbox_id", sandboxID, "error", err)
	}
}

// lockSkills takes the install locks of the skills in one sandbox (in
// name order), waiting up to lockWait for another runtime's installation. A
// lock error leaves the installation unlocked (this runtime's lock remains).
func (i *Installer) lockSkills(ctx context.Context, sandboxID string, names []string) (func(), error) {
	if i.Shared == nil {
		return func() {}, nil
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	token := uuid.NewString()
	var held []string
	release := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, k := range held {
			_ = i.Shared.Unlock(rctx, k, token)
		}
	}
	deadline := time.Now().Add(lockWait)
	for _, n := range sorted {
		k := lockKey(sandboxID, n)
		for {
			ok, err := i.Shared.TryLock(ctx, k, token, lockTTL)
			if err != nil {
				slog.Warn("skills: install lock unavailable, installing without it", "sandbox_id", sandboxID, "skill", n, "error", err)
				break
			}
			if ok {
				held = append(held, k)
				break
			}
			if time.Now().After(deadline) {
				release()
				return nil, fmt.Errorf("waiting for another installation of %s", n)
			}
			select {
			case <-ctx.Done():
				release()
				return nil, ctx.Err()
			case <-time.After(lockPoll):
			}
		}
	}
	return release, nil
}

func run(ctx context.Context, sb sandbox.Sandbox, cmd string) error {
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: cmd, Cwd: "/", Timeout: 60 * time.Second})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("command exited with %d", res.ExitCode)
	}
	return nil
}

// ensure installs the items the sandbox does not hold yet, in one batch,
// under this runtime's sandbox lock and the skills' Redis locks (re-checked
// once held: another runtime may have installed them meanwhile). Only a
// complete installation is recorded.
func (i *Installer) ensure(ctx context.Context, sb sandbox.Sandbox, items []installItem) error {
	missing := func(items []installItem) []installItem {
		var out []installItem
		for _, it := range items {
			if !i.installed(ctx, sb, it.skill, it.pkg.Revision) {
				out = append(out, it)
			}
		}
		return out
	}
	todo := missing(items)
	if len(todo) == 0 {
		return nil
	}
	defer i.locks.Lock(sb.ID())()
	names := make([]string, len(todo))
	for k, it := range todo {
		names[k] = it.skill.Name
	}
	release, err := i.lockSkills(ctx, sb.ID(), names)
	if err != nil {
		return err
	}
	defer release()
	if todo = missing(todo); len(todo) == 0 {
		return nil
	}
	if err := i.installAll(ctx, sb, todo); err != nil {
		return err
	}
	fields := map[string]string{}
	names = names[:0]
	for _, it := range todo {
		fields[it.skill.Name] = it.pkg.Revision
		names = append(names, it.skill.Name)
		i.done.Add(sb.ID()+"|"+it.skill.Name+"|"+it.pkg.Revision, struct{}{})
	}
	i.record(ctx, sb.ID(), fields)
	slog.Info("skills installed", "sandbox_id", sb.ID(), "skills", strings.Join(names, ","))
	return nil
}

// installAll copies the packages to /skills/<name>/ (replacing older
// revisions), writes their markers and makes them read-only, with one prepare
// and one protect command.
func (i *Installer) installAll(ctx context.Context, sb sandbox.Sandbox, items []installItem) error {
	if len(items) == 0 {
		return nil
	}
	dirs := make([]string, len(items))
	for k, it := range items {
		dirs[k] = Root + "/" + it.skill.Name
	}
	// Skill names are slugs ([a-z0-9-]): safe unquoted in a shell command.
	all := strings.Join(dirs, " ")
	if err := run(ctx, sb, fmt.Sprintf("mkdir -p %s && chmod -R u+w %s 2>/dev/null; rm -rf %s && mkdir -p %s", Root, all, all, all)); err != nil {
		return fmt.Errorf("prepare %s: %w", strings.Join(dirs, ", "), err)
	}
	type job struct {
		path string
		data []byte
	}
	var jobs []job
	for k, it := range items {
		for _, f := range it.pkg.Files {
			jobs = append(jobs, job{path.Join(dirs[k], f.Path), f.Data})
		}
	}
	sem := make(chan struct{}, writeConcurrency)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := sb.WriteFile(ctx, j.path, j.data); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("install %s: %w", j.path, err)
				}
				errMu.Unlock()
			}
		}(j)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr // nothing recorded: the next attempt starts over (prepare)
	}
	// The markers last, once every file is in place: a partial install has none.
	for k, it := range items {
		if err := sb.WriteFile(ctx, marker(dirs[k], it.pkg.Revision), nil); err != nil {
			return fmt.Errorf("install %s: %w", marker(dirs[k], it.pkg.Revision), err)
		}
	}
	// Read-only for the agent by convention: scripts executable, nothing writable.
	protect := make([]string, len(dirs))
	for k, d := range dirs {
		protect[k] = fmt.Sprintf("(test -d %[1]s/scripts && find %[1]s/scripts -type f -exec chmod a+x {} + ; true) && chmod -R a+rX,a-w %[1]s", d)
	}
	if err := run(ctx, sb, strings.Join(protect, " && ")); err != nil {
		return fmt.Errorf("protect %s: %w", strings.Join(dirs, ", "), err)
	}
	return nil
}

// Install copies one package to /skills/<name>/ and makes it read-only (once
// per sandbox and revision).
func (i *Installer) Install(ctx context.Context, sb sandbox.Sandbox, s Skill, pkg *Package) error {
	return i.ensure(ctx, sb, []installItem{{s, pkg}})
}

// Where a load_skill result says the skill's files are.
type Where int

const (
	// NoSandbox: the run has no sandbox; the scripts cannot run.
	NoSandbox Where = iota
	// Installed: the files are in the running sandbox.
	Installed
	// OnFirstCommand: no sandbox is running yet; the files are installed
	// before the run's first command (loading the skill started nothing).
	OnFirstCommand
)

// Instructions is the load_skill result: SKILL.md body and the file list.
func Instructions(s Skill, pkg *Package, where Where) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Skill: %s\n\n", s.Name)
	dir := Root + "/" + s.Name
	use := fmt.Sprintf("Relative paths in this skill (`scripts/…`) mean `%s/…`: run `python3 %s/scripts/X.py` from /workspace; never cd into or write to the skill folder. Write final deliverables to /workspace/outputs/.", dir, dir)
	switch where {
	case Installed:
		fmt.Fprintf(&b, "Installed at %s/ (read-only). %s\n\n", dir, use)
	case OnFirstCommand:
		fmt.Fprintf(&b, "Loading it started nothing: its files are at %s/ (read-only) from your first command. %s\n\n", dir, use)
	default:
		b.WriteString("No sandbox is available: follow the instructions without running the scripts.\n\n")
	}
	body := strings.TrimSpace(pkg.Body)
	if s.Untrusted() {
		b.WriteString(UserSkillNote + "\n\n")
		body = Fence(s.Name, body)
	}
	b.WriteString(body)
	b.WriteString("\n\n## Files\n")
	for _, p := range pkg.Paths() {
		fmt.Fprintf(&b, "- %s/%s/%s\n", Root, s.Name, p)
	}
	return b.String()
}
