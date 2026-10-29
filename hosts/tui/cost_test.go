package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// infoBackend is an optsBackend that lists models and describes them.
type infoBackend struct {
	optsBackend
	model string
	info  map[string]client.ModelInfo
}

func (b *infoBackend) Model() string {
	if b.model == "" {
		return "gpt-test"
	}
	return b.model
}
func (b *infoBackend) Models(context.Context) ([]string, error) {
	return []string{"gpt-test", "big-model", "plain"}, nil
}
func (b *infoBackend) SetModel(id string) { b.model = id }
func (b *infoBackend) ModelInfo(_ context.Context, id string) (client.ModelInfo, bool) {
	i, ok := b.info[id]
	return i, ok
}

// priced: $3 in, $15 out, $0.30 cached per Mtok, 200k window.
func priced() client.ModelInfo {
	return client.ModelInfo{ContextWindow: 200_000, MaxOutput: 64_000, Priced: true, Vision: true, Reasoning: true,
		InputPerToken: 3e-6, OutputPerToken: 15e-6, CacheReadPerToken: 0.3e-6}
}

func newInfoModel(info map[string]client.ModelInfo) (*Model, *infoBackend) {
	be := &infoBackend{info: info}
	m := New(be, Options{Now: func() time.Time { return t0 }, StaticCursor: true})
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 40})
	return m, be
}

func usageEvent(in, out, cached, ctx int) agui.Event {
	return agui.CustomEvent(agui.CustomUsage, map[string]any{"inputTokens": in, "outputTokens": out, "cachedInputTokens": cached,
		"contextTokens": ctx, "contextWindow": 128000})
}

func finish(model string, in, out, cached int) agui.Event {
	return agui.RunFinishedEvent("t", "r", "success", []agui.Usage{{Model: model, InputTokens: in, OutputTokens: out, CachedInputTokens: cached}})
}

func TestStatusLineShowsContextUsageAndSessionCost(t *testing.T) {
	m, _ := newInfoModel(map[string]client.ModelInfo{"gpt-test": priced()})
	startRun(t, m, "hi")

	feed(t, m, usageEvent(20_000, 1_000, 10_000, 12_300))

	line := RenderStatus(m.statusView(), 150)
	// 10k fresh at $3 + 10k cached at $0.30 + 1k out at $15 = 0.03 + 0.003 + 0.015
	for _, want := range []string{"ctx 12.3k/200k (6%)", "$0.048"} {
		if !strings.Contains(line, want) {
			t.Fatalf("status %q lacks %q", line, want)
		}
	}
}

func TestStatusLineHidesCostForAnUnknownModelAndMarksPartialCost(t *testing.T) {
	m, be := newInfoModel(nil)
	startRun(t, m, "hi")
	feed(t, m, usageEvent(1_000, 100, 0, 1_000))
	if line := RenderStatus(m.statusView(), 150); strings.Contains(line, "$") {
		t.Fatalf("cost shown for an unknown model: %q", line)
	}

	be.info = map[string]client.ModelInfo{"gpt-test": priced()}
	feed(t, m, finish("gpt-test", 1_000, 100, 0), usageEvent(1_000, 100, 0, 1_000))
	startRun(t, m, "again")
	feed(t, m, finish("other", 5_000, 500, 0))

	if line := RenderStatus(m.statusView(), 150); !strings.Contains(line, "~$0.0045") {
		t.Fatalf("partial cost not marked: %q", line)
	}
}

func TestContextWarnsPastSeventyPercent(t *testing.T) {
	line := RenderStatus(StatusView{Model: "m", State: "idle", CtxUsed: 190_000, CtxWindow: 200_000}, 100)

	if !strings.Contains(line, "ctx 190.0k/200k (95%)") {
		t.Fatalf("status %q", line)
	}
}

func TestSidebarContextShowsWindowBarCostAndPrice(t *testing.T) {
	m, _ := newInfoModel(map[string]client.ModelInfo{"gpt-test": priced()})
	startRun(t, m, "hi")
	feed(t, m, usageEvent(20_000, 1_000, 0, 100_000), finish("gpt-test", 20_000, 1_000, 0))

	got := m.renderSidebar(sidebarWidth, 30)

	for _, want := range []string{"window 100.0k / 200k (50%)", "█████░░░░░", "cost $0.075", "$3/$15 per Mtok", "max output 64k"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sidebar lacks %q:\n%s", want, got)
		}
	}
}

