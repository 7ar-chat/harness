package triggers_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/features/factory/triggers"
)

// controller creates tasks straight in the store; the other Controller methods
// are not used by triggers.
type controller struct {
	factory.Controller
	st  factory.Store
	err error
}

func (c *controller) Create(ctx context.Context, t factory.Task) (*factory.Task, bool, error) {
	if c.err != nil {
		return nil, false, c.err
	}
	return c.st.CreateTask(ctx, t)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }

func allTasks(t *testing.T, st factory.Store) []factory.Task {
	t.Helper()
	ts, err := st.ListTasks(context.Background(), factory.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func nightly() factory.Trigger {
	return factory.Trigger{
		Owner: "alice", Name: "nightly", Kind: factory.TriggerCron, Schedule: "0 2 * * *", Enabled: true,
		Template: factory.Template{Title: "Nightly audit {{.Date}}", Spec: "Audit at {{.Time}} ({{.Name}})", Repo: "acme/app", Priority: 3},
	}
}

func TestRegisterStoresCronTriggerWithNextFireAt(t *testing.T) {
	// Arrange
	clk := &clock{t: at("2026-10-31T12:00:00Z")}
	st := factorystore.NewMem(clk.Now)
	sch := triggers.NewScheduler(triggers.Options{Store: st, Controller: &controller{st: st}, Now: clk.Now})

	// Act
	got, err := sch.Register(context.Background(), nightly())

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !got.NextFireAt.Equal(at("2026-11-01T02:00:00Z")) || got.ID == "" {
		t.Errorf("trigger = %+v", got)
	}
	if stored, err := st.GetTrigger(context.Background(), got.ID); err != nil || !stored.NextFireAt.Equal(got.NextFireAt) {
		t.Errorf("stored = %+v, %v", stored, err)
	}
}

func TestRegisterRejectsInvalidTriggers(t *testing.T) {
	tests := map[string]func(*factory.Trigger){
		"bad schedule":    func(tr *factory.Trigger) { tr.Schedule = "every day" },
		"no name":         func(tr *factory.Trigger) { tr.Name = "" },
		"no title":        func(tr *factory.Trigger) { tr.Template.Title = "" },
		"broken template": func(tr *factory.Trigger) { tr.Template.Spec = "{{.Date" },
		"unknown kind":    func(tr *factory.Trigger) { tr.Kind = "carrier-pigeon" },
		"webhook event":   func(tr *factory.Trigger) { tr.Kind = factory.TriggerWebhook; tr.Event = "" },
	}
	for name, mod := range tests {
		t.Run(name, func(t *testing.T) {
			// Arrange
			st := factorystore.NewMem(nil)
			sch := triggers.NewScheduler(triggers.Options{Store: st, Controller: &controller{st: st}})
			tr := nightly()
			mod(&tr)

			// Act
			_, err := sch.Register(context.Background(), tr)

			// Assert
			if !errors.Is(err, factory.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
			if ts, _ := st.ListTriggers(context.Background(), ""); len(ts) != 0 {
				t.Errorf("%d triggers stored", len(ts))
			}
		})
	}
}

func TestTickCreatesTheTemplateTaskOnceAtTheFireTime(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := &clock{t: at("2026-10-31T12:00:00Z")}
	st := factorystore.NewMem(clk.Now)
	sch := triggers.NewScheduler(triggers.Options{Store: st, Controller: &controller{st: st}, Now: clk.Now})
	if _, err := sch.Register(ctx, nightly()); err != nil {
		t.Fatal(err)
	}

	// Act + Assert: nothing before the fire time.
	clk.Set(at("2026-11-01T01:59:59Z"))
	if got, err := sch.Tick(ctx); err != nil || len(got) != 0 {
		t.Fatalf("early Tick = %v, %v", got, err)
	}

	// Act: the fire time passes (the tick is a little late).
	clk.Set(at("2026-11-01T02:00:20Z"))
	got, err := sch.Tick(ctx)
	again, err2 := sch.Tick(ctx)

	// Assert
	if err != nil || err2 != nil {
		t.Fatalf("Tick errors: %v %v", err, err2)
	}
	if len(got) != 1 || len(again) != 0 {
		t.Fatalf("created %d then %d tasks, want 1 then 0", len(got), len(again))
	}
	task := got[0]
	if task.Title != "Nightly audit 2026-11-01" || task.Spec != "Audit at 2026-11-01T02:00:00Z (nightly)" ||
		task.Owner != "alice" || task.Repo != "acme/app" || task.Priority != 3 {
		t.Errorf("task = %+v", task)
	}
	if task.Source.Kind != factory.SourceCron || task.Source.Key != "cron:nightly:2026-11-01T02:00:00Z" {
		t.Errorf("source = %+v", task.Source)
	}
}

func TestTwoSchedulersFireATickOnce(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := &clock{t: at("2026-10-31T12:00:00Z")}
	st := factorystore.NewMem(clk.Now)
	mk := func() *triggers.Scheduler {
		return triggers.NewScheduler(triggers.Options{Store: st, Controller: &controller{st: st}, Now: clk.Now})
	}
	a, b := mk(), mk()
	if _, err := a.Register(ctx, nightly()); err != nil {
		t.Fatal(err)
	}
	clk.Set(at("2026-11-01T02:00:05Z"))

	// Act
	var wg sync.WaitGroup
	for _, s := range []*triggers.Scheduler{a, b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Tick(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	// Assert
	if ts := allTasks(t, st); len(ts) != 1 {
		t.Errorf("%d tasks, want 1", len(ts))
	}
}

func TestSameTickCreatedTwiceIsIdempotentThroughTheSourceKey(t *testing.T) {
	// Arrange: a second process whose claim raced past the first one's create.
	ctx := context.Background()
	clk := &clock{t: at("2026-10-31T12:00:00Z")}
	st := factorystore.NewMem(clk.Now)
	sch := triggers.NewScheduler(triggers.Options{Store: st, Controller: &controller{st: st}, Now: clk.Now})
	tr, err := sch.Register(ctx, nightly())
	if err != nil {
		t.Fatal(err)
	}
	clk.Set(at("2026-11-01T02:00:05Z"))
	if _, err := sch.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rewound := *tr // the trigger as it was before the first claim
	if _, err := st.PutTrigger(ctx, rewound); err != nil {
		t.Fatal(err)
	}

	// Act
	got, err := sch.Tick(ctx)

	// Assert
	if err != nil || len(got) != 0 {
		t.Errorf("Tick = %v, %v; want no new task", got, err)
	}
	if ts := allTasks(t, st); len(ts) != 1 {
		t.Errorf("%d tasks, want 1", len(ts))
	}
}

func TestTickFiresAMissedTriggerOnceAndSchedulesTheNextFuturePoint(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := &clock{t: at("2026-10-31T12:00:00Z")}
	st := factorystore.NewMem(clk.Now)
	sch := triggers.NewScheduler(triggers.Options{Store: st, Controller: &controller{st: st}, Now: clk.Now})
	tr, err := sch.Register(ctx, nightly())
	if err != nil {
		t.Fatal(err)
	}

	// Act: the process was down for three days.
	clk.Set(at("2026-11-03T10:00:00Z"))
	got, err := sch.Tick(ctx)

	// Assert
	if err != nil || len(got) != 1 {
		t.Fatalf("Tick = %v, %v; want one task", got, err)
	}
	stored, _ := st.GetTrigger(ctx, tr.ID)
	if !stored.NextFireAt.Equal(at("2026-11-04T02:00:00Z")) {
		t.Errorf("NextFireAt = %v, want 2026-11-04T02:00:00Z", stored.NextFireAt)
	}
}

func TestTickSkipsDisabledTriggersAndWebhookTriggers(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := &clock{t: at("2026-10-31T12:00:00Z")}
	st := factorystore.NewMem(clk.Now)
	sch := triggers.NewScheduler(triggers.Options{Store: st, Controller: &controller{st: st}, Now: clk.Now})
	off := nightly()
	off.Enabled = false
	hook := factory.Trigger{Owner: "a", Name: "hook", Kind: factory.TriggerWebhook, Event: "issues", Enabled: true, Template: factory.Template{Title: "x"}}
	for _, tr := range []factory.Trigger{off, hook} {
		if _, err := sch.Register(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	clk.Set(at("2026-11-02T00:00:00Z"))

	// Act
	got, err := sch.Tick(ctx)

	// Assert
	if err != nil || len(got) != 0 {
		t.Errorf("Tick = %v, %v; want nothing", got, err)
	}
}

func TestTickRetriesAFailedCreateOnTheNextTick(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := &clock{t: at("2026-10-31T12:00:00Z")}
	st := factorystore.NewMem(clk.Now)
	ctl := &controller{st: st, err: errors.New("database down")}
	sch := triggers.NewScheduler(triggers.Options{Store: st, Controller: ctl, Now: clk.Now})
	if _, err := sch.Register(ctx, nightly()); err != nil {
		t.Fatal(err)
	}
	clk.Set(at("2026-11-01T02:00:05Z"))

	// Act
	_, failErr := sch.Tick(ctx)
	ctl.err = nil
	got, err := sch.Tick(ctx)

	// Assert
	if failErr == nil {
		t.Error("the failed create was not reported")
	}
	if err != nil || len(got) != 1 || got[0].Source.Key != "cron:nightly:2026-11-01T02:00:00Z" {
		t.Errorf("retry Tick = %v, %v; want the 02:00 task", got, err)
	}
}

func TestTickDoesNotRetryATemplateThatFailsToRender(t *testing.T) {
	// Arrange: a template that parses but cannot execute.
	ctx := context.Background()
	clk := &clock{t: at("2026-10-31T12:00:00Z")}
	st := factorystore.NewMem(clk.Now)
	sch := triggers.NewScheduler(triggers.Options{Store: st, Controller: &controller{st: st}, Now: clk.Now})
	tr := nightly()
	tr.Template.Title = "{{index .Name 99}}"
	if _, err := sch.Register(ctx, tr); err != nil {
		t.Fatal(err)
	}
	clk.Set(at("2026-11-01T02:00:05Z"))

	// Act
	_, first := sch.Tick(ctx)
	_, second := sch.Tick(ctx)

	// Assert
	if !errors.Is(first, factory.ErrInvalid) || second != nil {
		t.Errorf("first = %v, second = %v; want a render error once, then nothing", first, second)
	}
}

func TestRunTicksUntilContextEnds(t *testing.T) {
	// Arrange
	clk := &clock{t: at("2026-10-31T12:00:00Z")}
	st := factorystore.NewMem(clk.Now)
	sch := triggers.NewScheduler(triggers.Options{Store: st, Controller: &controller{st: st}, Now: clk.Now, Every: 5 * time.Millisecond})
	if _, err := sch.Register(context.Background(), nightly()); err != nil {
		t.Fatal(err)
	}
	clk.Set(at("2026-11-01T02:00:05Z"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	// Act
	go func() { done <- sch.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(allTasks(t, st)) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	// Assert
	if n := len(allTasks(t, st)); n != 1 {
		t.Errorf("%d tasks, want 1", n)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
