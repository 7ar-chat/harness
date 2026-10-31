package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/hosts/client"
)

// ---- sessions and conversations ----

func TestSessionsOnABackendWithoutThemAndWithNoneSavedSaySo(t *testing.T) {
	plain, _ := newModel(&fakeBackend{})
	submitSlash(plain, "/sessions")

	empty := newExt()
	empty.sessions = nil
	m, _ := newModel(empty)
	submitSlash(m, "/sessions")

	if !strings.Contains(plain.Transcript(), "no saved conversations") {
		t.Fatalf("plain:\n%s", plain.Transcript())
	}
	if !strings.Contains(m.Transcript(), "no saved conversations") || m.picker != nil {
		t.Fatalf("empty:\n%s", m.Transcript())
	}
}

func TestSessionsAndNewAreRefusedWhileARunIsActive(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)
	startRun(t, m, "work")

	submitSlash(m, "/sessions")
	submitSlash(m, "/new")

	if m.picker != nil || be.newCalls != 0 || strings.Count(m.Transcript(), "esc cancels it first") != 2 {
		t.Fatalf("newCalls %d:\n%s", be.newCalls, m.Transcript())
	}
}

func TestNewClearsTheConversationAndTheSessionUsage(t *testing.T) {
	// Arrange
	be := newExt()
	m, _ := newModel(be)
	startRun(t, m, "one")
	feed(t, m, usageEvent(1000, 100, 0, 1000), finish("gpt-test", 1000, 100, 0))

	// Act
	submitSlash(m, "/new")

	// Assert
	if be.newCalls != 1 || len(m.blocks) != 0 || m.inp.sb.nRuns != 0 || m.statusView().Usage.In != 0 {
		t.Fatalf("newCalls %d blocks %d runs %d usage %+v", be.newCalls, len(m.blocks), m.inp.sb.nRuns, m.statusView().Usage)
	}
}

func TestNewOnABackendWithoutSessionsStillClearsTheView(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	m.add(block{kind: blockUser, text: "old"})

	submitSlash(m, "/new")

	if len(m.blocks) != 0 {
		t.Fatalf("blocks %d", len(m.blocks))
	}
}

type failingThreads struct {
	richBackend
	err error
}

func (f *failingThreads) NewThread(context.Context) error { return f.err }
func (f *failingThreads) Sessions(context.Context) ([]client.Session, error) {
	return nil, f.err
}

func TestNewAndSessionsFailuresAreReportedAndKeepTheConversation(t *testing.T) {
	m, _ := newModel(&failingThreads{err: errors.New("store down")})
	m.add(block{kind: blockUser, text: "keep me"})

	submitSlash(m, "/new")
	submitSlash(m, "/sessions")

	tr := m.Transcript()
	if !strings.Contains(tr, "keep me") || !strings.Contains(tr, "store down") || !strings.Contains(tr, "sessions: store down") {
		t.Fatalf("transcript:\n%s", tr)
	}
}

func TestResumedConversationShowsToolCallsResultsAndStoredErrors(t *testing.T) {
	be := newExt()
	be.thread = &client.Thread{ID: "t1", Messages: []client.Message{
		{Role: client.RoleUser, Content: "list files"},
		{Role: client.RoleAssistant, Content: "listing", ToolCalls: []client.ToolCall{{ID: "c1", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "list_files", Arguments: `{"path":"."}`}}}},
		{Role: client.RoleTool, ToolCallID: "c1", Content: "a.go"},
		{Role: client.RoleAssistant, Error: "model overloaded"},
	}}
	m, _ := newModel(be)

	update(m, threadMsg{th: be.thread})

	tr := m.Transcript()
	for _, want := range []string{"list files", "listing", "list_files", "a.go", "model overloaded", "resumed: (untitled)"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("transcript lacks %q:\n%s", want, tr)
		}
	}
}

func TestThreadSwitchErrorIsShownAsAnError(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	update(m, threadMsg{err: errors.New("thread gone")})

	if !strings.Contains(m.Transcript(), "thread gone") {
		t.Fatal(m.Transcript())
	}
}

func TestModelListFailureOffersManualEntryButAPartialListStillOpensThePicker(t *testing.T) {
	m, _ := newModel(&richBackend{})

	update(m, modelsMsg{err: errors.New("provider down")})
	failed := m.Transcript()
	update(m, modelsMsg{ids: []string{"a"}, err: errors.New("partial")})

	if !strings.Contains(failed, "provider down") || !strings.Contains(failed, "/model <id> sets one by hand") {
		t.Fatalf("transcript:\n%s", failed)
	}
	if m.picker == nil {
		t.Fatal("a partial list should still open the picker")
	}
}

func TestModelCommandOnABackendWithoutSwitchingSaysSo(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	submitSlash(m, "/model x")
	press(m, tea.KeyF2)

	if strings.Count(m.Transcript(), "cannot switch models") != 2 {
		t.Fatal(m.Transcript())
	}
}

// ---- history commands ----

