package harness_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/longtest"
)

func newFeatureHarness(t *testing.T, features ...harness.Feature) (*harness.Harness, error) {
	t.Helper()
	return harness.New(harness.Options{
		Config: featureConfig(t, nil), Models: llmtest.Factory{M: llmtest.New()},
		Backend: sandboxtest.NewBackend(), Features: features,
	})
}

func TestOnlyOneFeatureMaySetEachSingleSlot(t *testing.T) {
	// Arrange: each slot is claimed by two features; the second one must be refused and named
	type slot struct {
		name string
		set  func(*harness.Builder) error
		want string
	}
	slots := []slot{
		{"answers", func(b *harness.Builder) error { return b.SetAnswers(nopAnswers{}) }, "an answer checker is already set"},
		{"images", func(b *harness.Builder) error { return b.SetImages(nopImages{}) }, "an image attacher is already set"},
		{"checker", func(b *harness.Builder) error { return b.SetChecker(nopChecker{}, nil) }, "a deliverable checker is already set"},
		{"purger", func(b *harness.Builder) error { return b.SetPurger(nopPurger{}) }, "a purger is already set"},
	}
	for _, s := range slots {
		t.Run(s.name, func(t *testing.T) {
			first := harness.FeatureFunc("first", s.set)
			second := harness.FeatureFunc("second", s.set)

			// Act
			_, err := newFeatureHarness(t, first, second)
			_, alone := newFeatureHarness(t, first)

			// Assert
			if err == nil || !strings.Contains(err.Error(), `feature "second"`) || !strings.Contains(err.Error(), s.want) {
				t.Fatalf("err %v, want it to name feature \"second\" and say %q", err, s.want)
			}
			if alone != nil {
				t.Fatalf("a single feature may set the slot: %v", alone)
			}
		})
	}
}

func TestFeaturesShareServicesByTypeAndSeeWhatIsInstalled(t *testing.T) {
	// Arrange
	type greeter struct{ greeting string }
	var seen struct {
		hasBefore, hasLater bool
		got                 *greeter
		foundMissing        bool
		store               harness.Store
	}
	provider := harness.FeatureFunc("provider", func(b *harness.Builder) error {
		seen.hasBefore = b.Has("consumer")
		return harness.Provide(b, &greeter{"hello"})
	})
	consumer := harness.FeatureFunc("consumer", func(b *harness.Builder) error {
		if err := b.Require("provider"); err != nil {
			return err
		}
		seen.hasLater = b.Has("provider")
		seen.got, _ = harness.Lookup[*greeter](b)
		_, seen.foundMissing = harness.Lookup[*memstore.Store](b)
		var err error
		seen.store, err = harness.StoreAs[harness.Store](b)
		return err
	})

	// Act
	_, err := newFeatureHarness(t, provider, consumer)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if seen.hasBefore || !seen.hasLater {
		t.Fatalf("Has: consumer visible to the provider %v, provider visible to the consumer %v", seen.hasBefore, seen.hasLater)
	}
	if seen.got == nil || seen.got.greeting != "hello" || seen.foundMissing {
		t.Fatalf("Lookup got %+v, found an unpublished type: %v", seen.got, seen.foundMissing)
	}
	if seen.store == nil {
		t.Fatal("StoreAs returned no store")
	}
}

func TestProvidingTheSameServiceTypeTwiceFailsNamingTheFeature(t *testing.T) {
	// Arrange
	type svc struct{}
	a := harness.FeatureFunc("a", func(b *harness.Builder) error { return harness.Provide(b, &svc{}) })
	bFeature := harness.FeatureFunc("b", func(b *harness.Builder) error { return harness.Provide(b, &svc{}) })

	// Act
	_, err := newFeatureHarness(t, a, bFeature)

	// Assert
	if err == nil || !strings.Contains(err.Error(), `feature "b"`) || !strings.Contains(err.Error(), "already provided") {
		t.Fatalf("err %v", err)
	}
}

func TestStoreAsNamesTheFeatureWhenTheStoreLacksTheCapability(t *testing.T) {
	// Arrange
	type needsFridge interface{ Fridge() }
	f := harness.FeatureFunc("cold", func(b *harness.Builder) error {
		_, err := harness.StoreAs[needsFridge](b)
		return err
	})

	// Act
	_, err := newFeatureHarness(t, f)

	// Assert
	if err == nil || !strings.Contains(err.Error(), `feature "cold" needs a store implementing`) {
		t.Fatalf("err %v", err)
	}
}

func TestAFeatureWithoutANameIsRefused(t *testing.T) {
	// Arrange / Act
	_, blank := newFeatureHarness(t, harness.FeatureFunc("  ", func(*harness.Builder) error { return nil }))
	_, withNil := newFeatureHarness(t, nil, harness.Plan())

	// Assert
	if blank == nil || !strings.Contains(blank.Error(), "has no name") {
		t.Fatalf("blank name: %v", blank)
	}
	if withNil != nil {
		t.Fatalf("a nil feature in the list is skipped: %v", withNil)
	}
}

