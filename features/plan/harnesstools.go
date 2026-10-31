// Package harnesstools provides the harness v2 agent tools update_plan and
// ask_user (the memory tools are in memorytools).
package harnesstools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

// Limits of the tools' arguments.
const (
	MaxPlanItems     = 50
	maxPlanTitle     = 300
	maxQuestionChars = 2000
	MaxOptions       = 8
	maxOptionLabel   = 100
	maxOptionDesc    = 300
)

// Store is the persistence used by the tools.
type Store interface {
	SetThreadPlan(ctx context.Context, threadID string, items []store.PlanItem) error
}

// Provider is the harness tools provider.
type Provider struct {
	st Store
}

// New returns the provider.
func New(st Store) *Provider { return &Provider{st: st} }

// Name implements tools.Provider.
func (p *Provider) Name() string { return "harness" }

// Tools implements tools.Provider.
func (p *Provider) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	b := &binding{p: p, rc: rc}
	var out []ai.BaseTool
	add := func(t ai.InvokableTool, err error) error {
		if err != nil {
			return err
		}
		out = append(out, t)
		return nil
	}
	plan, err := ai.InferTool("update_plan", planDesc, b.updatePlan)
	errs := []error{
		add(&planTool{InvokableTool: plan, b: b}, err),
		add(ai.InferTool("ask_user", askDesc, b.askUser)),
	}
	if err := errors.Join(errs...); err != nil {
		return nil, nil, err
	}
	return out, func() {}, nil
}

const planDesc = `Create or update your plan: the todo list of a multi-step task, shown to the user.
Send the complete list every time (it replaces the previous one). Keep exactly one item "in_progress" while you work,
mark items "completed" as soon as they are done, and add items when you discover new work. Not needed for simple one-step requests.`

const askDesc = `Ask the user a question and wait for the answer. Use it only when you cannot continue without the user:
a decision with several reasonable options, missing information you cannot find or infer, or a confirmation before an
irreversible action. Offer 2-5 short options when they make answering easy. The run stops after this call; the user's
answer is returned as this tool's result when they reply. Ask one question at a time.`

type binding struct {
	p  *Provider
	rc tools.RunContext
	mu sync.Mutex
	// plan is the last plan saved by this run (nil: the run wrote none).
	plan []store.PlanItem
}

// planTool is update_plan; it is a tools.StopGuard for the plan of the current run.
type planTool struct {
	ai.InvokableTool
	b *binding
}

var (
	_ tools.StopGuard    = (*planTool)(nil)
	_ tools.TurnObserver = (*planTool)(nil)
)

// ObserveTurn implements tools.TurnObserver: a run resuming a turn (after an
// approval or an answer) keeps guarding the plan an earlier run of the turn
// wrote (A-21).
func (t *planTool) ObserveTurn(calls []tools.TurnCall) {
	t.b.mu.Lock()
	defer t.b.mu.Unlock()
	if t.b.plan != nil {
		return
	}
	for i := len(calls) - 1; i >= 0; i-- {
		c := calls[i]
		if c.Name != "update_plan" || c.Failed {
			continue
		}
		var in struct {
			Items []store.PlanItem `json:"items"`
		}
		if json.Unmarshal([]byte(c.Arguments), &in) == nil && in.Items != nil {
			t.b.plan = in.Items
		}
		return
	}
}

// StopReminder implements tools.StopGuard: the unfinished items of a plan
// written or updated in this run (a plan left by an earlier run is not the
// current task's and never blocks the answer).
func (t *planTool) StopReminder(context.Context) string {
	t.b.mu.Lock()
	defer t.b.mu.Unlock()
	var open []string
	for _, it := range t.b.plan {
		if it.Status != store.PlanCompleted {
			open = append(open, fmt.Sprintf("%q (%s)", it.Title, it.Status))
		}
	}
	if len(open) == 0 {
		return ""
	}
	return fmt.Sprintf("Your plan still has %d unfinished item(s): %s. Continue working on them now; if they are done or no longer needed, call update_plan to mark them completed or remove them, then give your final answer.",
		len(open), strings.Join(open, ", "))
}

