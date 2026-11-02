package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/modelinfo"
)

// FactorySpawner is the factory.Spawner over a Harness: every spawn is a new
// thread of the login, linked to its task and role through factory.ThreadInfo
// before the first run is queued, so the hooks and prompt sections of the
// factory features see it from the first step.
type FactorySpawner struct {
	H *Harness
	// Store records the ThreadInfo (nil: none is recorded).
	Store factory.Store
	// DefaultModel is used when a request names none (default: the config's).
	DefaultModel string
	// OnSpawn is called with each new thread of a task, after its ThreadInfo is recorded and before the run
	// is queued (nil: nothing).
	OnSpawn func(ctx context.Context, threadID string, t factory.Task, req factory.SpawnRequest)
	// Price converts a run's token usage to dollars (nil: tokens only).
	Price func(model string) (modelinfo.Info, bool)
}

// Spawn implements factory.Spawner.
func (s *FactorySpawner) Spawn(ctx context.Context, req factory.SpawnRequest) (*factory.SpawnResult, error) {
	if s == nil || s.H == nil {
		return nil, errors.New("factory spawner: no harness")
	}
	if strings.TrimSpace(req.Login) == "" || strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("%w: login and prompt are required", ErrInvalidRequest)
	}
	model := req.Model
	if model == "" {
		model = s.DefaultModel
	}
	if model == "" {
		model = s.H.cfg.LLMDefaultModel
	}
	title := req.Title
	if title == "" {
		title = autoTitle(req.Prompt)
	}
	th, err := s.H.NewThread(ctx, req.Login, title)
	if err != nil {
		return nil, fmt.Errorf("factory spawner: creating the thread: %w", err)
	}
	if s.Store != nil && (req.TaskID != "" || req.Role != "" || req.Policy != "" || req.ParentThreadID != "") {
		ti := factory.ThreadInfo{ThreadID: th.ID, Login: req.Login, TaskID: req.TaskID, Role: req.Role,
			Instructions: req.Instructions, Policy: req.Policy, ParentThreadID: req.ParentThreadID,
			ReadOnly: req.ReadOnly, Repo: req.Repo, BaseBranch: req.BaseBranch, Branch: req.Branch}
		if err := s.Store.SetThreadInfo(ctx, ti); err != nil {
			return nil, fmt.Errorf("factory spawner: recording the thread's task: %w", err)
		}
	}
	if s.OnSpawn != nil && req.TaskID != "" && s.Store != nil {
		if t, err := s.Store.GetTask(ctx, req.TaskID); err == nil {
			s.OnSpawn(ctx, th.ID, *t, req)
		}
	}
	opts := RunOptions{Model: model, Interactive: req.Interactive}
	if req.ReadOnly {
		opts.Mode = ModePlan
	}
	if req.NoSandbox {
		opts.Sandbox = SandboxOff
	}
	run, err := s.H.Send(ctx, req.Login, th.ID, req.Prompt, opts)
	if err != nil {
		return nil, fmt.Errorf("factory spawner: starting the run: %w", err)
	}
	res := &factory.SpawnResult{ThreadID: th.ID, RunID: run.ID}
	if req.Detach {
		return res, nil
	}
	out, err := s.wait(ctx, req.Login, res, model)
	if err == nil && out.WaitingFor == "" {
		// A delegated run that is over is never resumed: its sandbox only counts against the owner's quota.
		s.H.ReleaseSandbox(context.WithoutCancel(ctx), th.ID)
	}
	return out, err
}

// wait follows the run's events to its terminal one.
func (s *FactorySpawner) wait(ctx context.Context, login string, res *factory.SpawnResult, model string) (*factory.SpawnResult, error) {
	var last store.Event
	for ev := range s.H.Events(ctx, res.RunID, 0) {
		last = ev
		if agui.IsTerminal(ev.Type) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		_ = s.H.Cancel(context.WithoutCancel(ctx), login, res.RunID) // best effort: the caller gave up
		return nil, err
	}
	if !agui.IsTerminal(last.Type) {
		return nil, fmt.Errorf("factory spawner: the event stream of run %s ended without a result", res.RunID)
	}
	run, err := s.H.Store.GetRun(ctx, res.RunID)
	if err != nil {
		return nil, fmt.Errorf("factory spawner: reading run %s: %w", res.RunID, err)
	}
	res.Status = run.Status
	res.WaitingFor, res.Usage = outcomeOf(last, model, s.Price)
	if msgs, err := s.H.Store.ListMessages(ctx, res.ThreadID); err == nil {
		res.Answer = lastAssistantText(msgs)
	}
	if run.Status == store.StatusFailed && res.Answer == "" {
		res.Answer = strings.TrimSpace(run.ErrorCode + " " + run.Error)
	}
	return res, nil
}

// outcomeOf reads what a RUN_FINISHED / RUN_ERROR event says: whether the run
// waits for a human and what it cost.
func outcomeOf(ev store.Event, model string, price func(string) (modelinfo.Info, bool)) (waiting string, spend factory.Spend) {
	var body struct {
		Outcome struct {
			Type       string `json:"type"`
			Interrupts []struct {
				Reason string `json:"reason"`
			} `json:"interrupts"`
		} `json:"outcome"`
		Usage []agui.Usage `json:"usage"`
	}
	if json.Unmarshal(ev.Data, &body) != nil {
		return "", spend
	}
	if body.Outcome.Type == "interrupt" && len(body.Outcome.Interrupts) > 0 {
		waiting = "question"
		if body.Outcome.Interrupts[0].Reason == store.ReasonToolApproval {
			waiting = "approval"
		}
	}
	return waiting, SpendOf(body.Usage, model, price)
}

// SpendOf converts per-model usage to a factory.Spend. A model without a
// price adds its tokens and counts as unpriced rather than free.
func SpendOf(usage []agui.Usage, fallbackModel string, price func(string) (modelinfo.Info, bool)) factory.Spend {
	var s factory.Spend
	s.Runs = 1
	for _, u := range usage {
		s.Tokens += int64(u.InputTokens + u.OutputTokens)
		name := u.Model
		if name == "" {
			name = fallbackModel
		}
		var info modelinfo.Info
		ok := false
		if price != nil {
			info, ok = price(name)
		}
		if usd, priced := modelinfo.Cost(info, modelinfo.Usage{In: u.InputTokens, Out: u.OutputTokens, Cached: u.CachedInputTokens, Prompt: u.InputTokens}); ok && priced {
			s.USD += usd
		} else {
			s.Unpriced++
		}
	}
	return s
}

func lastAssistantText(msgs []*store.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Role == store.RoleAssistant && strings.TrimSpace(m.Content) != "" {
			return strings.TrimSpace(m.Content)
		}
	}
	return ""
}

var _ factory.Spawner = (*FactorySpawner)(nil)