func TestAfterRunsOnceEveryFeatureIsInstalledAndItsErrorAbortsNew(t *testing.T) {
	// Arrange
	var order []string
	early := harness.FeatureFunc("early", func(b *harness.Builder) error {
		b.After(func() error {
			order = append(order, "after")
			if !b.Has("late") {
				t.Error("After ran before the later feature was installed")
			}
			return errors.New("cross-feature check failed")
		})
		order = append(order, "early")
		return nil
	})
	late := harness.FeatureFunc("late", func(*harness.Builder) error { order = append(order, "late"); return nil })

	// Act
	_, err := newFeatureHarness(t, early, late)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "cross-feature check failed") {
		t.Fatalf("err %v", err)
	}
	if strings.Join(order, ",") != "early,late,after" {
		t.Fatalf("order %v", order)
	}
}

// denyTool refuses every call of the tool it is told to, and nothing else.
type denyTool struct{ name string }

func (d denyTool) NeedsApproval(context.Context, *store.Run, string, string) bool { return false }
func (d denyTool) Pause(context.Context, *store.Run, ai.ToolCall) (*tools.Interrupt, error) {
	return nil, nil
}
func (d denyTool) Denial(_ context.Context, _ *store.Run, tool, _ string) string {
	if tool == d.name {
		return "Error: " + tool + " is forbidden here."
	}
	return ""
}

func TestABeforeToolHookOfAFeatureCanRefuseACallTheModelThenSees(t *testing.T) {
	// Arrange
	ran := false
	feature := harness.FeatureFunc("guard", func(b *harness.Builder) error {
		b.AddTools(longtest.ExtraTool("danger", func(context.Context, string) (string, error) { ran = true; return "boom", nil }))
		b.BeforeTool(denyTool{name: "danger"})
		return nil
	})
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("title")}
		}
		if last := c.Messages[len(c.Messages)-1]; last.Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("saw: " + last.Content)}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "d1", "danger", "{}"), "tool_calls")}}
	})
	h, ctx := facade(t, nil, model, func(o *harness.Options) { o.Features = append(harness.ProfileMinimal(), feature) })
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	exchange(ctx, t, h, "alice", th.ID, "do the dangerous thing", harness.RunOptions{})

	// Assert
	if ran {
		t.Fatal("the refused tool ran")
	}
	msgs := messagesOf(ctx, t, h, th.ID)
	if last := msgs[len(msgs)-1]; last.Content != "saw: Error: danger is forbidden here." {
		t.Fatalf("last message %q", last.Content)
	}
}

func TestSandboxCreateHooksRunPrepareFirstAndTheirNoticesAreJoined(t *testing.T) {
	// Arrange
	var calls []string
	feature := harness.FeatureFunc("notes", func(b *harness.Builder) error {
		b.OnSandboxCreate(func(_ context.Context, thread string, _ sandbox.Sandbox, recreated bool) string {
			calls = append(calls, "created")
			return "created for " + thread
		})
		b.OnSandboxCreate(func(context.Context, string, sandbox.Sandbox, bool) string { calls = append(calls, "quiet"); return "" })
		b.PrepareSandbox(func(context.Context, string, sandbox.Sandbox, bool) string {
			calls = append(calls, "prepare")
			return "prepared"
		})
		return nil
	})
	h := buildFacade(t, nil, llmtest.New(), func(o *harness.Options) { o.Features = append(harness.ProfileMinimal(), feature) })

	// Act
	th, _ := h.NewThread(context.Background(), "alice", "")
	sb, err := h.Sandboxes.Acquire(context.Background(), th.ID, "alice")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ","); got != "prepare,created,quiet" {
		t.Fatalf("hook order %q", got)
	}
	if got := sandbox.NoticeOf(sb); got != "prepared\ncreated for "+th.ID {
		t.Fatalf("notice %q", got)
	}
}

// Minimal implementations of the single-slot interfaces.
type (
	nopAnswers struct{}
	nopImages  struct{}
	nopChecker struct{}
	nopPurger  struct{}
)

func (nopAnswers) CheckAnswer(context.Context, *store.Run, string, func(agui.Event) error) string {
	return ""
}
func (nopImages) AttachImages(_ context.Context, _ *store.Run, h []*ai.Message) []*ai.Message {
	return h
}
func (nopChecker) CheckerFor(string) string { return "" }
func (nopChecker) Check(context.Context, *store.Run, string) agent.CheckResult {
	return agent.CheckResult{}
}
func (nopPurger) Purge(context.Context) error { return nil }