func TestUndoRedoCompactAndForkReportBackendErrors(t *testing.T) {
	be := newExt()
	be.err = errors.New("snapshot missing")
	m, _ := newModel(be)

	for _, c := range []string{"/undo", "/compact", "/fork"} {
		submitSlash(m, c)
	}

	tr := m.Transcript()
	for _, want := range []string{"/undo: snapshot missing", "/compact: snapshot missing", "/fork: snapshot missing"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("lacks %q:\n%s", want, tr)
		}
	}
}

func TestHistoryCommandsOnAPlainBackendAreNotSupported(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	submitSlash(m, "/undo")

	if !strings.Contains(m.Transcript(), "/undo: not supported by this backend/server") {
		t.Fatal(m.Transcript())
	}
}

func TestCompactReportsTheTokensFreedOrThatNothingWasToCompact(t *testing.T) {
	cases := map[string]struct {
		res  *client.CompactResult
		want string
	}{
		"nothing":  {&client.CompactResult{}, "nothing to compact"},
		"tokens":   {&client.CompactResult{Compacted: true, TokensBefore: 90_000, TokensAfter: 12_000}, "compacted: 90.0k → 12.0k tokens"},
		"no count": {&client.CompactResult{Compacted: true}, "context compacted"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			be := newExt()
			be.compact = tc.res
			m, _ := newModel(be)

			submitSlash(m, "/compact")

			if !strings.Contains(m.Transcript(), tc.want) {
				t.Fatalf("lacks %q:\n%s", tc.want, m.Transcript())
			}
		})
	}
}

func TestUndoListsAtMostFiveFilesAndASingularFile(t *testing.T) {
	be := newExt()
	be.undoRes = &client.UndoResult{Files: []string{"1", "2", "3", "4", "5", "6", "7"}, CanUndo: true}
	m, _ := newModel(be)
	submitSlash(m, "/redo")
	many := m.Transcript()

	be.undoRes = &client.UndoResult{Files: []string{"only.go"}}
	submitSlash(m, "/redo")

	if !strings.Contains(many, "7 files changed — 1, 2, 3, 4, 5, …") || !strings.Contains(many, "/undo available") {
		t.Fatalf("many:\n%s", many)
	}
	if !strings.Contains(m.Transcript(), "1 file changed — only.go") {
		t.Fatalf("one:\n%s", m.Transcript())
	}
}

func TestUndoWithNoResultBodyStillReportsAndForkOfAnUntitledThread(t *testing.T) {
	be := newExt()
	be.undoRes = nil
	be.forked = &client.Thread{ID: "t7"}
	be.thread = &client.Thread{ID: "t7"}
	m, _ := newModel(be)

	submitSlash(m, "/undo")
	undone := m.Transcript()
	submitSlash(m, "/fork")

	if !strings.Contains(undone, "undone (no file changes)") || !strings.Contains(m.Transcript(), "forked: (untitled)") {
		t.Fatalf("undo:\n%s\nfork:\n%s", undone, m.Transcript())
	}
}

func TestUnsupportedHistoryErrorFromTheServerIsAnExplicitNotice(t *testing.T) {
	be := newExt()
	be.err = client.ErrNotSupported
	m, _ := newModel(be)

	submitSlash(m, "/compact")

	if !strings.Contains(m.Transcript(), "/compact: not supported by this backend/server") {
		t.Fatal(m.Transcript())
	}
}

// ---- custom commands ----

func TestReloadOnABackendWithoutCommandsAndFailedLoadsAreReported(t *testing.T) {
	plain, _ := newModel(&fakeBackend{})
	submitSlash(plain, "/reload")

	be := newExt()
	m := customSetup(t, be)
	be.err = errors.New("server 500")
	submitSlash(m, "/reload")

	if !strings.Contains(plain.Transcript(), "/reload: not supported by this backend/server") {
		t.Fatalf("plain:\n%s", plain.Transcript())
	}
	if !strings.Contains(m.Transcript(), "custom commands: server 500") {
		t.Fatalf("failed:\n%s", m.Transcript())
	}
}

func TestACustomCommandWithAnExpansionErrorShowsTheError(t *testing.T) {
	be := newExt()
	be.cmds = []client.Command{{Name: "review"}}
	m := customSetup(t, be)
	be.err = errors.New("template broken")

	submitSlash(m, "/review x")

	if !strings.Contains(m.Transcript(), "/review: template broken") || len(be.Calls()) != 0 {
		t.Fatalf("calls %v:\n%s", be.Calls(), m.Transcript())
	}
}

func TestAnExpandedCommandIsNotSentWhenARunStartedMeanwhile(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)
	startRun(t, m, "busy")

	update(m, expandedMsg{name: "review", args: "x", prompt: "do it"})

	if len(be.Calls()) != 1 || !strings.Contains(m.Transcript(), "a run started meanwhile") {
		t.Fatalf("calls %v:\n%s", be.Calls(), m.Transcript())
	}
}

