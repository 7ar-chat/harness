package delegate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/engine/tools"
)

// Defaults and limits.
const (
	DefaultMaxDepth    = 2
	DefaultMaxParallel = 3
	DefaultTimeout     = 30 * time.Minute

	// maxReportChars caps one report so a chatty sub-agent cannot flood the caller's context.
	maxReportChars = 8000
	// maxChain bounds the walk up ParentThreadID links (a cycle in the store ends there).
	maxChain = 8
	// maxItems bounds one delegate_many call; maxPerRun the delegations of one run.
	maxItems  = 8
	maxPerRun = 24
	maxTitle  = 80
)

// Options configures a Provider.
type Options struct {
	// Spawner runs the sub-agents.
	Spawner factory.Spawner
	// Store reads the calling thread's ThreadInfo: its login, task, policy and
	// repository are inherited, its ParentThreadID chain gives the depth. Nil
	// treats every thread as an ordinary one at depth 0.
	Store factory.Store
	// Roles is the catalogue (nil: DefaultRoles).
	Roles map[string]Role
	// MaxDepth is how deep delegation nests: a thread already that many
	// delegations below an ordinary thread gets no delegate tool (0: 2).
	MaxDepth int
	// MaxParallel bounds the sub-agents one run has running at once (0: 3).
	MaxParallel int
	// Models are the model ids the caller may pick with the model argument;
	// empty means the caller cannot choose and the role or spawner default applies.
	Models []string
	// Timeout bounds one delegation (0: 30 minutes).
	Timeout time.Duration
	// Task loads the factory task of a thread so the sub-agent also gets the
	// task's title and spec. Optional.
	Task func(ctx context.Context, taskID string) (*factory.Task, error)
	// OnSpend is told what each finished sub-run spent, for the thread's task
	// (only when the thread has one). Optional.
	OnSpend func(ctx context.Context, taskID string, d factory.Spend)
}

// Provider offers the delegate and delegate_many tools.
type Provider struct {
	spawner  factory.Spawner
	store    factory.Store
	roles    map[string]Role
	maxDepth int
	parallel int
	models   []string
	timeout  time.Duration
	task     func(ctx context.Context, taskID string) (*factory.Task, error)
	onSpend  func(ctx context.Context, taskID string, d factory.Spend)
	// branchSeq numbers isolated branches so two sub-agents never share one.
	branchSeq atomic.Int64
}

// New returns a Provider; zero Options fields take their defaults.
func New(o Options) *Provider {
	p := &Provider{spawner: o.Spawner, store: o.Store, roles: o.Roles, maxDepth: o.MaxDepth, parallel: o.MaxParallel,
		models: o.Models, timeout: o.Timeout, task: o.Task, onSpend: o.OnSpend}
	if p.roles == nil {
		p.roles = DefaultRoles()
	}
	if p.maxDepth <= 0 {
		p.maxDepth = DefaultMaxDepth
	}
	p.maxDepth = min(p.maxDepth, maxChain)
	if p.parallel <= 0 {
		p.parallel = DefaultMaxParallel
	}
	if p.timeout <= 0 {
		p.timeout = DefaultTimeout
	}
	return p
}

// Name implements tools.Provider.
func (p *Provider) Name() string { return "delegate" }

type input struct {
	Role     string `json:"role" jsonschema_description:"The role of the sub-agent (see the tool description for the roles)"`
	Task     string `json:"task" jsonschema_description:"A self-contained task: the sub-agent does not see this conversation, so name files, symbols, constraints and what the report must contain"`
	Context  string `json:"context,omitempty" jsonschema_description:"Background the sub-agent needs: findings so far, decisions made, error output"`
	Isolated bool   `json:"isolated,omitempty" jsonschema_description:"Writer roles only: work on a branch of its own (derived from this thread's branch) instead of the shared one"`
	Model    string `json:"model,omitempty" jsonschema_description:"Optional model id for the sub-agent; leave empty for the role's default"`
}

