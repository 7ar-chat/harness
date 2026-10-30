// Package config loads the service configuration from environment variables.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config is the whole service configuration. See README "Configuration".
type Config struct {
	Port int `env:"PORT" envDefault:"8000"`
	// Production is on when HARNESS_PRODUCTION has any non-empty value
	// ("1", "true", "yes" alike).
	Production    bool   `env:"-"`
	ProductionRaw string `env:"HARNESS_PRODUCTION"`

	PostgresHost     string `env:"POSTGRES_HOST" envDefault:"postgres"`
	PostgresPort     string `env:"POSTGRES_PORT" envDefault:"5432"`
	PostgresDB       string `env:"POSTGRES_DB" envDefault:"db"`
	PostgresUser     string `env:"POSTGRES_USER" envDefault:"postgres"`
	PostgresPassword string `env:"POSTGRES_PASSWORD" envDefault:""`
	PostgresSchema   string `env:"POSTGRES_SCHEMA" envDefault:"work"`
	PostgresSSLMode  string `env:"POSTGRES_SSLMODE" envDefault:"require"`
	DBPoolMaxConns   int    `env:"DB_POOL_MAX_CONNS" envDefault:"20"`
	// PingDBAtStartup=false starts without touching Postgres/Redis (lazy connections), like authn.
	PingDBAtStartup bool `env:"PING_DB_AT_STARTUP" envDefault:"true"`

	UseSentinel        bool   `env:"USE_SENTINEL" envDefault:"false"`
	RedisSentinelHosts string `env:"REDIS_SENTINEL_HOSTS"`
	RedisMasterName    string `env:"REDIS_MASTER_NAME" envDefault:"mymaster"`
	RedisPassword      string `env:"REDIS_PASSWORD"`
	RedisHost          string `env:"REDIS_HOST" envDefault:"redis"`
	RedisPort          string `env:"REDIS_PORT" envDefault:"6379"`
	RedisDB            int    `env:"REDIS_DB" envDefault:"0"`
	// RedisPoolSize must exceed concurrent SSE tailers (each holds a connection during XREAD BLOCK).
	RedisPoolSize int `env:"REDIS_POOL_SIZE" envDefault:"256"`

	// LLMProvider is "openai" (any OpenAI-compatible endpoint at LLM_BASE_URL) or "anthropic".
	LLMProvider string `env:"LLM_PROVIDER" envDefault:"openai"`
	LLMBaseURL  string `env:"LLM_BASE_URL"`
	LLMAPIKey   string `env:"LLM_API_KEY"`
	// LLMProviderName is the selected provider (a name of Providers, or openai/anthropic);
	// LLMProvider is its type. LLMKeySource says where LLMAPIKey came from ("env LLM_API_KEY",
	// "auth.json", ...; never the key). Providers are the named providers of the config files.
	LLMProviderName string              `env:"-"`
	LLMKeySource    string              `env:"-"`
	Providers       map[string]Provider `env:"-"`
	keyHint         []string
	// ExploreModel is EXPLORE_MODEL: the model of the read-only explore sub-agent of the coding profile
	// (default LLM_DEFAULT_MODEL; a cheaper model saves the most).
	ExploreModel string `env:"EXPLORE_MODEL"`
	// CodeChecks is CODE_CHECKS: ".ext=command;…" syntax checks run after write_file/edit_file ({path} is the
	// file; see codecheck.Defaults). They are on with AGENT_PROFILE=coding, or whenever this is set.
	CodeChecks string `env:"CODE_CHECKS"`
	// LLMSessionHeader is LLM_SESSION_HEADER: the name of a request header that carries the thread id, for
	// providers that route a conversation to one backend by session (OpenCode Go: x-opencode-session).
	LLMSessionHeader string `env:"LLM_SESSION_HEADER"`
	// LLMCacheTTL is the prompt-cache lifetime of the anthropic provider: "5m" (default) or "1h".
	LLMCacheTTL     string   `env:"LLM_CACHE_TTL"`
	LLMDefaultModel string   `env:"LLM_DEFAULT_MODEL" envDefault:"gpt-5.6-terra"`
	LLMModels       []string `env:"LLM_MODELS" envDefault:"gpt-5.6-sol,gpt-5.6-terra" envSeparator:","`
	LLMTimeout      int      `env:"LLM_TIMEOUT_SECONDS" envDefault:"300"`

	AgentMaxSteps           int `env:"AGENT_MAX_STEPS" envDefault:"60"`
	AgentMaxToolOutputChars int `env:"AGENT_MAX_TOOL_OUTPUT_CHARS" envDefault:"30000"`

	RuntimeConcurrency   int `env:"RUNTIME_CONCURRENCY" envDefault:"16"`
	RuntimeShutdownGrace int `env:"RUNTIME_SHUTDOWN_GRACE_SECONDS" envDefault:"15"`

	// SandboxBackend is "opensandbox" (default; needs OPENSANDBOX_URL) or "local": commands run
	// on this machine in LOCAL_WORKSPACE (default: the current directory), seen as /workspace.
	// The local backend has no isolation; use it on your own machine or inside a container.
	SandboxBackend string `env:"SANDBOX_BACKEND"`
	LocalWorkspace string `env:"LOCAL_WORKSPACE"`
	LocalStateDir  string `env:"LOCAL_STATE_DIR"`
	// AgentPermissions is AGENT_PERMISSIONS: ask (default on the local backend), auto-edit, yolo, yolo-unsafe or
	// readonly (see package permissions). It only applies to the local backend and the coding profile.
	AgentPermissions string `env:"AGENT_PERMISSIONS"`
	// AgentPermissionsAllow|Ask|Deny are AGENT_PERMISSIONS_ALLOW|ASK|DENY: extra rules, separated by newlines or ";;".
	AgentPermissionsAllow string `env:"AGENT_PERMISSIONS_ALLOW"`
	AgentPermissionsAsk   string `env:"AGENT_PERMISSIONS_ASK"`
	AgentPermissionsDeny  string `env:"AGENT_PERMISSIONS_DENY"`
	// UserInstructions is HARNESS_USER_INSTRUCTIONS: read the host user's ~/.har/AGENTS.md even with a remote
	// sandbox backend (the local backend always reads it). Off for servers: the host's home is not the tenants'.
	UserInstructions bool `env:"HARNESS_USER_INSTRUCTIONS"`

	OpenSandboxURL            string `env:"OPENSANDBOX_URL"`
	OpenSandboxAPIKey         string `env:"OPENSANDBOX_API_KEY"`
	OpenSandboxImage          string `env:"OPENSANDBOX_IMAGE" envDefault:"python:3.11-slim"`
	OpenSandboxUseServerProxy bool   `env:"OPENSANDBOX_USE_SERVER_PROXY" envDefault:"false"`
	// OpenSandboxCredentialProxy creates sandboxes with the egress sidecar's Credential Vault, so GitHub
	// authentication is added to outbound requests outside the sandbox and no token enters it. Needs
	// opensandbox-server >= 0.2.0 and egress >= 1.1.1 in dns+nft mode.
	OpenSandboxCredentialProxy bool     `env:"OPENSANDBOX_CREDENTIAL_PROXY" envDefault:"false"`
	SandboxTimeoutSeconds      int      `env:"SANDBOX_TIMEOUT_SECONDS" envDefault:"1800"`
	SandboxCommandTimeout      int      `env:"SANDBOX_COMMAND_TIMEOUT_SECONDS" envDefault:"120"`
	SandboxCPU                 string   `env:"SANDBOX_CPU" envDefault:"1"`
	SandboxMemory              string   `env:"SANDBOX_MEMORY" envDefault:"2Gi"`
	SandboxEgressAllowlist     []string `env:"SANDBOX_EGRESS_ALLOWLIST" envSeparator:","`
	// SandboxEgressUnrestricted explicitly accepts unrestricted sandbox egress in production.
	SandboxEgressUnrestricted bool `env:"SANDBOX_EGRESS_UNRESTRICTED" envDefault:"false"`
	// SandboxEgressDenyCIDRs are blocked in unrestricted mode (other sandboxes, host, proxy, metadata;
	// CGNAT pod ranges, "this network", IPv6 ULA and link-local, IPv4-mapped IPv6). Validated at load (C-19).
	SandboxEgressDenyCIDRs []string `env:"SANDBOX_EGRESS_DENY_CIDRS" envDefault:"172.16.0.0/12,10.0.0.0/8,192.168.0.0/16,169.254.0.0/16,100.64.0.0/10,0.0.0.0/8,fc00::/7,fe80::/10,::ffff:0:0/96" envSeparator:","`
	// SandboxUnhealthyRecreate: a sandbox unhealthy for longer (and checked ≥ 3 times) is recreated.
	SandboxUnhealthyRecreate int `env:"SANDBOX_UNHEALTHY_RECREATE_SECONDS" envDefault:"300"`
	MaxSandboxesPerUser      int `env:"MAX_SANDBOXES_PER_USER" envDefault:"5"`

	MCPServersJSON string `env:"MCP_SERVERS"`
	MCPServers     []MCPServer
	// MCPAllowStdio is HARNESS_MCP_STDIO (or mcp.allow_stdio in the user's config.yml): MCP servers that are
	// local programs (stdio) may be started. Only with the local sandbox backend; a project file cannot set it.
	MCPAllowStdio bool `env:"HARNESS_MCP_STDIO"`

	GlitchtipDSN string `env:"GLITCHTIP_API_DSN"`

	// Harness v2 §1–§3: artifacts, attachments.
	ArtifactsS3Bucket    string `env:"ARTIFACTS_S3_BUCKET"`
	ArtifactsS3Endpoint  string `env:"ARTIFACTS_S3_ENDPOINT"`
	ArtifactsS3Region    string `env:"ARTIFACTS_S3_REGION" envDefault:"us-east-1"`
	ArtifactsS3PathStyle bool   `env:"ARTIFACTS_S3_PATH_STYLE" envDefault:"true"`
	ArtifactsS3Prefix    string `env:"ARTIFACTS_S3_PREFIX" envDefault:"harness/"`
	// ArtifactsS3CreateBucket creates the bucket at startup when missing (local RustFS; production buckets are provisioned).
	ArtifactsS3CreateBucket bool   `env:"ARTIFACTS_S3_CREATE_BUCKET"`
	AWSAccessKeyID          string `env:"AWS_ACCESS_KEY_ID"`
	AWSSecretAccessKey      string `env:"AWS_SECRET_ACCESS_KEY"`
	// MaxUploadBytes bounds one attachment (413 file_too_large beyond).
	MaxUploadBytes int64 `env:"HARNESS_MAX_UPLOAD_BYTES" envDefault:"104857600"`
	// MaxConcurrentUploads bounds uploads per api process (429 too_many_uploads beyond).
	MaxConcurrentUploads int `env:"HARNESS_MAX_CONCURRENT_UPLOADS" envDefault:"4"`
	// MaxConcurrentUploadsPerUser bounds one user's uploads per api process (429 too_many_uploads beyond).
	// Default 4: the composer uploads 4 files in parallel.
	MaxConcurrentUploadsPerUser int `env:"HARNESS_MAX_CONCURRENT_UPLOADS_PER_USER" envDefault:"4"`
	// PreviewMaxConcurrency bounds the Office → PDF conversions per api process.
	PreviewMaxConcurrency int `env:"PREVIEW_MAX_CONCURRENCY" envDefault:"2"`
	// PurgeAfterDays: deleted threads are hard-deleted (with their artifacts and
	// unreferenced blobs) this many days after deletion (≤ 0 disables the purge).
	PurgeAfterDays int `env:"HARNESS_PURGE_AFTER_DAYS" envDefault:"30"`
	// BlobGCGraceHours: a content blob is only deleted once it has had no
	// artifact version referencing it for this many hours (≤ 0: 24), so content
	// reused by a concurrent upload or snapshot is never collected.
	BlobGCGraceHours int `env:"HARNESS_BLOB_GC_GRACE_HOURS" envDefault:"24"`
	// Workspace checkpoints (README "Workspace checkpoints"): a local Git checkpoint of
	// /workspace before every AI turn, per-file history / diff / restore, and the
	// .git history archived (tar.gz, at most HARNESS_GIT_ARCHIVE_MAX_BYTES; a larger
	// one is not archived) into the blob store after each run, restored into
	// every new sandbox of the thread. Files larger than
	// HARNESS_WORKSPACE_CHECKPOINT_MAX_FILE_BYTES (default 20 MiB, ≤ 0: no limit)
	// are kept out of the checkpoints (large uploads would exceed the archive cap).
	WorkspaceCheckpoints bool  `env:"HARNESS_WORKSPACE_CHECKPOINTS" envDefault:"true"`
	GitArchiveMaxBytes   int64 `env:"HARNESS_GIT_ARCHIVE_MAX_BYTES" envDefault:"52428800"`
	CheckpointMaxFile    int64 `env:"HARNESS_WORKSPACE_CHECKPOINT_MAX_FILE_BYTES" envDefault:"20971520"`

	// SkillsDir is the user skills directory (SKILLS_DIR; "" = ~/.har/skills,
	// see SkillsPath): one <name>/SKILL.md folder per skill.
	SkillsDir string `env:"SKILLS_DIR"`

	// Memory (README "Configuration"): write policy (warn records missing
	// triggers, strict blocks them), delete mode (soft keeps the entry as
	// deleted until the retention, hard removes it), default scope, embedding
	// backend (simple = bag-of-words cosine; disabled|none|keyword only
	// change the reported capabilities), the recalled memories in the
	// system prompt, the limits (≤ 0 entries: no limit)
	// and the consolidation job (dedup + purge of deleted entries; ≤ 0 hours disables it).
	MemoryWriteMode            string `env:"HARNESS_MEMORY_WRITE_MODE" envDefault:"warn"`
	MemoryDeleteMode           string `env:"HARNESS_MEMORY_DELETE_MODE" envDefault:"soft"`
	MemoryDefaultScope         string `env:"HARNESS_MEMORY_DEFAULT_SCOPE" envDefault:"personal"`
	MemoryEmbeddingBackend     string `env:"HARNESS_MEMORY_EMBEDDING_BACKEND" envDefault:"simple"`
	MemoryRecallInjection      bool   `env:"HARNESS_MEMORY_RECALL_INJECTION" envDefault:"true"`
	MemoryMaxEntries           int    `env:"HARNESS_MEMORY_MAX_ENTRIES" envDefault:"1000"`
	MemoryMaxContentBytes      int    `env:"HARNESS_MEMORY_MAX_CONTENT_BYTES" envDefault:"200000"`
	MemoryConsolidationHours   int    `env:"HARNESS_MEMORY_CONSOLIDATION_HOURS" envDefault:"168"`
	MemoryDeletedRetentionDays int    `env:"HARNESS_MEMORY_DELETED_RETENTION_DAYS" envDefault:"30"`
	// MemoryExtract proposes memories from each completed run (pending until confirmed), with
	// MemoryExtractModel (default: the run's model). With HARNESS_MEMORY_EMBEDDING_BACKEND=openai,
	// entries and queries get vectors from the OpenAI-compatible endpoint MemoryEmbeddingURL
	// (default: LLM_BASE_URL, else api.openai.com) with MemoryEmbeddingKey (default LLM_API_KEY).
	MemoryExtract        bool   `env:"HARNESS_MEMORY_EXTRACT"`
	MemoryExtractModel   string `env:"HARNESS_MEMORY_EXTRACT_MODEL"`
	MemoryEmbeddingURL   string `env:"HARNESS_MEMORY_EMBEDDING_URL"`
	MemoryEmbeddingKey   string `env:"HARNESS_MEMORY_EMBEDDING_KEY"`
	MemoryEmbeddingModel string `env:"HARNESS_MEMORY_EMBEDDING_MODEL"`
	// Harness v2 §4–§9 (context windows, reasoning/vision models — LLM_VISION_MODELS
	// also decides which models receive attached images —, compaction).
	Harness
	// Harness v3 loop & context (loop.go): stream idle timeout, step retries, tool
	// output spill, in-run masking, output reserve, reasoning passback, MCP call
	// timeout, DeepSeek defaults.
	Loop
	// Harness v3 (stream C2): LLM_PROMPT_VARIANTS, LLM_REASONING_EFFORTS (prompts.go).
	PromptAndEfforts
	// Tool and prompt selection per profile: AGENT_TOOLS, SKILLS_IN_CODING, MEMORY_IN_CODING (toolprofile.go).
	ToolProfile
	// Stream R: models called through the Responses API (LLM_RESPONSES_MODELS, responses.go).
	Responses
	// Harness v3 runtime & API (stream B): RUNTIME_TERMINATION_GRACE_SECONDS,
	// MAX_STREAMS_PER_USER, APPROVAL_TTL_HOURS, APPROVAL_RISKY_TOOL_PATTERNS (runtime_api.go).
	RuntimeAPI

	// Layers says how the configuration was assembled (files, sources of the values); see LoadWith.
	Layers *Layers `env:"-"`

	envSet    func(string) bool
	envLookup func(string) (string, bool)
}