func TestCustomCommandOnABackendWithoutCommandsIsNotSupported(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	_ = m.runCustom("review", "x")

	if !strings.Contains(m.Transcript(), "/review: not supported by this backend/server") {
		t.Fatal(m.Transcript())
	}
}

// ---- /skills, /mcp, /status ----

func TestSkillsListsOnlySkillsAndMcpOnlyMcpServers(t *testing.T) {
	m, _ := newModel(newStatusBackend())
	submitSlash(m, "/skills")
	skills := m.View()
	press(m, tea.KeyEsc)
	submitSlash(m, "/mcp")
	mcp := m.View()

	if !strings.Contains(skills, "/review") || strings.Contains(skills, "/deploy") {
		t.Fatalf("skills:\n%s", skills)
	}
	if !strings.Contains(mcp, "docs") || strings.Contains(mcp, "config.yml") {
		t.Fatalf("mcp:\n%s", mcp)
	}
}

func TestSkillsAndMcpFailuresAndEmptyAnswersAreNotices(t *testing.T) {
	be := newStatusBackend()
	be.extBackend.err = client.ErrNotSupported
	m, _ := newModel(be)
	submitSlash(m, "/skills")

	be2 := newStatusBackend()
	be2.items = []StatusItem{{SectionBackend, "kind", "x"}}
	m2, _ := newModel(be2)
	submitSlash(m2, "/mcp")

	be3 := newStatusBackend()
	be3.err = errors.New("boom")
	m3, _ := newModel(be3)
	submitSlash(m3, "/mcp")

	plain, _ := newModel(&fakeBackend{})
	submitSlash(plain, "/skills")
	submitSlash(plain, "/mcp")

	if !strings.Contains(m.Transcript(), "skills: not supported by this backend/server") || m.picker != nil {
		t.Fatalf("skills:\n%s", m.Transcript())
	}
	if !strings.Contains(m2.Transcript(), "no MCP servers are configured") {
		t.Fatalf("empty mcp:\n%s", m2.Transcript())
	}
	if !strings.Contains(m3.Transcript(), "mcp: boom") {
		t.Fatalf("failing mcp:\n%s", m3.Transcript())
	}
	if !strings.Contains(plain.Transcript(), "/skills: not supported") || !strings.Contains(plain.Transcript(), "/mcp: this backend does not report MCP servers") {
		t.Fatalf("plain:\n%s", plain.Transcript())
	}
}

func TestInfoDialogWithNothingToShowIsANotice(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	m.openInfo("Skills", nil)

	if m.picker != nil || !strings.Contains(m.Transcript(), "skills: nothing to show") {
		t.Fatal(m.Transcript())
	}
}

func TestURLHostOfAnUnparsableAddressDoesNotLeakIt(t *testing.T) {
	if got := urlHost("not a url with secret"); got != "(unparsable address)" {
		t.Fatalf("host %q", got)
	}
}

// ---- cost ----

func TestCostBeforeAnyRunAndContextBeforeAnyUsageSaySo(t *testing.T) {
	m, _ := newInfoModel(nil)

	submitSlash(m, "/cost")
	submitSlash(m, "/context")

	tr := m.Transcript()
	if !strings.Contains(tr, "cost: no run yet") || !strings.Contains(tr, "context: nothing reported yet") {
		t.Fatalf("transcript:\n%s", tr)
	}
}

func TestContextWithAnUnknownWindowShowsTheUsageOnly(t *testing.T) {
	m, _ := newInfoModel(nil)
	m.inp.sb.ctx = 4200

	submitSlash(m, "/context")

	if !strings.Contains(m.Transcript(), "context: 4.2k tokens in use (window unknown)") {
		t.Fatal(m.Transcript())
	}
}

func TestPriceFormatsRoundSensiblyAndFreeModelsAreFree(t *testing.T) {
	cases := map[float64]string{0.0004: "$0.0004", 0.0456: "$0.046", 12.345: "$12.35"}
	for in, want := range cases {
		if got := fmtUSD(in); got != want {
			t.Errorf("fmtUSD(%v) = %q, want %q", in, got, want)
		}
	}
	if got := fmtMtok(0); got != "$0" {
		t.Errorf("fmtMtok(0) = %q", got)
	}
	if got := fmtMtok(1.25e-6); got != "$1.25" {
		t.Errorf("fmtMtok = %q", got)
	}
	free := client.ModelInfo{Priced: true}
	if got := priceLine(free); got != "free" {
		t.Errorf("free model: %q", got)
	}
	if got := priceLine(client.ModelInfo{InputPerToken: 1e-6}); got != "" {
		t.Errorf("unpriced model: %q", got)
	}
}

func TestRunErrorWithoutUsageBooksTheLastReportedUsage(t *testing.T) {
	m, _ := newInfoModel(map[string]client.ModelInfo{"gpt-test": priced()})
	startRun(t, m, "go")
	feed(t, m, usageEvent(2000, 200, 0, 2000), agui.RunErrorEvent("boom", "overloaded"))

	submitSlash(m, "/cost")

	if tr := m.Transcript(); !strings.Contains(tr, "run 1 · gpt-test · in 2.0k out 200") {
		t.Fatalf("transcript:\n%s", tr)
	}
}

