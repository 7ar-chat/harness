package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/config"
)

// userCfg loads with the given user config.yml (and no project file).
func userCfg(t *testing.T, body string, environ ...string) (*config.Config, error) {
	t.Helper()
	dir, user := repo(t, "", body)
	return load(t, dir, user, environ...)
}

func putFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigFile_InvalidValuesAreRejectedWithTheFileAndTheKey(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"yaml syntax":                {"key: [unclosed\n", config.ConfigFile},
		"not a mapping":              {"- a\n- b\n", config.ConfigFile},
		"integer from list":          {"limits:\n  max_steps: [1]\n", "limits.max_steps: expected a single value"},
		"integer from float":         {"limits:\n  max_steps: 1.5\n", `expected an integer, got "1.5"`},
		"integer from word":          {"limits:\n  max_steps: many\n", `expected an integer, got "many"`},
		"bool from word":             {"discover_models: maybe\n", `discover_models: expected true or false, got "maybe"`},
		"float from word":            {"limits:\n  compaction_threshold: high\n", `expected a number, got "high"`},
		"list from int":              {"models: 5\n", "models: expected a list, got int"},
		"list of lists":              {"models: [[a]]\n", "models: expected a single value"},
		"list of maps":               {"models: [{a: b}]\n", "models: expected a single value"},
		"permission lines from int":  {"permissions:\n  deny: 5\n", "permissions.deny: expected a list"},
		"pairs from int":             {"context_windows: 5\n", "context_windows: expected key: value pairs"},
		"pairs with list value":      {"context_windows:\n  m: [1]\n", "context_windows: expected a single value"},
		"section is a scalar":        {"sandbox: local\n", "sandbox must be a section"},
		"api_key_env empty":          {"api_key_env: \"\"\n", "api_key_env must be the name of an environment variable"},
		"api_key_env not a string":   {"api_key_env: 5\n", "api_key_env must be the name of an environment variable"},
		"api_key_env var unset":      {"api_key_env: NOT_SET_ANYWHERE\n", "environment variable NOT_SET_ANYWHERE is not set"},
		"sandbox api_key_env unset":  {"sandbox:\n  api_key_env: NOT_SET_ANYWHERE\n", "sandbox.api_key_env: the environment variable NOT_SET_ANYWHERE is not set"},
		"token key":                  {"token: abc\n", "secrets do not belong in a configuration file"},
		"env section not a mapping":  {"env: [a]\n", "env must be a section"},
		"env value not a scalar":     {"env:\n  AGENT_MAX_STEPS: [1]\n", "env.AGENT_MAX_STEPS: expected a single value"},
		"mcp section scalar":         {"mcp: 3\n", "mcp must be a section"},
		"mcp servers scalar":         {"mcp:\n  servers: 3\n", "mcp.servers must map server names"},
		"mcp allow_stdio not a bool": {"mcp:\n  allow_stdio: \"yes\"\n", "mcp.allow_stdio must be true or false"},
		"mcp timeout not a number":   {"mcp:\n  call_timeout_seconds: soon\n", "mcp.call_timeout_seconds:"},
		"mcp unknown key":            {"mcp:\n  bogus: 1\n", `unknown key "mcp.bogus"`},
		"agents scalar":              {"agents: 3\n", "agents must map roles"},
		"agents role scalar":         {"agents:\n  default: 3\n", "agents.default must be a section"},
		"agents unavailable role":    {"agents:\n  title:\n    model: x\n", "agents.title is not available yet"},
		"agents unknown role":        {"agents:\n  wizard:\n    model: x\n", `unknown key "agents.wizard"`},
		"agents effort":              {"agents:\n  default:\n    reasoning_effort: high\n", "agents.default.reasoning_effort is not available yet"},
		"agents key of other role":   {"agents:\n  explore:\n    profile: coding\n", `unknown key "agents.explore.profile"`},
		"agents model not scalar":    {"agents:\n  default:\n    model: [a]\n", "agents.default.model:"},
		"providers scalar":           {"providers: 3\n", "providers must map provider names"},
		"sandbox backend unknown":    {"sandbox:\n  backend: docker\n", "sandbox.backend must be local, opensandbox or none"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			_, err := userCfg(t, tc.body)

			// Assert
			if err == nil {
				t.Fatalf("accepted:\n%s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), config.ConfigFile) {
				t.Fatalf("error %q should name the file and contain %q", err, tc.want)
			}
		})
	}
}

