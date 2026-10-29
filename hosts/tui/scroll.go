package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Wheel acceleration: events closer than accelGap belong to one gesture; each
// adds accelStep to the multiplier of the speed, up to accelMax gestures'
// worth, and a pause of accelGap starts over (macOS-style).
const (
	accelGap  = 150 * time.Millisecond
	accelStep = 0.3
	accelMax  = 20
)

// The actions of the keymap for scrolling by line and jumping between your messages.
const (
	actLineUp   = "messages_line_up"
	actLineDown = "messages_line_down"
	actNextUser = "messages_next"
	actPrevUser = "messages_previous"
	actLastUser = "messages_last_user"
)

// accelerator turns wheel notches into whole lines: speed lines per notch,
// more while the notches keep coming, the fraction carried to the next notch
// (a speed of 0.5 scrolls one line every second notch).
type accelerator struct {
	speed   float64
	enabled bool
	now     func() time.Time
	last    time.Time
	streak  int
	frac    float64
}

// notch registers one wheel event and returns how many lines to scroll.
func (a *accelerator) notch() int {
	t := a.now()
	if a.last.IsZero() || t.Sub(a.last) > accelGap {
		a.streak = 0
	} else {
		a.streak = min(a.streak+1, accelMax)
	}
	a.last = t
	mult := 1.0
	if a.enabled {
		mult += float64(a.streak) * accelStep
	}
	v := a.speed*mult + a.frac
	n := int(v)
	a.frac = v - float64(n)
	return n
}

// wheel scrolls the transcript by a wheel notch; other mouse events are ignored.
func (m *Model) wheel(ev tea.MouseMsg) {
	if ev.Action != tea.MouseActionPress {
		return
	}
	switch ev.Button {
	case tea.MouseButtonWheelUp:
		m.vp.ScrollUp(m.pf.wheel.notch())
	case tea.MouseButtonWheelDown:
		m.vp.ScrollDown(m.pf.wheel.notch())
	}
}
