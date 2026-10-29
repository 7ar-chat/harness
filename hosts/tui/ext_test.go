package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// extBackend adds history, custom commands, run options and thread admin to richBackend.
type extBackend struct {
	richBackend
	xmu     sync.Mutex
	xcalls  []string
	err     error // returned by every history and command call
	mode    string
	effort  string
	cur     string
	undoRes *client.UndoResult
	compact *client.CompactResult
	forked  *client.Thread
	cmds    []client.Command
	expand  string
}

func (e *extBackend) log(s string) {
	e.xmu.Lock()
	defer e.xmu.Unlock()
	e.xcalls = append(e.xcalls, s)
}
func (e *extBackend) XCalls() []string {
	e.xmu.Lock()
	defer e.xmu.Unlock()
	return append([]string(nil), e.xcalls...)
}

func (e *extBackend) Compact(context.Context) (*client.CompactResult, error) {
	e.log("compact")
	return e.compact, e.err
}
func (e *extBackend) Undo(context.Context) (*client.UndoResult, error) {
	e.log("undo")
	return e.undoRes, e.err
}
func (e *extBackend) Redo(context.Context) (*client.UndoResult, error) {
	e.log("redo")
	return e.undoRes, e.err
}
func (e *extBackend) Fork(_ context.Context, id string) (*client.Thread, error) {
	e.log("fork:" + id)
	return e.forked, e.err
}
func (e *extBackend) Commands(context.Context) ([]client.Command, error) { return e.cmds, e.err }
func (e *extBackend) ExpandCommand(_ context.Context, name, args string) (string, error) {
	e.log("expand:" + name + ":" + args)
	return e.expand, e.err
}
func (e *extBackend) SetMode(m string)            { e.mode = m }
func (e *extBackend) Mode() string                { return e.mode }
func (e *extBackend) SetEffort(s string)          { e.effort = s }
func (e *extBackend) Effort() string              { return e.effort }
func (e *extBackend) Attach(...client.Attachment) {}
func (e *extBackend) CurrentThread() string       { return e.cur }
func (e *extBackend) RenameSession(_ context.Context, id, t string) error {
	e.log("rename:" + id + ":" + t)
	return e.err
}
func (e *extBackend) PinSession(_ context.Context, id string, p bool) error {
	e.log("pin:" + id + ":" + map[bool]string{true: "on", false: "off"}[p])
	for i := range e.sessions {
		if e.sessions[i].ID == id && e.err == nil {
			e.sessions[i].Pinned = p
		}
	}
	return e.err
}
func (e *extBackend) DeleteSession(_ context.Context, id string) error {
	e.log("delete:" + id)
	return e.err
}

func newExt() *extBackend {
	return &extBackend{mode: client.ModeBuild, cur: "t1", richBackend: richBackend{
		sessions: []client.Session{{ID: "t1", Title: "Fix the build", Messages: 3}, {ID: "t2", Title: "Docs", Messages: 1}},
		thread:   &client.Thread{ID: "t1", Title: "Fix the build", Messages: []client.Message{{Role: "user", Content: "kept question"}, {Role: "assistant", Content: "kept answer"}}},
	}}
}

func submitSlash(m *Model, line string) {
	typeText(m, line)
	press(m, tea.KeyEnter)
}

func TestUndoPutsThePromptBackReloadsTheTranscriptAndListsTheFiles(t *testing.T) {
	be := newExt()
	be.undoRes = &client.UndoResult{Prompt: "fix the build", Files: []string{"a.go", "b.go"}, CanRedo: true}
	m, _ := newModel(be)
	m.add(block{kind: blockUser, text: "stale line"})

	submitSlash(m, "/undo")

	if got := m.in.Value(); got != "fix the build" {
		t.Fatalf("input %q", got)
	}
	tr := m.Transcript()
	for _, want := range []string{"kept answer", "2 files changed", "a.go, b.go", "/redo available"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("transcript lacks %q:\n%s", want, tr)
		}
	}
	if strings.Contains(tr, "stale line") || strings.Contains(tr, "resumed:") || be.opened != "t1" {
		t.Fatalf("opened %q:\n%s", be.opened, tr)
	}
}

