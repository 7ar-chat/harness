package triggers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

// Options configures the Scheduler and the Webhook; each reads the fields it
// documents.
type Options struct {
	Store      factory.Store
	Controller factory.Controller
	Now        func() time.Time // nil: time.Now

	// Every is how often the Scheduler looks for due triggers (0: 15s).
	Every time.Duration

	// Secret signs the webhook deliveries (GitHub "secret"); empty rejects every delivery.
	Secret []byte
	// OnEvent receives every verified, non-duplicate delivery before the
	// triggers run (the pull request loop listens to CI and review events).
	OnEvent func(ctx context.Context, e factory.Event)
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

const defaultTick = 15 * time.Second

// Scheduler fires cron triggers.
type Scheduler struct {
	o Options
}

// NewScheduler returns a Scheduler over o.Store that creates tasks through
// o.Controller. Several Schedulers (processes) may share a store.
func NewScheduler(o Options) *Scheduler {
	if o.Store == nil || o.Controller == nil {
		panic("triggers: Options.Store and Options.Controller are required")
	}
	if o.Every <= 0 {
		o.Every = defaultTick
	}
	return &Scheduler{o: o}
}

// Register validates t (a cron schedule, a task template that renders) and
// stores it with its next fire time. Webhook triggers are validated and
// stored as they are.
func (s *Scheduler) Register(ctx context.Context, t factory.Trigger) (*factory.Trigger, error) {
	if strings.TrimSpace(t.Name) == "" {
		return nil, fmt.Errorf("%w: trigger name", factory.ErrInvalid)
	}
	if strings.TrimSpace(t.Template.Title) == "" {
		return nil, fmt.Errorf("%w: trigger template title", factory.ErrInvalid)
	}
	if err := checkTemplate(t.Template); err != nil {
		return nil, fmt.Errorf("%w: %w", factory.ErrInvalid, err)
	}
	switch t.Kind {
	case factory.TriggerCron:
		sch, err := ParseSchedule(t.Schedule)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", factory.ErrInvalid, err)
		}
		t.NextFireAt = sch.Next(s.o.now())
	case factory.TriggerWebhook:
		if strings.TrimSpace(t.Event) == "" {
			return nil, fmt.Errorf("%w: webhook trigger event", factory.ErrInvalid)
		}
		t.NextFireAt = time.Time{}
	default:
		return nil, fmt.Errorf("%w: trigger kind %q", factory.ErrInvalid, t.Kind)
	}
	out, err := s.o.Store.PutTrigger(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("triggers: storing %q: %w", t.Name, err)
	}
	return out, nil
}

// Run ticks until ctx ends and returns ctx.Err().
func (s *Scheduler) Run(ctx context.Context) error {
	tk := time.NewTicker(s.o.Every)
	defer tk.Stop()
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("triggers: scheduler tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tk.C:
		}
	}
}