func TestContextBarColoursAndSidebarSectionsRender(t *testing.T) {
	m, _ := newInfoModel(map[string]client.ModelInfo{"gpt-test": priced()})
	startRun(t, m, "go")
	feed(t, m,
		agui.StateSnapshotEvent(map[string]any{"plan": []map[string]any{
			{"id": "1", "title": "write", "status": "completed"},
			{"id": "2", "title": "test", "status": "in_progress"},
			{"id": "3", "title": "ship", "status": "weird"}}}),
		agui.CustomEvent(agui.CustomTitle, map[string]any{"title": "Fix build"}),
		usageEvent(10, 10, 0, 195_000))
	feed(t, m, toolEvents("c1", "edit_file", `{"path":"a.go","oldText":"a","newText":"b"}`, `{"path":"a.go","replacements":1}`, false)...)
	feed(t, m, toolEvents("c2", "write_file", `{"path":"a.go"}`, "ok", false)...)
	feed(t, m, toolEvents("c3", "edit_file", `{"path":"bad.go"}`, "nope", true)...)

	got := m.renderSidebar(sidebarWidth, 40)

	for _, want := range []string{"Fix build", "☑ write", "◐ test", "☐ ship", "Modified files", "a.go", "window 195.0k / 200k (97%)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sidebar lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "a.go") != 1 || strings.Contains(got, "bad.go") {
		t.Fatalf("touched files must be unique and skip failed edits:\n%s", got)
	}
}

func TestSidebarTruncatesToItsHeightAndShowsTheThreadAndMode(t *testing.T) {
	be := newExt()
	be.cur = "thread-42"
	m, _ := newModel(be)

	got := m.renderSidebar(sidebarWidth, 3)

	lines := strings.Split(got, "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], "…") {
		t.Fatalf("not cut to 3 rows:\n%s", got)
	}
	full := m.renderSidebar(sidebarWidth, 30)
	if !strings.Contains(full, "thread-42") || !strings.Contains(full, "gpt-test · build") {
		t.Fatalf("sidebar:\n%s", full)
	}
}

func TestSidebarToggleNeedsWideEnoughTerminal(t *testing.T) {
	m, _ := newModel(&fakeBackend{}) // 80 columns
	press(m, tea.KeyCtrlB)
	narrow := m.Transcript()
	update(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	auto := m.sidebarCols()
	typeText(m, "/sidebar")
	press(m, tea.KeyEnter)
	off := m.sidebarCols()

	if !strings.Contains(narrow, "the side panel needs 100 columns") || auto == 0 || off != 0 {
		t.Fatalf("narrow:\n%s\nauto %d off %d", narrow, auto, off)
	}
}

func TestUsageBarFillsInProportionAndIsClamped(t *testing.T) {
	if got := usageBar(50, 10); strings.Count(got, "█") != 5 || strings.Count(got, "░") != 5 {
		t.Fatalf("50%%: %q", got)
	}
	if got := usageBar(150, 10); strings.Count(got, "█") != 10 {
		t.Fatalf("over 100%%: %q", got)
	}
	if got := usageBar(-5, 10); strings.Count(got, "█") != 0 {
		t.Fatalf("negative: %q", got)
	}
}

// ---- model picker state ----

func TestFavoriteToggleTwiceRemovesItAndAFailedSaveIsAnotice(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "file")
	os.WriteFile(blocked, []byte("x"), 0o644)
	m := New(newExt(), Options{StaticCursor: true, StateDir: filepath.Join(blocked, "state")}) // not a directory
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	m.toggleFavorite("a")
	m.toggleFavorite("a")

	if len(m.ext.favorites) != 0 {
		t.Fatalf("favorites %v", m.ext.favorites)
	}
	if !strings.Contains(m.Transcript(), "could not save favorites") {
		t.Fatalf("a failing state directory must be reported:\n%s", m.Transcript())
	}
}

func TestCycleRecentGoesBackwardFromAnUnlistedModel(t *testing.T) {
	be := newExt()
	be.model = "unlisted"
	m, _ := newModel(be)
	m.ext.recents = []string{"r1", "r2", "r3"}

	press(m, tea.KeyF2)
	forward := be.model
	be.model = "unlisted"
	update(m, tea.KeyMsg{Type: tea.KeyF14})

	if forward != "r1" || be.model != "r3" {
		t.Fatalf("forward %q backward %q", forward, be.model)
	}
}

func TestModelPickerGroupsPlainIdsUnderOtherWhenSomeHaveAProvider(t *testing.T) {
	be := newExt()
	be.models = []string{"openai/gpt", "local"}
	m, _ := newModel(be)

	submitSlash(m, "/model")

	v := m.View()
	if !strings.Contains(v, "── openai") || !strings.Contains(v, "── other") {
		t.Fatalf("groups:\n%s", v)
	}
}

