package exploretools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/explore"
)

// stubTool is a file tool of the source provider: it answers with out (or fails with err) and records its arguments.
type stubTool struct {
	name string
	out  string
	err  error
	args []string
}

func (s *stubTool) Info(context.Context) (*ai.ToolInfo, error) {
	return &ai.ToolInfo{Name: s.name, Desc: s.name, Parameters: map[string]any{"type": "object"}}, nil
}

func (s *stubTool) InvokableRun(_ context.Context, args string) (string, error) {
	s.args = append(s.args, args)
	return s.out, s.err
}

// infoless is a tool whose Info fails: the explorer cannot tell what it is.
type infoless struct{ stubTool }

func (infoless) Info(context.Context) (*ai.ToolInfo, error) { return nil, errors.New("no info") }

// baseOnly is a tool that cannot be invoked.
type baseOnly struct{}

func (baseOnly) Info(context.Context) (*ai.ToolInfo, error) {
	return &ai.ToolInfo{Name: "read_file", Parameters: map[string]any{"type": "object"}}, nil
}

type source struct {
	list    []ai.BaseTool
	err     error
	cleaned *bool
}

func (s source) Name() string { return "stub" }
func (s source) Tools(context.Context, tools.RunContext) ([]ai.BaseTool, func(), error) {
	if s.err != nil {
		return nil, nil, s.err
	}
	return s.list, func() {
		if s.cleaned != nil {
			*s.cleaned = true
		}
	}, nil
}

type failingFactory struct{ err error }

func (f failingFactory) Model(context.Context, string) (ai.Model, error) { return nil, f.err }

// withToolsFails is a model that refuses the explorer's tool list.
type withToolsFails struct{ *llmtest.Model }

func (withToolsFails) WithTools([]*ai.ToolInfo) (ai.Model, error) {
	return nil, errors.New("tools rejected")
}

type modelFactory struct{ m ai.Model }

func (f modelFactory) Model(context.Context, string) (ai.Model, error) { return f.m, nil }

