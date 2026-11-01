// Package policy governs what the agents of a factory task may do.
//
// Engine is the agent's BeforeToolHook, ToolDenier and RiskJudge for runs of
// threads that have a factory.ThreadInfo. Every tool call is classified
// (Classify: read, write in the workspace, exec, net, git push, force push,
// merge, secrets, destructive, outside the workspace, publish) and decided by
// the thread's policy (Decide):
//
//   - always denied, whatever the policy: force pushes, reading secrets,
//     destructive commands (sudo, rm -rf of a wide target, curl | sh, ...);
//   - sandboxed (the default) and autonomous: free inside the sandbox; a push
//     of the task branch is allowed, a push to a protected branch or the base
//     branch, a merge, a publish, a write outside /workspace and a write to a
//     protected path are denied;
//   - supervised: reads are free, every other call asks a human.
//
// Config.Deny and Config.Ask patterns (Bash(git push*), Write(*.lock),
// Tool(open_pr)) are checked first: Deny wins over Ask wins over the table.
//
// A run whose thread has no ThreadInfo (an ordinary chat) is not governed at
// all: the Engine never denies, never asks.
//
// Approvals: Ask is only possible with Options.Inner, the BeforeToolHook
// that records the pending approval (the approvals gate, which Pause
// delegates to). Without an Inner a call the policy would ask about is
// denied instead (the denial says to use ask_user), and Pause returns an
// error: this package does not build its own approval records.
package policy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/approvals"
	"github.com/7ar-chat/harness/engine/tools"
)

// DefaultProtected are the paths no task may write unless the policy is
// supervised and a human approves.
var DefaultProtected = []string{".github/workflows/**", "**/.env*", "**/*.pem", "**/id_rsa*", "**/.git-credentials"}

// builtinProtectedBranches are protected whatever ProtectedBranches says.
var builtinProtectedBranches = []string{"main", "master"}

const (
	cacheTTL    = 30 * time.Second
	cacheMax    = 1024
	auditTime   = 5 * time.Second
	commandKeep = 200
)

// Config holds extra rules, checked before the policy table. A pattern is
// Name(glob): Bash(terraform apply*) matches a simple command of an
// execute_command call, Write(*.lock) a written path, Read(secrets/**) a path
// read by a read tool, Tool(open_pr) a tool name (a bare glob is a tool
// name). "*" matches any character, "/" included.
type Config struct {
	Deny []string
	Ask  []string
}

// Options configure an Engine.
type Options struct {
	Store factory.Store
	// Protected are the globs of files no task writes (nil: DefaultProtected).
	// A glob without "/" matches the base name at any depth; "**" spans
	// directories.
	Protected []string
	// ProtectedBranches returns the protected branches of a repository (nil: only main and master and the task's base branch).
	ProtectedBranches func(ctx context.Context, repo string) []string
	// DefaultPolicy applies to a thread whose ThreadInfo has no policy (default sandboxed).
	DefaultPolicy string
	Config        Config
	// Escalate is told once per (task, rule) when a task tries a force push or to read secrets.
	Escalate func(ctx context.Context, taskID, reason string)
	// Audit records a policy event (nil: Store.AppendEvent).
	Audit func(ctx context.Context, e factory.TaskEvent)
	// Inner records the pending approval of a call the policy asks about
	// (nil: such calls are denied).
	Inner agent.BeforeToolHook
	// Now is the clock (nil: time.Now); tests move it.
	Now func() time.Time
}

// Engine decides the tool calls of factory tasks. Safe for concurrent use.
type Engine struct {
	o         Options
	protected []string
	deny, ask []rule
	threads   *ttlCache[*factory.ThreadInfo]
	branches  *ttlCache[[]string]

	mu        sync.Mutex
	escalated map[string]struct{}
}

var (
	_ agent.BeforeToolHook = (*Engine)(nil)
	_ agent.ToolDenier     = (*Engine)(nil)
	_ agent.RiskJudge      = (*Engine)(nil)
)