func TestPickerBackspaceNarrowsBackAndSpaceIsPartOfTheFilter(t *testing.T) {
	be := newExt()
	be.models = []string{"alpha one", "beta"}
	m, _ := newModel(be)
	submitSlash(m, "/model")

	typeText(m, "alp")
	narrow := strings.Contains(m.View(), "beta")
	press(m, tea.KeyBackspace)
	press(m, tea.KeyBackspace)
	press(m, tea.KeyBackspace)
	press(m, tea.KeyBackspace) // below empty: stays empty
	wide := strings.Contains(m.View(), "beta")
	typeText(m, "a")
	update(m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	typeText(m, "o")

	if narrow || !wide || m.picker.filter != "a o" || !strings.Contains(m.View(), "alpha one") {
		t.Fatalf("narrow %v wide %v filter %q", narrow, wide, m.picker.filter)
	}
}

func TestSessionPickerRenameCancelAndUntitledAndCurrentDeleteWarning(t *testing.T) {
	be := newExt()
	be.sessions = []client.Session{{ID: "t1", Title: ""}, {ID: "t2", Title: "Docs", Pinned: true}}
	m, _ := newModel(be)
	submitSlash(m, "/sessions")
	pinnedFirst := m.picker.items[0].label

	press(m, tea.KeyDown) // the untitled one (pinned sorts first)
	press(m, tea.KeyCtrlR)
	typeText(m, "new title")
	update(m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	typeText(m, "x")
	press(m, tea.KeyBackspace)
	press(m, tea.KeyEsc)
	cancelled := len(be.XCalls())
	press(m, tea.KeyCtrlR)
	press(m, tea.KeyEnter) // empty title: nothing is sent
	press(m, tea.KeyCtrlD)
	warn := m.View()

	if !strings.HasPrefix(pinnedFirst, "★") || cancelled != 0 || len(be.XCalls()) != 0 {
		t.Fatalf("first %q calls %v", pinnedFirst, be.XCalls())
	}
	if !strings.Contains(warn, "this is the current conversation") {
		t.Fatalf("no warning for the current conversation:\n%s", warn)
	}
}

func TestSessionAdminErrorsAreNotices(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)
	submitSlash(m, "/sessions")
	be.err = errors.New("db locked")

	press(m, tea.KeyCtrlO)

	if !strings.Contains(m.Transcript(), "pin: db locked") {
		t.Fatal(m.Transcript())
	}
}

// ---- themes, tips, notify, state ----

func TestThemeChoiceIsSavedAndReportedWhenTheStateCannotBeWritten(t *testing.T) {
	t.Cleanup(func() { applyTheme(themes[0]) })
	root := t.TempDir()
	blocked := filepath.Join(root, "f")
	os.WriteFile(blocked, []byte("x"), 0o644)
	m := New(newExt(), Options{StaticCursor: true, StateDir: filepath.Join(blocked, "s")})

	m.setTheme(themes[len(themes)-1])

	if !strings.Contains(m.Transcript(), "theme: could not save the choice") {
		t.Fatalf("transcript:\n%s", m.Transcript())
	}
}

func TestThemePickerPreviewsAndEscRestoresTheCurrentTheme(t *testing.T) {
	t.Cleanup(func() { applyTheme(themes[0]) })
	m, _ := newModel(&fakeBackend{})
	before := curTheme

	submitSlash(m, "/themes")
	press(m, tea.KeyDown)
	previewed := curTheme
	press(m, tea.KeyEsc)

	if previewed == before || curTheme != before || m.picker != nil {
		t.Fatalf("before %q previewed %q after esc %q", before, previewed, curTheme)
	}
}

func TestThemePickerEnterKeepsTheChoice(t *testing.T) {
	t.Cleanup(func() { applyTheme(themes[0]) })
	dir := t.TempDir()
	m := New(&fakeBackend{}, Options{StaticCursor: true, StateDir: dir})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	submitSlash(m, "/themes")
	press(m, tea.KeyDown)
	chosen := curTheme
	press(m, tea.KeyEnter)

	again := New(&fakeBackend{}, Options{StaticCursor: true, StateDir: dir})
	_ = again
	if curTheme != chosen || curTheme == themes[0].Name {
		t.Fatalf("theme %q (chosen %q)", curTheme, chosen)
	}
}

func TestUnknownThemeArgumentIsReported(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	submitSlash(m, "/themes neon-pink")

	if !strings.Contains(m.Transcript(), `unknown theme "neon-pink"`) {
		t.Fatal(m.Transcript())
	}
}

func TestTipsAndNotifyChoicesReportWhenTheyCannotBeSaved(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "f")
	os.WriteFile(blocked, []byte("x"), 0o644)
	m := New(newExt(), Options{StaticCursor: true, StateDir: filepath.Join(blocked, "s")})

	submitSlash(m, "/tips")
	submitSlash(m, "/notify")

	tr := m.Transcript()
	if !strings.Contains(tr, "tips: could not save the choice") || !strings.Contains(tr, "notifications: could not save the choice") {
		t.Fatalf("transcript:\n%s", tr)
	}
	if !m.pf.tipsOff || !m.inp.attn.Enabled {
		t.Fatalf("the in-session choice must still apply: tipsOff %v attn %+v", m.pf.tipsOff, m.inp.attn)
	}
}

