package tui

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// optsBackend is a fakeBackend that takes attachments and knows its sidebar info.
type optsBackend struct {
	fakeBackend
	atts []client.Attachment
}

func (o *optsBackend) SetMode(string)   {}
func (o *optsBackend) Mode() string     { return "build" }
func (o *optsBackend) SetEffort(string) {}
func (o *optsBackend) Effort() string   { return "" }
func (o *optsBackend) Attach(a ...client.Attachment) {
	o.atts = append(o.atts, a...)
}
func (o *optsBackend) SidebarInfo() SidebarInfo {
	return SidebarInfo{MCP: []string{"fs"}, Skills: []string{"review"}}
}

// newWS is a model on a workspace directory, with the state directory inside it.
func newWS(t *testing.T, be Backend, files map[string]string) (*Model, string) {
	t.Helper()
	root := t.TempDir()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := New(be, Options{Now: func() time.Time { return t0 }, StaticCursor: true, Workspace: root, StateDir: filepath.Join(root, ".state")})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m, root
}

func lastSend(be *fakeBackend) string {
	cs := be.Calls()
	for i := len(cs) - 1; i >= 0; i-- {
		if strings.HasPrefix(cs[i], "send:") {
			return strings.TrimPrefix(cs[i], "send:")
		}
	}
	return ""
}

func TestResolveInWorkspaceRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(root, "ok.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("no symlinks")
	}
	for _, p := range []string{"../" + filepath.Base(outside) + "/secret.txt", "link/secret.txt", filepath.Join(outside, "secret.txt")} {
		if _, err := resolveInWorkspace(root, p); err == nil {
			t.Errorf("%s was accepted", p)
		}
	}
	if _, err := resolveInWorkspace(root, "ok.txt"); err != nil {
		t.Errorf("ok.txt: %v", err)
	}
}

func TestAtMenuCompletesAFileAndSendAttachesItsContent(t *testing.T) {
	be := &fakeBackend{}
	m, _ := newWS(t, be, map[string]string{"cmd/main.go": "package main\n", "README.md": "# hi\n", "node_modules/x/a.js": "x"})
	m.inp.files.done(filesMsg{listFiles(m.ctx, m.inp.workspace)})
	typeText(m, "look at @main")
	if got := m.View(); !strings.Contains(got, "@cmd/main.go") {
		t.Fatalf("no menu:\n%s", got)
	}
	if strings.Contains(m.View(), "node_modules") {
		t.Fatal("node_modules is indexed")
	}
	press(m, tea.KeyEnter) // inserts the path
	if got := m.in.Value(); got != "look at @cmd/main.go " {
		t.Fatalf("input = %q", got)
	}
	press(m, tea.KeyEnter) // sends
	sent := lastSend(be)
	if !strings.Contains(sent, "Called Read on cmd/main.go") || !strings.Contains(sent, "package main") {
		t.Fatalf("sent = %q", sent)
	}
	if !strings.Contains(m.Transcript(), "Called Read on cmd/main.go") {
		t.Fatalf("no notice:\n%s", m.Transcript())
	}
}

func TestMentionOutsideTheWorkspaceIsNotAttached(t *testing.T) {
	be := &fakeBackend{}
	m, _ := newWS(t, be, nil)
	secret := filepath.Join(t.TempDir(), "s.txt")
	os.WriteFile(secret, []byte("TOPSECRET"), 0o644)
	startRun(t, m, "read @"+secret+" and @../x")
	if strings.Contains(lastSend(be), "TOPSECRET") {
		t.Fatal("a file outside the workspace was attached")
	}
	if !strings.Contains(m.Transcript(), "outside the workspace") {
		t.Fatalf("no refusal notice:\n%s", m.Transcript())
	}
}

func TestMentionIsCappedAndSaysSo(t *testing.T) {
	be := &fakeBackend{}
	m, _ := newWS(t, be, map[string]string{"big.txt": strings.Repeat("a", maxMentionFile*2)})
	startRun(t, m, "see @big.txt")
	sent := lastSend(be)
	if len(sent) > maxMentionFile+1000 || !strings.Contains(sent, "truncated to the first") {
		t.Fatalf("not capped: %d bytes", len(sent))
	}
}

