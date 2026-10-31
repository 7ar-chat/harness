package runtime_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/coding/sandboxtools"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/artifacts"
	"github.com/GhaziBenDahmane/harness/features/goals"
	"github.com/GhaziBenDahmane/harness/features/memory/memorytools"
	"github.com/GhaziBenDahmane/harness/features/plan"
	"github.com/GhaziBenDahmane/harness/features/skills"
	"github.com/GhaziBenDahmane/harness/features/skills/skilltools"
)

// scriptedRig wires artifacts, skills and attachments like cmd/work/main.go.
type scriptedRig struct {
	*rig
	arts *artifacts.Service
	reg  *skills.Registry
	inst *skills.Installer
}

func newScriptedRig(t *testing.T, vision bool, turns ...llmtest.Turn) *scriptedRig {
	t.Helper()
	return newScriptedRigWith(t, os.DirFS(filepath.Join("..", "..", "features", "skills", "testdata", "builtin")), vision, turns...)
}

// newScriptedRigWith is newScriptedRig with the built-in skills of builtin (e.g. the real ones, skillsfs.Builtin()).
func newScriptedRigWith(t *testing.T, builtin fs.FS, vision bool, turns ...llmtest.Turn) *scriptedRig {
	t.Helper()
	r := newRig(t, turns...)
	v := &scriptedRig{rig: r}
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) { sb.SetExec(sandboxtest.FindExec(sb, nil)) }
	v.arts = artifacts.New(r.st, artifacts.PGBlobs{S: r.st}, nil, artifacts.Options{})
	v.reg = skills.NewRegistry(builtin, r.st)
	v.inst = &skills.Installer{Registry: v.reg, Shared: redisx.NewSkillInstalls(r.rdb)}
	h := &r.agent.Harness
	h.Sections = append([]agent.PromptSection{v.reg.PromptSection, artifacts.PromptSection}, h.Sections...)
	h.ToolHooks = []agent.ToolHook{v.arts}
	h.Answers = v.arts
	h.Images = artifacts.Images{Svc: v.arts, Vision: func(m string) bool { return vision }}
	v.wire()
	// Like cmd/work: the end-of-run snapshot first, the goal judge sees the artifacts.
	for _, hook := range r.opts.Hooks {
		if g, ok := hook.(*goals.Hook); ok {
			g.ListArtifacts = v.arts.ProducedPaths
		}
	}
	r.opts.Hooks = append([]runtime.Hook{artifacts.EndOfRunHook{Svc: v.arts}}, r.opts.Hooks...)
	return v
}

// wire (re)creates the sandbox manager and the tools, like a runtime process start.
func (v *scriptedRig) wire() {
	v.mgr = sandbox.NewManager(v.backend, v.st, redisx.NewCoordinator(v.rdb), sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour,
		OnCreate: func(ctx context.Context, threadID string, sb sandbox.Sandbox, recreated bool) string {
			return v.arts.RestoreWorkspace(ctx, threadID, sb, recreated)
		}})
	v.arts.Sandbox = v.mgr
	v.inst = &skills.Installer{Registry: v.reg, Shared: redisx.NewSkillInstalls(v.rdb), Messages: v.st}
	v.agent.Tools = tools.NewRegistry(
		sandboxtools.New(v.mgr, sandboxtools.Options{CommandTimeout: 5 * time.Second, MaxSandboxesPerUser: 5, OnAcquire: v.inst.EnsureRunSkills,
			BeforeCommand: v.inst.EnsureCommandSkills, OnWrite: v.inst.NoteContent, AfterFailedCommand: v.inst.AfterFailedCommand}),
		&skilltools.Provider{Registry: v.reg, Installer: v.inst, Sandbox: v.mgr},
		harnesstools.New(v.st),
		memorytools.New(v.mem),
	)
	v.rt = nil
}

// sandbox returns the thread's (single) sandbox created during the run.
func (v *scriptedRig) sandbox() *sandboxtest.Sandbox {
	v.t.Helper()
	created := v.backend.Created()
	if len(created) == 0 {
		v.t.Fatal("no sandbox created")
	}
	return created[len(created)-1]
}