func TestStateFileSetFailsWithoutWritingWhenTheDirectoryIsBlocked(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "f")
	os.WriteFile(blocked, []byte("x"), 0o644)
	s := newStateFile(filepath.Join(blocked, "d"))

	if err := s.set("k", 1); err == nil {
		t.Fatal("expected an error")
	}
	var v int
	if err := s.get("k", &v); err != nil || v != 0 {
		t.Fatalf("get after failed set: %d %v", v, err)
	}
}

// ---- timeline and stash ----

func TestTimelineAndUserJumpsOnAnEmptyConversationSayNoMessages(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	submitSlash(m, "/timeline")
	m.jumpUser(1)

	if strings.Count(m.Transcript(), "no messages of yours in this conversation yet") != 2 || m.picker != nil {
		t.Fatal(m.Transcript())
	}
}

func TestJumpingToTheNextMessageAfterTheLastReturnsToTheBottom(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	m.add(block{kind: blockUser, text: "question"})
	m.add(block{kind: blockAssistant, text: strings.Repeat("a long answer line\n\n", 60)})
	m.refresh()
	m.jumpUser(0)
	m.vp.ScrollDown(5) // the question is now above the top of the view
	m.inp.sel = 0

	m.jumpUser(1)

	if m.inp.sel != -1 || !m.vp.AtBottom() {
		t.Fatalf("sel %d atBottom %v", m.inp.sel, m.vp.AtBottom())
	}
}

func TestStashRefusesAnEmptyStashAndEmptyInputAndKeepsOnly200(t *testing.T) {
	m, _ := newWS(t, &fakeBackend{}, nil)

	submitSlash(m, "/stash pop")
	submitSlash(m, "/stash list")
	submitSlash(m, "/stash")
	for i := 0; i < maxHistory; i++ {
		m.inp.stash = append(m.inp.stash, "x")
	}
	submitSlash(m, "/stash fresh")

	tr := m.Transcript()
	if strings.Count(tr, "the stash is empty") != 2 || !strings.Contains(tr, "nothing to stash") {
		t.Fatalf("transcript:\n%s", tr)
	}
	if len(m.inp.stash) != maxHistory || m.inp.stash[len(m.inp.stash)-1] != "fresh" {
		t.Fatalf("stash length %d", len(m.inp.stash))
	}
}

func TestHistoryIgnoresArrowsWhileTheInputHasAnUnrelatedDraftOrSeveralLines(t *testing.T) {
	m, _ := newWS(t, &fakeBackend{}, nil)
	m.inp.hist.items = []string{"old"}

	press(m, tea.KeyDown)
	nothing := m.in.Value()
	typeText(m, "draft")
	press(m, tea.KeyUp)

	if nothing != "" || m.in.Value() == "old" {
		t.Fatalf("down on empty gave %q, up over a draft gave %q", nothing, m.in.Value())
	}
}

func TestHistoryWithNothingSentDoesNothingOnUp(t *testing.T) {
	m, _ := newWS(t, &fakeBackend{}, nil)

	press(m, tea.KeyUp)

	if m.in.Value() != "" {
		t.Fatalf("input %q", m.in.Value())
	}
}

// ---- leader and keys ----

func TestLeaderTimeoutDefaultsWhenTheConfigDoesNotSetOne(t *testing.T) {
	km := NewKeymap(TUIConfig{})
	if km.timeout != DefaultLeaderTimeout*time.Millisecond {
		t.Fatalf("timeout %v", km.timeout)
	}
	if km.Leader() != DefaultLeader {
		t.Fatalf("leader %q", km.Leader())
	}
}