func TestMentionsAreOffWithoutAWorkspace(t *testing.T) {
	be := &fakeBackend{}
	m, _ := newModel(be)
	startRun(t, m, "see @go.mod")
	if got := lastSend(be); got != "see @go.mod" {
		t.Fatalf("sent = %q", got)
	}
}

func TestListFilesHonoursGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret.txt\n"), 0o644)
	os.WriteFile(filepath.Join(root, "secret.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Skip("git init failed")
	}
	got := strings.Join(listFiles(context.Background(), root), ",")
	if strings.Contains(got, "secret.txt") || !strings.Contains(got, "a.txt") {
		t.Fatalf("files = %s", got)
	}
}

func TestBangRunsTheCommandAndFeedsTheNextMessage(t *testing.T) {
	be := &fakeBackend{}
	m, root := newWS(t, be, map[string]string{"f.txt": "x"})
	typeText(m, "!ls")
	press(m, tea.KeyEnter)
	if be.Calls() != nil {
		t.Fatal("a shell command was sent to the model")
	}
	tr := m.Transcript()
	if !strings.Contains(tr, "$ ls") || !strings.Contains(tr, "f.txt") {
		t.Fatalf("no output:\n%s", tr)
	}
	startRun(t, m, "why?")
	sent := lastSend(be)
	if !strings.HasPrefix(sent, "I ran `ls` and got:") || !strings.HasSuffix(sent, "why?") {
		t.Fatalf("sent = %q", sent)
	}
	_ = root
	m.inp.shellCtx = nil
}

func TestBangIsDisabledWithoutAWorkspace(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	typeText(m, "!echo hi")
	press(m, tea.KeyEnter)
	if !strings.Contains(m.Transcript(), "local workspace") || m.starting {
		t.Fatalf("transcript:\n%s", m.Transcript())
	}
}

func TestCapOutputKeepsHeadAndTail(t *testing.T) {
	s := strings.Repeat("a", 100) + strings.Repeat("z", 100)
	got := capOutput(s, 50)
	if !strings.HasPrefix(got, strings.Repeat("a", 25)) || !strings.HasSuffix(got, strings.Repeat("z", 25)) || !strings.Contains(got, "bytes omitted") {
		t.Fatalf("got %q", got)
	}
}

func TestHistoryWalksPreviousMessagesAndPersists(t *testing.T) {
	be := &fakeBackend{}
	m, root := newWS(t, be, nil)
	for _, s := range []string{"first", "second"} {
		m.starting, m.running = false, false
		typeText(m, s)
		press(m, tea.KeyEnter)
	}
	press(m, tea.KeyUp)
	if m.in.Value() != "second" {
		t.Fatalf("up: %q", m.in.Value())
	}
	press(m, tea.KeyUp)
	if m.in.Value() != "first" {
		t.Fatalf("up up: %q", m.in.Value())
	}
	press(m, tea.KeyDown)
	press(m, tea.KeyDown)
	if m.in.Value() != "" {
		t.Fatalf("down down: %q", m.in.Value())
	}
	m2 := New(&fakeBackend{}, Options{StaticCursor: true, StateDir: filepath.Join(root, ".state")})
	if got := m2.inp.hist.items; len(got) != 2 || got[1] != "second" {
		t.Fatalf("reloaded = %v", got)
	}
}

func TestHistoryKeepsTheLast200(t *testing.T) {
	dir := t.TempDir()
	var h history
	for i := 0; i < 250; i++ {
		h.add(strings.Repeat("x", i%7+1)+string(rune('a'+i%26))+string(rune('A'+i/26)), dir)
	}
	if got := loadHistory(dir).items; len(got) != maxHistory {
		t.Fatalf("len = %d", len(got))
	}
}