func TestRedoDoesNotRefillTheInput(t *testing.T) {
	be := newExt()
	be.undoRes = &client.UndoResult{Prompt: "fix the build", CanUndo: true}
	m, _ := newModel(be)

	submitSlash(m, "/redo")

	if m.in.Value() != "" || !strings.Contains(m.Transcript(), "redone (no file changes)") {
		t.Fatalf("input %q:\n%s", m.in.Value(), m.Transcript())
	}
	if got := be.XCalls(); len(got) != 1 || got[0] != "redo" {
		t.Fatalf("calls %v", got)
	}
}

func TestHistoryCommandsAreRefusedDuringARun(t *testing.T) {
	for _, cmd := range []string{"/undo", "/redo", "/compact", "/fork"} {
		be := newExt()
		m, _ := newModel(be)
		startRun(t, m, "hello")

		submitSlash(m, cmd)

		if len(be.XCalls()) != 0 || !strings.Contains(m.Transcript(), "esc cancels it first") {
			t.Fatalf("%s ran during a run: %v\n%s", cmd, be.XCalls(), m.Transcript())
		}
	}
}

func TestHistoryCommandsSayWhenTheBackendOrServerLacksThem(t *testing.T) {
	for _, cmd := range []string{"/undo", "/redo", "/compact", "/fork"} {
		plain, _ := newModel(&richBackend{})
		submitSlash(plain, cmd)
		if !strings.Contains(plain.Transcript(), "not supported by this backend/server") {
			t.Fatalf("%s without the capability:\n%s", cmd, plain.Transcript())
		}

		be := newExt()
		be.err = client.ErrNotSupported
		old, _ := newModel(be)
		submitSlash(old, cmd)
		if !strings.Contains(old.Transcript(), "not supported by this backend/server") || old.in.Value() != "" {
			t.Fatalf("%s on an old server:\n%s", cmd, old.Transcript())
		}
	}
}

func TestHistoryErrorsAreShown(t *testing.T) {
	be := newExt()
	be.err = errors.New("nothing to undo")
	m, _ := newModel(be)

	submitSlash(m, "/undo")

	if !strings.Contains(m.Transcript(), "/undo: nothing to undo") {
		t.Fatal(m.Transcript())
	}
}

func TestCompactReportsTokensBeforeAndAfter(t *testing.T) {
	be := newExt()
	be.compact = &client.CompactResult{Compacted: true, TokensBefore: 12300, TokensAfter: 3100}
	m, _ := newModel(be)

	submitSlash(m, "/summarize") // the alias

	if !strings.Contains(m.Transcript(), "compacted: 12.3k → 3.1k tokens") {
		t.Fatal(m.Transcript())
	}
	be.compact = &client.CompactResult{}
	submitSlash(m, "/compact")
	if !strings.Contains(m.Transcript(), "nothing to compact") {
		t.Fatal(m.Transcript())
	}
}

func TestForkReplaysTheCopy(t *testing.T) {
	be := newExt()
	be.forked = &client.Thread{ID: "t9", Title: "copy"}
	be.thread = &client.Thread{ID: "t9", Title: "copy", Messages: []client.Message{{Role: "user", Content: "original question"}}}
	m, _ := newModel(be)

	submitSlash(m, "/fork")

	tr := m.Transcript()
	if be.opened != "t9" || !strings.Contains(tr, "original question") || !strings.Contains(tr, "forked: copy") {
		t.Fatalf("opened %q:\n%s", be.opened, tr)
	}
}

func TestEffortCommandAndCycleKeyShowInTheStatusLine(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)

	submitSlash(m, "/effort high")
	if be.effort != "high" || !strings.Contains(m.View(), "effort high") {
		t.Fatalf("effort %q:\n%s", be.effort, m.View())
	}
	submitSlash(m, "/effort default")
	if be.effort != "" {
		t.Fatalf("effort %q", be.effort)
	}
	submitSlash(m, "/effort max")
	if !strings.Contains(m.Transcript(), `unknown level "max"`) {
		t.Fatal(m.Transcript())
	}
	var seen []string
	for range 5 {
		press(m, tea.KeyCtrlT)
		seen = append(seen, be.effort)
	}
	if strings.Join(seen, ",") != "low,medium,high,,low" {
		t.Fatalf("cycle %q", seen)
	}
}