func (c *Config) normalize() error {
	if strings.EqualFold(strings.TrimSpace(c.SandboxBackend), "none") { // sandbox.backend: none
		c.SandboxBackend, c.OpenSandboxURL = "", ""
	}
	c.Production = strings.TrimSpace(c.ProductionRaw) != "" // C-16
	c.LLMModels = trimAll(c.LLMModels)
	if err := c.validateMemory(); err != nil {
		return err
	}
	c.SandboxEgressAllowlist = trimAll(c.SandboxEgressAllowlist)
	c.SandboxEgressDenyCIDRs = trimAll(c.SandboxEgressDenyCIDRs)
	if err := c.validateEgress(); err != nil { // stream A (C-19)
		return err
	}
	if c.LLMDefaultModel != "" && !c.ModelAllowed(c.LLMDefaultModel) {
		c.LLMModels = append([]string{c.LLMDefaultModel}, c.LLMModels...)
	}
	if c.ExploreModel = strings.TrimSpace(c.ExploreModel); c.ExploreModel != "" && !c.ModelAllowed(c.ExploreModel) {
		c.LLMModels = append(c.LLMModels, c.ExploreModel)
	}
	switch c.LLMProvider {
	case "openai", "anthropic":
	default:
		return fmt.Errorf("LLM_PROVIDER must be openai, anthropic or the name of a provider defined under providers: in a config.yml, got %q", c.LLMProvider)
	}
	if strings.TrimSpace(c.MCPServersJSON) != "" {
		if err := json.Unmarshal([]byte(c.MCPServersJSON), &c.MCPServers); err != nil {
			return fmt.Errorf("MCP_SERVERS: %w", err)
		}
		seen := map[string]bool{}
		for _, s := range c.MCPServers {
			if s.Name == "" || (s.URL == "" && s.Command == "") {
				return fmt.Errorf("MCP_SERVERS: every server needs name and url (or command)")
			}
			if seen[s.Name] {
				return fmt.Errorf("MCP_SERVERS: duplicate server name %q", s.Name)
			}
			seen[s.Name] = true
			if err := validURL(s.URL); err != nil {
				return fmt.Errorf("MCP_SERVERS: server %q: %w", s.Name, err)
			}
		}
	}
	if err := c.normalizePromptAndEfforts(); err != nil { // before the presets (efforts per model)
		return err
	}
	if err := c.normalizeHarness(); err != nil {
		return err
	}
	if err := c.normalizeRuntimeAPI(); err != nil {
		return err
	}
	if c.RuntimeConcurrency < 1 {
		c.RuntimeConcurrency = 1
	}
	if c.AgentMaxSteps < 1 {
		c.AgentMaxSteps = 1
	}
	return nil
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// IsDebug reports debug mode: on unless HARNESS_PRODUCTION is set.
func (c *Config) IsDebug() bool { return !c.Production }

// ModelAllowed reports whether a model id is in the LLM_MODELS allow-list.
func (c *Config) ModelAllowed(model string) bool {
	for _, m := range c.LLMModels {
		if m == model {
			return true
		}
	}
	return false
}

// Names of the built-in sandbox backends (SANDBOX_BACKEND).
const (
	BackendOpenSandbox = "opensandbox"
	BackendLocal       = "local"
	BackendNone        = "none"
)

// SandboxBackendName is the name of the selected sandbox backend (lower case):
// SANDBOX_BACKEND when set (a built-in or one a host registered with
// backends.Register), else "opensandbox" when OPENSANDBOX_URL is set, else
// "none".
func (c *Config) SandboxBackendName() string {
	if n := strings.ToLower(strings.TrimSpace(c.SandboxBackend)); n != "" {
		return n
	}
	if strings.TrimSpace(c.OpenSandboxURL) != "" {
		return BackendOpenSandbox
	}
	return BackendNone
}

// SandboxEnabled reports whether sandbox tools are configured.
func (c *Config) SandboxEnabled() bool { return c.SandboxBackendName() != BackendNone }

// LocalSandbox reports whether tools run on this machine (SANDBOX_BACKEND=local).
func (c *Config) LocalSandbox() bool {
	return c.SandboxBackendName() == BackendLocal
}

// EgressMode is the effective sandbox egress: "disabled" (no sandbox), "allowlist"
// or "unrestricted". In production unrestricted egress must be explicitly accepted
// with SANDBOX_EGRESS_UNRESTRICTED=true; otherwise an error is returned.
func (c *Config) EgressMode() (string, error) {
	switch {
	case c.SandboxBackendName() != BackendOpenSandbox: // none, local or a host's own backend (its own isolation)
		return "disabled", nil
	case len(c.SandboxEgressAllowlist) > 0:
		return "allowlist", nil
	case !c.IsDebug() && !c.SandboxEgressUnrestricted:
		return "", fmt.Errorf("SANDBOX_EGRESS_ALLOWLIST is empty in production: set it, or set SANDBOX_EGRESS_UNRESTRICTED=true to accept unrestricted sandbox egress")
	default:
		return "unrestricted", nil
	}
}

// SandboxTimeout is the idle lifetime of a sandbox.
func (c *Config) SandboxTimeout() time.Duration {
	return time.Duration(c.SandboxTimeoutSeconds) * time.Second
}

// CommandTimeout is the default execute_command timeout.
func (c *Config) CommandTimeout() time.Duration {
	return time.Duration(c.SandboxCommandTimeout) * time.Second
}

// PostgresDSN builds the pgx DSN.
func (c *Config) PostgresDSN() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.PostgresUser, c.PostgresPassword),
		Host:   c.PostgresHost + ":" + c.PostgresPort,
		Path:   "/" + c.PostgresDB,
	}
	q := url.Values{}
	q.Set("search_path", c.PostgresSchema)
	q.Set("sslmode", c.PostgresSSLMode)
	q.Set("application_name", "harness")
	u.RawQuery = q.Encode()
	return u.String()
}
