package tui

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeProvisioner struct {
	fakeConnector
	configured bool
	models     map[string][]string
	modelsErr  error
	applied    []string
	applyErr   error
}

func (f *fakeProvisioner) Configured() bool         { return f.configured }
func (f *fakeProvisioner) Active() (string, string) { return "", "" }
func (f *fakeProvisioner) ConfigPath() string       { return "/home/u/.har/config.yml" }
func (f *fakeProvisioner) Models(_ context.Context, p string) ([]string, error) {
	return f.models[p], f.modelsErr
}
func (f *fakeProvisioner) Apply(_ context.Context, p, model string) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, p+"/"+model)
	f.configured = true
	return nil
}

func newSetupModel(fp *fakeProvisioner) (*Model, *fakeBackend) {
	fb := &fakeBackend{}
	m := New(fb, Options{StaticCursor: true, Connector: fp})
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	return m, fb
}

func TestHomeScreenExplainsHowToConnect(t *testing.T) {
	fp := &fakeProvisioner{}
	m, _ := newSetupModel(fp)

	v := m.Render()

	for _, want := range []string{"Connect an AI provider", "Pick a provider", "Paste your API key", "Choose a model", "Press enter", "/connect", "OPENAI_API_KEY", "/home/u/.har/config.yml", "not connected", "no provider"} {
		if !strings.Contains(v, want) {
			t.Errorf("home screen lacks %q:\n%s", want, v)
		}
	}
	golden(t, "home_setup", v)
}

func TestEnterOnTheHomeScreenStartsTheSetupAndAMessageDoesNotSend(t *testing.T) {
	fp := &fakeProvisioner{fakeConnector: fakeConnector{providers: []ProviderInfo{{Name: "openai", BaseURL: "https://api.openai.com/v1"}}}}
	m, fb := newSetupModel(fp)

	press(m, kEnter)
	if v := m.Render(); !strings.Contains(v, "Step 1 of 3") || !strings.Contains(v, "needs a key") {
		t.Fatalf("setup did not start:\n%s", v)
	}
	press(m, kEsc)
	typeText(m, "hello")
	press(m, kEnter)

	if len(fb.Calls()) != 0 {
		t.Fatalf("a message was sent while not connected: %v", fb.Calls())
	}
	if m.picker == nil || m.in.Value() != "hello" {
		t.Fatalf("the setup should open and keep the draft: picker %v input %q", m.picker != nil, m.in.Value())
	}
}

func TestSetupWithAKeyInTheEnvironmentSkipsStraightToTheModel(t *testing.T) {
	fp := &fakeProvisioner{
		fakeConnector: fakeConnector{providers: []ProviderInfo{{Name: "anthropic", KeyFrom: "env ANTHROPIC_API_KEY"}}},
		models:        map[string][]string{"anthropic": {"claude-a", "claude-b"}},
	}
	m, _ := newSetupModel(fp)

	press(m, kEnter) // start
	press(m, kEnter) // anthropic: a key is there already
	if v := m.Render(); !strings.Contains(v, "Choose a model for anthropic") || !strings.Contains(v, "claude-b") {
		t.Fatalf("models not offered:\n%s", v)
	}
	press(m, kDown)
	press(m, kEnter)

	if len(fp.applied) != 1 || fp.applied[0] != "anthropic/claude-b" {
		t.Fatalf("applied %v", fp.applied)
	}
	if tr := m.Transcript(); !strings.Contains(tr, "connected to anthropic · model claude-b") {
		t.Fatalf("transcript:\n%s", tr)
	}
	if m.needsSetup() || strings.Contains(m.Render(), "Press enter") {
		t.Fatalf("still on the home screen:\n%s", m.Render())
	}
}

func TestSetupAsksForTheKeyThenTheModelWithoutARestart(t *testing.T) {
	fp := &fakeProvisioner{
		fakeConnector: fakeConnector{providers: []ProviderInfo{{Name: "openai"}}},
		models:        map[string][]string{"openai": {"gpt-x"}},
	}
	m, _ := newSetupModel(fp)

	press(m, kEnter)
	press(m, kEnter)
	if v := m.Render(); !strings.Contains(v, "Step 2 of 3") {
		t.Fatalf("no key step:\n%s", v)
	}
	typeText(m, "sk-secret")
	press(m, kEnter)
	press(m, kEnter) // the only model

	if fp.saved["openai"] != "sk-secret" || len(fp.applied) != 1 || fp.applied[0] != "openai/gpt-x" {
		t.Fatalf("saved %v applied %v", fp.saved, fp.applied)
	}
	if tr := m.Transcript(); strings.Contains(tr, "sk-secret") || strings.Contains(tr, "restart") {
		t.Fatalf("transcript:\n%s", tr)
	}
}

func TestSetupLetsYouTypeAModelWhenTheListFails(t *testing.T) {
	fp := &fakeProvisioner{
		fakeConnector: fakeConnector{providers: []ProviderInfo{{Name: "mylab", NoKey: true}}},
		modelsErr:     errors.New("401 unauthorized"),
	}
	m, _ := newSetupModel(fp)

	press(m, kEnter)
	press(m, kEnter)
	typeText(m, "lab-1")
	press(m, kEnter)

	if tr := m.Transcript(); !strings.Contains(tr, "could not list the models of mylab") || len(fp.applied) != 1 || fp.applied[0] != "mylab/lab-1" {
		t.Fatalf("applied %v:\n%s", fp.applied, tr)
	}
}

func TestSetupReportsAFailedApplyAndStaysOnTheHomeScreen(t *testing.T) {
	fp := &fakeProvisioner{
		fakeConnector: fakeConnector{providers: []ProviderInfo{{Name: "openai", HasKey: true}}},
		models:        map[string][]string{"openai": {"m"}},
		applyErr:      errors.New("bad key"),
	}
	m, _ := newSetupModel(fp)

	press(m, kEnter)
	press(m, kEnter)
	press(m, kEnter)

	if tr := m.Transcript(); !strings.Contains(tr, "could not connect to openai: bad key") || !m.needsSetup() {
		t.Fatalf("transcript:\n%s", tr)
	}
}
