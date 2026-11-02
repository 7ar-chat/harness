package tui

import tea "charm.land/bubbletea/v2"

// kt is a key as the tests press it; km builds the Bubble Tea message of it.
type kt struct {
	code rune
	mod  tea.KeyMod
}

func km(k kt, mods ...tea.KeyMod) tea.KeyPressMsg {
	m := k.mod
	for _, x := range mods {
		m |= x
	}
	return tea.KeyPressMsg{Code: k.code, Mod: m}
}

var (
	kBackspace = kt{code: tea.KeyBackspace}
	kCtrlB     = kt{code: 'b', mod: tea.ModCtrl}
	kCtrlC     = kt{code: 'c', mod: tea.ModCtrl}
	kCtrlD     = kt{code: 'd', mod: tea.ModCtrl}
	kCtrlE     = kt{code: 'e', mod: tea.ModCtrl}
	kCtrlF     = kt{code: 'f', mod: tea.ModCtrl}
	kCtrlG     = kt{code: 'g', mod: tea.ModCtrl}
	kCtrlK     = kt{code: 'k', mod: tea.ModCtrl}
	kCtrlL     = kt{code: 'l', mod: tea.ModCtrl}
	kCtrlO     = kt{code: 'o', mod: tea.ModCtrl}
	kCtrlP     = kt{code: 'p', mod: tea.ModCtrl}
	kCtrlQ     = kt{code: 'q', mod: tea.ModCtrl}
	kCtrlR     = kt{code: 'r', mod: tea.ModCtrl}
	kCtrlS     = kt{code: 's', mod: tea.ModCtrl}
	kCtrlT     = kt{code: 't', mod: tea.ModCtrl}
	kCtrlU     = kt{code: 'u', mod: tea.ModCtrl}
	kCtrlX     = kt{code: 'x', mod: tea.ModCtrl}
	kCtrlY     = kt{code: 'y', mod: tea.ModCtrl}
	kCtrlZ     = kt{code: 'z', mod: tea.ModCtrl}
	kDown      = kt{code: tea.KeyDown}
	kEnd       = kt{code: tea.KeyEnd}
	kEnter     = kt{code: tea.KeyEnter}
	kEsc       = kt{code: tea.KeyEscape}
	kF14       = kt{code: tea.KeyF14}
	kF2        = kt{code: tea.KeyF2}
	kF5        = kt{code: tea.KeyF5}
	kHome      = kt{code: tea.KeyHome}
	kPgDown    = kt{code: tea.KeyPgDown}
	kPgUp      = kt{code: tea.KeyPgUp}
	kShiftTab  = kt{code: tea.KeyTab, mod: tea.ModShift}
	kTab       = kt{code: tea.KeyTab}
	kUp        = kt{code: tea.KeyUp}
)