// New returns an Engine.
func New(o Options) *Engine {
	if o.DefaultPolicy == "" {
		o.DefaultPolicy = factory.PolicySandboxed
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	prot := o.Protected
	if prot == nil {
		prot = DefaultProtected
	}
	return &Engine{
		o:         o,
		protected: slices.Clone(prot),
		deny:      parseRules(o.Config.Deny),
		ask:       parseRules(o.Config.Ask),
		threads:   newCache[*factory.ThreadInfo](o.Now),
		branches:  newCache[[]string](o.Now),
		escalated: map[string]struct{}{},
	}
}

// Context is what Decide needs to know about the task.
type Context struct {
	// Base is the task's base branch; Branch its own branch.
	Base, Branch string
	// ProtectedPaths are globs; ProtectedBranches branch names or globs.
	ProtectedPaths    []string
	ProtectedBranches []string
	// CanAsk says a human can be asked: without it an Ask becomes a Deny.
	CanAsk bool
}

// Decision is the verdict on one action.
type Decision struct {
	Allow bool
	Ask   bool
	// Deny is the tool error shown to the model (it says what to do instead); empty when the call is allowed or asked.
	Deny string
	// Rule names what decided, for the audit trail.
	Rule string
}

func allow(rule string) Decision { return Decision{Allow: true, Rule: rule} }

// Decide applies the policy table to an action. The Config rules are not
// part of it (the Engine applies them first).
func Decide(a Action, policy string, c Context) Decision {
	switch a.Kind {
	case KindGitForcePush:
		return Decision{Rule: "forbidden:force-push", Deny: "Not run: force pushes, branch deletions and mirror pushes are always blocked by policy " + policy +
			". Push your own branch normally (git push origin HEAD) and call open_pr; if history has to be rewritten, create a new branch instead, or ask a human with ask_user."}
	case KindSecrets:
		return Decision{Rule: "forbidden:secrets", Deny: "Not run: this reads credentials or the process environment, which policy " + policy +
			" always blocks. The tools you were given (open_pr, pr_status, ci_log) authenticate for you, so never print, copy or upload tokens. If you need access to something, ask a human with ask_user."}
	case KindDestructive:
		why := a.Detail
		if why == "" {
			why = "it can destroy the workspace or the machine"
		}
		return Decision{Rule: "forbidden:destructive", Deny: "Not run: " + why + "; policy " + policy +
			" always blocks this. Run narrower commands without elevated privileges inside /workspace/repo (name the exact files to delete, download to a file and read it before running it)."}
	}

	// A protected path is never written by a task; a human may approve it when supervised.
	if a.Kind != KindRead {
		for _, p := range a.Paths {
			if pat, ok := matchProtected(c.ProtectedPaths, p); ok {
				d := Decision{Rule: "protected-path:" + pat, Deny: "Not run: " + p + " is a protected path (" + pat + ") that policy " + policy +
					" does not let tasks change. Leave it as it is and explain in the pull request what has to change there, or ask a human with ask_user."}
				return supervise(d, policy, c)
			}
		}
	}

	if policy == factory.PolicySupervised {
		if a.Kind == KindRead {
			return allow("policy:supervised:read")
		}
		return supervise(Decision{Rule: "policy:supervised:" + a.Kind.String(), Deny: deniedByKind(a, policy)}, policy, c)
	}

	// sandboxed, autonomous and any policy name we do not know.
	switch a.Kind {
	case KindRead, KindWriteWorkspace, KindExec, KindNet:
		return allow("policy:" + policy + ":" + a.Kind.String())
	case KindGitPush:
		for _, b := range a.Branches {
			switch {
			case b == unknownBranch:
				return Decision{Rule: "protected-branch:unknown", Deny: "Not run: this push names a branch the policy cannot read, and policy " + policy +
					" only lets you push your own branch. Write the branch out: git push origin HEAD (or git push origin " + branchOr(c.Branch) + "), then call open_pr."}
			case protectedBranch(b, c):
				return Decision{Rule: "protected-branch:" + b, Deny: "Not run: pushing to the protected branch " + b + " is blocked by policy " + policy +
					". Push your own branch and call open_pr; ask a human with ask_user if you need the merge."}
			}
		}
		return allow("policy:" + policy + ":git_push")
	}
	return Decision{Rule: "policy:" + policy + ":" + a.Kind.String(), Deny: deniedByKind(a, policy)}
}

func branchOr(b string) string {
	if b == "" {
		return "<your branch>"
	}
	return b
}

func protectedBranch(b string, c Context) bool {
	if b == c.Base && b != "" {
		return true
	}
	for _, pat := range c.ProtectedBranches {
		if pat == b || globMatch(pat, b) {
			return true
		}
	}
	return false
}

// supervise turns a denial into a question when a human can be asked.
func supervise(d Decision, policy string, c Context) Decision {
	if policy == factory.PolicySupervised && c.CanAsk {
		return Decision{Ask: true, Rule: d.Rule}
	}
	if policy == factory.PolicySupervised && !strings.Contains(d.Deny, "ask_user") {
		d.Deny += " Nobody can approve it here: ask a human with ask_user."
	} else if policy == factory.PolicySupervised {
		d.Deny += " Nobody can approve it here."
	}
	return d
}

func deniedByKind(a Action, policy string) string {
	switch a.Kind {
	case KindMerge:
		return "Not run: merging pull requests is blocked by policy " + policy + ". Open the pull request with open_pr and leave the merge to a human; ask with ask_user if it is urgent."
	case KindPublish:
		return "Not run: publishing packages, images or releases is blocked by policy " + policy + ". Build and test locally, and ask a human with ask_user to publish."
	case KindOutsideWorkspace:
		d := a.Detail
		if d == "" {
			d = "it acts outside /workspace"
		}
		return "Not run: " + d + "; policy " + policy + " keeps tasks inside /workspace. Use paths under /workspace/repo (or /tmp for scratch files)."
	case KindGitPush:
		return "Not run: git push needs a human under policy " + policy + ". Commit your work and call open_pr, or ask a human with ask_user."
	}
	return "Not run: " + a.Kind.String() + " calls need a human under policy " + policy + ". Ask with ask_user, or use a read-only tool."
}

// evaluation is the verdict on one tool call.
type evaluation struct {
	d      Decision
	a      Action
	info   *factory.ThreadInfo
	policy string
}

// evaluate judges a call; ok is false when the call is not governed.
func (e *Engine) evaluate(ctx context.Context, run *store.Run, toolName, args string) (ev evaluation, ok bool) {
	if run == nil || run.ThreadID == "" {
		return ev, false
	}
	info := e.threadInfo(ctx, run.ThreadID)
	if info == nil {
		return ev, false
	}
	pol := info.Policy
	if pol == "" {
		pol = e.o.DefaultPolicy
	}
	if pol == "" {
		return ev, false
	}
	a := Classify(toolName, args)
	c := Context{Base: info.BaseBranch, Branch: info.Branch, ProtectedPaths: e.protected, CanAsk: e.o.Inner != nil}
	if a.Kind == KindGitPush {
		c.ProtectedBranches = e.protectedBranches(ctx, info.Repo)
	}
	ev = evaluation{a: a, info: info, policy: pol}
	switch {
	case e.configMatch(e.deny, toolName, a) != "":
		pat := e.configMatch(e.deny, toolName, a)
		ev.d = Decision{Rule: "config.deny:" + pat, Deny: "Not run: " + describe(toolName, a) + " matches the deny rule " + pat +
			" of the project's policy. Do not retry it; choose another approach, or ask a human with ask_user."}
	case e.configMatch(e.ask, toolName, a) != "":
		pat := e.configMatch(e.ask, toolName, a)
		ev.d = Decision{Ask: true, Rule: "config.ask:" + pat}
		if !c.CanAsk {
			ev.d = Decision{Rule: ev.d.Rule, Deny: "Not run: " + describe(toolName, a) + " matches the rule " + pat +
				", which needs a human's approval, and nobody can approve it here. Ask a human with ask_user."}
		}
	default:
		ev.d = Decide(a, pol, c)
	}
	return ev, true
}

func describe(toolName string, a Action) string {
	if a.Command != "" {
		return "`" + clip(a.Command, 80) + "`"
	}
	if len(a.Paths) > 0 {
		return toolName + " on " + a.Paths[0]
	}
	return "the call " + toolName
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && s[i]&0xC0 == 0x80 { // do not cut a UTF-8 sequence
		i--
	}
	return s[:i] + "..."
}

func (e *Engine) threadInfo(ctx context.Context, threadID string) *factory.ThreadInfo {
	if v, ok := e.threads.get(threadID); ok {
		return v
	}
	if e.o.Store == nil {
		return nil
	}
	ti, err := e.o.Store.GetThreadInfo(ctx, threadID)
	switch {
	case err == nil:
		e.threads.put(threadID, ti)
		return ti
	case errors.Is(err, factory.ErrNotFound):
		e.threads.put(threadID, nil)
	default:
		slog.Warn("policy: loading the thread info failed; the thread is not governed", "thread_id", threadID, "error", err)
	}
	return nil
}

func (e *Engine) protectedBranches(ctx context.Context, repo string) []string {
	names := slices.Clone(builtinProtectedBranches)
	if e.o.ProtectedBranches == nil {
		return names
	}
	extra, ok := e.branches.get(repo)
	if !ok {
		extra = e.o.ProtectedBranches(ctx, repo)
		e.branches.put(repo, extra)
	}
	return append(names, extra...)
}

// NeedsApproval implements agent.BeforeToolHook: only a supervised task (or an
// Ask rule) with an approval gate waits for a human.
func (e *Engine) NeedsApproval(ctx context.Context, run *store.Run, toolName, arguments string) bool {
	ev, ok := e.evaluate(ctx, run, toolName, arguments)
	if !ok || !ev.d.Ask {
		return false
	}
	return !e.approvedForRun(run, toolName)
}

// Risky implements agent.RiskJudge.
func (e *Engine) Risky(ctx context.Context, run *store.Run, toolName, arguments string) bool {
	ev, ok := e.evaluate(ctx, run, toolName, arguments)
	return ok && ev.d.Ask
}

// approvedForRun: "approve for the run" covers file edits (their path was
// shown), never commands.
func (e *Engine) approvedForRun(run *store.Run, toolName string) bool {
	if toolName != "write_file" && toolName != "edit_file" {
		return false
	}
	return slices.Contains(run.ApprovedTools, approvals.Kind(toolName))
}

// Pause implements agent.BeforeToolHook: it records the audit event and has
// Options.Inner record the approval.
func (e *Engine) Pause(ctx context.Context, run *store.Run, tc ai.ToolCall) (*tools.Interrupt, error) {
	if e.o.Inner == nil {
		return nil, errors.New("policy: no approval gate configured")
	}
	if ev, ok := e.evaluate(ctx, run, tc.Function.Name, tc.Function.Arguments); ok && ev.d.Ask {
		e.audit(ctx, ev, tc.Function.Name, "ask")
	}
	return e.o.Inner.Pause(ctx, run, tc)
}

// Denial implements agent.ToolDenier.
func (e *Engine) Denial(ctx context.Context, run *store.Run, toolName, arguments string) string {
	ev, ok := e.evaluate(ctx, run, toolName, arguments)
	if !ok || ev.d.Deny == "" {
		return ""
	}
	e.audit(ctx, ev, toolName, "deny")
	if ev.a.Kind == KindGitForcePush || ev.a.Kind == KindSecrets {
		e.escalate(ctx, ev)
	}
	return ev.d.Deny
}

func (e *Engine) escalate(ctx context.Context, ev evaluation) {
	if e.o.Escalate == nil || ev.info.TaskID == "" {
		return
	}
	key := ev.info.TaskID + "\x00" + ev.d.Rule
	e.mu.Lock()
	_, seen := e.escalated[key]
	if !seen {
		if len(e.escalated) >= cacheMax*4 {
			clear(e.escalated)
		}
		e.escalated[key] = struct{}{}
	}
	e.mu.Unlock()
	if seen {
		return
	}
	e.o.Escalate(ctx, ev.info.TaskID, fmt.Sprintf("policy %s blocked a %s attempt (%s): %s",
		ev.policy, ev.a.Kind, ev.d.Rule, redact(ev.a.Command, commandKeep)))
}

func (e *Engine) audit(ctx context.Context, ev evaluation, toolName, decision string) {
	if ev.info.TaskID == "" {
		return
	}
	data := map[string]any{
		"tool":     toolName,
		"rule":     ev.d.Rule,
		"kind":     ev.a.Kind.String(),
		"decision": decision,
		"policy":   ev.policy,
	}
	cmd := ev.a.Command
	if cmd == "" && len(ev.a.Paths) > 0 {
		cmd = ev.a.Paths[0]
	}
	if cmd != "" {
		data["command"] = redact(cmd, commandKeep)
	}
	text := fmt.Sprintf("%s %s: %s (%s)", decision, toolName, ev.a.Kind, ev.d.Rule)
	te := factory.TaskEvent{TaskID: ev.info.TaskID, Kind: factory.EventPolicy, Text: text, Data: data, At: e.o.Now()}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTime)
	defer cancel()
	switch {
	case e.o.Audit != nil:
		e.o.Audit(actx, te)
	case e.o.Store != nil:
		if err := e.o.Store.AppendEvent(actx, te); err != nil {
			slog.Warn("policy: recording the audit event failed", "task_id", te.TaskID, "error", err)
		}
	}
}

