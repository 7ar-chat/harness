package harness

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/coding/sandboxtools"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/agent/prompts"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/artifacts"
	"github.com/GhaziBenDahmane/harness/features/artifacts/workspace"
	"github.com/GhaziBenDahmane/harness/features/goals"
	"github.com/GhaziBenDahmane/harness/features/memory"
	"github.com/GhaziBenDahmane/harness/features/memory/memorytools"
	"github.com/GhaziBenDahmane/harness/features/skills"
	"github.com/GhaziBenDahmane/harness/features/skills/skillsfs"
	"github.com/GhaziBenDahmane/harness/features/skills/skilltools"
	"github.com/GhaziBenDahmane/harness/features/titles"
	"github.com/GhaziBenDahmane/harness/features/verify"
)

// Skills offers the skills (<name>/SKILL.md folders): the load_skill tool, the
// skills index in the prompt, and their installation into sandboxes. The
// built-in skills, Options.ExtraSkills and the user directory (SKILLS_DIR) are
// read; the project skills come from the host with the local backend and from each
// thread's own /workspace with a remote one (skills.LoadSandbox). It publishes Builder.Skills and Builder.Installer.
func Skills() Feature {
	return builtin{FeatureSkills, func(b *Builder) error {
		builtinFS := b.Options.BuiltinSkills
		if builtinFS == nil {
			builtinFS = skillsfs.Builtin()
		}
		reg := skills.NewRegistry(builtinFS, b.Store)
		reg.AddFS(skills.SourceExtra, b.Options.ExtraSkills)
		reg.Discover(skillDiscovery(b.Config))
		installer := &skills.Installer{Registry: reg, Shared: b.Infra.Installs, Messages: b.Store,
			SandboxProject: !b.Config.LocalSandbox(), Sandboxes: b.Sandboxes}
		b.Skills, b.Installer = reg, installer
		b.prof.reg = reg

		b.SandboxTools(func(o *sandboxtools.Options) {
			prev := o.OnAcquire
			o.OnAcquire = func(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox) {
				if prev != nil {
					prev(ctx, rc, sb)
				}
				installer.EnsureRunSkills(ctx, rc, sb)
			}
			o.BeforeCommand = installer.EnsureCommandSkills
			o.OnWrite = installer.NoteContent
			o.AfterFailedCommand = installer.AfterFailedCommand
		})
		b.AddTools(&skilltools.Provider{Registry: reg, Installer: installer, Sandbox: b.Sandboxes})
		b.Section(b.prof.skillsSection(installer.PromptSection))
		b.StepNote(agent.StepNote(b.prof.skillsSection(installer.StepNote)))
		return nil
	}}
}

// skillDiscovery says which host directories hold skills. Project skills (the
// repository's .har, .agents and .claude folders) and the skills of the user's
// other agent tools are read with the local backend only, where the host
// directories are the user's own; a remote sandbox keeps its files to itself.
func skillDiscovery(cfg *config.Config) skills.DiscoverOptions {
	o := skills.DiscoverOptions{SkillsDir: strings.TrimSpace(cfg.SkillsDir), UserDir: config.UserDir(), NoUserDir: config.NoUserDir()}
	if cfg.LocalSandbox() {
		o.ForeignUserDirs = true
		o.ProjectRoot = cfg.LocalWorkspace
		if o.ProjectRoot == "" && cfg.Layers != nil {
			o.ProjectRoot = cfg.Layers.ProjectRoot
		}
		if o.ProjectRoot == "" {
			o.ProjectRoot, _ = os.Getwd()
		}
	}
	return o
}

// Artifacts tracks the files the agent produces (snapshots on tool results,
// previews, links in answers, image attachments) and purges deleted threads'
// data after HARNESS_PURGE_AFTER_DAYS. Contents go to Options.Blobs (default:
// the Store). It publishes Builder.Artifacts.
func Artifacts() Feature {
	return builtin{FeatureArtifacts, func(b *Builder) error {
		cfg := b.Config
		svc := artifacts.New(b.Store, b.Blobs, b.Sandboxes, artifacts.Options{PreviewConcurrency: cfg.PreviewMaxConcurrency,
			BlobGCGrace: time.Duration(cfg.BlobGCGraceHours) * time.Hour})
		b.Artifacts = svc
		b.ToolHook(svc)
		if err := b.SetAnswers(svc); err != nil {
			return err
		}
		if err := b.SetImages(artifacts.Images{Svc: svc, Vision: cfg.SupportsVision}); err != nil {
			return err
		}
		b.OnSandboxCreate(func(ctx context.Context, threadID string, sb sandbox.Sandbox, recreated bool) string {
			if cfg.LocalSandbox() {
				return "" // the host keeps its files; nothing to restore into the user's project
			}
			return svc.RestoreWorkspace(ctx, threadID, sb, recreated)
		})
		b.EndOfRun(artifacts.EndOfRunHook{Svc: svc})
		return b.SetPurger(artifacts.PurgeJob{Svc: svc, After: time.Duration(cfg.PurgeAfterDays) * 24 * time.Hour})
	}}
}