func TestConfigFile_EmptyAndCommentOnlyFilesChangeNothing(t *testing.T) {
	for name, body := range map[string]string{"comment": "# nothing here\n", "blank": "\n\n"} {
		t.Run(name, func(t *testing.T) {
			dir, user := repo(t, "", "")
			putFile(t, filepath.Join(user, config.ConfigFile), body)

			cfg, err := load(t, dir, user)

			if err != nil || cfg.Layers.UserFile == "" || len(cfg.Layers.Values) != 0 {
				t.Fatalf("err %v layers %+v", err, cfg.Layers)
			}
		})
	}
}

func TestConfigFile_ConvertsEveryValueShape(t *testing.T) {
	// Arrange
	body := `models: a, b
default_model: a
context_windows:
  a: 1000
  b: 2000
model_labels: "a=Alpha"
limits:
  max_steps: 7
  compaction_threshold: 0.4
discover_models: false
explore_model:
`

	// Act
	cfg, err := userCfg(t, body)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.LLMModels, "|"); got != "a|b" {
		t.Errorf("a comma separated string is a list, like the environment variable: %q", got)
	}
	if cfg.ContextWindow("a") != 1000 || cfg.ContextWindow("b") != 2000 {
		t.Errorf("windows from a mapping: a=%d b=%d", cfg.ContextWindow("a"), cfg.ContextWindow("b"))
	}
	if cfg.ModelLabel("a") != "Alpha" {
		t.Errorf("a pairs string is taken as is: %q", cfg.ModelLabel("a"))
	}
	if cfg.AgentMaxSteps != 7 || cfg.Threshold() != 0.4 {
		t.Errorf("steps %d threshold %v", cfg.AgentMaxSteps, cfg.Threshold())
	}
	if cfg.ExploreModel != "" {
		t.Errorf("an empty YAML value is an empty setting: %q", cfg.ExploreModel)
	}
}

func TestConfigFile_APIKeyEnvOfTheSandboxFeedsTheSandboxKey(t *testing.T) {
	// Act
	cfg, err := userCfg(t, "sandbox:\n  api_key_env: MY_SB_KEY\n", "MY_SB_KEY= spaced-key ")
	_, missing := userCfg(t, "sandbox:\n  api_key_env: MY_SB_KEY\n", "MY_SB_KEY=")

	// Assert
	if err != nil || cfg.OpenSandboxAPIKey != " spaced-key " {
		t.Fatalf("err %v key %q", err, cfg.OpenSandboxAPIKey)
	}
	if missing == nil || !strings.Contains(missing.Error(), "MY_SB_KEY is not set") {
		t.Fatalf("an empty variable counts as unset: %v", missing)
	}
}

func TestConfigFile_APIKeyEnvNameIsTrimmed(t *testing.T) {
	cfg, err := userCfg(t, "api_key_env: \"  MY_KEY  \"\n", "MY_KEY=k1")

	if err != nil || cfg.LLMAPIKey != "k1" {
		t.Fatalf("err %v key %q", err, cfg.LLMAPIKey)
	}
	if cfg.LLMKeySource != "user file api_key_env" {
		t.Errorf("source %q", cfg.LLMKeySource)
	}
}

func TestConfigFile_SandboxBackendNoneDisablesTheSandboxAndForgetsItsURL(t *testing.T) {
	// Act
	cfg, err := userCfg(t, "sandbox:\n  backend: none\n", "OPENSANDBOX_URL=http://sandbox:8080")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SandboxEnabled() || cfg.OpenSandboxURL != "" || cfg.SandboxBackendName() != config.BackendNone {
		t.Fatalf("enabled %v url %q backend %q", cfg.SandboxEnabled(), cfg.OpenSandboxURL, cfg.SandboxBackendName())
	}
}

func TestConfigFile_AgentsSectionSetsRoleModelsAndTheirSources(t *testing.T) {
	// Arrange
	body := "models: [big, small, tiny]\nagents:\n  default:\n    model: big\n    profile: coding\n  explore:\n    model: small\n  compaction:\n    model: tiny\n"

	// Act
	cfg, err := userCfg(t, body)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMDefaultModel != "big" || cfg.AgentProfile != "coding" || cfg.ExploreModel != "small" || cfg.CompactionModel != "tiny" {
		t.Fatalf("default %q profile %q explore %q compaction %q", cfg.LLMDefaultModel, cfg.AgentProfile, cfg.ExploreModel, cfg.CompactionModel)
	}
	if cfg.Layers.Sources["EXPLORE_MODEL"] != config.SourceUser {
		t.Errorf("source %v", cfg.Layers.Sources["EXPLORE_MODEL"])
	}
}