func TestStashParksAndRestoresTheInput(t *testing.T) {
	m, root := newWS(t, &fakeBackend{}, nil)
	typeText(m, "half a thou")
	typeText(m, "ght")
	m.in.SetValue("/stash")
	m.inp.draft = "half a thought"
	press(m, tea.KeyEnter)
	if m.in.Value() != "" || len(m.inp.stash) != 1 {
		t.Fatalf("input %q stash %v", m.in.Value(), m.inp.stash)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".state", "tui-stash")); !strings.Contains(string(b), "half a thought") {
		t.Fatalf("not persisted: %q", b)
	}
	typeText(m, "/stash pop")
	press(m, tea.KeyEnter)
	if m.in.Value() != "half a thought" || len(m.inp.stash) != 0 {
		t.Fatalf("pop: %q %v", m.in.Value(), m.inp.stash)
	}
}

func TestStashListOpensAPicker(t *testing.T) {
	m, _ := newWS(t, &fakeBackend{}, nil)
	m.inp.stash = []string{"one", "two"}
	typeText(m, "/stash list")
	press(m, tea.KeyEnter)
	if m.picker == nil {
		t.Fatal("no picker")
	}
	press(m, tea.KeyDown) // "one" (newest first: two, one)
	press(m, tea.KeyEnter)
	if m.in.Value() != "one" || len(m.inp.stash) != 1 {
		t.Fatalf("picked %q %v", m.in.Value(), m.inp.stash)
	}
}

func pngFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("\x89PNG\r\n\x1a\nfake"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPastedImagePathBecomesAChipAndIsAttachedOnSend(t *testing.T) {
	be := &optsBackend{}
	m, root := newWS(t, be, nil)
	p := pngFile(t, root, "shot.png")
	update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("'" + p + "'"), Paste: true})
	if !strings.Contains(m.View(), "[image: shot.png]") || m.in.Value() != "" {
		t.Fatalf("view:\n%s", m.View())
	}
	typeText(m, "what is this")
	press(m, tea.KeyEnter)
	if len(be.atts) != 1 || be.atts[0].MIME != "image/png" || be.atts[0].Name != "shot.png" {
		t.Fatalf("atts = %+v", be.atts)
	}
	if strings.Contains(m.View(), "[image:") {
		t.Fatal("chip survived the send")
	}
}

func TestBackspaceOnEmptyInputRemovesTheLastChip(t *testing.T) {
	be := &optsBackend{}
	m, root := newWS(t, be, nil)
	typeText(m, "/image "+pngFile(t, root, "a.png"))
	press(m, tea.KeyEnter)
	if len(m.inp.atts) != 1 {
		t.Fatalf("atts = %d", len(m.inp.atts))
	}
	press(m, tea.KeyBackspace)
	if len(m.inp.atts) != 0 {
		t.Fatal("chip not removed")
	}
}

func TestImageNeedsABackendThatTakesAttachments(t *testing.T) {
	m, root := newWS(t, &fakeBackend{}, nil)
	typeText(m, "/image "+pngFile(t, root, "a.png"))
	press(m, tea.KeyEnter)
	if len(m.inp.atts) != 0 || !strings.Contains(m.Transcript(), "cannot take attachments") {
		t.Fatalf("transcript:\n%s", m.Transcript())
	}
}

func TestLongPasteIsSummarisedInTheInputAndSentInFull(t *testing.T) {
	be := &fakeBackend{}
	m, _ := newWS(t, be, nil)
	long := strings.Repeat("line\n", 15) + "end"
	update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(long), Paste: true})
	if m.in.Value() != "[pasted 16 lines]" {
		t.Fatalf("input = %q", m.in.Value())
	}
	press(m, tea.KeyEnter)
	if got := lastSend(be); got != long {
		t.Fatalf("sent = %q", got)
	}
	update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("short"), Paste: true})
	if m.in.Value() != "short" {
		t.Fatalf("short paste = %q", m.in.Value())
	}
}

