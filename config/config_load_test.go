package config_test

import (
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
)

func TestLoadAppliesDocumentedDefaults(t *testing.T) {
	// Without environment, Load returns the README defaults.
	// Arrange

	// Act
	cfg, err := config.Load()

	// Assert
	if err != nil || cfg.Port != 8000 || cfg.PostgresSchema != "work" || cfg.PostgresSSLMode != "require" ||
		cfg.LLMProvider != "openai" || cfg.LLMDefaultModel != "gpt-5.6-terra" || len(cfg.LLMModels) != 2 || cfg.AgentMaxSteps != 60 ||
		cfg.RuntimeConcurrency != 16 || cfg.RuntimeShutdownGrace != 15 || cfg.SandboxEnabled() || cfg.MaxSandboxesPerUser != 5 || !cfg.PingDBAtStartup || !cfg.IsDebug() {
		t.Fatalf("err %v cfg %+v", err, cfg)
	}
	if dsn := cfg.PostgresDSN(); !strings.Contains(dsn, "search_path=work") || !strings.Contains(dsn, "application_name=harness") || !strings.Contains(dsn, "sslmode=require") {
		t.Fatalf("dsn %s", dsn)
	}
}

func TestLoadParsesMCPServersAndLists(t *testing.T) {
	// MCP_SERVERS JSON and comma lists are parsed; the default model joins the allow-list.
	// Arrange
	t.Setenv("MCP_SERVERS", `[{"name":"kb","url":"http://kb-mcp:8000/mcp","tools":["search"]}]`)
	t.Setenv("SANDBOX_EGRESS_ALLOWLIST", "pypi.org, files.pythonhosted.org")
	t.Setenv("LLM_MODELS", "a,b")
	t.Setenv("LLM_DEFAULT_MODEL", "c")
	t.Setenv("DJANGO_PRODUCTION", "true")

	// Act
	cfg, err := config.Load()

	// Assert
	if err != nil || len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Tools[0] != "search" ||
		len(cfg.SandboxEgressAllowlist) != 2 || cfg.SandboxEgressAllowlist[1] != "files.pythonhosted.org" || !cfg.ModelAllowed("c") || cfg.IsDebug() {
		t.Fatalf("err %v cfg %+v", err, cfg)
	}
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	// Unknown LLM providers and malformed MCP JSON are configuration errors.
	// Arrange
	t.Setenv("LLM_PROVIDER", "magic")

	// Act
	_, providerErr := config.Load()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("MCP_SERVERS", `[{"name":""}]`)
	_, mcpErr := config.Load()

	// Assert
	if providerErr == nil || mcpErr == nil {
		t.Fatalf("provider %v mcp %v", providerErr, mcpErr)
	}
}

func TestRuntimeRefusesUnrestrictedEgressInProduction(t *testing.T) {
	// In production a runtime with sandboxes needs SANDBOX_EGRESS_ALLOWLIST, unless unrestricted egress is explicitly accepted.
	// Arrange
	t.Setenv("DJANGO_PRODUCTION", "1")
	t.Setenv("OPENSANDBOX_URL", "http://host.docker.internal:8090")
	cfg, _ := config.Load()
	t.Setenv("SANDBOX_EGRESS_UNRESTRICTED", "true")
	optedOut, _ := config.Load()
	t.Setenv("SANDBOX_EGRESS_UNRESTRICTED", "")
	t.Setenv("SANDBOX_EGRESS_ALLOWLIST", "pypi.org")
	allowlisted, _ := config.Load()

	// Act
	_, errDefault := cfg.EgressMode()
	modeOptedOut, errOptedOut := optedOut.EgressMode()
	modeAllow, errAllow := allowlisted.EgressMode()

	// Assert
	if errDefault == nil || errOptedOut != nil || modeOptedOut != "unrestricted" || errAllow != nil || modeAllow != "allowlist" {
		t.Fatalf("default %v, opted out %q %v, allowlist %q %v", errDefault, modeOptedOut, errOptedOut, modeAllow, errAllow)
	}
}

func TestDevelopmentAllowsUnrestrictedEgress(t *testing.T) {
	// Outside production an empty allow-list means unrestricted egress (logged), and no sandbox means no egress.
	// Arrange
	t.Setenv("OPENSANDBOX_URL", "http://host.docker.internal:8090")
	dev, _ := config.Load()
	t.Setenv("OPENSANDBOX_URL", "")
	t.Setenv("DJANGO_PRODUCTION", "1")
	noSandbox, _ := config.Load()

	// Act
	devMode, devErr := dev.EgressMode()
	noneMode, noneErr := noSandbox.EgressMode()

	// Assert
	if devErr != nil || devMode != "unrestricted" || noneErr != nil || noneMode != "disabled" {
		t.Fatalf("dev %q %v, no sandbox %q %v", devMode, devErr, noneMode, noneErr)
	}
}

