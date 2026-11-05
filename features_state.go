package harness

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/artifacts"
	"github.com/7ar-chat/harness/features/artifacts/workspace"
	"github.com/7ar-chat/harness/features/browsersandbox"
	"github.com/7ar-chat/harness/features/documents"
	"github.com/7ar-chat/harness/features/goals"
	"github.com/7ar-chat/harness/features/memory"
	"github.com/7ar-chat/harness/features/memory/memorytools"
	"github.com/7ar-chat/harness/features/skills"
	"github.com/7ar-chat/harness/features/skills/skillsfs"
	"github.com/7ar-chat/harness/features/skills/skilltools"
	"github.com/7ar-chat/harness/features/titles"
	"github.com/7ar-chat/harness/features/verify"
	"github.com/7ar-chat/harness/prompts"
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

// Documents is the users' document library (a CV, templates...): stored under HARNESS_DOCUMENTS_DIR, named in the prompt,
// listed and copied into the sandbox by tools. Without HARNESS_DOCUMENTS_DIR it contributes nothing.
func Documents() Feature {
	return builtin{FeatureDocuments, func(b *Builder) error {
		if b.Config.DocumentsDir == "" {
			return nil
		}
		svc := documents.New(b.Config.DocumentsDir)
		svc.BrowserRoot = b.Config.DocumentsBrowserRoot
		svc.Sandboxes = b.Sandboxes
		svc.HTTP = documents.PublicClient() // addresses a model read on web pages: never a private or loopback host
		b.Documents = svc
		b.AddTools(documents.Provider{Svc: svc})
		b.Section(svc.PromptSection)
		return nil
	}}
}

// BrowserServer is the name of the MCP server whose tools drive a web browser.
const BrowserServer = "browser"

// Browser tells the agent how to use the browser tools and how to apply for jobs, when an MCP server named "browser"
// (MCP_SERVERS) is configured. Without it, it contributes nothing.
func Browser() Feature {
	return builtin{FeatureBrowser, func(b *Builder) error {
		if kind := b.Config.BrowserSandboxBackend(); kind != "" {
			backend := b.Options.BrowserBackend
			if backend == nil {
				var err error
				if backend, err = NewSandboxBackend(b.Config.BrowserSandboxConfig()); err != nil {
					return fmt.Errorf("BROWSER_BACKEND=%s: %w", kind, err)
				}
			}
			m := browsersandbox.New(backend)
			if b.Documents != nil {
				m.Docs = browserLibrary{b.Documents}
			}
			m.Idle = time.Duration(b.Config.BrowserIdleMinutes) * time.Minute
			b.BrowserSandboxes = m
			b.BrowserServers = func(ctx context.Context, threadID, login string) ([]config.MCPServer, error) {
				servers, err := m.Servers(ctx, threadID, login)
				out := make([]config.MCPServer, 0, len(servers))
				for _, s := range servers {
					out = append(out, config.MCPServer{Name: BrowserServer, URL: s.URL, Headers: s.Headers,
						Tools: append([]string(nil), config.BrowserTools...), Origin: "BROWSER_BACKEND"})
				}
				return out, err
			}
			b.Section(browserSection)
			return nil
		}
		for _, s := range b.Config.MCPServers {
			if s.Name == BrowserServer {
				b.Section(browserSection)
				return nil
			}
		}
		return nil
	}}
}

// browserSection is the browser rules; the job-application mode has its own routine, so it gets only how to drive a page.
func browserSection(_ context.Context, in agent.Input) string {
	if in.Variant == prompts.Jobs {
		return prompts.BrowserBasics
	}
	return prompts.Browser
}

// memoryOptions maps the HARNESS_MEMORY_* settings to the memory service's options.
func memoryOptions(cfg *config.Config) memory.Options {
	o := memory.Options{
		WriteMode: cfg.MemoryWriteMode, DeleteMode: cfg.MemoryDeleteMode, DefaultScope: cfg.MemoryDefaultScope,
		EmbeddingBackend: cfg.MemoryEmbeddingBackend, RecallInjection: cfg.MemoryRecallInjection,
		MaxEntries: cfg.MemoryMaxEntries, MaxContentBytes: cfg.MemoryMaxContentBytes,
		Extract: cfg.MemoryExtract, ExtractModel: cfg.MemoryExtractModel, ExtractAutoConfirm: cfg.MemoryExtractAutoConfirm,
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

// browserLibrary lets a browser sandbox read the user's documents: they are copied to /docs/<tag>/, where the browser's
// file chooser and documents_list's browser path expect them.
type browserLibrary struct{ svc *documents.Service }

func (l browserLibrary) Signature(login string) (string, error) {
	docs, err := l.svc.List(login)
	if err != nil {
		return "", err
	}
	var sig strings.Builder
	for _, d := range docs {
		fmt.Fprintf(&sig, "%s:%d:%d;", d.Name, d.Size, d.Modified.UnixNano())
	}
	return sig.String(), nil
}

func (l browserLibrary) CopyTo(ctx context.Context, login string, sb sandbox.Sandbox) error {
	_, err := l.svc.CopyToDir(ctx, login, sb, path.Join(browsersandbox.DocsDir, documents.Tag(login)), "")
	return err
}