func TestExportMarkdown(t *testing.T) {
	bl := []block{
		{kind: blockUser, text: "fix it"},
		{kind: blockAssistant, text: "On it."},
		{kind: blockTool, tool: ToolView{Name: "read_file", Args: `{"path":"a.go"}`, Result: strings.Repeat("l\n", 30), Done: true}},
	}
	got := exportMarkdown("My chat", bl, t0)
	for _, want := range []string{"# My chat", "## User\n\nfix it", "## Assistant\n\nOn it.", "### Tool: read_file", "`a.go`", "(truncated)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Count(got, "\nl\n") > exportResultLines {
		t.Error("result not truncated")
	}
}

func TestEditorLoadsTheEditedFileIntoTheInput(t *testing.T) {
	m, _ := newWS(t, &fakeBackend{}, nil)
	f := filepath.Join(t.TempDir(), "msg.md")
	os.WriteFile(f, []byte("edited text\n"), 0o644)
	update(m, editorDoneMsg{path: f})
	if m.in.Value() != "edited text" {
		t.Fatalf("input = %q", m.in.Value())
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "code --wait")
	if got := editorCommand(); len(got) != 2 || got[1] != "--wait" {
		t.Fatalf("editor = %v", got)
	}
}

func TestThinkingShowsReasoningTextDimmed(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	startRun(t, m, "go")
	feed(t, m, agui.New(agui.ReasoningMessageStart, map[string]any{"messageId": "r"}),
		agui.New(agui.ReasoningMessageContent, map[string]any{"messageId": "r", "delta": "a\nb\nc\nd\ne"}))
	live := m.Transcript()
	if strings.Contains(live, "\n  a") || !strings.Contains(live, "  e") {
		t.Fatalf("streaming should show the last 3 lines:\n%s", live)
	}
	feed(t, m, agui.New(agui.ReasoningMessageEnd, map[string]any{"messageId": "r"}))
	if strings.Contains(m.Transcript(), "  c") {
		t.Fatalf("hidden by default:\n%s", m.Transcript())
	}
	typeText(m, "/thinking")
	press(m, tea.KeyEnter)
	golden(t, "thinking_shown", m.Transcript())
}

func TestTimestampsAndDetailsToggles(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	m.add(block{kind: blockUser, text: "hi"})
	typeText(m, "/timestamps")
	press(m, tea.KeyEnter)
	if !strings.Contains(m.Transcript(), "12:00") {
		t.Fatalf("no time:\n%s", m.Transcript())
	}
	typeText(m, "/details")
	press(m, tea.KeyEnter)
	if !m.expanded {
		t.Fatal("details did not expand")
	}
}

func TestSidebarShowsPlanFilesAndModel(t *testing.T) {
	be := &optsBackend{}
	m := New(be, Options{Now: func() time.Time { return t0 }, StaticCursor: true})
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 30})
	startRun(t, m, "plan it")
	feed(t, m, agui.StateSnapshotEvent(map[string]any{"plan": []map[string]string{
		{"id": "1", "title": "read code", "status": "completed"},
		{"id": "2", "title": "write fix", "status": "in_progress"},
		{"id": "3", "title": "run tests", "status": "pending"},
	}}))
	feed(t, m, toolEvents("c1", "edit_file", `{"path":"a/b.go","oldText":"x","newText":"y"}`, `{"path":"a/b.go","replacements":1}`, false)...)
	feed(t, m, agui.CustomEvent(agui.CustomTitle, map[string]any{"title": "Fix the bug"}))
	feed(t, m, agui.CustomEvent(agui.CustomUsage, map[string]any{"inputTokens": 1200, "outputTokens": 300, "contextTokens": 12000, "contextWindow": 100000}))
	golden(t, "sidebar", m.View())
}

func TestSidebarWidthRules(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	for _, c := range []struct {
		w    int
		mode sidebarMode
		want bool
	}{{90, sbOn, false}, {120, sbAuto, false}, {120, sbOn, true}, {150, sbAuto, true}, {150, sbOff, false}} {
		m.width, m.inp.sb.mode = c.w, c.mode
		if got := m.sidebarCols() > 0; got != c.want {
			t.Errorf("width %d mode %d: shown=%v", c.w, c.mode, got)
		}
	}
}

