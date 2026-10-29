package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// clock is an injectable time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestWheelWithoutAccelerationScrollsSpeedLinesPerNotch(t *testing.T) {
	c := &clock{t: t0}
	a := accelerator{speed: 3, now: c.now}

	var got []int
	for i := 0; i < 4; i++ {
		got = append(got, a.notch())
		c.advance(10 * time.Millisecond)
	}

	if fmt.Sprint(got) != "[3 3 3 3]" {
		t.Fatalf("lines per notch %v", got)
	}
}

func TestWheelAccelerationGrowsWithConsecutiveNotchesAndDecaysAfterAPause(t *testing.T) {
	c := &clock{t: t0}
	a := accelerator{speed: 1, enabled: true, now: c.now}

	first := a.notch()
	var fast int
	for i := 0; i < 10; i++ {
		c.advance(20 * time.Millisecond)
		fast = a.notch()
	}
	c.advance(accelGap + time.Millisecond)
	afterPause := a.notch()

	if first != 1 {
		t.Fatalf("first notch scrolled %d lines", first)
	}
	if fast <= first*2 {
		t.Fatalf("no acceleration after 10 quick notches: %d lines", fast)
	}
	if afterPause != 1 {
		t.Fatalf("speed did not decay after a pause: %d lines", afterPause)
	}
}

func TestWheelAccelerationIsCapped(t *testing.T) {
	c := &clock{t: t0}
	a := accelerator{speed: 1, enabled: true, now: c.now}

	var last int
	for i := 0; i < 200; i++ {
		c.advance(5 * time.Millisecond)
		last = a.notch()
	}

	if want := int(1 + accelMax*accelStep); last != want {
		t.Fatalf("capped speed %d lines, want %d", last, want)
	}
}

func TestSlowScrollSpeedCarriesTheFractionOver(t *testing.T) {
	c := &clock{t: t0}
	a := accelerator{speed: 0.25, now: c.now}

	total := 0
	for i := 0; i < 8; i++ {
		total += a.notch()
		c.advance(time.Second)
	}

	if total != 2 {
		t.Fatalf("8 notches at 0.25 scrolled %d lines, want 2", total)
	}
}

func TestScrollSpeedAndAccelerationComeFromTuiJSON(t *testing.T) {
	c := ParseTUIConfig([]byte(`{"scroll_speed": 0.5, "scroll_acceleration": {"enabled": true}}`))

	if c.ScrollSpeed != 0.5 || !c.ScrollAccel || len(c.Notices) != 0 {
		t.Fatalf("%+v", c)
	}
	bad := ParseTUIConfig([]byte(`{"scroll_speed": 0.0001, "scroll_acceleration": true}`))
	if bad.ScrollSpeed != DefaultScrollSpeed || len(bad.Notices) != 2 {
		t.Fatalf("%+v", bad)
	}
}

func longTranscript(m *Model) {
	for i := 0; i < 20; i++ {
		m.add(block{kind: blockUser, text: fmt.Sprintf("question %d\nwith a second line", i)})
		m.add(block{kind: blockAssistant, text: fmt.Sprintf("answer %d", i)})
	}
	m.refresh()
	m.vp.GotoBottom()
}

func wheelMsg(b tea.MouseButton) tea.MouseMsg {
	return tea.MouseMsg{Action: tea.MouseActionPress, Button: b, Type: tea.MouseWheelUp}
}

func TestMouseWheelScrollsTheTranscript(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	longTranscript(m)
	bottom := m.vp.YOffset

	update(m, wheelMsg(tea.MouseButtonWheelUp))
	up := m.vp.YOffset
	update(m, wheelMsg(tea.MouseButtonWheelDown))

	if up != bottom-DefaultScrollSpeed || m.vp.YOffset != bottom {
		t.Fatalf("offsets bottom %d, after wheel up %d, after wheel down %d", bottom, up, m.vp.YOffset)
	}
}

func TestLineScrollActionsHaveDefaultsAndAreRebindable(t *testing.T) {
	m, _ := newConfigured(t, `{"keybinds": {"messages_line_up": "ctrl+k"}}`, "")
	longTranscript(m)
	bottom := m.vp.YOffset

	update(m, tea.KeyMsg{Type: tea.KeyCtrlK})
	oneUp := m.vp.YOffset
	update(m, tea.KeyMsg{Type: tea.KeyCtrlE, Alt: true})

	if oneUp != bottom-1 || m.vp.YOffset != bottom {
		t.Fatalf("bottom %d, line up %d, line down %d", bottom, oneUp, m.vp.YOffset)
	}
}

func TestCtrlZSuspendsTheProgram(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlZ})

	got := msgs(cmd)
	if len(got) != 1 {
		t.Fatalf("messages %v", got)
	}
	if _, ok := got[0].(tea.SuspendMsg); !ok {
		t.Fatalf("ctrl+z gave %T, want a suspend", got[0])
	}
}

func TestMouseCommandTogglesMouseReporting(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	typeText(m, "/mouse")
	_, off := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	typeText(m, "/mouse")
	_, on := m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if got := fmt.Sprintf("%T", msgs(off)[0]); !strings.Contains(got, "disableMouse") {
		t.Fatalf("first /mouse sent %s", got)
	}
	if got := fmt.Sprintf("%T", msgs(on)[0]); !strings.Contains(got, "enableMouseCellMotion") {
		t.Fatalf("second /mouse sent %s", got)
	}
}

func TestLoneQuestionMarkIsHelpAndThemeIsAnAliasOfThemes(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	typeText(m, "?")
	press(m, tea.KeyEnter)
	help := m.Transcript()
	typeText(m, "/theme")
	press(m, tea.KeyEnter)

	if !strings.Contains(help, "commands") {
		t.Fatalf("? did not show help:\n%s", help)
	}
	if m.picker == nil || m.picker.title != "theme" {
		t.Fatal("/theme did not open the theme picker")
	}
}