// Tick fires the triggers that are due now and returns the tasks it created.
// A trigger that was missed for a long time fires once, not once per missed
// tick.
func (s *Scheduler) Tick(ctx context.Context) ([]factory.Task, error) {
	now := s.o.now()
	due, err := s.o.Store.ClaimDueTriggers(ctx, now, func(t factory.Trigger) time.Time {
		sch, err := ParseSchedule(t.Schedule)
		if err != nil {
			slog.Warn("triggers: stored schedule is invalid; the trigger will not fire again", "trigger", t.Name, "schedule", t.Schedule, "error", err)
			return time.Time{}
		}
		return sch.Next(now)
	})
	if err != nil {
		return nil, fmt.Errorf("triggers: claiming due triggers: %w", err)
	}
	var created []factory.Task
	var firstErr error
	for _, t := range due {
		task, err := s.fire(ctx, t)
		if err != nil {
			slog.Warn("triggers: firing a cron trigger failed", "trigger", t.Name, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			if !errors.Is(err, factory.ErrInvalid) { // a task that can never be created is not retried every tick
				s.retryLater(ctx, t)
			}
			continue
		}
		if task != nil {
			created = append(created, *task)
		}
	}
	return created, firstErr
}

// fire creates the task of one tick of t (t.NextFireAt is the tick).
func (s *Scheduler) fire(ctx context.Context, t factory.Trigger) (*factory.Task, error) {
	at := t.NextFireAt.UTC()
	data := map[string]string{
		"Name": t.Name, "Time": at.Format(time.RFC3339), "Date": at.Format("2006-01-02"),
	}
	task, err := renderTask(t, data, factory.Source{
		Kind: factory.SourceCron, Key: "cron:" + t.Name + ":" + at.Format(time.RFC3339),
	})
	if err != nil {
		return nil, fmt.Errorf("trigger %q: %w: %w", t.Name, factory.ErrInvalid, err)
	}
	got, isNew, err := s.o.Controller.Create(ctx, *task)
	if err != nil {
		return nil, fmt.Errorf("trigger %q: creating its task: %w", t.Name, err)
	}
	if !isNew {
		return nil, nil // another process already fired this tick
	}
	return got, nil
}

// retryLater makes the next tick try t's fire time again after a failed
// create; the source key keeps a tick that did get created from doubling.
func (s *Scheduler) retryLater(ctx context.Context, t factory.Trigger) {
	cur, err := s.o.Store.GetTrigger(ctx, t.ID)
	if err != nil {
		return
	}
	if !cur.NextFireAt.IsZero() && cur.LastFiredAt.Equal(t.NextFireAt) { // untouched since our claim
		cur.NextFireAt = t.NextFireAt
		if _, err := s.o.Store.PutTrigger(ctx, *cur); err != nil {
			slog.Warn("triggers: rescheduling a failed fire failed", "trigger", t.Name, "error", err)
		}
	}
}

// checkTemplate parses the template texts of tpl so Register rejects a broken
// trigger when it is written, not when it fires.
func checkTemplate(tpl factory.Template) error {
	for name, text := range map[string]string{"title": tpl.Title, "spec": tpl.Spec, "repo": tpl.Repo, "baseBranch": tpl.BaseBranch} {
		if _, err := template.New(name).Option("missingkey=zero").Parse(text); err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
	}
	return nil
}

// renderTask builds the task of trigger t from its template, rendered with data.
func renderTask(t factory.Trigger, data map[string]string, src factory.Source) (*factory.Task, error) {
	render := func(name, text string) (string, error) {
		tp, err := template.New(name).Option("missingkey=zero").Parse(text)
		if err != nil {
			return "", fmt.Errorf("template %s: %w", name, err)
		}
		var b bytes.Buffer
		if err := tp.Execute(&b, data); err != nil {
			return "", fmt.Errorf("template %s: %w", name, err)
		}
		return b.String(), nil
	}
	tpl := t.Template
	title, err := render("title", tpl.Title)
	if err != nil {
		return nil, err
	}
	spec, err := render("spec", tpl.Spec)
	if err != nil {
		return nil, err
	}
	repo, err := render("repo", tpl.Repo)
	if err != nil {
		return nil, err
	}
	base, err := render("baseBranch", tpl.BaseBranch)
	if err != nil {
		return nil, err
	}
	return &factory.Task{
		ProjectID: t.ProjectID, Owner: t.Owner, Title: strings.TrimSpace(title), Spec: spec,
		Role: tpl.Role, Policy: tpl.Policy, Model: tpl.Model,
		Repo: strings.TrimSpace(repo), BaseBranch: strings.TrimSpace(base),
		Budget: tpl.Budget, MaxAttempts: tpl.MaxAttempts, AutoMerge: tpl.AutoMerge,
		Labels: append([]string(nil), tpl.Labels...), Priority: tpl.Priority,
		Source: src,
	}, nil
}