type manyInput struct {
	Items []input `json:"items" jsonschema_description:"The delegations to run concurrently (at most 8); each is as for delegate"`
}

// call is what one run's delegations share.
type call struct {
	p     *Provider
	rc    tools.RunContext
	info  *factory.ThreadInfo
	sem   chan struct{}
	count atomic.Int64
}

// Tools implements tools.Provider. A thread already at the maximum delegation
// depth gets no tools, which is what bounds recursion.
func (p *Provider) Tools(ctx context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	if p.spawner == nil {
		return nil, nil, errors.New("delegate: no spawner")
	}
	depth, info, err := p.depth(ctx, rc.ThreadID)
	if err != nil {
		return nil, nil, err
	}
	if depth >= p.maxDepth {
		return nil, nil, nil
	}
	c := &call{p: p, rc: rc, info: info, sem: make(chan struct{}, p.parallel)}
	one, err := ai.InferTool("delegate", p.describeOne(), func(ctx context.Context, in input) (string, error) {
		return c.one(ctx, in)
	})
	if err != nil {
		return nil, nil, err
	}
	many, err := ai.InferTool("delegate_many", p.describeMany(), func(ctx context.Context, in manyInput) (string, error) {
		return c.many(ctx, in)
	})
	if err != nil {
		return nil, nil, err
	}
	return []ai.BaseTool{one, many}, nil, nil
}

// depth is the number of delegations between threadID and an ordinary thread,
// and the thread's own info (nil when it has none).
func (p *Provider) depth(ctx context.Context, threadID string) (int, *factory.ThreadInfo, error) {
	if p.store == nil || threadID == "" {
		return 0, nil, nil
	}
	var first *factory.ThreadInfo
	n, id := 0, threadID
	for n < maxChain {
		ti, err := p.store.GetThreadInfo(ctx, id)
		if errors.Is(err, factory.ErrNotFound) {
			break
		}
		if err != nil {
			return 0, nil, fmt.Errorf("delegate: thread info of %s: %w", id, err)
		}
		if first == nil {
			first = ti
		}
		if ti.ParentThreadID == "" {
			break
		}
		n++
		id = ti.ParentThreadID
	}
	return n, first, nil
}