var redactions = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic|token)?\s*)\S+`),
	regexp.MustCompile(`(?i)([A-Za-z0-9_]*(?:token|secret|password|passwd|api_?key|credential)[A-Za-z0-9_]*\s*[=:]\s*)\S+`),
	regexp.MustCompile(`(://[^/\s:@]+:)[^@\s/]+@`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,})\b`),
}

// redact hides the secrets in a command and keeps its first n bytes.
func redact(cmd string, n int) string {
	cmd = redactions[0].ReplaceAllString(cmd, "${1}[redacted]")
	cmd = redactions[1].ReplaceAllString(cmd, "${1}[redacted]")
	cmd = redactions[2].ReplaceAllString(cmd, "${1}[redacted]@")
	cmd = redactions[3].ReplaceAllString(cmd, "[redacted]")
	return clip(cmd, n)
}

// ttlCache is a bounded cache whose entries expire.
type ttlCache[V any] struct {
	mu  sync.Mutex
	m   map[string]cacheEntry[V]
	now func() time.Time
}

type cacheEntry[V any] struct {
	v   V
	exp time.Time
}

func newCache[V any](now func() time.Time) *ttlCache[V] {
	return &ttlCache[V]{m: map[string]cacheEntry[V]{}, now: now}
}

func (c *ttlCache[V]) get(k string) (v V, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	en, found := c.m[k]
	if !found || !c.now().Before(en.exp) {
		delete(c.m, k)
		return v, false
	}
	return en.v, true
}

func (c *ttlCache[V]) put(k string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.m) >= cacheMax {
		for key, en := range c.m {
			if !now.Before(en.exp) {
				delete(c.m, key)
			}
		}
		for key := range c.m { // still full: drop arbitrary entries
			if len(c.m) < cacheMax {
				break
			}
			delete(c.m, key)
		}
	}
	c.m[k] = cacheEntry[V]{v: v, exp: now.Add(cacheTTL)}
}