// Workspace checkpoints /workspace in a local Git history before every turn
// (per-file history, diff, restore), archives it after the run, and tells the
// model about it. It requires the Artifacts feature and publishes
// Builder.Workspace.
func Workspace() Feature {
	return builtin{FeatureWorkspace, func(b *Builder) error {
		if err := b.Require(FeatureArtifacts); err != nil {
			return err
		}
		cfg := b.Config
		svc := workspace.New(b.Sandboxes, b.Store, b.Store, b.Artifacts, workspace.Options{
			Disabled: !cfg.WorkspaceCheckpoints || cfg.LocalSandbox(), ArchiveMaxBytes: cfg.GitArchiveMaxBytes,
			MaxFileBytes: cfg.CheckpointMaxFile})
		b.Workspace = svc
		b.PrepareSandbox(func(ctx context.Context, threadID string, sb sandbox.Sandbox, _ bool) string {
			if !cfg.LocalSandbox() {
				svc.OnCreate(ctx, threadID, sb) // the checkpoint history first, then the files
			}
			return ""
		})
		b.SandboxTools(func(o *sandboxtools.Options) {
			prev := o.OnAcquire
			o.OnAcquire = func(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox) {
				if prev != nil {
					prev(ctx, rc, sb)
				}
				svc.OnAcquire(ctx, rc, sb) // "Before AI turn" checkpoint of a sandbox created mid-run
			}
		})
		b.Section(agent.Part(prompts.Git, func(ctx context.Context, in agent.Input) string {
			if svc.PromptSection(ctx, in) == "" {
				return ""
			}
			return prompts.GitCheckpoints
		}))
		b.RunStart(svc)
		b.EndOfRun(workspace.EndOfRunHook{Svc: svc})
		return nil
	}}
}

// Titles names new threads from their first exchange.
func Titles() Feature {
	return builtin{FeatureTitles, func(b *Builder) error {
		b.EndOfRun(&titles.Hook{Store: b.Store, Models: b.Models, SupportsReasoning: b.Config.SupportsReasoning})
		return nil
	}}
}

// Verify runs the checkers of the deliverables the agent writes and keeps the
// agent from finishing on an unverified one. It requires the Artifacts
// feature (and uses Skills, when installed, to install a checker's skill) and
// publishes Builder.Verifier.
func Verify() Feature {
	return builtin{FeatureVerify, func(b *Builder) error {
		if err := b.Require(FeatureArtifacts); err != nil {
			return err
		}
		o := verify.Options{Sandbox: b.Sandboxes, Messages: b.Store, Artifacts: b.Artifacts}
		if b.Installer != nil {
			o.EnsureSkill = b.Installer.EnsureSkill
		}
		v := verify.New(o)
		b.Verifier = v
		return b.SetChecker(v, v)
	}}
}

// Goals lets a thread carry a goal that is judged after every run and, while
// unmet, continues with another run. It uses Artifacts and Verify, when
// installed, to show the judge what was produced.
func Goals() Feature {
	return builtin{FeatureGoals, func(b *Builder) error {
		h := &goals.Hook{Store: b.Store, Models: b.Models, Queue: b.Infra.Broker, SupportsReasoning: b.Config.SupportsReasoning}
		if b.Artifacts != nil {
			h.ListArtifacts = b.Artifacts.ProducedPaths
		}
		if b.Verifier != nil {
			h.Evidence = b.Verifier.Evidence
		}
		b.ContextSection(goals.ContextSection(b.Store))
		b.TailSection(goals.TurnsSection(b.Store))
		b.EndOfRun(h)
		return nil
	}}
}

// Memory gives the agent a long-term memory: the memory tools, the memory
// prompt and the recall of relevant entries, and a periodic consolidation job.
// It publishes Builder.Memory.
func Memory() Feature {
	return builtin{FeatureMemory, func(b *Builder) error {
		cfg := b.Config
		svc := memory.New(b.Store, memoryOptions(cfg))
		svc.KeyFor, svc.UserKeyFor, svc.Shared = b.Options.MemoryKey, b.Options.MemoryUserKey, b.Options.MemoryShared
		svc.Messages = b.Store.ListMessages
		b.Memory = svc
		if svc.Options().Extract {
			b.EndOfRun(&memory.Extractor{Svc: svc, Models: b.Models, SupportsReasoning: b.Config.SupportsReasoning})
		}
		b.AddTools(memorytools.New(svc))
		b.Section(b.prof.memorySection(memory.PromptSection(svc)))
		b.ContextSection(b.prof.memorySection(memory.RecallSection(svc)))
		b.Job(runtime.Job{Name: "memory-consolidation", Every: time.Duration(max(cfg.MemoryConsolidationHours, 0)) * time.Hour,
			Run: memory.Consolidator{Store: b.Store, Retention: time.Duration(max(cfg.MemoryDeletedRetentionDays, 0)) * 24 * time.Hour}.Run})
		return nil
	}}
}

// memoryOptions maps the HARNESS_MEMORY_* settings to the memory service's options.
func memoryOptions(cfg *config.Config) memory.Options {
	o := memory.Options{
		WriteMode: cfg.MemoryWriteMode, DeleteMode: cfg.MemoryDeleteMode, DefaultScope: cfg.MemoryDefaultScope,
		EmbeddingBackend: cfg.MemoryEmbeddingBackend, RecallInjection: cfg.MemoryRecallInjection,
		MaxEntries: cfg.MemoryMaxEntries, MaxContentBytes: cfg.MemoryMaxContentBytes,
		Extract: cfg.MemoryExtract, ExtractModel: cfg.MemoryExtractModel,
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = -1 // "≤ 0: no limit" is the service's negative value (0 is its default)
	}
	if strings.EqualFold(strings.TrimSpace(cfg.MemoryEmbeddingBackend), "openai") {
		url, key := cfg.MemoryEmbeddingURL, cfg.MemoryEmbeddingKey
		if url == "" {
			url = cfg.LLMBaseURL
		}
		if key == "" {
			key = cfg.LLMAPIKey
		}
		o.Embedder = &memory.OpenAIEmbedder{BaseURL: url, APIKey: key, ModelName: cfg.MemoryEmbeddingModel}
	}
	return o
}
