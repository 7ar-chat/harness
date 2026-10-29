package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/hosts/client"
)

func TestEveryTipNamesACommandOrActionThatExists(t *testing.T) {
	if len(tipPool) < 15 {
		t.Fatalf("%d tips, want at least 15", len(tipPool))
	}
	for _, tp := range tipPool {
		if tp.cmd != "" {
			c, ok := lookup(tp.cmd)
			if !ok || c.name != tp.cmd {
				t.Errorf("tip about /%s: no such command", tp.cmd)
			}
		}
		if tp.action != "" {
			if _, ok := defaultKeys[tp.action]; !ok {
				t.Errorf("tip about the action %q: not in the keymap", tp.action)
			}
		}
		if tp.cmd != "" && tp.action != "" || strings.TrimSpace(tp.text) == "" {
			t.Errorf("malformed tip %+v", tp)
		}
	}
}

func TestTipsShowTheKeysOfTheUsersKeymap(t *testing.T) {
	m, _ := newConfigured(t, `{"keybinds": {"tools": "ctrl+t", "command.stash": "<leader>k"}}`, "")

	tools := tip{action: "tools", text: "expands"}.render(m)
	stash := tip{cmd: "stash", text: "parks"}.render(m)

	if tools != "ctrl+t expands" || stash != "/stash [ctrl+x k] parks" {
		t.Fatalf("%q, %q", tools, stash)
	}
}

func TestTipRotatesWithTheLaunchAndTipsToggleHidesItAndIsRemembered(t *testing.T) {
	dir := t.TempDir()
	m, _ := newConfigured(t, ``, dir)
	if !strings.Contains(m.welcome(), "tip: ") {
		t.Fatalf("no tip:\n%s", m.welcome())
	}

	submitSlash(m, "/tips")
	hidden := m.welcome()
	restored, _ := newConfigured(t, ``, dir)
	restoredHidden := !strings.Contains(restored.welcome(), "tip: ")
	submitSlash(restored, "/tips")
	shownAgain, _ := newConfigured(t, ``, dir)

	if strings.Contains(hidden, "tip: ") {
		t.Fatalf("tip still shown:\n%s", hidden)
	}
	if !restoredHidden {
		t.Fatal("the hidden choice was not restored")
	}
	if !strings.Contains(shownAgain.welcome(), "tip: ") {
		t.Fatal("the second /tips did not persist")
	}
	first, second := New(&fakeBackend{}, Options{StaticCursor: true, Now: func() time.Time { return t0 }}), New(&fakeBackend{}, Options{StaticCursor: true, Now: func() time.Time { return t0.Add(time.Minute) }})
	if first.tipLine() == second.tipLine() {
		t.Fatal("the tip did not change from one launch to the next")
	}
}

func TestSessionPickerOrdersPinnedFirstAndMarksThemFromTheSessionList(t *testing.T) {
	// Arrange
	be := newExt()
	be.sessions = []client.Session{
		{ID: "a", Title: "Newest", Messages: 1},
		{ID: "b", Title: "Old but pinned", Messages: 1, Pinned: true},
		{ID: "c", Title: "Oldest", Messages: 1},
	}
	m, _ := newModel(be)

	// Act
	submitSlash(m, "/sessions")

	// Assert
	var order []string
	for _, it := range m.picker.items {
		order = append(order, it.id)
	}
	if strings.Join(order, "") != "bac" {
		t.Fatalf("order %v", order)
	}
	if !strings.HasPrefix(m.picker.items[0].label, "★ ") || strings.Contains(m.picker.items[1].label, "★") {
		t.Fatalf("labels %q %q", m.picker.items[0].label, m.picker.items[1].label)
	}
}
