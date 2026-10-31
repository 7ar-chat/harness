package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/hosts/client"
)

func TestOnlyRunFinishedAndRunErrorEndTheStream(t *testing.T) {
	for typ, want := range map[string]bool{agui.RunFinished: true, agui.RunError: true, agui.TextMessageEnd: false, agui.Custom: false} {
		if got := (Ev{Type: typ}).Terminal(); got != want {
			t.Errorf("Terminal(%s) = %v, want %v", typ, got, want)
		}
	}
}

func TestCachePercentIsZeroWithoutInputAndRoundsDownOtherwise(t *testing.T) {
	if got := (Usage{Cached: 50}).CachePercent(); got != 0 {
		t.Errorf("no input: %d", got)
	}
	if got := (Usage{In: 3, Cached: 1}).CachePercent(); got != 33 {
		t.Errorf("one third: %d", got)
	}
}

func TestRenderToolCallCollapsesLongResultsAndExpandsThemOnRequest(t *testing.T) {
	tv := ToolView{Name: "run_command", Args: `{"command":"seq 20"}`, Done: true, Result: strings.Repeat("row\n", 20)}

	collapsed := RenderToolCall(tv, 80, false)
	expanded := RenderToolCall(tv, 80, true)

	if !strings.Contains(collapsed, "run_command seq 20") || !strings.Contains(collapsed, "ctrl+o expands") || strings.Count(collapsed, "row") != collapsedLines {
		t.Fatalf("collapsed:\n%s", collapsed)
	}
	if strings.Count(expanded, "row") != 20 || strings.Contains(expanded, "ctrl+o expands") {
		t.Fatalf("expanded:\n%s", expanded)
	}
}

func TestTildePathShortensOnlyTheHomePrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if got := tildePath(filepath.Join(home, "proj")); got != "~"+string(os.PathSeparator)+"proj" {
		t.Errorf("inside home: %q", got)
	}
	if got := tildePath("/elsewhere/proj"); got != "/elsewhere/proj" {
		t.Errorf("outside home: %q", got)
	}
}

func TestDraftTextPrefersTheInputElseTheDraftBeforeTheSlashCommand(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	m.inp.draft = "parked"

	empty := m.draftText()
	typeText(m, "typed")

	if empty != "parked" || m.draftText() != "typed" {
		t.Fatalf("empty %q typed %q", empty, m.draftText())
	}
}

func TestPriceLineAndBadgesOfAModelWithoutAWindow(t *testing.T) {
	m, _ := newInfoModel(map[string]client.ModelInfo{"gpt-test": {Priced: true, InputPerToken: 2e-6, OutputPerToken: 8e-6}})

	if got := m.modelBadges("gpt-test"); got != "$2/$8 per Mtok" {
		t.Fatalf("badges %q", got)
	}
	if got := m.modelBadges("unknown"); got != "" {
		t.Fatalf("unknown model: %q", got)
	}
}