func (p *Provider) rosterLines() string {
	var b strings.Builder
	for _, r := range sortedRoles(p.roles) {
		fmt.Fprintf(&b, "- %s", r.Name)
		if r.ReadOnly {
			b.WriteString(" (read-only)")
		}
		if r.Description != "" {
			b.WriteString(": " + r.Description)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func (p *Provider) modelNote() string {
	if len(p.models) == 0 {
		return ""
	}
	return "\nThe model argument may be one of: " + strings.Join(p.models, ", ") + "."
}

func (p *Provider) describeOne() string {
	return "Hand a task to a sub-agent with its own thread and sandbox and wait for its report. Use it to split off work that is large, independent or needs a different role; do small things yourself.\n" +
		"The sub-agent does not see this conversation: write a self-contained task. It works on the same repository and branch (read-only roles never change anything); with isolated=true a writer gets a branch of its own that you must merge or cherry-pick yourself.\n" +
		"Roles:\n" + p.rosterLines() + p.modelNote() + "\n" +
		"This tool is not read-only, so the calls of one step run one after the other: to run several sub-agents at once use delegate_many. The report ends with the sub-agent's status; a failed or cancelled sub-agent returns an error text, not a report."
}

func (p *Provider) describeMany() string {
	return fmt.Sprintf("Run up to %d sub-agents at once (at most %d items per call) and return all their reports in order. Each item is as for delegate; items are independent and must not depend on one another's results or edit the same files. Roles:\n%s%s",
		p.parallel, maxItems, p.rosterLines(), p.modelNote())
}

func (c *call) one(ctx context.Context, in input) (string, error) {
	r, err := c.p.resolve(in)
	if err != nil {
		return "", err
	}
	if err := c.reserve(1); err != nil {
		return "", err
	}
	return c.run(ctx, r, in), nil
}

func (c *call) many(ctx context.Context, in manyInput) (string, error) {
	if len(in.Items) == 0 {
		return "", tools.Errorf("items must not be empty")
	}
	if len(in.Items) > maxItems {
		return "", tools.Errorf("at most %d items per delegate_many call, got %d", maxItems, len(in.Items))
	}
	roles := make([]Role, len(in.Items))
	for i, it := range in.Items {
		r, err := c.p.resolve(it)
		if err != nil {
			return "", tools.Errorf("item %d: %v", i+1, err)
		}
		roles[i] = r
	}
	if err := c.reserve(len(in.Items)); err != nil {
		return "", err
	}
	reports := make([]string, len(in.Items))
	var wg sync.WaitGroup
	for i := range in.Items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reports[i] = c.run(ctx, roles[i], in.Items[i])
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, r := range reports {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "=== item %d of %d ===\n%s", i+1, len(reports), r)
	}
	return b.String(), nil
}

// reserve counts n more delegations of this run, refusing past the per-run cap.
func (c *call) reserve(n int) error {
	if got := c.count.Add(int64(n)); got > maxPerRun {
		c.count.Add(-int64(n))
		return tools.Errorf("this run already delegated %d times (limit %d): finish with what you have or do the rest yourself", got-int64(n), maxPerRun)
	}
	return nil
}

// resolve validates one delegation.
func (p *Provider) resolve(in input) (Role, error) {
	r, ok := lookup(p.roles, in.Role)
	if !ok {
		names := make([]string, 0, len(p.roles))
		for _, x := range sortedRoles(p.roles) {
			names = append(names, x.Name)
		}
		return Role{}, tools.Errorf("unknown role %q; available roles: %s", in.Role, strings.Join(names, ", "))
	}
	if strings.TrimSpace(in.Task) == "" {
		return Role{}, tools.Errorf("task must not be empty")
	}
	if in.Model != "" {
		allowed := false
		for _, m := range p.models {
			allowed = allowed || m == in.Model
		}
		if !allowed {
			if len(p.models) == 0 {
				return Role{}, tools.Errorf("model %q cannot be chosen: leave model empty", in.Model)
			}
			return Role{}, tools.Errorf("model %q is not allowed; use one of: %s", in.Model, strings.Join(p.models, ", "))
		}
	}
	return r, nil
}

// run spawns one sub-agent and renders its outcome for the model. A failure
// is a report too: the model reads it and decides what to do next.
func (c *call) run(ctx context.Context, r Role, in input) string {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return failure(r.Name, "cancelled before it started", nil)
	}

	req := c.request(ctx, r, in)
	sctx, cancel := context.WithTimeout(ctx, c.p.timeout)
	defer cancel()
	res, err := c.p.spawner.Spawn(sctx, req)
	if res != nil && req.TaskID != "" && c.p.onSpend != nil {
		c.p.onSpend(ctx, req.TaskID, res.Usage)
	}
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded):
		return failure(r.Name, fmt.Sprintf("timed out after %s and was cancelled", c.p.timeout), res)
	case err != nil:
		return failure(r.Name, "did not complete: "+err.Error(), res)
	case res == nil:
		return failure(r.Name, "the spawner returned no result", nil)
	}
	return render(r.Name, res)
}

