package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// statusBackend adds StatusBackend to the extension fake.
type statusBackend struct {
	*extBackend
	items []StatusItem
	err   error
}

func (s *statusBackend) Status(context.Context) ([]StatusItem, error) { return s.items, s.err }

func newStatusBackend() *statusBackend {
	be := newExt()
	be.cmds = []client.Command{
		{Name: "review", Description: "review a diff", Source: "skill"},
		{Name: "triage", Description: "sort issues", Source: "skill"},
		{Name: "deploy", Source: "project"},
	}
	return &statusBackend{extBackend: be, items: []StatusItem{
		{SectionBackend, "kind", "in-process"},
		{SectionConfig, "config.yml · user", "/home/u/.har/config.yml"},
		{SectionConfig, "permissions", "ask (default)"},
		{SectionMCP, "fs", "stdio: mcp-fs (+2 args) (MCP_SERVERS)"},
		{SectionMCP, "docs", "http: https://docs.example.com (user), 3 tools allowed"},
	}}
}

func TestStatusDialogShowsSessionBackendConfigAndExtensions(t *testing.T) {
	// Arrange
	be := newStatusBackend()
	m, _ := newModel(be)
	m.inp.workspace = "/work/proj"
	update(m, tea.WindowSizeMsg{Width: 100, Height: 40})

	// Act
	submitSlash(m, "/status")
	view := m.View()

	// Assert
	for _, want := range []string{"Status", "── Session", "model", "gpt-test", "thread", "t1", "workspace", "/work/proj", "── Backend", "in-process", "── Config", "config.yml · user", "── MCP", "── Extensions", "skills", "custom commands"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q in:\n%s", want, view)
		}
	}
	if strings.Index(view, "── Session") > strings.Index(view, "── Backend") || strings.Index(view, "── Backend") > strings.Index(view, "── Config") {
		t.Errorf("sections out of order:\n%s", view)
	}
}

func TestStatusIsReadOnlyAndEscCloses(t *testing.T) {
	m, _ := newModel(newStatusBackend())
	submitSlash(m, "/status")

	press(m, tea.KeyEnter)

	if m.picker != nil {
		t.Fatal("enter did not close the dialog")
	}
	if got := m.in.Value(); got != "" {
		t.Fatalf("the dialog changed the input: %q", got)
	}
}

func TestStatusWorksOnABackendWithoutStatusBackend(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	submitSlash(m, "/status")

	if v := m.View(); !strings.Contains(v, "gpt-test") || !strings.Contains(v, "workspace") {
		t.Fatalf("no basic status:\n%s", v)
	}
}

func TestMCPListsOnlyTheMCPServers(t *testing.T) {
	m, _ := newModel(newStatusBackend())
	update(m, tea.WindowSizeMsg{Width: 100, Height: 40})

	submitSlash(m, "/mcp")
	view := m.View()

	if !strings.Contains(view, "MCP servers") || !strings.Contains(view, "docs") || !strings.Contains(view, "3 tools allowed") || strings.Contains(view, "permissions") {
		t.Fatalf("mcp list:\n%s", view)
	}
}

func TestMCPWithoutAStatusBackendSaysSo(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	submitSlash(m, "/mcp")

	if m.picker != nil || !strings.Contains(m.Transcript(), "does not report MCP") {
		t.Fatalf("picker %v\n%s", m.picker, m.Transcript())
	}
}

func TestSkillsListsOnlySkills(t *testing.T) {
	m, _ := newModel(newStatusBackend())

	submitSlash(m, "/skills")
	view := m.View()

	if !strings.Contains(view, "/review") || !strings.Contains(view, "sort issues") || strings.Contains(view, "/deploy") {
		t.Fatalf("skills:\n%s", view)
	}
}

func TestStatusErrorsBecomeNoticesAndKeepTheBasics(t *testing.T) {
	be := newStatusBackend()
	be.err = client.ErrNotSupported
	m, _ := newModel(be)

	submitSlash(m, "/status")

	if !strings.Contains(m.Transcript(), "not supported by this backend/server") || m.picker == nil {
		t.Fatalf("picker %v\n%s", m.picker, m.Transcript())
	}
}

func TestClientBackendStatusNamesTheRemoteHostOnly(t *testing.T) {
	b := &ClientBackend{RemoteURL: "https://user:secret@harness.example.com:8443/api?token=abc",
		StatusFunc: func(context.Context) ([]StatusItem, error) { return []StatusItem{{SectionMCP, "x", "y"}}, nil }}

	items, err := b.Status(context.Background())

	if err != nil || len(items) != 2 || items[0].Value != "remote harness.example.com:8443" {
		t.Fatalf("%+v, %v", items, err)
	}
	for _, it := range items {
		if strings.Contains(it.Value, "secret") || strings.Contains(it.Value, "abc") {
			t.Fatalf("leaked: %+v", it)
		}
	}
	local := &ClientBackend{}
	if items, _ := local.Status(context.Background()); items[0].Value != "in-process" {
		t.Fatalf("%+v", items)
	}
}

func TestClientBackendStatusKeepsTheBackendKindWhenTheHostFuncFails(t *testing.T) {
	b := &ClientBackend{StatusFunc: func(context.Context) ([]StatusItem, error) { return nil, errors.New("boom") }}

	items, err := b.Status(context.Background())

	if err == nil || len(items) != 1 {
		t.Fatalf("%+v, %v", items, err)
	}
}