type planInput struct {
	Items []struct {
		ID     string `json:"id" jsonschema_description:"Stable identifier of the item (e.g. 1, 2, 3)"`
		Title  string `json:"title" jsonschema_description:"Short description of the step"`
		Status string `json:"status" jsonschema:"enum=pending,enum=in_progress,enum=completed" jsonschema_description:"pending, in_progress (exactly one item while you work) or completed"`
	} `json:"items" jsonschema_description:"The complete plan, in order"`
	Explanation string `json:"explanation,omitempty" jsonschema_description:"Optional note on what changed"`
}

func (b *binding) updatePlan(ctx context.Context, in planInput) (string, error) {
	if len(in.Items) > MaxPlanItems {
		return "", tools.Errorf("a plan has at most %d items", MaxPlanItems)
	}
	items := make([]store.PlanItem, 0, len(in.Items))
	seen := map[string]bool{}
	done, active := 0, []string{}
	for i, it := range in.Items {
		id, title := strings.TrimSpace(it.ID), strings.TrimSpace(it.Title)
		switch {
		case id == "" || title == "":
			return "", tools.Errorf("item %d needs an id and a title", i+1)
		case seen[id]:
			return "", tools.Errorf("item id %q is used twice", id)
		case utf8.RuneCountInString(title) > maxPlanTitle:
			return "", tools.Errorf("item %q: the title is longer than %d characters", id, maxPlanTitle)
		}
		switch it.Status {
		case store.PlanCompleted:
			done++
		case store.PlanInProgress:
			active = append(active, title)
		case store.PlanPending:
		default:
			return "", tools.Errorf("item %q: status must be pending, in_progress or completed", id)
		}
		seen[id] = true
		items = append(items, store.PlanItem{ID: store.CleanText(id), Title: store.CleanText(title), Status: it.Status})
	}
	if err := b.p.st.SetThreadPlan(ctx, b.rc.ThreadID, items); err != nil {
		return "", fmt.Errorf("saving the plan failed: %w", err)
	}
	b.mu.Lock()
	b.plan = items
	b.mu.Unlock()
	tools.AfterResult(ctx, agui.StateSnapshotEvent(map[string]any{"plan": items}))
	out := fmt.Sprintf("Plan updated: %d/%d completed.", done, len(items))
	switch {
	case len(active) == 1:
		out += " In progress: " + active[0] + "."
	case len(active) > 1:
		out += fmt.Sprintf(" Warning: %d items are in progress; keep exactly one.", len(active))
	case done < len(items):
		out += " No item is in progress; mark the one you work on."
	}
	return out, nil
}

type askInput struct {
	Question string `json:"question" jsonschema_description:"The question, self-contained and short"`
	Options  []struct {
		Label       string `json:"label" jsonschema_description:"Short answer choice"`
		Description string `json:"description,omitempty" jsonschema_description:"Optional detail on this choice"`
	} `json:"options,omitempty" jsonschema_description:"Optional answer choices (2-5 recommended)"`
	AllowFreeText *bool `json:"allowFreeText,omitempty" jsonschema_description:"Whether the user may type another answer (default true)"`
}

func (b *binding) askUser(_ context.Context, in askInput) (string, error) {
	q := strings.TrimSpace(in.Question)
	if q == "" || utf8.RuneCountInString(q) > maxQuestionChars {
		return "", tools.Errorf("question is required (at most %d characters)", maxQuestionChars)
	}
	if len(in.Options) > MaxOptions {
		return "", tools.Errorf("at most %d options", MaxOptions)
	}
	intr := &tools.Interrupt{Reason: "ask_user", Question: store.CleanText(q), AllowFreeText: in.AllowFreeText == nil || *in.AllowFreeText}
	intr.Options = []tools.QuestionOption{}
	for _, o := range in.Options {
		label, desc := strings.TrimSpace(o.Label), strings.TrimSpace(o.Description)
		if label == "" || utf8.RuneCountInString(label) > maxOptionLabel || utf8.RuneCountInString(desc) > maxOptionDesc {
			return "", tools.Errorf("each option needs a label (at most %d characters, description at most %d)", maxOptionLabel, maxOptionDesc)
		}
		intr.Options = append(intr.Options, tools.QuestionOption{Label: store.CleanText(label), Description: store.CleanText(desc)})
	}
	if len(intr.Options) == 0 && !intr.AllowFreeText {
		return "", tools.Errorf("give options or allow a free-text answer")
	}
	return "", intr
}
