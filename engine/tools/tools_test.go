package tools_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/engine/tools"
)

// named is a tool that only has a name.
type named string

func (n named) Info(context.Context) (*ai.ToolInfo, error) { return &ai.ToolInfo{Name: string(n)}, nil }

// brokenInfo is a tool whose description cannot be built.
type brokenInfo struct{}

func (brokenInfo) Info(context.Context) (*ai.ToolInfo, error) { return nil, errors.New("no info") }

// nilInfo is a tool that describes itself with nothing.
type nilInfo struct{}

func (nilInfo) Info(context.Context) (*ai.ToolInfo, error) { return nil, nil }

// provider offers fixed tools, fails with err, and records the order of its cleanup in log.
type provider struct {
	name  string
	tools []ai.BaseTool
	err   error
	log   *[]string
	rc    *tools.RunContext
}

func (p *provider) Name() string { return p.name }
func (p *provider) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	if p.rc != nil {
		*p.rc = rc
	}
	if p.err != nil {
		return nil, nil, p.err
	}
	cleanup := func() {
		if p.log != nil {
			*p.log = append(*p.log, p.name)
		}
	}
	return p.tools, cleanup, nil
}

func toolNames(t *testing.T, list []ai.BaseTool) []string {
	t.Helper()
	var out []string
	for _, tl := range list {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, info.Name)
	}
	return out
}

func TestRegistryMergesProvidersInOrderAndDropsDuplicateNames(t *testing.T) {
	// The first provider to offer a name keeps it; later duplicates are dropped and the order of the tool list is stable.
	// Arrange
	var seen tools.RunContext
	reg := tools.NewRegistry(
		&provider{name: "a", tools: []ai.BaseTool{named("read"), named("write")}, rc: &seen},
		nil,
		&provider{name: "b", tools: []ai.BaseTool{named("write"), named("search")}},
	)
	rc := tools.RunContext{ThreadID: "t", RunID: "r", Login: "alice", Sandbox: tools.SandboxOff}

	// Act
	list, cleanup, err := reg.Tools(context.Background(), rc)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(t, list); !reflect.DeepEqual(got, []string{"read", "write", "search"}) {
		t.Fatalf("tools = %v", got)
	}
	if seen != rc {
		t.Fatalf("provider got run context %+v, want %+v", seen, rc)
	}
	cleanup()
	if reg.Name() != "registry" {
		t.Fatalf("name = %q", reg.Name())
	}
}