func run(t *testing.T, p *exploretools.Provider, ctx context.Context, args string) (string, error) {
	t.Helper()
	ts, cleanup, err := p.Tools(ctx, tools.RunContext{ThreadID: "t", Login: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		t.Fatal("the explore tool has nothing to clean up")
	}
	return ts[0].(ai.InvokableTool).InvokableRun(ctx, args)
}

func TestTheExploreToolIsNamedAndReadOnly(t *testing.T) {
	// Arrange
	p := &exploretools.Provider{Source: source{}, Models: llmtest.Factory{M: llmtest.New()}, Model: "m"}

	// Act
	ts, _, err := p.Tools(context.Background(), tools.RunContext{})

	// Assert
	if err != nil || p.Name() != "explore" || len(ts) != 1 {
		t.Fatalf("name %q tools %d err %v", p.Name(), len(ts), err)
	}
	info, _ := ts[0].Info(context.Background())
	ro, ok := ts[0].(interface{ ReadOnly() bool })
	if info.Name != "explore" || !ok || !ro.ReadOnly() {
		t.Fatalf("info %+v readOnly %v: explore calls must be able to run in parallel", info, ok)
	}
}

func TestExploreFailsClearlyWhenItCannotStart(t *testing.T) {
	read := &stubTool{name: "read_file", out: "x"}
	cases := []struct {
		name string
		p    *exploretools.Provider
		args string
		want string
	}{
		{"empty task", &exploretools.Provider{Source: source{list: []ai.BaseTool{read}}, Models: llmtest.Factory{M: llmtest.New()}, Model: "m"}, `{"task":"  "}`,
			"task must not be empty"},
		{"file tools unavailable", &exploretools.Provider{Source: source{err: errors.New("sandbox down")}, Models: llmtest.Factory{M: llmtest.New()}, Model: "m"}, `{"task":"x"}`,
			"the file tools are unavailable: sandbox down"},
		{"no read-only tool", &exploretools.Provider{Source: source{list: []ai.BaseTool{&stubTool{name: "bash"}, &stubTool{name: "write_file"}, &infoless{}, baseOnly{}}},
			Models: llmtest.Factory{M: llmtest.New()}, Model: "m"}, `{"task":"x"}`, "no file tools are available (no sandbox)"},
		{"model unavailable", &exploretools.Provider{Source: source{list: []ai.BaseTool{read}}, Models: failingFactory{errors.New("no such model")}, Model: "m"}, `{"task":"x"}`,
			"the explorer model is unavailable: no such model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			_, err := run(t, tc.p, context.Background(), tc.args)

			// Assert
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExploreFailsWhenTheModelRejectsItsToolList(t *testing.T) {
	// Arrange
	p := &exploretools.Provider{Source: source{list: []ai.BaseTool{&stubTool{name: "read_file"}}}, Models: modelFactory{withToolsFails{llmtest.New()}}, Model: "m"}

	// Act
	_, err := run(t, p, context.Background(), `{"task":"x"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "tools rejected") {
		t.Fatalf("err %v", err)
	}
}

func TestExploreUsesOnlyTheReadOnlyToolsOfItsSourceAndCleansUp(t *testing.T) {
	// Arrange
	cleaned := false
	read := &stubTool{name: "read_file", out: "content"}
	bash := &stubTool{name: "bash", out: "ran"}
	fake := llmtest.New(
		llmtest.Turn{Chunks: []*ai.Message{
			llmtest.Finish(llmtest.ToolCall(0, "c1", "bash", `{"command":"rm -rf /"}`), "tool_calls"),
			llmtest.Finish(llmtest.ToolCall(1, "c2", "read_file", ``), "tool_calls"),
		}},
		llmtest.Turn{Chunks: llmtest.Text("report")},
	)
	p := &exploretools.Provider{Source: source{list: []ai.BaseTool{read, bash, &infoless{}, baseOnly{}}, cleaned: &cleaned}, Models: llmtest.Factory{M: fake}, Model: "m"}

	// Act
	out, err := run(t, p, context.Background(), `{"task":"look"}`)

	// Assert
	if err != nil || out != "report" || !cleaned {
		t.Fatalf("out %q err %v cleaned %v", out, err, cleaned)
	}
	if len(bash.args) != 0 {
		t.Fatalf("the explorer ran bash: %v", bash.args)
	}
	if len(read.args) != 1 || read.args[0] != "{}" {
		t.Fatalf("read_file called with %q, want an empty argument string turned into {}", read.args)
	}
	offered := fake.Calls()[0].Tools
	if len(offered) != 1 || offered[0].Name != "read_file" {
		t.Fatalf("offered tools %v", offered)
	}
}

func TestAToolFailureAndAHugeResultReachTheModelAsText(t *testing.T) {
	// Arrange
	huge := &stubTool{name: "search_files", out: "a" + strings.Repeat("é", 5000)} // 10001 bytes: byte 8000 is inside a rune
	broken := &stubTool{name: "list_files", err: errors.New("permission denied")}
	fake := llmtest.New(
		llmtest.Turn{Chunks: []*ai.Message{
			llmtest.Finish(llmtest.ToolCall(0, "c1", "search_files", `{"pattern":"x"}`), "tool_calls"),
			llmtest.Finish(llmtest.ToolCall(1, "c2", "list_files", `{"path":"/nope"}`), "tool_calls"),
		}},
		llmtest.Turn{Chunks: llmtest.Text("done")},
	)
	p := &exploretools.Provider{Source: source{list: []ai.BaseTool{huge, broken}}, Models: llmtest.Factory{M: fake}, Model: "m"}
	rec := &recorder{}

	// Act
	_, err := run(t, p, tools.WithCall(context.Background(), "call", rec), `{"task":"x"}`)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, m := range fake.Calls()[1].Messages {
		if m.Role == ai.RoleTool {
			results = append(results, m.Content)
		}
	}
	if len(results) != 2 {
		t.Fatalf("%d tool results", len(results))
	}
	if !strings.HasSuffix(results[0], "\n[output cut; narrow the search or read with offset and limit]") || len(results[0]) > 8000+100 {
		t.Fatalf("huge result has %d bytes, suffix %q", len(results[0]), results[0][max(0, len(results[0])-60):])
	}
	if cut := strings.TrimSuffix(results[0], "\n[output cut; narrow the search or read with offset and limit]"); strings.ContainsRune(cut, '\ufffd') || len(cut) != 7999 {
		t.Fatalf("the cut result is not valid UTF-8 at the last rune boundary before 8000 bytes: %d bytes", len(cut))
	}
	if !strings.HasPrefix(results[1], "error: ") || !strings.Contains(results[1], "permission denied") {
		t.Fatalf("failure result %q", results[1])
	}
	var errored []bool
	for _, v := range rec.values() {
		if v["phase"] == agui.SubagentTool {
			errored = append(errored, v["tool"].(map[string]any)["isError"].(bool))
		}
	}
	if len(errored) != 2 || errored[0] || !errored[1] {
		t.Fatalf("isError flags %v, want [false true]", errored)
	}
}

func TestTheReportIsTrimmedCutAtSixThousandBytesOrReplacedWhenEmpty(t *testing.T) {
	cases := []struct {
		name, answer string
		check        func(t *testing.T, out string)
	}{
		{"empty", "  \n ", func(t *testing.T, out string) {
			if out != "The explorer returned no report." {
				t.Fatalf("out %q", out)
			}
		}},
		{"trimmed", "\n  Found it.  \n", func(t *testing.T, out string) {
			if out != "Found it." {
				t.Fatalf("out %q", out)
			}
		}},
		{"cut on a rune boundary", "a" + strings.Repeat("é", 4000), func(t *testing.T, out string) {
			body := strings.TrimSuffix(out, "…")
			if !strings.HasSuffix(out, "…") || len(body) != 5999 || strings.ContainsRune(body, '\ufffd') {
				t.Fatalf("report has %d bytes before the ellipsis", len(body))
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			tl := exploreTool(t, llmtest.Turn{Chunks: llmtest.Text(tc.answer)})

			// Act
			out, err := tl.InvokableRun(context.Background(), `{"task":"x"}`)

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, out)
		})
	}
}

func TestAnExplorerModelFailureIsReportedAsAFailedRun(t *testing.T) {
	// Arrange
	tl := exploreTool(t, llmtest.Turn{Err: errors.New("provider exploded")})
	rec := &recorder{}

	// Act
	_, err := tl.InvokableRun(tools.WithCall(context.Background(), "call", rec), `{"task":"x"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "the explorer failed") {
		t.Fatalf("err %v", err)
	}
	vs := rec.values()
	if end := vs[len(vs)-1]; end["phase"] != agui.SubagentEnd || end["failed"] != true || end["reportChars"] != 0 {
		t.Fatalf("end event %v", end)
	}
	if len(rec.usage) != 1 {
		t.Fatalf("usage reported %d times", len(rec.usage))
	}
}

func TestACancelledParentRunSurfacesItsContextError(t *testing.T) {
	// Arrange
	tl := exploreTool(t, llmtest.Turn{Block: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	_, err := tl.InvokableRun(ctx, `{"task":"x"}`)

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled (not an explorer failure)", err)
	}
}

func TestSubagentEventsSummarizeEachToolCallAndTheModelsInterimText(t *testing.T) {
	// Arrange
	long := strings.Repeat("w", 300)
	tl := exploreTool(t,
		llmtest.Turn{Chunks: []*ai.Message{
			{Role: ai.RoleAssistant, Content: "  Searching the handlers  "},
			llmtest.Finish(llmtest.ToolCall(0, "c1", "search_files", `{"pattern":"Handle","path":"/workspace/pkg"}`), "tool_calls"),
			llmtest.Finish(llmtest.ToolCall(1, "c2", "read_file", `{"path":"/workspace/pkg/a.go","offset":10}`), "tool_calls"),
			llmtest.Finish(llmtest.ToolCall(2, "c3", "read_file", `{"path":"/workspace/pkg/a.go","offset":0,"limit":5}`), "tool_calls"),
			llmtest.Finish(llmtest.ToolCall(3, "c4", "list_files", `{"path":"/workspace/pkg"}`), "tool_calls"),
			llmtest.Finish(llmtest.ToolCall(4, "c5", "search_files", `{"pattern":"`+long+`"}`), "tool_calls"),
			llmtest.Finish(llmtest.ToolCall(5, "c6", "read_file", `not json`), "tool_calls"),
		}},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	rec := &recorder{}

	// Act
	if _, err := tl.InvokableRun(tools.WithCall(context.Background(), "call", rec), `{"task":"`+strings.Repeat("t", 500)+`"}`); err != nil {
		t.Fatal(err)
	}

	// Assert
	var texts, args []string
	var task string
	for _, v := range rec.values() {
		switch v["phase"] {
		case agui.SubagentStart:
			task = v["task"].(string)
		case agui.SubagentText:
			texts = append(texts, v["text"].(string))
		case agui.SubagentTool:
			args = append(args, v["tool"].(map[string]any)["args"].(string))
		}
	}
	if len([]rune(task)) != 400 || !strings.HasSuffix(task, "…") {
		t.Errorf("task in the start event has %d runes", len([]rune(task)))
	}
	if len(texts) != 1 || texts[0] != "Searching the handlers" {
		t.Errorf("interim text %q", texts)
	}
	if len(args) != 6 {
		t.Fatalf("args %q", args)
	}
	if args[0] != `"Handle" in /workspace/pkg` {
		t.Errorf("search summary %q", args[0])
	}
	if args[1] != "/workspace/pkg/a.go:10-" {
		t.Errorf("read from offset %q", args[1])
	}
	if args[2] != "/workspace/pkg/a.go" {
		t.Errorf("read with a zero offset %q", args[2])
	}
	if args[3] != "/workspace/pkg" {
		t.Errorf("listing %q", args[3])
	}
	if len([]rune(args[4])) != 120 || !strings.HasSuffix(args[4], "…") || !strings.HasPrefix(args[4], `"www`) {
		t.Errorf("long pattern summary has %d runes: %q", len([]rune(args[4])), args[4])
	}
	if args[5] != "" {
		t.Errorf("unparsable arguments summary %q", args[5])
	}
}

func TestAToolResultInAnEventIsItsFirstLineCutAt160Runes(t *testing.T) {
	// Arrange
	long := &stubTool{name: "read_file", out: "\n  " + strings.Repeat("ü", 200) + "\nsecond line"}
	fake := llmtest.New(
		llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "read_file", `{"path":"/workspace/a"}`), "tool_calls")}},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	p := &exploretools.Provider{Source: source{list: []ai.BaseTool{long}}, Models: llmtest.Factory{M: fake}, Model: "m"}
	rec := &recorder{}

	// Act
	if _, err := run(t, p, tools.WithCall(context.Background(), "call", rec), `{"task":"x"}`); err != nil {
		t.Fatal(err)
	}

	// Assert
	for _, v := range rec.values() {
		if v["phase"] == agui.SubagentTool {
			if got := v["tool"].(map[string]any)["result"].(string); got != strings.Repeat("ü", 159)+"…" {
				t.Fatalf("result %q", got)
			}
			return
		}
	}
	t.Fatal("no tool event")
}

func TestTheExplorerStopsAtItsStepLimitAndAsksForTheReport(t *testing.T) {
	// Arrange
	read := &stubTool{name: "read_file", out: "x"}
	call := llmtest.Finish(llmtest.ToolCall(0, "c", "read_file", `{"path":"/workspace/a"}`), "tool_calls")
	fake := llmtest.New(llmtest.Turn{Chunks: []*ai.Message{call}}, llmtest.Turn{Chunks: []*ai.Message{call}}, llmtest.Turn{Chunks: llmtest.Text("partial report")})
	p := &exploretools.Provider{Source: source{list: []ai.BaseTool{read}}, Models: llmtest.Factory{M: fake}, Model: "m", MaxSteps: 3}

	// Act
	out, err := run(t, p, context.Background(), `{"task":"x"}`)

	// Assert
	calls := fake.Calls()
	if err != nil || out != "partial report" || len(calls) != 3 {
		t.Fatalf("out %q err %v calls %d", out, err, len(calls))
	}
	last := calls[2].Messages
	if nudge := last[len(last)-1]; nudge.Role != ai.RoleUser || nudge.Content != "Step limit reached: write your report now from what you found." {
		t.Fatalf("last message before the final step: %+v", nudge)
	}
	if calls[2].ToolChoice == nil {
		t.Error("the last step still allows tool calls")
	}
}