func TestConfigFile_ProjectAgentModelMustBeOneTheUserConfigured(t *testing.T) {
	// Arrange
	user := "models: [allowed]\n"
	dir, userDir := repo(t, "agents:\n  explore:\n    model: stranger\n", user)

	// Act
	_, err := load(t, dir, userDir)
	okDir, okUser := repo(t, "agents:\n  explore:\n    model: allowed\n", user)
	cfg, okErr := load(t, okDir, okUser)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "stranger") || !strings.Contains(err.Error(), "agents.explore.model") || !strings.Contains(err.Error(), config.UserConfigHint) {
		t.Fatalf("err %v", err)
	}
	if okErr != nil || cfg.ExploreModel != "allowed" {
		t.Fatalf("err %v explore %q", okErr, cfg.ExploreModel)
	}
}

func TestConfigFile_ProjectMayUseAModelTheUserSelectedExplicitly(t *testing.T) {
	// Arrange: the user pins the default model outside the list (via the environment), the project repeats it.
	dir, user := repo(t, "default_model: pinned\n", "models: [other]\n")

	// Act
	cfg, err := load(t, dir, user, "LLM_DEFAULT_MODEL=pinned")

	// Assert
	if err != nil || cfg.LLMDefaultModel != "pinned" {
		t.Fatalf("err %v model %q", err, cfg.LLMDefaultModel)
	}
}

func TestConfigFile_ProjectModelErrorPointsAtTheProviderListWhenOneIsSelected(t *testing.T) {
	// Arrange
	userBody := "provider: lab\nproviders:\n  lab:\n    type: openai\n    base_url: https://lab.example/v1\n    models: [m1]\n"
	dir, user := repo(t, "default_model: m2\n", userBody)

	// Act
	_, err := load(t, dir, user)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "providers.lab.models") {
		t.Fatalf("err %v", err)
	}
}

func TestConfigFile_McpCallTimeoutIsReadFromTheMcpSection(t *testing.T) {
	cfg, err := userCfg(t, "mcp:\n  call_timeout_seconds: 7\n")

	if err != nil || cfg.MCPCallTimeout() != 7*time.Second {
		t.Fatalf("err %v timeout %v", err, cfg.MCPCallTimeout())
	}
}

func TestLoad_ReportsAnUnreadableConfigFileByPath(t *testing.T) {
	// Arrange: config.yml is a directory.
	dir, user := repo(t, "", "")
	file := filepath.Join(user, config.ConfigFile)
	if err := os.Mkdir(file, 0o755); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := load(t, dir, user)

	// Assert
	if err == nil || !strings.Contains(err.Error(), file) {
		t.Fatalf("err %v", err)
	}
}

func TestLoad_AnInvalidTrustFileStopsTheLoadInsteadOfSilentlyDistrustingTheProject(t *testing.T) {
	// Arrange
	dir, user := repo(t, "", "")
	writeTrustFile(t, user, `{"projects": [`, 0o600)

	// Act
	_, err := load(t, dir, user)
	envTrusted, envErr := load(t, dir, user, "HARNESS_TRUST_PROJECT=1")

	// Assert
	if err == nil || !strings.Contains(err.Error(), config.TrustFile) {
		t.Fatalf("err %v", err)
	}
	if envErr != nil || !envTrusted.Layers.Trusted {
		t.Fatalf("the environment trust does not consult the file: %v", envErr)
	}
}

func TestLoad_RejectsEnvironmentValuesOfTheWrongType(t *testing.T) {
	_, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{"LLM_STEP_RETRIES=lots"}})

	if err == nil || !strings.Contains(err.Error(), "LLMStepRetries") {
		t.Fatalf("err %v", err)
	}
}

func TestLoad_ClampsConcurrencyAndStepsToAtLeastOne(t *testing.T) {
	cfg, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{"RUNTIME_CONCURRENCY=0", "AGENT_MAX_STEPS=-3"}})

	if err != nil || cfg.RuntimeConcurrency != 1 || cfg.AgentMaxSteps != 1 {
		t.Fatalf("err %v concurrency %d steps %d", err, cfg.RuntimeConcurrency, cfg.AgentMaxSteps)
	}
}

