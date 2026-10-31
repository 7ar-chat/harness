package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/hosts/client"
)

// richBackend adds model and session switching to the fake.
type richBackend struct {
	fakeBackend
	model    string
	models   []string
	sessions []client.Session
	thread   *client.Thread
	opened   string
	newCalls int
}

func (r *richBackend) Model() string {
	if r.model == "" {
		return "gpt-test"
	}
	return r.model
}
func (r *richBackend) Models(context.Context) ([]string, error) { return r.models, nil }
func (r *richBackend) SetModel(id string)                       { r.model = id }
func (r *richBackend) Sessions(context.Context) ([]client.Session, error) {
	return r.sessions, nil
}
func (r *richBackend) NewThread(context.Context) error { r.newCalls++; return nil }
func (r *richBackend) OpenThread(_ context.Context, id string) (*client.Thread, error) {
	r.opened = id
	return r.thread, nil
}

func TestSlashCompletionListsMatchesAndTabCompletes(t *testing.T) {
	m, _ := newModel(&richBackend{})

	typeText(m, "/mo")
	if v := m.View(); !strings.Contains(v, "/model") || !strings.Contains(v, "pick a model") {
		t.Fatalf("no completion list:\n%s", v)
	}
	press(m, tea.KeyTab)

	if got := m.in.Value(); got != "/model " {
		t.Fatalf("tab completed to %q", got)
	}
}

func TestModelCommandOpensAFilterablePickerAndSwitches(t *testing.T) {
	rb := &richBackend{models: []string{"alpha-1", "beta-2", "beta-3"}}
	m, _ := newModel(rb)

	typeText(m, "/model")
	press(m, tea.KeyEnter)
	if m.picker == nil {
		t.Fatalf("no picker:\n%s", m.View())
	}
	typeText(m, "beta")
	press(m, tea.KeyDown)
	press(m, tea.KeyEnter)

	if rb.model != "beta-3" || m.picker != nil {
		t.Fatalf("model %q, picker open %v", rb.model, m.picker != nil)
	}
	if v := m.View(); !strings.Contains(v, "beta-3") {
		t.Fatalf("status does not show the model:\n%s", v)
	}
}

func TestModelPickerAcceptsATypedIdTheListLacks(t *testing.T) {
	rb := &richBackend{models: []string{"alpha-1"}}
	m, _ := newModel(rb)

	typeText(m, "/model")
	press(m, tea.KeyEnter)
	typeText(m, "my-finetune")
	press(m, tea.KeyEnter)

	if rb.model != "my-finetune" {
		t.Fatalf("model %q", rb.model)
	}
}

func TestModelCommandWithAnArgumentSetsItWithoutAPicker(t *testing.T) {
	rb := &richBackend{}
	m, _ := newModel(rb)

	typeText(m, "/model gpt-9")
	press(m, tea.KeyEnter)

	if rb.model != "gpt-9" || m.picker != nil {
		t.Fatalf("model %q picker %v", rb.model, m.picker != nil)
	}
}

func TestSessionsCommandReplaysTheChosenConversation(t *testing.T) {
	rb := &richBackend{
		sessions: []client.Session{{ID: "t1", Title: "Fix the build", Messages: 3}},
		thread: &client.Thread{ID: "t1", Title: "Fix the build", Messages: []client.Message{
			{Role: "user", Content: "why does it fail"},
			{Role: "assistant", Content: "looking", ToolCalls: []client.ToolCall{{ID: "c1", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "exec", Arguments: `{"command":"make"}`}}}},
			{Role: "tool", ToolCallID: "c1", Content: "ok"},
		}},
	}
	m, _ := newModel(rb)

	typeText(m, "/sessions")
	press(m, tea.KeyEnter)
	press(m, tea.KeyEnter)

	tr := m.Transcript()
	for _, want := range []string{"why does it fail", "looking", "exec", "resumed: Fix the build"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("transcript lacks %q:\n%s", want, tr)
		}
	}
	if rb.opened != "t1" {
		t.Fatalf("opened %q", rb.opened)
	}
}

func TestNewCommandClearsTheTranscriptAndRefusesDuringARun(t *testing.T) {
	rb := &richBackend{}
	m, _ := newModel(rb)
	startRun(t, m, "hello")

	typeText(m, "/new")
	press(m, tea.KeyEnter)
	if rb.newCalls != 0 || !strings.Contains(m.Transcript(), "esc cancels it first") {
		t.Fatalf("/new ran during a run:\n%s", m.Transcript())
	}

	m2, _ := newModel(rb)
	typeText(m2, "old message")
	press(m2, tea.KeyEnter)
	feed(t, m2, agui.RunFinishedEvent("t", "run1", "success", nil))
	typeText(m2, "/new")
	press(m2, tea.KeyEnter)
	if rb.newCalls != 1 || strings.Contains(m2.Transcript(), "old message") {
		t.Fatalf("new calls %d:\n%s", rb.newCalls, m2.Transcript())
	}
}

func TestUnknownCommandIsReportedAndPathsAreSentAsMessages(t *testing.T) {
	fb := &fakeBackend{}
	m, _ := newModel(fb)

	typeText(m, "/nope")
	press(m, tea.KeyEnter)
	if !strings.Contains(m.Transcript(), "unknown command /nope") || len(fb.Calls()) != 0 {
		t.Fatalf("calls %v:\n%s", fb.Calls(), m.Transcript())
	}
	m.in.Reset()
	typeText(m, "/etc/hosts is wrong")
	press(m, tea.KeyEnter)

	if got := fb.Calls(); len(got) != 1 || got[0] != "send:/etc/hosts is wrong" {
		t.Fatalf("calls %v", got)
	}
}

func TestPlainBackendSaysItCannotSwitchModels(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	typeText(m, "/model")
	press(m, tea.KeyEnter)

	if !strings.Contains(m.Transcript(), "cannot switch models") {
		t.Fatal(m.Transcript())
	}
}