func TestEffortIsReportedAsUnsupportedOnAPlainBackend(t *testing.T) {
	m, _ := newModel(&richBackend{})

	submitSlash(m, "/effort low")
	press(m, tea.KeyCtrlT)

	if strings.Count(m.Transcript(), "not supported by this backend/server") != 2 {
		t.Fatal(m.Transcript())
	}
}

func TestTabTogglesPlanAndBuildAndTheModeShowsInStatusAndPrompt(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)
	if v := m.View(); !strings.Contains(v, "BUILD") || !strings.Contains(v, "build │") {
		t.Fatalf("build mode not shown:\n%s", v)
	}

	press(m, tea.KeyTab)
	if be.mode != client.ModePlan {
		t.Fatalf("mode %q", be.mode)
	}
	if v := m.View(); !strings.Contains(v, "PLAN") || !strings.Contains(v, "plan │") {
		t.Fatalf("plan mode not shown:\n%s", v)
	}
	press(m, tea.KeyShiftTab)
	if be.mode != client.ModeBuild {
		t.Fatalf("mode %q", be.mode)
	}
	submitSlash(m, "/plan")
	if be.mode != client.ModePlan {
		t.Fatalf("/plan left mode %q", be.mode)
	}
	submitSlash(m, "/build")
	if be.mode != client.ModeBuild {
		t.Fatalf("/build left mode %q", be.mode)
	}
}

func TestTabCompletesSlashSuggestionsBeforeTogglingTheMode(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)

	typeText(m, "/pla")
	press(m, tea.KeyTab)

	if m.in.Value() != "/plan " || be.mode != client.ModeBuild {
		t.Fatalf("input %q mode %q", m.in.Value(), be.mode)
	}
}

func TestTabIsLeftToTheInputOnABackendWithoutModes(t *testing.T) {
	m, _ := newModel(&richBackend{})

	press(m, tea.KeyTab)

	if strings.Contains(m.Transcript(), "mode") || strings.Contains(m.View(), "BUILD") {
		t.Fatalf("a mode appeared:\n%s", m.View())
	}
}

func TestSessionPickerRenamesAfterAnInlineInput(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)
	submitSlash(m, "/sessions")

	press(m, tea.KeyCtrlR)
	for range len("Fix the build") {
		press(m, tea.KeyBackspace)
	}
	typeText(m, "Green build")
	if v := m.View(); !strings.Contains(v, "rename: Green build") || !strings.Contains(v, "ctrl+r rename") {
		t.Fatalf("no inline input or hint:\n%s", v)
	}
	press(m, tea.KeyEnter)

	if got := be.XCalls(); len(got) != 1 || got[0] != "rename:t1:Green build" {
		t.Fatalf("calls %v", got)
	}
	if m.picker == nil || !strings.Contains(m.Transcript(), "renamed: Green build") {
		t.Fatalf("picker open %v:\n%s", m.picker != nil, m.Transcript())
	}
}

func TestSessionPickerDeletesOnlyAfterConfirmation(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)
	submitSlash(m, "/sessions")

	press(m, tea.KeyDown)
	press(m, tea.KeyCtrlD)
	if v := m.View(); !strings.Contains(v, `delete "Docs"?`) {
		t.Fatalf("no confirmation:\n%s", v)
	}
	typeText(m, "n")
	if len(be.XCalls()) != 0 {
		t.Fatalf("deleted without a yes: %v", be.XCalls())
	}
	press(m, tea.KeyCtrlD)
	typeText(m, "y")

	if got := be.XCalls(); len(got) != 1 || got[0] != "delete:t2" {
		t.Fatalf("calls %v", got)
	}
}

func TestSessionPickerPinsAndUnpins(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)
	submitSlash(m, "/sessions")

	press(m, tea.KeyCtrlO)
	if v := m.View(); !strings.Contains(v, "★ Fix the build") {
		t.Fatalf("not marked pinned:\n%s", v)
	}
	press(m, tea.KeyCtrlO)

	if got := be.XCalls(); strings.Join(got, ",") != "pin:t1:on,pin:t1:off" {
		t.Fatalf("calls %v", got)
	}
}

