package skilltools_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/skills"
	"github.com/7ar-chat/harness/features/skills/skilltools"
)

// manager is a sandbox.Manager over one optional sandbox.
type manager struct {
	mu          sync.Mutex
	enabled     bool
	sb          sandbox.Sandbox
	getErr      error
	invalidated []string
}

func (m *manager) Enabled() bool { return m.enabled }
func (m *manager) Acquire(ctx context.Context, threadID, _ string) (sandbox.Sandbox, error) {
	return m.Get(ctx, threadID)
}
func (m *manager) Get(context.Context, string) (sandbox.Sandbox, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	if m.sb == nil {
		return nil, sandbox.ErrNoSandbox
	}
	return m.sb, nil
}
func (m *manager) Kill(context.Context, string) error { return nil }
func (m *manager) Invalidate(_ context.Context, threadID, sandboxID string) {
	m.mu.Lock()
	m.invalidated = append(m.invalidated, threadID+"/"+sandboxID)
	m.mu.Unlock()
}

type runSkills map[string][]string

func (r runSkills) RunSkills(_ context.Context, runID string) ([]string, error) { return r[runID], nil }

func skillFS() fstest.MapFS {
	return fstest.MapFS{
		"docx/SKILL.md":         {Data: []byte("---\nname: docx\ndescription: Word documents.\n---\n\nUse python-docx.\n")},
		"docx/scripts/make.py":  {Data: []byte("print('docx')\n")},
		"notes/SKILL.md":        {Data: []byte("---\nname: notes\ndescription: Notes.\nrequires: docx\n---\n\nTake notes.\n")},
		"notes/scripts/note.py": {Data: []byte("print('note')\n")},
	}
}

type rig struct {
	provider *skilltools.Provider
	inst     *skills.Installer
	mgr      *manager
	sb       *sandboxtest.Sandbox
	rc       tools.RunContext
	tool     ai.InvokableTool
}

func newRig(t *testing.T, requested runSkills) *rig {
	t.Helper()
	reg := skills.NewRegistry(skillFS(), requested)
	inst := &skills.Installer{Registry: reg}
	sb := sandboxtest.NewSandbox("sb-1")
	mgr := &manager{enabled: true, sb: sb}
	r := &rig{provider: &skilltools.Provider{Registry: reg, Installer: inst, Sandbox: mgr}, inst: inst, mgr: mgr, sb: sb,
		rc: tools.RunContext{ThreadID: "th-1", RunID: "run-1", Login: "alice"}}
	r.tool = r.build(t)
	return r
}

func (r *rig) build(t *testing.T) ai.InvokableTool {
	t.Helper()
	list, cleanup, err := r.provider.Tools(context.Background(), r.rc)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("nil cleanup")
	}
	cleanup()
	if len(list) != 1 {
		t.Fatalf("tools %d", len(list))
	}
	inv, ok := list[0].(ai.InvokableTool)
	if !ok {
		t.Fatalf("%T is not invokable", list[0])
	}
	return inv
}

func (r *rig) load(name string) (string, error) {
	return r.tool.InvokableRun(context.Background(), `{"name":"`+name+`"}`)
}

func installed(sb *sandboxtest.Sandbox, name string) bool {
	_, ok := sb.File("/skills/" + name + "/SKILL.md")
	return ok
}

func TestProviderOffersOneReadOnlyLoadSkillTool(t *testing.T) {
	// The provider is named "skills" and offers load_skill, marked read-only (it may run in parallel with other reads), with a required name parameter.
	// Arrange
	r := newRig(t, nil)

	// Act
	info, err := r.tool.Info(context.Background())

	// Assert
	if r.provider.Name() != "skills" {
		t.Fatalf("name %q", r.provider.Name())
	}
	if err != nil || info.Name != "load_skill" || !strings.Contains(info.Desc, "Load a skill") {
		t.Fatalf("info %+v %v", info, err)
	}
	ro, ok := r.tool.(tools.ReadOnly)
	if !ok || !ro.ReadOnly() {
		t.Fatalf("load_skill must be read-only: %T", r.tool)
	}
	if req, _ := info.Parameters["required"].([]any); len(req) != 1 || req[0] != "name" {
		t.Fatalf("parameters %+v", info.Parameters)
	}
}

