package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The kinds of event that call the user back, as the keys of tui.json
// "attention.sounds" and the HARNESS_EVENT of a sound command.
const (
	attnDone       = "done"
	attnQuestion   = "question"
	attnPermission = "permission"
	attnError      = "error"
)

var attnKinds = []string{attnDone, attnQuestion, attnPermission, attnError}

// maxBells is how many bells volume 1 rings: a terminal bell has no volume, so
// the setting is a repeat count.
const maxBells = 3

// soundTimeout bounds a sound command.
const soundTimeout = 10 * time.Second

// parseAttention reads tui.json "attention":
//
//	{"enabled": true, "notifications": true, "sound": true, "volume": 0.5,
//	 "sound_command": ["afplay", "/path/done.aiff"],
//	 "sounds": {"done": true, "question": true, "permission": true, "error": false}}
//
// volume (0..1) sets how many times the bell rings (0 mutes it). sound_command
// is an argv run without a shell instead of the bell, with HARNESS_EVENT and
// HARNESS_VOLUME in its environment; it is read from the user's tui.json only.
// sounds switches single kinds of event off.
func parseAttention(c *TUIConfig, v json.RawMessage, bad func(field, why string)) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(v, &raw) != nil {
		bad("attention", "must be an object")
		return
	}
	a := Attention{Notifications: true, Sound: true}
	for _, k := range sortedKeys(raw) {
		field := "attention." + k
		switch k {
		case "enabled", "notifications", "sound":
			var b bool
			if json.Unmarshal(raw[k], &b) != nil {
				bad(field, "must be true or false")
				continue
			}
			switch k {
			case "enabled":
				a.Enabled = b
			case "notifications":
				a.Notifications = b
			default:
				a.Sound = b
			}
		case "volume":
			var f float64
			if json.Unmarshal(raw[k], &f) != nil || f < 0 || f > 1 {
				bad(field, "must be a number from 0 to 1")
				continue
			}
			a.Bells = int(f*maxBells + 0.999)
			if f == 0 {
				a.Sound = false
			}
		case "sound_command":
			var argv []string
			if json.Unmarshal(raw[k], &argv) != nil || len(argv) == 0 || argv[0] == "" || strings.ContainsRune(strings.Join(argv, ""), 0) {
				bad(field, "must be an argv array such as [\"afplay\", \"/path/sound.aiff\"]")
				continue
			}
			a.SoundCommand = argv
		case "sounds":
			var m map[string]bool
			if json.Unmarshal(raw[k], &m) != nil {
				bad(field, "must be an object {done, question, permission, error: true|false}")
				continue
			}
			for _, kind := range sortedBools(m) {
				if !isAttnKind(kind) {
					c.Notices = append(c.Notices, fmt.Sprintf("tui.json: unknown field \"attention.sounds.%s\" ignored", kind))
					continue
				}
				if !m[kind] {
					if a.Mute == nil {
						a.Mute = map[string]bool{}
					}
					a.Mute[kind] = true
				}
			}
		default:
			c.Notices = append(c.Notices, fmt.Sprintf("tui.json: unknown field %q ignored", field))
		}
	}
	c.Attention = &a
}

func sortedBools(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func isAttnKind(k string) bool {
	for _, a := range attnKinds {
		if a == k {
			return true
		}
	}
	return false
}

// initAttention applies tui.json's attention object and then the saved /notify
// choice; an Options.Attention set by the host stands over both.
func (m *Model) initAttention(cfg TUIConfig, o Options) {
	if o.Attention.Enabled || o.Attention.Notifications || o.Attention.Sound {
		return
	}
	if cfg.Attention != nil {
		m.inp.attn = *cfg.Attention
	}
	var saved *bool
	_ = m.uiStore.get("notify", &saved)
	if saved != nil {
		a := &m.inp.attn
		a.Enabled = *saved
		if a.Enabled && !a.Notifications && !a.Sound {
			a.Notifications, a.Sound = true, true
		}
	}
}

// runSound runs a sound command: no shell, a time limit, output discarded. It
// is a variable so tests do not start processes.
var runSound = func(argv []string, kind string, bells int) {
	ctx, cancel := context.WithTimeout(context.Background(), soundTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "HARNESS_EVENT="+kind, fmt.Sprintf("HARNESS_VOLUME=%d", bells))
	_ = cmd.Run()
}

// notify signals the user, when attention is enabled for this kind of event
// and the terminal is blurred: a desktop notification, then the bell or the
// sound command. The text is stripped of control characters.
func (m *Model) notify(kind, title, body string) {
	a := m.inp.attn
	if !a.Enabled || !m.inp.blurred || a.Mute[kind] {
		return
	}
	seq := ""
	var sound []string
	if a.Sound {
		if len(a.SoundCommand) > 0 {
			sound = a.SoundCommand
		} else {
			seq += strings.Repeat("\a", max(a.Bells, 1))
		}
	}
	if a.Notifications {
		seq += notifySeq(termProgram(), cleanText(title), cleanText(body))
	}
	if seq == "" && sound == nil {
		return
	}
	out, bells := m.inp.out, max(a.Bells, 1)
	m.inp.cmds = append(m.inp.cmds, func() tea.Msg {
		if seq != "" {
			fmt.Fprint(out, seq) // one write: the renderer's frames cannot split it
		}
		if sound != nil {
			go runSound(sound, kind, bells)
		}
		return nil
	})
}

// cmdNotify toggles attention and remembers the choice in the state file.
func (m *Model) cmdNotify(string) tea.Cmd {
	a := &m.inp.attn
	a.Enabled = !a.Enabled
	state := "off"
	if a.Enabled {
		state = "on"
		if !a.Notifications && !a.Sound {
			a.Notifications, a.Sound = true, true
		}
	}
	if err := m.uiStore.set("notify", a.Enabled); err != nil {
		m.notice("notifications: could not save the choice: %v", err)
	}
	m.notice("notifications: %s (only while this terminal is in the background)", state)
	return nil
}