func TestLoad_MCPServersEnvironmentIsValidated(t *testing.T) {
	cases := map[string]struct{ json, want string }{
		"malformed json": {`[{"name":`, "MCP_SERVERS:"},
		"missing name":   {`[{"url":"http://a/mcp"}]`, "every server needs name and url"},
		"missing target": {`[{"name":"a"}]`, "every server needs name and url"},
		"duplicate name": {`[{"name":"a","url":"http://a"},{"name":"a","url":"http://b"}]`, `duplicate server name "a"`},
		"bad url":        {`[{"name":"a","url":"http://[::1"}]`, `server "a"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{"MCP_SERVERS=" + tc.json}})

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoad_ModelAllowListIncludesTheDefaultAndExploreModels(t *testing.T) {
	// Act
	cfg, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{
		"LLM_MODELS=a, b ,,", "LLM_DEFAULT_MODEL=z", "EXPLORE_MODEL= x ",
	}})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.LLMModels, ","); got != "z,a,b,x" {
		t.Fatalf("allow-list %q: the default goes first, the explore model last, blanks are dropped", got)
	}
}

func TestAddModels_AppendsOnlyNewTrimmedIDsKeepingTheConfiguredOnesFirst(t *testing.T) {
	// Arrange
	cfg := &config.Config{}
	cfg.LLMModels = []string{"a", "b"}

	// Act
	cfg.AddModels(" c ", "", "a", "   ", "c", "d")

	// Assert
	if got := strings.Join(cfg.LLMModels, ","); got != "a,b,c,d" {
		t.Fatalf("got %q", got)
	}
}

func TestSandboxDurationsFollowTheirSettings(t *testing.T) {
	cfg, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{"SANDBOX_TIMEOUT_SECONDS=90", "SANDBOX_COMMAND_TIMEOUT_SECONDS=7"}})

	if err != nil {
		t.Fatal(err)
	}
	if cfg.SandboxTimeout() != 90*time.Second || cfg.CommandTimeout() != 7*time.Second {
		t.Fatalf("sandbox %v command %v", cfg.SandboxTimeout(), cfg.CommandTimeout())
	}
}

func TestPermissionStrictnessRanksModesAndNormalisesSpelling(t *testing.T) {
	cases := []struct {
		mode string
		rank int
		ok   bool
	}{
		{"readonly", 4, true}, {"read-only", 4, true}, {" READ_ONLY ", 4, true},
		{"", 3, true}, {"ask", 3, true}, {"Ask", 3, true},
		{"auto-edit", 2, true}, {"auto_edit", 2, true}, {"autoedit", 2, true},
		{"yolo", 1, true}, {"yolo-unsafe", 0, true}, {"YOLO_UNSAFE", 0, true},
		{"anything-else", 0, false},
	}
	for _, tc := range cases {
		if r, ok := config.PermissionStrictness(tc.mode); r != tc.rank || ok != tc.ok {
			t.Errorf("PermissionStrictness(%q) = %d,%v want %d,%v", tc.mode, r, ok, tc.rank, tc.ok)
		}
	}
}

func TestUntrustedProjectCanTightenButNeverLoosenPermissions(t *testing.T) {
	cases := map[string]struct {
		user, project string
		wantMode      string
		wantIgnored   string
	}{
		"looser than the implicit ask": {"", "permissions:\n  mode: yolo\n", "", `permissions.mode "yolo" ignored: it is looser than "ask"`},
		"looser than the user's mode":  {"permissions:\n  mode: readonly\n", "permissions:\n  mode: auto-edit\n", "readonly", `looser than "readonly"`},
		"stricter is kept":             {"permissions:\n  mode: auto-edit\n", "permissions:\n  mode: readonly\n", "readonly", ""},
		"equal is kept":                {"permissions:\n  mode: ask\n", "permissions:\n  mode: ask\n", "ask", ""},
		"unranked spelling is kept":    {"permissions:\n  mode: readonly\n", "permissions:\n  mode: custom-mode\n", "custom-mode", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir, user := repo(t, tc.project, tc.user)

			cfg, err := load(t, dir, user)

			if err != nil {
				t.Fatal(err)
			}
			if cfg.AgentPermissions != tc.wantMode {
				t.Errorf("mode %q, want %q", cfg.AgentPermissions, tc.wantMode)
			}
			joined := strings.Join(cfg.Layers.Ignored, "\n")
			if tc.wantIgnored == "" && joined != "" {
				t.Errorf("nothing should be ignored: %s", joined)
			}
			if !strings.Contains(joined, tc.wantIgnored) {
				t.Errorf("ignored %q lacks %q", joined, tc.wantIgnored)
			}
		})
	}
}

func TestATrustedProjectMayLoosenPermissions(t *testing.T) {
	dir, user := repo(t, "permissions:\n  mode: yolo\n  allow: [\"Bash(make*)\"]\n", "permissions:\n  mode: readonly\n")

	cfg, err := load(t, dir, user, "HARNESS_TRUST_PROJECT=1")

	if err != nil || cfg.AgentPermissions != "yolo" || cfg.AgentPermissionsAllow != "Bash(make*)" || len(cfg.Layers.Ignored) != 0 {
		t.Fatalf("err %v mode %q allow %q ignored %v", err, cfg.AgentPermissions, cfg.AgentPermissionsAllow, cfg.Layers.Ignored)
	}
}

func TestDump_ListsIgnoredSettingsAndNotStartedServers(t *testing.T) {
	// Arrange
	dir, user := repo(t, "permissions:\n  allow: [\"Bash(make*)\"]\n", "mcp:\n  servers:\n    mine:\n      url: http://mine/mcp\n      type: sse\n")
	cfg, err := load(t, dir, user, "COMPACTION_MODEL=", "SANDBOX_CPU=2")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	// Act
	cfg.Dump(&out, nil)

	// Assert
	got := out.String()
	for _, want := range []string{"ignored:", "permissions.allow (1 rule(s)) ignored", "mcp servers:", "sse: http://mine/mcp", "project not trusted"} {
		if !strings.Contains(got, want) {
			t.Errorf("dump lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "COMPACTION_MODEL") {
		t.Errorf("an empty value that only comes from the environment is noise:\n%s", got)
	}
	if !strings.Contains(got, "SANDBOX_CPU") {
		t.Errorf("a set environment value must be listed:\n%s", got)
	}
}

func TestDump_ShowsWhoTrustsTheProjectAndSortsSettingsByName(t *testing.T) {
	// Arrange
	dir, user := repo(t, "", "limits:\n  max_steps: 5\n")
	cfg, err := load(t, dir, user, "HARNESS_TRUST_PROJECT=1", "COMPACTION_MODEL=m")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	// Act
	cfg.Dump(&out, nil)

	// Assert
	got := out.String()
	if !strings.Contains(got, "project trusted (env HARNESS_TRUST_PROJECT)") {
		t.Errorf("trust line missing:\n%s", got)
	}
	if a, b := strings.Index(got, "AGENT_MAX_STEPS"), strings.Index(got, "COMPACTION_MODEL"); a < 0 || b < 0 || a > b {
		t.Errorf("rows must be sorted by name (AGENT_MAX_STEPS=%d, COMPACTION_MODEL=%d):\n%s", a, b, got)
	}
}

func TestDump_MasksSecretsFromFlagsAndKeepsBlankOnesBlank(t *testing.T) {
	// Arrange
	cfg := &config.Config{}
	var out bytes.Buffer

	// Act
	cfg.Dump(&out, map[string]string{"OPENSANDBOX_API_KEY": "topsecret", "-token": "", "-model": "m"})

	// Assert
	got := out.String()
	if strings.Contains(got, "topsecret") {
		t.Fatalf("secret printed:\n%s", got)
	}
	if !strings.Contains(got, "OPENSANDBOX_API_KEY") || !strings.Contains(got, "****") {
		t.Errorf("a masked secret should still be listed:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "-token") && strings.Contains(line, "****") {
			t.Errorf("an empty secret must not look set: %q", line)
		}
	}
	for _, want := range []string{"project root: (none)", "project file: (none)", "user file:    (none)", "auth file:    (none)", "precedence:"} {
		if !strings.Contains(got, want) {
			t.Errorf("dump of a bare config lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "trust:") || strings.Contains(got, "provider:") {
		t.Errorf("no trust or provider line without a project or provider:\n%s", got)
	}
}