func TestSessionPickerAdminOnAnOldServerSaysSo(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)
	submitSlash(m, "/sessions")
	be.err = client.ErrNotSupported

	press(m, tea.KeyCtrlO)

	if !strings.Contains(m.Transcript(), "not supported by this backend/server") {
		t.Fatal(m.Transcript())
	}
}

func TestModelPickerListsFavoritesThenRecentsThenProviderGroups(t *testing.T) {
	be := newExt()
	be.models = []string{"openai/gpt-5", "anthropic/sonnet", "openai/mini", "anthropic/haiku"}
	be.model = "openai/gpt-5"
	m, _ := newModel(be)
	m.ext.favorites = []string{"openai/mini"}
	m.ext.recents = []string{"anthropic/haiku", "openai/mini"}

	submitSlash(m, "/model")

	var ids []string
	for _, it := range m.picker.items {
		ids = append(ids, it.id)
	}
	if got := strings.Join(ids, " "); got != "openai/mini anthropic/haiku openai/gpt-5 anthropic/sonnet" {
		t.Fatalf("order %q", got)
	}
	v := m.View()
	for _, want := range []string{"── favorites", "★ openai/mini", "── recent", "── openai", "── anthropic"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view lacks %q:\n%s", want, v)
		}
	}
}

func TestModelPickerFavoriteToggleAndRecentsPersistWithoutLosingOtherKeys(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	be := newExt()
	be.models = []string{"a", "b", "c"}
	be.model = "a"
	m := New(be, Options{StaticCursor: true, StateDir: dir})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	submitSlash(m, "/model")
	press(m, tea.KeyDown)
	press(m, tea.KeyCtrlF) // favorite b
	if m.picker.items[0].id != "b" || !strings.Contains(m.picker.items[0].label, "★") {
		t.Fatalf("b not first: %+v", m.picker.items)
	}
	press(m, tea.KeyEnter) // use b

	var st struct {
		Theme     string   `json:"theme"`
		Favorites []string `json:"favorites"`
		Recents   []string `json:"recents"`
	}
	data, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.Theme != "dark" || len(st.Favorites) != 1 || st.Favorites[0] != "b" || len(st.Recents) != 1 || st.Recents[0] != "b" {
		t.Fatalf("state %s", data)
	}

	again := New(be, Options{StaticCursor: true, StateDir: dir}) // a new session reads them back
	if len(again.ext.favorites) != 1 || len(again.ext.recents) != 1 {
		t.Fatalf("not reloaded: %+v", again.ext)
	}
}

func TestRecentModelsAreCappedAtFiveAndF2CyclesThem(t *testing.T) {
	be := newExt()
	m, _ := newModel(be)
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5", "m6"} {
		submitSlash(m, "/model "+id)
	}
	if len(m.ext.recents) != 5 || m.ext.recents[0] != "m6" || m.ext.recents[4] != "m2" {
		t.Fatalf("recents %v", m.ext.recents)
	}

	press(m, tea.KeyF2)
	if be.model != "m5" {
		t.Fatalf("f2 chose %q", be.model)
	}
	press(m, tea.KeyF2)
	if be.model != "m4" {
		t.Fatalf("f2 chose %q", be.model)
	}
	update(m, tea.KeyMsg{Type: tea.KeyF14}) // shift+f2 as a terminal sends it
	if be.model != "m5" {
		t.Fatalf("shift+f2 chose %q", be.model)
	}
}

func TestF2WithoutRecentsSaysSo(t *testing.T) {
	m, _ := newModel(newExt())

	press(m, tea.KeyF2)

	if !strings.Contains(m.Transcript(), "no other recent model") {
		t.Fatal(m.Transcript())
	}
}

func TestStateFileKeepsOtherKeysAndIsInertWithoutADirectory(t *testing.T) {
	none := newStateFile("")
	if err := none.set("k", 1); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "nested")
	s := newStateFile(dir)
	if err := s.set("theme", "light"); err != nil {
		t.Fatal(err)
	}
	if err := s.set("favorites", []string{"x"}); err != nil {
		t.Fatal(err)
	}
	var theme string
	var favs []string
	_ = s.get("theme", &theme)
	_ = s.get("favorites", &favs)
	if theme != "light" || len(favs) != 1 {
		t.Fatalf("theme %q favorites %v", theme, favs)
	}
}

