package harness

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	aiagent "github.com/7ar-chat/harness/ai/agent"
	"github.com/7ar-chat/harness/coding/codecheck"
	"github.com/7ar-chat/harness/coding/gitsafe"
	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/approvals"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/explore"
	"github.com/7ar-chat/harness/features/mcp"
	"github.com/7ar-chat/harness/features/permissions"
	"github.com/7ar-chat/harness/features/plan"
	"github.com/7ar-chat/harness/features/projectdocs"
)

// Names of the built-in features (Feature.Name, Builder.Has, Builder.Require).
const (
	FeatureSandbox     = "sandbox"
	FeatureCodecheck   = "codecheck"
	FeatureExplore     = "explore"
	FeatureSkills      = "skills"
	FeatureMCP         = "mcp"
	FeaturePlan        = "plan"
	FeatureProjectDocs = "projectdocs"
	FeatureArtifacts   = "artifacts"
	FeatureWorkspace   = "workspace"
	FeatureTitles      = "titles"
	FeatureVerify      = "verify"
	FeatureGoals       = "goals"
	FeatureMemory      = "memory"
	FeatureToolProfile = "toolprofile"
	FeaturePermissions = "permissions"
)

type builtin struct {
	name    string
	install func(*Builder) error
}

func (f builtin) Name() string             { return f.name }
func (f builtin) Install(b *Builder) error { return f.install(b) }

// Sandbox offers the sandbox tools: execute_command, job_output, job_kill,
// read_file, view_file, search_files, write_file, edit_file, list_files. Other
// features hook into them through Builder.SandboxTools. Without a configured
// sandbox backend it contributes nothing.
func Sandbox() Feature {
	return builtin{FeatureSandbox, func(b *Builder) error {
		if !b.Sandboxes.Enabled() {
			return nil
		}
		late := &lateProvider{name: "sandbox"}
		b.sandboxTools = late
		b.AddTools(late)
		b.After(func() error {
			opts := sandboxtools.Options{CommandTimeout: b.Config.CommandTimeout(), MaxSandboxesPerUser: b.Config.MaxSandboxesPerUser}
			for _, f := range b.sbOpts {
				f(&opts)
			}
			if b.Infra.Jobs != nil {
				opts.Jobs = b.Infra.Jobs
			}
			p := sandboxtools.New(b.Sandboxes, opts)
			late.p = p
			if p != nil && b.Infra.Jobs != nil {
				b.Background(func(ctx context.Context) { p.KeepResumingJobs(ctx, 2*time.Minute, nil) })
			}
			return nil
		})
		return nil
	}}
}

// Codecheck adds a syntax check to the notice of write_file and edit_file
// (the coding agent's immediate feedback; AGENT_CODE_CHECKS adds commands).
// With coding false it only installs when AGENT_CODE_CHECKS is set. It
// requires the Sandbox feature.
func Codecheck(coding bool) Feature {
	return builtin{FeatureCodecheck, func(b *Builder) error {
		if err := b.Require(FeatureSandbox); err != nil {
			return err
		}
		if !coding && strings.TrimSpace(b.Config.CodeChecks) == "" {
			return nil
		}
		extra, err := codecheck.Parse(b.Config.CodeChecks)
		if err != nil {
			return err
		}
		check := codecheck.New(extra).Check
		b.SandboxTools(func(o *sandboxtools.Options) {
			o.CheckWrite = check
			o.Coding = coding
		})
		return nil
	}}
}

// Explore offers the explore tool, a read-only sub-agent that searches and
// reads the workspace and reports back (LLM_EXPLORE_MODEL, default the default
// model). Without a sandbox it contributes nothing. It requires the Sandbox
// feature.
func Explore() Feature {
	return builtin{FeatureExplore, func(b *Builder) error {
		if err := b.Require(FeatureSandbox); err != nil {
			return err
		}
		if b.sandboxTools == nil {
			return nil
		}
		if m := cmp.Or(b.Config.ExploreModel, b.Config.LLMDefaultModel); m != "" {
			b.AddTools(&exploretools.Provider{Source: b.sandboxTools, Models: b.Models, Model: m, Caller: &aiagent.Caller{
				FirstChunk: time.Duration(b.Config.LLMTimeout) * time.Second, Idle: b.Config.StreamIdle(), Ceiling: b.Config.CallCeiling(),
				Retries: agent.StepRetriesSetting(b.Config.LLMStepRetries),
			}})
		}
		return nil
	}}
}

// MCP offers the tools of the MCP servers of the configuration (MCP_SERVERS).
func MCP() Feature {
	return builtin{FeatureMCP, func(b *Builder) error {
		var pending []config.MCPServer
		if b.Config.Layers != nil {
			pending = b.Config.Layers.MCPPending
		}
		p := mcptools.NewWithPending(b.Config.MCPServers, pending, b.Options.Version, mcptools.MaxResultChars)
		if p != nil {
			p.CallTimeout = b.Config.MCPCallTimeout()
			if b.Config.Layers != nil {
				p.TrustDir = b.Config.Layers.UserDir
			}
			if b.Store != nil {
				p.History = b.Store.ListMessages
			}
			b.AddTools(p)
			// A pending project server is only offered for approval where the approval gate asks the user.
			b.Configure(func(hs *agent.Harness) {
				for _, h := range hs.BeforeTool {
					if hk, ok := h.(*permissions.Hook); ok && hk.Inner != nil {
						h = hk.Inner
					}
					if g, ok := h.(*approvals.Gate); ok {
						g.Describe = p.Describe
						p.Ask = true
					}
				}
			})
		}
		return nil
	}}
}

