package tui

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/core/agui"
)

func finishRunOutput(t *testing.T, m *Model, out *bytes.Buffer, ev agui.Event) string {
	t.Helper()
	startRun(t, m, "go")
	_, cmd := m.Update(ev2(t, ev))
	msgs(cmd)
	return out.String()
}

func ev2(t *testing.T, e agui.Event) EventMsg { t.Helper(); return ev(t, e) }

func TestAttentionConfigIsReadFromTuiJSON(t *testing.T) {
	c := ParseTUIConfig([]byte(`{"attention": {"enabled": true, "notifications": false, "volume": 0.5,
		"sound_command": ["afplay", "/s/done.aiff"], "sounds": {"error": false}}}`))

	a := c.Attention
	if a == nil || !a.Enabled || a.Notifications || !a.Sound || a.Bells != 2 || strings.Join(a.SoundCommand, " ") != "afplay /s/done.aiff" || !a.Mute[attnError] || a.Mute[attnDone] {
		t.Fatalf("%+v notices %v", a, c.Notices)
	}
	if len(c.Notices) != 0 {
		t.Fatalf("notices %v", c.Notices)
	}
}

func TestAttentionConfigRejectsBadValues(t *testing.T) {
	c := ParseTUIConfig([]byte(`{"attention": {"volume": 2, "sound_command": "afplay x", "sounds": {"oops": false}, "enabled": 1, "extra": 1}}`))

	all := strings.Join(c.Notices, "\n")
	for _, want := range []string{"attention.volume", "attention.sound_command", "attention.sounds.oops", "attention.enabled", "attention.extra"} {
		if !strings.Contains(all, want) {
			t.Errorf("no notice about %s:\n%s", want, all)
		}
	}
	if c.Attention == nil || c.Attention.Enabled || c.Attention.SoundCommand != nil {
		t.Fatalf("%+v", c.Attention)
	}
}

func TestVolumeZeroMutesTheBellAndFullVolumeRingsThree(t *testing.T) {
	quiet := ParseTUIConfig([]byte(`{"attention": {"enabled": true, "volume": 0}}`)).Attention
	loud := ParseTUIConfig([]byte(`{"attention": {"enabled": true, "volume": 1}}`)).Attention

	if quiet.Sound || loud.Bells != maxBells {
		t.Fatalf("quiet %+v loud %+v", quiet, loud)
	}
}

func TestAttentionFromConfigAppliesToTheModelAndOptionsStandOverIt(t *testing.T) {
	cfg := ParseTUIConfig([]byte(`{"attention": {"enabled": true, "sound": false}}`))
	fromCfg := New(&fakeBackend{}, Options{StaticCursor: true, Config: &cfg})
	fromHost := New(&fakeBackend{}, Options{StaticCursor: true, Config: &cfg, Attention: Attention{Enabled: true, Sound: true}})

	if !fromCfg.inp.attn.Enabled || fromCfg.inp.attn.Sound || !fromCfg.inp.attn.Notifications {
		t.Fatalf("config: %+v", fromCfg.inp.attn)
	}
	if !fromHost.inp.attn.Sound {
		t.Fatalf("host: %+v", fromHost.inp.attn)
	}
}

func TestNotifyChoiceIsSavedAndRestored(t *testing.T) {
	dir := t.TempDir()
	m, _ := newConfigured(t, ``, dir)

	submitSlash(m, "/notify")
	again, _ := newConfigured(t, ``, dir)

	if !again.inp.attn.Enabled || !again.inp.attn.Notifications {
		t.Fatalf("not restored: %+v", again.inp.attn)
	}
	submitSlash(again, "/notify")
	third, _ := newConfigured(t, `{"attention": {"enabled": true}}`, dir)
	if third.inp.attn.Enabled {
		t.Fatal("the saved off did not win over tui.json")
	}
}

func TestAttentionIsGatedByEventKind(t *testing.T) {
	finished := agui.New(agui.RunFinished, map[string]any{"outcome": map[string]any{"type": "success"}})
	failed := agui.New(agui.RunError, map[string]any{"message": "boom", "code": "x"})
	for _, c := range []struct {
		name string
		mute map[string]bool
		ev   agui.Event
		want bool
	}{
		{"done is signalled", nil, finished, true},
		{"done muted", map[string]bool{attnDone: true}, finished, false},
		{"error is signalled", map[string]bool{attnDone: true}, failed, true},
		{"error muted", map[string]bool{attnError: true}, failed, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			m, _ := newModel(&fakeBackend{})
			m.inp.out, m.inp.attn = &out, Attention{Enabled: true, Notifications: true, Mute: c.mute}
			update(m, tea.BlurMsg{})

			got := finishRunOutput(t, m, &out, c.ev)

			if (got != "") != c.want {
				t.Fatalf("signal %q, want signal=%v", got, c.want)
			}
		})
	}
}

func TestAttentionSignalsAQuestionAndAPermission(t *testing.T) {
	for kind, wantTitle := range map[string]string{"approval": "approval needed", "question": "harness: question"} {
		t.Run(kind, func(t *testing.T) {
			var out bytes.Buffer
			m, _ := newModel(&fakeBackend{})
			m.inp.out, m.inp.attn = &out, Attention{Enabled: true, Notifications: true}
			update(m, tea.BlurMsg{})
			startRun(t, m, "go")
			e := approvalInterrupt()
			if kind == "question" {
				e = askUserInterrupt()
			}

			_, cmd := m.Update(ev2(t, e))
			msgs(cmd)

			if !strings.Contains(out.String(), wantTitle) {
				t.Fatalf("output %q", out.String())
			}
		})
	}
}

func TestBellRepeatsAndTheSoundCommandRunsInsteadOfTheBell(t *testing.T) {
	finished := agui.New(agui.RunFinished, map[string]any{"outcome": map[string]any{"type": "success"}})
	var mu sync.Mutex
	var ran [][]string
	done := make(chan struct{}, 1)
	old := runSound
	runSound = func(argv []string, kind string, bells int) {
		mu.Lock()
		ran = append(ran, append(append([]string{}, argv...), kind))
		mu.Unlock()
		done <- struct{}{}
	}
	t.Cleanup(func() { runSound = old })

	var bell bytes.Buffer
	m, _ := newModel(&fakeBackend{})
	m.inp.out, m.inp.attn = &bell, Attention{Enabled: true, Sound: true, Bells: 3}
	update(m, tea.BlurMsg{})
	got := finishRunOutput(t, m, &bell, finished)

	var quiet bytes.Buffer
	m2, _ := newModel(&fakeBackend{})
	m2.inp.out, m2.inp.attn = &quiet, Attention{Enabled: true, Sound: true, SoundCommand: []string{"play", "x"}}
	update(m2, tea.BlurMsg{})
	got2 := finishRunOutput(t, m2, &quiet, finished)
	<-done

	if got != "\a\a\a" {
		t.Fatalf("bell output %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if got2 != "" || len(ran) != 1 || strings.Join(ran[0], " ") != "play x done" {
		t.Fatalf("terminal output %q, sound commands %v", got2, ran)
	}
}