// customSetup returns a model on a backend with custom commands, after Init loaded them.
func customSetup(t *testing.T, be *extBackend) *Model {
	t.Helper()
	t.Cleanup(func() { setUserCommands(nil) })
	m, _ := newModel(be)
	for _, r := range msgs(m.Init()) {
		update(m, r)
	}
	return m
}

func TestCustomCommandsAreLoadedAtStartupAndListedInCompletionAndHelp(t *testing.T) {
	be := newExt()
	be.cmds = []client.Command{
		{Name: "review", Description: "review a file", Source: "project", ArgHint: "<file>"},
		{Name: "undo", Description: "shadowed", Source: "user"},
		{Name: "bad_name", Description: "untypeable"},
	}
	m := customSetup(t, be)

	typeText(m, "/rev")
	if v := m.View(); !strings.Contains(v, "/review <file>") || !strings.Contains(v, "review a file [project]") {
		t.Fatalf("not in completion:\n%s", v)
	}
	m.in.Reset()
	submitSlash(m, "/help")
	tr := m.Transcript()
	for _, want := range []string{"/review <file>", "review a file [project]", "undo (a builtin has this name)", "bad_name"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("help lacks %q:\n%s", want, tr)
		}
	}
	if len(be.XCalls()) != 0 {
		t.Fatalf("shadowed command was expanded: %v", be.XCalls())
	}
}

func TestRunningACustomCommandSendsTheExpandedPromptShownAsTheCommand(t *testing.T) {
	be := newExt()
	be.cmds = []client.Command{{Name: "review", Description: "review a file"}}
	be.expand = "Please review src/x.go carefully"
	m := customSetup(t, be)

	submitSlash(m, "/review src/x.go")

	if got := be.XCalls(); len(got) != 1 || got[0] != "expand:review:src/x.go" {
		t.Fatalf("calls %v", got)
	}
	if calls := be.Calls(); len(calls) != 1 || calls[0] != "send:Please review src/x.go carefully" {
		t.Fatalf("sent %v", calls)
	}
	tr := m.Transcript()
	if !strings.Contains(tr, "/review src/x.go") || strings.Contains(tr, "carefully") || !m.Running() {
		t.Fatalf("running %v:\n%s", m.Running(), tr)
	}
}

func TestCustomCommandIsRefusedDuringARunAndReportsExpansionErrors(t *testing.T) {
	be := newExt()
	be.cmds = []client.Command{{Name: "review"}}
	m := customSetup(t, be)
	startRun(t, m, "hello")
	submitSlash(m, "/review x")
	if len(be.XCalls()) != 0 || !strings.Contains(m.Transcript(), "esc cancels it first") {
		t.Fatalf("ran during a run: %v", be.XCalls())
	}

	be2 := newExt()
	be2.cmds = []client.Command{{Name: "review"}}
	m2 := customSetup(t, be2)
	be2.err = client.ErrNotSupported
	submitSlash(m2, "/review x")
	if !strings.Contains(m2.Transcript(), "not supported by this backend/server") || len(be2.Calls()) != 0 {
		t.Fatalf("sent %v:\n%s", be2.Calls(), m2.Transcript())
	}
}

func TestReloadRefetchesTheCommandsAndAnOldServerLeavesNoneRegistered(t *testing.T) {
	be := newExt()
	m := customSetup(t, be)
	be.cmds = []client.Command{{Name: "deploy", Description: "ship it"}}

	submitSlash(m, "/reload")
	if !strings.Contains(m.Transcript(), "1 custom command loaded") {
		t.Fatal(m.Transcript())
	}
	if _, ok := lookup("deploy"); !ok {
		t.Fatal("deploy not registered")
	}
	be.err = client.ErrNotSupported
	submitSlash(m, "/reload")
	if !strings.Contains(m.Transcript(), "/reload: not supported by this backend/server") {
		t.Fatal(m.Transcript())
	}

	plain, _ := newModel(&richBackend{})
	if cmds := plain.initCmds(); cmds != nil {
		t.Fatal("a backend without commands asked for them")
	}
}

