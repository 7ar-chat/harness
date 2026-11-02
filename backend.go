package harness

import (
	"strings"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
)

// llmOptions maps the configuration (LLM_*) to the options of the default chat-model factory.
func llmOptions(cfg *config.Config) llm.Options {
	var responses []string
	for _, m := range cfg.LLMResponsesModels {
		if m = strings.TrimSpace(m); m != "" {
			responses = append(responses, m)
		}
	}
	return llm.Options{
		Provider: cfg.LLMProvider, BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Models: cfg.LLMModels,
		ResponsesModels: responses, TimeoutSeconds: cfg.LLMTimeout, SessionHeader: cfg.LLMSessionHeader, CacheTTL: cfg.LLMCacheTTL,
	}
}

// SandboxSettings maps the configuration (SANDBOX_BACKEND, OPENSANDBOX_*,
// SANDBOX_*, LOCAL_*) to the settings the registered sandbox backends are built from.
func SandboxSettings(cfg *config.Config) backends.Settings {
	return backends.Settings{
		Name: cfg.SandboxBackendName(),
		OpenSandbox: backends.OpenSandboxSettings{
			URL: cfg.OpenSandboxURL, APIKey: cfg.OpenSandboxAPIKey, UseServerProxy: cfg.OpenSandboxUseServerProxy,
			Image: cfg.OpenSandboxImage, CPU: cfg.SandboxCPU, Memory: cfg.SandboxMemory,
			TimeoutSeconds: cfg.SandboxTimeoutSeconds, EgressAllowlist: cfg.SandboxEgressAllowlist,
			EgressDenyCIDRs: cfg.SandboxEgressDenyCIDRs, CredentialProxy: cfg.OpenSandboxCredentialProxy,
		},
		Local: backends.LocalSettings{Workspace: cfg.LocalWorkspace, StateDir: cfg.LocalStateDir},
	}
}

// NewSandboxBackend builds the backend the configuration selects
// (Config.SandboxBackendName): the registered factory of SANDBOX_BACKEND, or of
// "opensandbox" when only OPENSANDBOX_URL is set, or none (a nil
// sandbox.Backend). Every process that can create a sandbox goes through it, so
// every one of them applies the production refusal of unrestricted egress
// (config.EgressMode) before a backend is built.
func NewSandboxBackend(cfg *config.Config) (sandbox.Backend, error) {
	if _, err := cfg.EgressMode(); err != nil {
		return nil, err
	}
	return backends.NewBackend(SandboxSettings(cfg))
}