// request builds the SpawnRequest of one delegation from the calling thread.
func (c *call) request(ctx context.Context, r Role, in input) factory.SpawnRequest {
	req := factory.SpawnRequest{
		Login:          c.rc.Login,
		Title:          title(r.Name, in.Task),
		Model:          r.Model,
		Role:           r.Name,
		Instructions:   r.Instructions,
		ReadOnly:       r.ReadOnly,
		ParentThreadID: c.rc.ThreadID,
	}
	if in.Model != "" {
		req.Model = in.Model
	}
	if ti := c.info; ti != nil {
		if ti.Login != "" {
			req.Login = ti.Login
		}
		req.Policy, req.TaskID = ti.Policy, ti.TaskID
		req.Repo, req.BaseBranch, req.Branch = ti.Repo, ti.BaseBranch, ti.Branch
	}
	var taskCtx string
	if req.TaskID != "" && c.p.task != nil {
		t, err := c.p.task(ctx, req.TaskID)
		switch {
		case err != nil || t == nil:
			slog.Warn("delegate: task unavailable, delegating without it", "task_id", req.TaskID, "error", err)
		default:
			taskCtx = fmt.Sprintf("Task %s: %s\n%s", t.ID, t.Title, t.Spec)
			if req.Repo == "" {
				req.Repo, req.BaseBranch, req.Branch = t.Repo, t.BaseBranch, t.Branch
			}
			if req.Policy == "" {
				req.Policy = t.Policy
			}
		}
	}
	if in.Isolated && !r.ReadOnly && req.Branch != "" {
		req.Branch = fmt.Sprintf("%s-%s-%d", req.Branch, r.Name, c.p.branchSeq.Add(1))
	}
	req.Prompt = prompt(in, taskCtx)
	return req
}

func prompt(in input, taskCtx string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(in.Task))
	if s := strings.TrimSpace(in.Context); s != "" {
		b.WriteString("\n\nContext from the delegating agent:\n" + s)
	}
	if taskCtx != "" {
		b.WriteString("\n\nThe task this work belongs to:\n" + strings.TrimSpace(taskCtx))
	}
	return b.String()
}

func title(role, task string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(task), "\n")
	return role + ": " + clip(line, maxTitle)
}

// failure is the text of a sub-run that did not deliver a report.
func failure(role, why string, res *factory.SpawnResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DELEGATION FAILED: the %s sub-agent %s.", role, why)
	if res != nil {
		fmt.Fprintf(&b, "\nstatus: %s\nthread: %s\ntokens: %d", orUnknown(res.Status), res.ThreadID, res.Usage.Tokens)
		if a := strings.TrimSpace(res.Answer); a != "" {
			b.WriteString("\nlast output:\n" + clip(a, maxReportChars/2))
		}
	}
	b.WriteString("\nNothing it did can be relied on as finished. Retry with a narrower or clearer task, try another role, or do the work yourself.")
	return b.String()
}

// render is the report of a sub-run that ended.
func render(role string, res *factory.SpawnResult) string {
	if res.Status != "" && res.Status != "completed" {
		return failure(role, "ended with status "+res.Status, res)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s sub-agent report\nstatus: %s\nthread: %s\ntokens: %d\n", role, orUnknown(res.Status), res.ThreadID, res.Usage.Tokens)
	if res.WaitingFor != "" {
		fmt.Fprintf(&b, "WARNING: the sub-agent stopped waiting for a human (%s); its work is incomplete.\n", res.WaitingFor)
	}
	b.WriteString("---\n")
	answer := strings.TrimSpace(res.Answer)
	if answer == "" {
		answer = "(the sub-agent finished without a final message)"
	}
	b.WriteString(clipReport(answer, maxReportChars-utf8.RuneCountInString(b.String())))
	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// clipReport cuts s to n runes keeping the head and the tail, where the
// conclusion usually is, and says how much was dropped.
func clipReport(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	note := fmt.Sprintf("\n[report truncated: %d characters omitted]\n", len(rs)-n)
	keep := max(n-utf8.RuneCountInString(note), 0)
	head := keep * 2 / 3
	return string(rs[:head]) + note + string(rs[len(rs)-(keep-head):])
}

// clip cuts s to n runes, marking the cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	rs := []rune(s)
	return string(rs[:n]) + "…"
}

var _ tools.Provider = (*Provider)(nil)