func TestEgressDenyCIDRsDefaultToPrivateRanges(t *testing.T) {
	// SANDBOX_EGRESS_DENY_CIDRS defaults to the private and link-local ranges and is configurable.
	// Arrange
	def, _ := config.Load()
	t.Setenv("SANDBOX_EGRESS_DENY_CIDRS", "10.0.0.0/8, 100.64.0.0/10")

	// Act
	custom, err := config.Load()

	// Assert
	if err != nil || strings.Join(def.SandboxEgressDenyCIDRs, ",") != "172.16.0.0/12,10.0.0.0/8,192.168.0.0/16,169.254.0.0/16,100.64.0.0/10,0.0.0.0/8,fc00::/7,fe80::/10,::ffff:0:0/96" ||
		strings.Join(custom.SandboxEgressDenyCIDRs, ",") != "10.0.0.0/8,100.64.0.0/10" {
		t.Fatalf("default %v custom %v err %v", def.SandboxEgressDenyCIDRs, custom.SandboxEgressDenyCIDRs, err)
	}
}

func TestLoadParsesHarnessModelMetadataAndCompaction(t *testing.T) {
	// LLM_CONTEXT_WINDOWS, LLM_REASONING_MODELS, LLM_VISION_MODELS and the compaction settings are parsed; unknown models get the 128000 default window.
	// Arrange
	t.Setenv("LLM_MODELS", "a,b")
	t.Setenv("LLM_CONTEXT_WINDOWS", "a=200000, b = 32000")
	t.Setenv("LLM_REASONING_MODELS", "a")
	t.Setenv("LLM_VISION_MODELS", "a, b")
	t.Setenv("COMPACTION_THRESHOLD", "0.8")
	t.Setenv("COMPACTION_MODEL", "b")

	// Act
	cfg, err := config.Load()

	// Assert
	if err != nil || cfg.ContextWindow("a") != 200000 || cfg.ContextWindow("b") != 32000 || cfg.ContextWindow("zzz") != 128000 ||
		!cfg.SupportsReasoning("a") || cfg.SupportsReasoning("b") || !cfg.SupportsVision("b") ||
		cfg.CompactionThreshold != 0.8 || cfg.CompactionModel != "b" {
		t.Fatalf("err %v cfg %+v", err, cfg)
	}
}

func TestLoadRejectsInvalidHarnessSettings(t *testing.T) {
	// Malformed LLM_CONTEXT_WINDOWS entries and thresholds outside (0, 1) are configuration errors.
	// Arrange
	t.Setenv("LLM_CONTEXT_WINDOWS", "a=lots")

	// Act
	_, windowErr := config.Load()
	t.Setenv("LLM_CONTEXT_WINDOWS", "")
	t.Setenv("COMPACTION_THRESHOLD", "1.5")
	_, thresholdErr := config.Load()

	// Assert
	if windowErr == nil || thresholdErr == nil {
		t.Fatalf("window %v threshold %v", windowErr, thresholdErr)
	}
}

func TestLoadDefaultsToWorkGPT56Models(t *testing.T) {
	// Without environment, Work Chat offers Work's GPT-5.6 Sol and Terra (default Terra) with a 272k window, reasoning and vision.
	// Arrange

	// Act
	cfg, err := config.Load()

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMDefaultModel != "gpt-5.6-terra" || strings.Join(cfg.LLMModels, ",") != "gpt-5.6-sol,gpt-5.6-terra" {
		t.Fatalf("models %q default %q", cfg.LLMModels, cfg.LLMDefaultModel)
	}
	for model, label := range map[string]string{"gpt-5.6-sol": "GPT-5.6 Sol", "gpt-5.6-terra": "GPT-5.6 Terra"} {
		if cfg.ModelLabel(model) != label || cfg.ContextWindow(model) != 272_000 || !cfg.SupportsReasoning(model) || !cfg.SupportsVision(model) {
			t.Fatalf("%s: label %q window %d reasoning %v vision %v", model, cfg.ModelLabel(model), cfg.ContextWindow(model),
				cfg.SupportsReasoning(model), cfg.SupportsVision(model))
		}
	}
	if cfg.ModelLabel("unknown-model") != "unknown-model" {
		t.Fatalf("unlabelled model shows %q", cfg.ModelLabel("unknown-model"))
	}
}

func TestLoadRejectsMalformedModelLabels(t *testing.T) {
	// LLM_MODEL_LABELS entries must be model=Label; custom labels replace the defaults.
	// Arrange

	// Act
	t.Setenv("LLM_MODEL_LABELS", "gpt-5.6-sol")
	_, bad := config.Load()
	t.Setenv("LLM_MODEL_LABELS", "gpt-5.6-sol=Sol")
	cfg, custom := config.Load()

	// Assert
	if bad == nil || !strings.Contains(bad.Error(), "LLM_MODEL_LABELS") {
		t.Fatalf("malformed labels accepted: %v", bad)
	}
	if custom != nil || cfg.ModelLabel("gpt-5.6-sol") != "Sol" || cfg.ModelLabel("gpt-5.6-terra") != "gpt-5.6-terra" {
		t.Fatalf("custom labels: %v %q %q", custom, cfg.ModelLabel("gpt-5.6-sol"), cfg.ModelLabel("gpt-5.6-terra"))
	}
}