// stdioToolGlobs are the tool name globs of the MCP servers that run a local program.
func stdioToolGlobs(cfg *config.Config) []string {
	var out []string
	for _, s := range cfg.MCPServers {
		if s.IsStdio() {
			out = append(out, strings.TrimSuffix(mcptools.ToolName(s.Name, "x"), "x")+"*")
		}
	}
	return out
}

// Plan offers update_plan and ask_user.
func Plan() Feature {
	return builtin{FeaturePlan, func(b *Builder) error {
		b.AddTools(harnesstools.New(b.Store))
		return nil
	}}
}

// ProjectDocs adds the instructions of the project and of the user (AGENTS.md,
// CLAUDE.md, GEMINI.md, Cursor and Copilot files, .har/instructions.md; see
// package projectdocs) to the prompt, and the notes of subdirectories to the
// results of read_file, write_file and edit_file. The user's own ~/.har/AGENTS.md
// is read with the local backend, or when HARNESS_USER_INSTRUCTIONS is set.
func ProjectDocs() Feature {
	return builtin{FeatureProjectDocs, func(b *Builder) error {
		o := projectdocs.Options{Manager: b.Sandboxes}
		if b.Config.LocalSandbox() || b.Config.UserInstructions {
			o.UserDir = config.UserDir()
		}
		docs := projectdocs.New(o)
		b.Section(docs.Section)
		b.SandboxTools(func(so *sandboxtools.Options) {
			prev := so.AfterPath
			so.AfterPath = func(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox, p string) string {
				note := docs.AfterPath(ctx, rc, sb, p)
				if prev != nil {
					note = strings.TrimSpace(prev(ctx, rc, sb, p) + "\n" + note)
				}
				return note
			}
		})
		return nil
	}}
}

// ToolProfile applies the tool profile: with coding true the lean coding
// allow-list (no document viewer, skills only when the user has some, no
// memory unless AGENT_MEMORY_IN_CODING), and in both cases the AGENT_TOOLS
// overrides. It also drops the prompt sections of the skills and memory
// features whose tools it hides. It takes effect on the
// Skills and Memory features whatever the order.
func ToolProfile(coding bool) Feature {
	return builtin{FeatureToolProfile, func(b *Builder) error {
		reg := b.prof.reg
		*b.prof = *newProfile(b.Config, reg, coding)
		b.WrapTools(b.prof.Wrap)
		return nil
	}}
}

// Permissions puts the permission layer (allow/ask/deny rules, modes;
// AGENT_PERMISSIONS*) in front of the tools and snapshots the git working tree
// at the start of runs on the local backend. It applies to the local backend
// and, with coding true, to the coding agent on a remote sandbox; with neither
// it contributes nothing (the work profile on a remote sandbox).
//
// The mode is AGENT_PERMISSIONS; without it the local backend asks and the
// coding agent on a remote sandbox, which is isolated, does not (yolo: deny
// rules still apply). The interactive approvals gate stays behind it for the
// tools the policy has no rule for.
func Permissions(coding bool) Feature {
	return builtin{FeaturePermissions, func(b *Builder) error {
		cfg := b.Config
		if !cfg.LocalSandbox() && !coding {
			return nil
		}
		ws := cfg.LocalWorkspace
		if ws == "" && cfg.LocalSandbox() {
			ws, _ = os.Getwd()
		}
		mode := cfg.AgentPermissions
		if strings.TrimSpace(mode) == "" && !cfg.LocalSandbox() {
			mode = string(permissions.ModeYolo)
		}
		policy, err := permissions.Load(permissions.Options{
			Workspace: ws, Mode: mode,
			Allow: cfg.AgentPermissionsAllow, Ask: cfg.AgentPermissionsAsk, Deny: cfg.AgentPermissionsDeny,
			AskTools: stdioToolGlobs(cfg), Trusted: cfg.Layers != nil && cfg.Layers.Trusted,
		})
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		b.Configure(func(hs *agent.Harness) {
			var out []agent.BeforeToolHook
			wrapped := false
			for _, h := range hs.BeforeTool {
				if g, ok := h.(*approvals.Gate); ok && !wrapped {
					out = append(out, &permissions.Hook{Policy: policy, Inner: g})
					wrapped = true
					continue
				}
				out = append(out, h)
			}
			hs.BeforeTool = out
			if cfg.LocalSandbox() && ws != "" {
				hs.RunStart = append(hs.RunStart, gitsafe.Hook{Dir: ws})
			}
		})
		return nil
	}}
}

var _ tools.Provider = (*lateProvider)(nil)