func TestLoadSkillInstallsIntoTheRunningSandboxWithDependenciesAndReportsIt(t *testing.T) {
	// With a running sandbox, loading a skill returns its instructions as installed and copies it and what it requires to /skills.
	// Arrange
	r := newRig(t, nil)

	// Act
	out, err := r.load("notes")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "# Skill: notes\n\nInstalled at /skills/notes/ (read-only).") || !strings.Contains(out, "Take notes.") ||
		!strings.Contains(out, "- /skills/notes/scripts/note.py") {
		t.Fatalf("result:\n%s", out)
	}
	if !installed(r.sb, "notes") || !installed(r.sb, "docx") {
		t.Fatalf("installed: %v", r.sb.Paths("/skills"))
	}
}

func TestLoadSkillTrimsTheNameAndFindsRequestedAttachments(t *testing.T) {
	// A padded name resolves, and a skill attached to the run is served from its attachment.
	// Arrange
	r := newRig(t, runSkills{"run-1": {"docx"}})

	// Act
	out, err := r.tool.InvokableRun(context.Background(), `{"name":"  docx "}`)

	// Assert
	if err != nil || !strings.Contains(out, "Use python-docx.") || !installed(r.sb, "docx") {
		t.Fatalf("%v:\n%s", err, out)
	}
}

func TestLoadSkillWithoutARunningSandboxPromisesInstallOnFirstCommandAndInstallsNothing(t *testing.T) {
	// Loading never starts a sandbox: with none running, the result says the files come with the first command and the skill is installed when the run acquires its sandbox.
	// Arrange
	r := newRig(t, nil)
	r.mgr.sb = nil

	// Act
	out, err := r.load("docx")
	r.inst.EnsureRunSkills(context.Background(), r.rc, r.sb)

	// Assert
	if err != nil || !strings.Contains(out, "Loading it started nothing: its files are at /skills/docx/") {
		t.Fatalf("%v:\n%s", err, out)
	}
	if !installed(r.sb, "docx") {
		t.Fatal("the loaded skill must be installed when the sandbox starts")
	}
}

func TestLoadSkillWhenTheSandboxLookupFailsStillReturnsTheInstructions(t *testing.T) {
	// An unexpected sandbox error (not "no sandbox") does not fail the tool: the model gets the instructions and the skill is installed later.
	// Arrange
	r := newRig(t, nil)
	r.mgr.getErr = errors.New("provider unreachable")

	// Act
	out, err := r.load("docx")

	// Assert
	if err != nil || !strings.Contains(out, "from your first command") || installed(r.sb, "docx") {
		t.Fatalf("%v:\n%s", err, out)
	}
}

func TestLoadSkillWithoutSandboxSupportSaysScriptsCannotRun(t *testing.T) {
	// A disabled sandbox manager, no manager, or a run with the sandbox off all answer "no sandbox" and install nothing.
	cases := map[string]func(r *rig){
		"disabled manager": func(r *rig) { r.mgr.enabled = false },
		"no manager":       func(r *rig) { r.provider.Sandbox = nil },
		"sandbox off":      func(r *rig) { r.rc.Sandbox = tools.SandboxOff },
	}
	for name, arm := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r := newRig(t, nil)
			arm(r)
			tool := r.build(t)

			// Act
			out, err := tool.InvokableRun(context.Background(), `{"name":"docx"}`)

			// Assert
			if err != nil || !strings.Contains(out, "No sandbox is available") || installed(r.sb, "docx") || len(r.sb.Execs()) != 0 {
				t.Fatalf("%v:\n%s", err, out)
			}
		})
	}
}

func TestLoadSkillRejectsAnUnknownNameListingTheAvailableOnes(t *testing.T) {
	// An unknown skill is an error for the model that names the available skills; with none available it says so.
	// Arrange
	r := newRig(t, nil)
	empty := &skilltools.Provider{Installer: &skills.Installer{Registry: skills.NewRegistry(nil, nil)}, Sandbox: r.mgr}
	list, _, _ := empty.Tools(context.Background(), r.rc)

	// Act
	_, err := r.load("ghost")
	_, errEmpty := list[0].(ai.InvokableTool).InvokableRun(context.Background(), `{"name":"ghost"}`)

	// Assert
	var ue *tools.UserError
	if !errors.As(err, &ue) || ue.Msg != `unknown skill "ghost"; available skills: docx, notes` {
		t.Fatalf("error %v", err)
	}
	if !errors.As(errEmpty, &ue) || ue.Msg != `unknown skill "ghost"; available skills: none` {
		t.Fatalf("empty catalog error %v", errEmpty)
	}
}