type fakeConnector struct {
	providers []ProviderInfo
	saved     map[string]string
	err       error
	restart   bool
}

func (f *fakeConnector) Providers() []ProviderInfo { return f.providers }
func (f *fakeConnector) Save(p, k string) error {
	if f.err != nil {
		return f.err
	}
	if f.saved == nil {
		f.saved = map[string]string{}
	}
	f.saved[p] = k
	return nil
}

type restartConnector struct{ fakeConnector }

func (r *restartConnector) RestartNeeded() bool { return true }

func newConnectModel(c Connector) *Model {
	m := New(newExt(), Options{StaticCursor: true, Connector: c})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m
}

func TestConnectListsProvidersAsksForAMaskedKeyAndSaves(t *testing.T) {
	fc := &fakeConnector{providers: []ProviderInfo{{Name: "openai", BaseURL: "https://api.openai.com/v1", HasKey: true}, {Name: "opencode", BaseURL: "https://x.test"}}}
	m := newConnectModel(fc)

	submitSlash(m, "/connect")
	if v := m.View(); !strings.Contains(v, "openai") || !strings.Contains(v, "key stored") || !strings.Contains(v, "no key") || !strings.Contains(v, "https://x.test") {
		t.Fatalf("providers not listed:\n%s", v)
	}
	press(m, tea.KeyDown)
	press(m, tea.KeyEnter)
	typeText(m, "sk-secret-123")

	v := m.View()
	if strings.Contains(v, "sk-secret") || !strings.Contains(v, "•••••••••••••") || !strings.Contains(v, "API key for opencode") {
		t.Fatalf("key not masked:\n%s", v)
	}
	press(m, tea.KeyEnter)

	if fc.saved["opencode"] != "sk-secret-123" || m.picker != nil {
		t.Fatalf("saved %v picker %v", fc.saved, m.picker != nil)
	}
	if tr := m.Transcript(); strings.Contains(tr, "sk-secret") || !strings.Contains(tr, "new runs use it") {
		t.Fatalf("transcript:\n%s", tr)
	}
}

func TestConnectAcceptsANewProviderNameAndSaysWhenARestartIsNeeded(t *testing.T) {
	rc := &restartConnector{}
	m := newConnectModel(rc)

	submitSlash(m, "/connect")
	typeText(m, "mylab")
	press(m, tea.KeyEnter)
	typeText(m, "k-1")
	press(m, tea.KeyEnter)

	if rc.saved["mylab"] != "k-1" || !strings.Contains(m.Transcript(), "restart to apply") {
		t.Fatalf("saved %v:\n%s", rc.saved, m.Transcript())
	}
}

func TestConnectEscCancelsWithoutSavingAndErrorsNeverShowTheKey(t *testing.T) {
	fc := &fakeConnector{providers: []ProviderInfo{{Name: "p"}}}
	m := newConnectModel(fc)
	submitSlash(m, "/connect")
	press(m, tea.KeyEnter)
	typeText(m, "abc")
	press(m, tea.KeyEsc)
	if m.picker != nil || len(fc.saved) != 0 {
		t.Fatalf("esc did not cancel: %v", fc.saved)
	}

	fc.err = errors.New("auth.json is readable by other users")
	submitSlash(m, "/connect p")
	typeText(m, "abc")
	press(m, tea.KeyEnter)
	if tr := m.Transcript(); !strings.Contains(tr, "readable by other users") || strings.Contains(tr, "abc") {
		t.Fatalf("transcript:\n%s", tr)
	}
}

func TestConnectWithoutAConnectorPointsToTheAuthCommand(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	submitSlash(m, "/connect")

	if !strings.Contains(m.Transcript(), "harness auth login") || m.picker != nil {
		t.Fatal(m.Transcript())
	}
}

func TestStatusLineWithoutRunOptionsIsUnchanged(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	feed(t, m, agui.RunFinishedEvent("t", "run1", "success", nil))

	if v := m.View(); strings.Contains(v, "BUILD") || strings.Contains(v, "effort") {
		t.Fatalf("run options leaked into the status line:\n%s", v)
	}
}