func TestCtrlBTogglesTheSidebarAndShrinksTheTranscript(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	full := m.contentWidth()
	update(m, tea.KeyMsg{Type: tea.KeyCtrlB})
	if m.contentWidth() >= full {
		t.Fatal("transcript did not shrink")
	}
	update(m, tea.KeyMsg{Type: tea.KeyCtrlB})
	if m.contentWidth() != full {
		t.Fatal("not restored")
	}
}

func TestWelcomeScreen(t *testing.T) {
	m := New(&fakeBackend{}, Options{Now: func() time.Time { return t0 }, StaticCursor: true, Workspace: "/work/proj"})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	golden(t, "welcome", m.View())
	m.add(block{kind: blockUser, text: "hi"})
	m.refresh()
	if strings.Contains(m.View(), "tip:") {
		t.Fatal("welcome stays after the first message")
	}
}

func TestMessageNavigationSelectsAndCopies(t *testing.T) {
	var out bytes.Buffer
	m, _ := newModel(&fakeBackend{})
	m.inp.out = &out
	m.add(block{kind: blockUser, text: "question"})
	m.add(block{kind: blockAssistant, text: "answer one"})
	m.add(block{kind: blockUser, text: "again"})
	update(m, tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	update(m, tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	if m.inp.sel != 1 || !strings.Contains(m.Transcript(), "▌ answer one") {
		t.Fatalf("sel = %d\n%s", m.inp.sel, m.Transcript())
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y"), Alt: true})
	msgs(cmd)
	if !strings.Contains(out.String(), "\x1b]52;c;") {
		t.Fatalf("no OSC 52: %q", out.String())
	}
	press(m, tea.KeyEsc)
	if m.inp.sel != -1 {
		t.Fatal("esc did not clear the selection")
	}
}

func TestAttentionIsGatedOnFocusAndConfig(t *testing.T) {
	for _, c := range []struct {
		name    string
		attn    Attention
		blurred bool
		want    bool
	}{
		{"disabled", Attention{}, true, false},
		{"focused", Attention{Enabled: true, Notifications: true, Sound: true}, false, false},
		{"blurred", Attention{Enabled: true, Notifications: true, Sound: true}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			m, _ := newModel(&fakeBackend{})
			m.inp.out, m.inp.attn = &out, c.attn
			if c.blurred {
				update(m, tea.BlurMsg{})
			}
			startRun(t, m, "go")
			_, cmd := m.Update(ev(t, agui.New(agui.RunFinished, map[string]any{"outcome": map[string]any{"type": "success"}})))
			msgs(cmd)
			got := out.String()
			if c.want && (!strings.Contains(got, "\a") || !strings.Contains(got, "\x1b]9;harness: run finished")) {
				t.Fatalf("no signal: %q", got)
			}
			if !c.want && got != "" {
				t.Fatalf("unexpected signal: %q", got)
			}
		})
	}
}

func TestNotifyCommandTogglesAndBlurRefocus(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	typeText(m, "/notify")
	press(m, tea.KeyEnter)
	if !m.inp.attn.Enabled || !m.inp.attn.Notifications {
		t.Fatalf("attn = %+v", m.inp.attn)
	}
	update(m, tea.BlurMsg{})
	update(m, tea.FocusMsg{})
	if m.inp.blurred {
		t.Fatal("still blurred after focus")
	}
}

func TestNotifySeqStripsControlCharacters(t *testing.T) {
	if got := notifySeq("ghostty", "t", "b"); got != "\x1b]777;notify;t;b\x07" {
		t.Fatalf("777: %q", got)
	}
	if got := cleanText("a\x1b]0;evil\x07b"); strings.ContainsAny(got, "\x1b\x07;") {
		t.Fatalf("clean: %q", got)
	}
	if got := titleFor("Fix", "running"); got != "harness · Fix · running" {
		t.Fatalf("title: %q", got)
	}
}