func TestLoadSkillFailsWhenTheRunPlanCannotBeBuilt(t *testing.T) {
	// A failing skill store is reported as a listing error, not as an unknown skill.
	// Arrange
	boom := errors.New("store down")
	reg := skills.NewRegistry(skillFS(), failingStore{boom})
	p := &skilltools.Provider{Registry: reg, Installer: &skills.Installer{Registry: reg}, Sandbox: &manager{enabled: true}}
	list, _, _ := p.Tools(context.Background(), tools.RunContext{RunID: "r"})

	// Act
	_, err := list[0].(ai.InvokableTool).InvokableRun(context.Background(), `{"name":"docx"}`)

	// Assert
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "listing skills") {
		t.Fatalf("error %v", err)
	}
}

type failingStore struct{ err error }

func (f failingStore) RunSkills(context.Context, string) ([]string, error) { return nil, f.err }

func TestLoadSkillOfACatalogSkillFoundGoneFromTheRegistryFails(t *testing.T) {
	// A skill in the run's plan but unreadable afterwards is reported with its name, not as success.
	// Arrange
	reg := skills.NewRegistry(nil, nil)
	reg.AddFS(skills.SourceUser, skillFS()) // catalog only: not attached by default
	other := skills.NewRegistry(nil, nil)
	inst := &skills.Installer{Registry: reg}
	rc := tools.RunContext{ThreadID: "th", RunID: "r"}
	if _, err := inst.Plan(context.Background(), rc); err != nil { // the plan lists docx, cached for the run
		t.Fatal(err)
	}
	inst.Registry = other // the registry no longer has it
	p := &skilltools.Provider{Registry: reg, Installer: inst, Sandbox: &manager{enabled: true}}
	list, _, _ := p.Tools(context.Background(), rc)

	// Act
	_, err := list[0].(ai.InvokableTool).InvokableRun(context.Background(), `{"name":"docx"}`)

	// Assert
	if !errors.Is(err, skills.ErrNotFound) || !strings.Contains(err.Error(), "loading skill docx") {
		t.Fatalf("error %v", err)
	}
}

func TestLoadSkillWhenTheSandboxIsGoneInvalidatesItAndStillAnswers(t *testing.T) {
	// A sandbox found gone during the install is dropped from the manager and the model still gets the instructions (installed at the next command); other install failures are retried before the next command.
	// Arrange
	gone := newRig(t, nil)
	gone.sb.Die()
	failing := newRig(t, nil)
	failing.sb.SetWriteErr(func(string) error { return errors.New("disk full") })

	// Act
	goneOut, goneErr := gone.load("docx")
	failOut, failErr := failing.load("docx")

	// Assert
	if goneErr != nil || !strings.Contains(goneOut, "from your first command") {
		t.Fatalf("gone: %v\n%s", goneErr, goneOut)
	}
	if len(gone.mgr.invalidated) != 1 || gone.mgr.invalidated[0] != "th-1/sb-1" {
		t.Fatalf("invalidated %v", gone.mgr.invalidated)
	}
	if failErr != nil || !strings.Contains(failOut, "from your first command") || len(failing.mgr.invalidated) != 0 {
		t.Fatalf("failing: %v invalidated %v\n%s", failErr, failing.mgr.invalidated, failOut)
	}
}

func TestLoadSkillRejectsMalformedArguments(t *testing.T) {
	// Arguments that are not a JSON object fail before any skill is touched.
	// Arrange
	r := newRig(t, nil)

	// Act
	_, err := r.tool.InvokableRun(context.Background(), `not json`)

	// Assert
	if err == nil || len(r.sb.Execs()) != 0 {
		t.Fatalf("error %v", err)
	}
}

var _ store.SkillStore = runSkills(nil)