func TestRegistrySkipsAFailingProviderAndUnusableTools(t *testing.T) {
	// A provider that fails is skipped without failing the run; tools that cannot describe themselves are left out.
	// Arrange
	var log []string
	reg := tools.NewRegistry(
		&provider{name: "bad", err: errors.New("mcp server down")},
		&provider{name: "mixed", tools: []ai.BaseTool{brokenInfo{}, nilInfo{}, named("ok")}, log: &log},
	)

	// Act
	list, cleanup, err := reg.Tools(context.Background(), tools.RunContext{RunID: "r"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(t, list); !reflect.DeepEqual(got, []string{"ok"}) {
		t.Fatalf("tools = %v", got)
	}
	cleanup()
	if !reflect.DeepEqual(log, []string{"mixed"}) {
		t.Fatalf("cleanups run = %v, want only the providers that succeeded", log)
	}
}

func TestRegistryCleanupRunsEveryProviderCleanup(t *testing.T) {
	// The cleanup func returned for the run releases the resources of all providers.
	// Arrange
	var log []string
	reg := tools.NewRegistry(
		&provider{name: "a", tools: []ai.BaseTool{named("x")}, log: &log},
		&provider{name: "b", tools: []ai.BaseTool{named("y")}, log: &log},
	)
	_, cleanup, err := reg.Tools(context.Background(), tools.RunContext{})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	cleanup()

	// Assert
	if !reflect.DeepEqual(log, []string{"a", "b"}) {
		t.Fatalf("cleanups = %v", log)
	}
}

func TestFilterOffersOnlyAllowedToolsInTheInnerOrder(t *testing.T) {
	// The filter keeps the inner order, asks Allow per run and tool name, and passes the cleanup through.
	// Arrange
	var log []string
	inner := &provider{name: "inner", tools: []ai.BaseTool{named("a"), brokenInfo{}, named("b"), named("c")}, log: &log}
	var asked []string
	f := tools.Filter{Inner: inner, Allow: func(_ context.Context, rc tools.RunContext, name string) bool {
		asked = append(asked, rc.RunID+":"+name)
		return name != "b"
	}}

	// Act
	list, cleanup, err := f.Tools(context.Background(), tools.RunContext{RunID: "r1"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(t, list); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Fatalf("tools = %v", got)
	}
	if !reflect.DeepEqual(asked, []string{"r1:a", "r1:b", "r1:c"}) {
		t.Fatalf("Allow asked about %v", asked)
	}
	cleanup()
	if f.Name() != "inner" || !reflect.DeepEqual(log, []string{"inner"}) {
		t.Fatalf("name %q cleanups %v", f.Name(), log)
	}
}

func TestFilterReturnsTheInnerError(t *testing.T) {
	// A failing inner provider fails the filter, with no tools.
	// Arrange
	boom := errors.New("boom")
	f := tools.Filter{Inner: &provider{name: "inner", err: boom}, Allow: func(context.Context, tools.RunContext, string) bool { return true }}

	// Act
	list, _, err := f.Tools(context.Background(), tools.RunContext{})

	// Assert
	if !errors.Is(err, boom) || len(list) != 0 {
		t.Fatalf("tools %v err %v", list, err)
	}
}

func TestErrorfBuildsAUserErrorForTheModel(t *testing.T) {
	// Arrange
	err := tools.Errorf("file %q not found (%d)", "a.txt", 2)

	// Act
	var ue *tools.UserError
	ok := errors.As(err, &ue)

	// Assert
	if !ok || ue.Msg != `file "a.txt" not found (2)` || err.Error() != ue.Msg {
		t.Fatalf("err %v as user error %v", err, ok)
	}
}

// emitter implements the optional emitter capabilities of the agent.
type emitter struct {
	canView   bool
	untrusted bool
	images    []tools.Image
	after     []agui.Event
	live      []agui.Event
	usage     []string
	outputs   []string
}

func (e *emitter) ToolOutput(id, stream, text string) {
	e.outputs = append(e.outputs, id+"/"+stream+"/"+text)
}
func (e *emitter) CanViewImages() bool           { return e.canView }
func (e *emitter) AttachImage(img tools.Image)   { e.images = append(e.images, img) }
func (e *emitter) EmitAfterResult(ev agui.Event) { e.after = append(e.after, ev) }
func (e *emitter) EmitLive(ev agui.Event)        { e.live = append(e.live, ev) }
func (e *emitter) AddSubagentUsage(model string, in, out, cached int) {
	e.usage = append(e.usage, fmt.Sprintf("%s,%d,%d,%d", model, in, out, cached))
}
func (e *emitter) Untrusted() bool { return e.untrusted }

// plainEmitter only receives tool output.
type plainEmitter struct{}

func (plainEmitter) ToolOutput(string, string, string) {}

func TestCallFromContextCarriesTheCallIDAndEmitter(t *testing.T) {
	// Arrange
	e := &emitter{}

	// Act
	id, got := tools.CallFromContext(tools.WithCall(context.Background(), "call-7", e))
	noID, noEmitter := tools.CallFromContext(context.Background())

	// Assert
	if id != "call-7" || got != tools.Emitter(e) {
		t.Fatalf("id %q emitter %v", id, got)
	}
	if noID != "" || noEmitter != nil {
		t.Fatalf("without a call: id %q emitter %v", noID, noEmitter)
	}
}

func TestImagesAreQueuedOnlyForModelsThatSeeThem(t *testing.T) {
	// Arrange
	img := tools.Image{Path: "/workspace/a.png", MIMEType: "image/png", Data: []byte{1, 2}}
	blind := &emitter{canView: false}
	sighted := &emitter{canView: true}

	// Act
	blindCan := tools.CanViewImages(tools.WithCall(context.Background(), "c", blind))
	blindAttached := tools.AttachImage(tools.WithCall(context.Background(), "c", blind), img)
	sightedCan := tools.CanViewImages(tools.WithCall(context.Background(), "c", sighted))
	sightedAttached := tools.AttachImage(tools.WithCall(context.Background(), "c", sighted), img)

	// Assert
	if blindCan || blindAttached || len(blind.images) != 0 {
		t.Fatalf("a model without vision: can view %v attached %v queued %d", blindCan, blindAttached, len(blind.images))
	}
	if !sightedCan || !sightedAttached || len(sighted.images) != 1 || sighted.images[0].Path != img.Path {
		t.Fatalf("a model with vision: can view %v attached %v queued %v", sightedCan, sightedAttached, sighted.images)
	}
}

func TestImagesWithoutAnAgentEmitterAreRefused(t *testing.T) {
	// Without any emitter, or with one that cannot take images, nothing is attached.
	// Arrange
	ctxPlain := tools.WithCall(context.Background(), "c", plainEmitter{})
	img := tools.Image{Path: "/workspace/a.png"}

	// Act / Assert
	for name, ctx := range map[string]context.Context{"no call": context.Background(), "plain emitter": ctxPlain} {
		if tools.CanViewImages(ctx) || tools.AttachImage(ctx, img) {
			t.Errorf("%s: images accepted", name)
		}
	}
}

func TestLiveEventsReachOnlyEmittersThatTakeThem(t *testing.T) {
	// AfterResult, Live and SubagentUsage are forwarded to the agent's emitter and silently dropped without one.
	// Arrange
	e := &emitter{}
	ctx := tools.WithCall(context.Background(), "c", e)
	plain := tools.WithCall(context.Background(), "c", plainEmitter{})
	ev := agui.Event{"type": "CUSTOM", "name": "x"}

	// Act
	tools.AfterResult(ctx, ev)
	tools.Live(ctx, ev)
	tools.SubagentUsage(ctx, "gpt", 1, 2, 3)
	for _, c := range []context.Context{context.Background(), plain} {
		tools.AfterResult(c, ev)
		tools.Live(c, ev)
		tools.SubagentUsage(c, "gpt", 9, 9, 9)
	}

	// Assert
	if len(e.after) != 1 || len(e.live) != 1 || !reflect.DeepEqual(e.usage, []string{"gpt,1,2,3"}) {
		t.Fatalf("after %v live %v usage %v", e.after, e.live, e.usage)
	}
}

func TestUntrustedFollowsTheEmitterAndDefaultsToFalse(t *testing.T) {
	// Arrange / Act
	untrusted := tools.Untrusted(tools.WithCall(context.Background(), "c", &emitter{untrusted: true}))
	trusted := tools.Untrusted(tools.WithCall(context.Background(), "c", &emitter{}))
	plain := tools.Untrusted(tools.WithCall(context.Background(), "c", plainEmitter{}))
	none := tools.Untrusted(context.Background())

	// Assert
	if !untrusted || trusted || plain || none {
		t.Fatalf("untrusted %v trusted %v plain %v none %v", untrusted, trusted, plain, none)
	}
}

func TestInterruptErrorNamesItsReason(t *testing.T) {
	// Arrange
	var err error = &tools.Interrupt{Reason: "tool_approval"}

	// Act
	var it *tools.Interrupt
	ok := errors.As(err, &it)

	// Assert
	if !ok || err.Error() != "interrupt: tool_approval" {
		t.Fatalf("err %q as interrupt %v", err, ok)
	}
}