func TestNormKeyMapsTheUsualAliases(t *testing.T) {
	for in, want := range map[string]string{" Escape ": "esc", "RETURN": "enter", "PageUp": "pgup", "pagedown": "pgdown", "Ctrl+X": "ctrl+x"} {
		if got := normKey(in); got != want {
			t.Errorf("normKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfigKeybindsRemapSendAndCancelAndUnboundDefaultsStopActing(t *testing.T) {
	be := &fakeBackend{}
	m, _ := newConfigured(t, `{"keybinds": {"send": "ctrl+s", "cancel": "ctrl+k"}}`, "")

	typeText(m, "hello")
	press(m, tea.KeyEnter)
	idle := len(be.Calls())
	press(m, tea.KeyCtrlS)

	if idle != 0 || !m.starting && !m.Running() && m.in.Value() != "" {
		t.Fatalf("enter must no longer send: input %q starting %v", m.in.Value(), m.starting)
	}
}

func TestConfigRejectsBadCursorKeybindsAndStringsWithEscapes(t *testing.T) {
	c := ParseTUIConfig([]byte(`{
		// comment with "quotes"
		"cursor": "block",
		"keybinds": {"send": [], "palette": 5, "nonsense": "x", "quit": "none,"},
		"leader": "  ",
		"theme": 3,
		"note": "a \"quoted // not a comment\" /* nor this */",
	}`))

	all := strings.Join(c.Notices, "\n")
	for _, want := range []string{"cursor must be an object", "keybinds.send has no key", "keybinds.palette must be a key or a list of keys", `unknown keybind "nonsense"`, "leader must be a key", "theme must be a string", `unknown field "note"`} {
		if !strings.Contains(all, want) {
			t.Errorf("no notice %q in:\n%s", want, all)
		}
	}
}

func TestConfigKeybindsMustBeAnObject(t *testing.T) {
	c := ParseTUIConfig([]byte(`{"keybinds": [1], "cursor": {"style": "x", "blinking": 3, "extra": 1}, "attention": 5}`))

	all := strings.Join(c.Notices, "\n")
	for _, want := range []string{"keybinds must be an object", "cursor.style", "cursor.blinking", `"cursor.extra"`, "attention must be an object"} {
		if !strings.Contains(all, want) {
			t.Errorf("no notice %q in:\n%s", want, all)
		}
	}
}

func TestAttentionSoundsMustBeAnObject(t *testing.T) {
	c := ParseTUIConfig([]byte(`{"attention": {"sounds": [1]}}`))

	if !strings.Contains(strings.Join(c.Notices, "\n"), "attention.sounds must be an object") {
		t.Fatalf("notices %v", c.Notices)
	}
}

func TestCtrlAltKeyNamesMatchBubbleTeasAltCtrlSpelling(t *testing.T) {
	m, _ := newConfigured(t, `{"keybinds": {"messages_line_up": "ctrl+alt+k"}}`, "")
	longTranscript(m)
	bottom := m.vp.YOffset

	update(m, tea.KeyMsg{Type: tea.KeyCtrlK, Alt: true})

	if m.vp.YOffset != bottom-1 {
		t.Fatalf("offset %d, want %d", m.vp.YOffset, bottom-1)
	}
}

func TestBareAndDeclaredCommandKeybindRunsTheCommandDirectly(t *testing.T) {
	m, _ := newConfigured(t, `{"keybinds": {"command.tips": "f5", "leader_timeout": "x"}}`, "")

	update(m, tea.KeyMsg{Type: tea.KeyF5})

	if !strings.Contains(m.Transcript(), "tips:") {
		t.Fatalf("transcript:\n%s", m.Transcript())
	}
}

func TestLeaderKeyEscapeAndSecondLeaderCancelWithoutAMessage(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	press(m, tea.KeyCtrlX)
	press(m, tea.KeyCtrlX)

	if m.lead.on || len(m.blocks) != 0 {
		t.Fatalf("leader on %v blocks %d", m.lead.on, len(m.blocks))
	}
}

func TestLeaderExpiryCommandFiresTheSequenceNumberItWasSetFor(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	seq := m.lead.seq

	if cmd == nil || seq == 0 {
		t.Fatalf("cmd %v seq %d", cmd, seq)
	}
}

func TestCommandsWithArgumentsAreCompletedInTheInputInsteadOfRun(t *testing.T) {
	m, _ := newModel(newExt())

	press(m, tea.KeyCtrlP)
	typeText(m, "effort")
	press(m, tea.KeyEnter)

	if m.in.Value() != "/effort " {
		t.Fatalf("input %q", m.in.Value())
	}
}

func TestPaletteOnlyRunsKnownCommands(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	press(m, tea.KeyCtrlP)

	m.picker.pick(m, pickItem{id: "no-such-command"})

	if len(m.blocks) != 0 || m.Quit() {
		t.Fatalf("blocks %d quit %v", len(m.blocks), m.Quit())
	}
}

func TestSlashLookupAcceptsUniquePrefixesOnly(t *testing.T) {
	if c, ok := lookup("qui"); !ok || c.name != "quit" {
		t.Fatalf("unique prefix: %v %v", c.name, ok)
	}
	if _, ok := lookup("s"); ok {
		t.Fatal("an ambiguous prefix must not resolve")
	}
	if _, ok := lookup("zzz"); ok {
		t.Fatal("unknown")
	}
}

func TestToolsAndDetailsCommandsToggleExpansion(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	submitSlash(m, "/tools")
	one := m.expanded
	submitSlash(m, "/details")

	if !one || m.expanded {
		t.Fatalf("after /tools %v, after /details %v", one, m.expanded)
	}
}

func TestUnknownSlashCommandIsReportedAndNotSent(t *testing.T) {
	be := &fakeBackend{}
	m, _ := newModel(be)

	submitSlash(m, "/definitely-not")

	if len(be.Calls()) != 0 || !strings.Contains(m.Transcript(), "unknown command /definitely-not") {
		t.Fatalf("calls %v:\n%s", be.Calls(), m.Transcript())
	}
}

// ---- mouse, focus and window ----

func TestOnlyWheelPressesScrollAndTheInputIsNotTypedInto(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	longTranscript(m)
	at := m.vp.YOffset

	update(m, tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonWheelUp, Type: tea.MouseWheelUp})
	update(m, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Type: tea.MouseLeft})

	if m.vp.YOffset != at || m.in.Value() != "" {
		t.Fatalf("offset %d (was %d) input %q", m.vp.YOffset, at, m.in.Value())
	}
}

func TestSubagentPaneKeysScrollAndCloseAndSwallowEverythingElse(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	exploreRun(t, m)
	feed(t, m, subEv("c1", agui.SubagentEnd, map[string]any{"reportChars": 5}),
		agui.New(agui.ToolCallResult, map[string]any{"toolCallId": "c1", "role": "tool", "content": "report"}))
	typeText(m, "/children")
	press(m, tea.KeyEnter)
	press(m, tea.KeyEnter)
	if !m.sub.open {
		t.Fatal("pane not open")
	}

	press(m, tea.KeyEnd)
	end := m.sub.off
	m.View() // the view clamps the offset to the content
	clamped := m.sub.off
	press(m, tea.KeyHome)
	home := m.sub.off
	press(m, tea.KeyDown)
	down := m.sub.off
	press(m, tea.KeyUp)
	up := m.sub.off
	press(m, tea.KeyPgDown)
	pgdn := m.sub.off
	press(m, tea.KeyPgUp)
	pgup := m.sub.off
	typeText(m, "x") // swallowed
	typed := m.in.Value()
	press(m, tea.KeyEsc)

	if end <= clamped || home != 0 || down != 1 || up != 0 || pgdn <= 0 || pgup != 0 {
		t.Fatalf("offsets end %d clamped %d home %d down %d up %d pgdn %d pgup %d", end, clamped, home, down, up, pgdn, pgup)
	}
	if typed != "" || m.sub.open {
		t.Fatalf("typed %q open %v", typed, m.sub.open)
	}
}

func TestSubagentTranscriptShowsModelDroppedLinesAndRunningState(t *testing.T) {
	tool := ToolView{Name: "explore", Sub: &Subagent{Agent: "explore", Model: "cheap", Dropped: 3, Lines: []SubLine{{Text: "thinking"}, {Tool: SubTool{Name: "read_file", Args: "a.go", Err: true}}}}}

	got := strings.Join(subTranscript(tool, 80), "\n")

	for _, want := range []string{"model cheap", "3 earlier lines not kept", "· thinking", "✗ read_file", "running…"} {
		if !strings.Contains(got, want) {
			t.Fatalf("transcript lacks %q:\n%s", want, got)
		}
	}
}

func TestSubagentFailedEndAndErrorResultAreMarked(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	startRun(t, m, "go")
	feed(t, m, toolEvents("c1", "explore", `{"task":"find it"}`, "boom", true)[:4]...)
	feed(t, m, subEv("c1", agui.SubagentStart, map[string]any{"task": "find it", "model": "cheap"}),
		subEv("c1", agui.SubagentText, map[string]any{"text": "hmm"}),
		subEv("c1", agui.SubagentEnd, map[string]any{"failed": true}),
		agui.New(agui.ToolCallResult, map[string]any{"toolCallId": "c1", "role": "tool", "content": "boom", "metadata": map[string]any{"isError": true}}))

	m.expanded = true
	expanded := m.Transcript()
	typeText(m, "/children")
	press(m, tea.KeyEnter)
	picker := m.View()

	if !strings.Contains(expanded, "✗") || !strings.Contains(expanded, "· hmm") {
		t.Fatalf("expanded:\n%s", expanded)
	}
	if !strings.Contains(picker, "failed") {
		t.Fatalf("picker:\n%s", picker)
	}
}

func TestEnterOnTheSuggestionOfAStashCommandStashesTheDraftNotTheCommandText(t *testing.T) {
	// Arrange
	m, _ := newWS(t, &fakeBackend{}, nil)
	typeText(m, "half a thought")
	m.inp.draft = "half a thought"
	m.in.SetValue("/stas") // the suggestion list is open for /stash

	// Act
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	// Assert
	if len(m.inp.stash) != 1 || m.inp.stash[0] != "half a thought" {
		t.Fatalf("stash %q: the draft must be parked, not the typed command", m.inp.stash)
	}
}

func TestSpaceTypedInTheSessionRenameIsOneSpace(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)
	submitSlash(m, "/sessions")

	press(m, tea.KeyCtrlR)
	for range len("Fix the build") {
		press(m, tea.KeyBackspace)
	}
	typeText(m, "Green")
	update(m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	typeText(m, "build")
	press(m, tea.KeyEnter)

	if got := be.XCalls(); len(got) != 1 || got[0] != "rename:t1:Green build" {
		t.Fatalf("calls %v", got)
	}
}