func TestCostCommandListsEachRunAndTheTotal(t *testing.T) {
	m, _ := newInfoModel(map[string]client.ModelInfo{"gpt-test": priced()})
	startRun(t, m, "one")
	feed(t, m, finish("gpt-test", 10_000, 1_000, 4_000))
	startRun(t, m, "two")
	feed(t, m, finish("mystery", 2_000, 200, 0))

	submitSlash(m, "/cost")

	tr := m.Transcript()
	for _, want := range []string{"run 1 · gpt-test · in 10.0k out 1.0k cached 4.0k · $0.034", "run 2 · mystery · in 2.0k out 200 · –", "total · in 12.0k out 1.2k cached 4.0k · ~$0.034"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("/cost lacks %q:\n%s", want, tr)
		}
	}
}

func TestContextCommandShowsTheWindowUsage(t *testing.T) {
	m, _ := newInfoModel(map[string]client.ModelInfo{"gpt-test": priced()})
	startRun(t, m, "hi")
	feed(t, m, usageEvent(50_000, 500, 0, 50_000))

	submitSlash(m, "/context")

	if tr := m.Transcript(); !strings.Contains(tr, "context: 50.0k of 200k tokens in use (25%) · max output 64k") {
		t.Fatalf("transcript:\n%s", tr)
	}
}

func TestModelPickerRowsShowWindowPriceAndBadgesAndStillFilter(t *testing.T) {
	info := map[string]client.ModelInfo{
		"gpt-test":  priced(),
		"big-model": {ContextWindow: 1_000_000, Priced: true, InputPerToken: 1.25e-6, OutputPerToken: 10e-6},
	}
	m, _ := newInfoModel(info)

	submitSlash(m, "/model")

	view := m.View()
	for _, want := range []string{"gpt-test", "200k · $3/$15 per Mtok · vision · reasoning", "big-model", "1M · $1.25/$10 per Mtok"} {
		if !strings.Contains(view, want) {
			t.Fatalf("picker lacks %q:\n%s", want, view)
		}
	}
	typeText(m, "reasoning")
	if view := m.View(); !strings.Contains(view, "gpt-test") || strings.Contains(view, "big-model") {
		t.Fatalf("filter on a badge:\n%s", view)
	}
}

func TestImageWarnsWhenTheModelHasNoVision(t *testing.T) {
	be := &infoBackend{info: map[string]client.ModelInfo{"gpt-test": {ContextWindow: 8000, Priced: true}}}
	m, root := newWS(t, be, nil)

	typeText(m, "/image "+pngFile(t, root, "a.png"))
	press(m, tea.KeyEnter)

	if tr := m.Transcript(); !strings.Contains(tr, "gpt-test is not listed as accepting images") {
		t.Fatalf("transcript:\n%s", tr)
	}
}

func TestImageDoesNotWarnWhenTheModelIsUnknownOrSeesImages(t *testing.T) {
	for name, info := range map[string]map[string]client.ModelInfo{"unknown": nil, "vision": {"gpt-test": priced()}} {
		be := &infoBackend{info: info}
		m, root := newWS(t, be, nil)

		typeText(m, "/image "+pngFile(t, root, "a.png"))
		press(m, tea.KeyEnter)

		if tr := m.Transcript(); strings.Contains(tr, "accepting images") {
			t.Fatalf("%s: warned:\n%s", name, tr)
		}
	}
}

func TestClientBackendServesInfoFromTheModelsAnswer(t *testing.T) {
	c := &infoClient{list: client.ModelList{Models: []string{"a"}, Default: "a", Info: map[string]client.ModelInfo{"a": priced()}}}
	be := &ClientBackend{C: c, ModelID: "a"}

	if _, ok := be.ModelInfo(context.Background(), "a"); ok {
		t.Fatal("info before any Models answer")
	}
	if _, err := be.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	i, ok := be.ModelInfo(context.Background(), "a")

	if !ok || i.ContextWindow != 200_000 {
		t.Fatalf("info %+v %v", i, ok)
	}
}

func TestClientBackendPrefersItsLocalLookup(t *testing.T) {
	be := &ClientBackend{C: &infoClient{}, InfoFunc: func(id string) (client.ModelInfo, bool) { return priced(), id == "x" }}

	_, hit := be.ModelInfo(context.Background(), "x")
	_, miss := be.ModelInfo(context.Background(), "y")

	if !hit || miss {
		t.Fatalf("hit %v miss %v", hit, miss)
	}
}

// infoClient is a client.Client that only answers Models.
type infoClient struct {
	client.Client
	list client.ModelList
}

func (c *infoClient) Models(context.Context) (client.ModelList, error) { return c.list, nil }
