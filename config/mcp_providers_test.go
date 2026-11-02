package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/config"
)

func TestMCPToolName(t *testing.T) {
	cases := []struct{ server, tool, want string }{
		{"kb", "search", "kb__search"},
		{"my server", "do.thing/now", "my_server__do_thing_now"},
		{"srv-1", "Tool_2", "srv-1__Tool_2"},
	}
	for _, tc := range cases {
		if got := config.MCPToolName(tc.server, tc.tool); got != tc.want {
			t.Errorf("MCPToolName(%q,%q) = %q, want %q", tc.server, tc.tool, got, tc.want)
		}
	}
	long := config.MCPToolName(strings.Repeat("s", 40), strings.Repeat("t", 40))
	if len(long) != 64 || !strings.HasPrefix(long, strings.Repeat("s", 40)+"__") {
		t.Errorf("names are cut at 64 characters: %d %q", len(long), long)
	}
	if got := config.MCPToolName("é", "t"); strings.ContainsRune(got, 'é') {
		t.Errorf("only [a-zA-Z0-9_-] may reach the model: %q", got)
	}
}

func TestMCPServerDescribe_NeverShowsHeadersOrEnvironment(t *testing.T) {
	cases := []struct {
		name string
		s    config.MCPServer
		want string
	}{
		{"stdio", config.MCPServer{Command: "npx", Args: []string{"-y", "pkg"}, Env: map[string]string{"TOKEN": "s3cret"}}, "stdio: npx -y pkg"},
		{"stdio without args", config.MCPServer{Command: "srv"}, "stdio: srv"},
		{"http by default", config.MCPServer{URL: "http://a/mcp", Headers: map[string]string{"Authorization": "Bearer s3cret"}}, "http: http://a/mcp"},
		{"explicit type", config.MCPServer{URL: "http://a/sse", Type: "sse"}, "sse: http://a/sse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.s.Describe()
			if got != tc.want || strings.Contains(got, "s3cret") {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// mcpUser loads a user config.yml and the user's mcp.json, with the environment given.
func mcpJSON(t *testing.T, body string, environ ...string) (*config.Config, error) {
	t.Helper()
	dir, user := repo(t, "", "")
	putFile(t, filepath.Join(user, config.MCPFile), body)
	return load(t, dir, user, environ...)
}

func TestMCPFile_TypesAreNormalisedAndDefaulted(t *testing.T) {
	// Arrange
	body := `{"mcpServers": {
		"a": {"url": "http://a/mcp"},
		"b": {"type": "Streamable-HTTP", "url": "http://b/mcp"},
		"c": {"type": "streamable_http", "url": "http://c/mcp"},
		"d": {"type": "SSE", "url": "http://d/sse"},
		"e": {"command": "echo"}
	}}`

	// Act
	cfg, err := mcpJSON(t, body, "SANDBOX_BACKEND=local", "HARNESS_MCP_STDIO=1")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range cfg.MCPServers {
		got[s.Name] = s.Type
	}
	want := map[string]string{"a": "http", "b": "http", "c": "http", "d": "sse", "e": "stdio"}
	for n, typ := range want {
		if got[n] != typ {
			t.Errorf("server %s type %q, want %q", n, got[n], typ)
		}
	}
	if len(cfg.MCPServers) != 5 || cfg.MCPServers[0].Name != "a" {
		t.Errorf("servers are loaded in name order: %+v", cfg.MCPServers)
	}
}

func TestMCPFile_InvalidDefinitionsAreRejectedNamingFileAndServer(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"not json":              {`{"mcpServers": `, config.MCPFile},
		"unknown top-level key": {`{"servers": {}}`, "unknown field"},
		"unknown field":         {`{"mcpServers": {"a": {"url": "http://a", "colour": "red"}}}`, `mcp server "a"`},
		"wrong field type":      {`{"mcpServers": {"a": {"url": 5}}}`, `mcp server "a"`},
		"bad type":              {`{"mcpServers": {"a": {"type": "grpc", "url": "http://a"}}}`, "type must be http, sse or stdio"},
		"stdio without command": {`{"mcpServers": {"a": {"type": "stdio", "url": "http://a"}}}`, "a stdio server needs a command"},
		"command with http":     {`{"mcpServers": {"a": {"type": "http", "command": "x"}}}`, "a stdio server needs a command"},
		"remote without url":    {`{"mcpServers": {"a": {"type": "sse"}}}`, "url is required"},
		"url var unset":         {`{"mcpServers": {"a": {"url": "http://${NOPE_HOST}/mcp"}}}`, "url: environment variable NOPE_HOST is not set"},
		"header var unset":      {`{"mcpServers": {"a": {"url": "http://a", "headers": {"X-Key": "${NOPE_KEY}"}}}}`, "header X-Key: environment variable NOPE_KEY is not set"},
		"env var unset":         {`{"mcpServers": {"a": {"command": "x", "env": {"K": "${NOPE_ENV}"}}}}`, "env K: environment variable NOPE_ENV is not set"},
		"command var unset":     {`{"mcpServers": {"a": {"command": "${NOPE_BIN}"}}}`, "command: environment variable NOPE_BIN is not set"},
		"arg var unset":         {`{"mcpServers": {"a": {"command": "x", "args": ["${NOPE_ARG}"]}}}`, "args: environment variable NOPE_ARG is not set"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := mcpJSON(t, tc.body)

			if err == nil {
				t.Fatalf("accepted %s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), config.MCPFile) {
				t.Fatalf("err %q, want %q naming %s", err, tc.want, config.MCPFile)
			}
		})
	}
}

func TestMCPFile_ReferencesExpandWithDefaultsAndEmptyCountsAsUnset(t *testing.T) {
	// Arrange
	body := `{"mcpServers": {
		"a": {"url": "http://${HOST:-fallback}:${PORT:-80}/mcp?k=${KEY}", "headers": {"A": "${H1}-${H1}"}}
	}}`

	// Act
	cfg, err := mcpJSON(t, body, "PORT=", "KEY=v1", "H1=x")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.MCPServers[0]
	if s.URL != "http://fallback:80/mcp?k=v1" || s.Headers["A"] != "x-x" {
		t.Fatalf("url %q headers %v", s.URL, s.Headers)
	}
}

func TestMCPFile_DefaultMayBeEmptyAndOnlyTheFirstMissingVariableIsReported(t *testing.T) {
	cfg, err := mcpJSON(t, `{"mcpServers": {"a": {"url": "http://a/mcp${SUFFIX:-}"}}}`)
	_, missing := mcpJSON(t, `{"mcpServers": {"a": {"url": "http://${FIRST_MISSING}/${SECOND_MISSING}"}}}`)

	if err != nil || cfg.MCPServers[0].URL != "http://a/mcp" {
		t.Fatalf("err %v servers %+v", err, cfg.MCPServers)
	}
	if missing == nil || !strings.Contains(missing.Error(), "FIRST_MISSING") || strings.Contains(missing.Error(), "SECOND_MISSING") {
		t.Fatalf("err %v", missing)
	}
}

func TestMCPFile_MissingFileAndEmptyServerListAddNothing(t *testing.T) {
	cfg, err := mcpJSON(t, `{"mcpServers": {}}`)
	missing, err2 := userCfg(t, "")

	if err != nil || err2 != nil || len(cfg.MCPServers) != 0 || len(missing.MCPServers) != 0 {
		t.Fatalf("%v %v %+v %+v", err, err2, cfg.MCPServers, missing.MCPServers)
	}
	if len(cfg.Layers.MCPSources) != 0 {
		t.Errorf("a file without servers is not a source: %v", cfg.Layers.MCPSources)
	}
}

func TestMCPFile_UnreadableFileIsAnErrorNotSilentlyIgnored(t *testing.T) {
	dir, user := repo(t, "", "")
	if err := os.Mkdir(filepath.Join(user, config.MCPFile), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := load(t, dir, user)

	if err == nil || !strings.Contains(err.Error(), config.MCPFile) {
		t.Fatalf("err %v", err)
	}
}

func TestMCPSources_LaterSourcesWinAndAreRecordedInPriorityOrder(t *testing.T) {
	// Arrange: the same server name in every source.
	dir, user := repo(t, "mcp:\n  servers:\n    kb:\n      url: http://project-yaml/mcp\n", "mcp:\n  servers:\n    kb:\n      url: http://user-yaml/mcp\n")
	root := filepath.Join(dir, "..", "..")
	putFile(t, filepath.Join(user, config.MCPFile), `{"mcpServers":{"kb":{"url":"http://user-json/mcp"}}}`)
	putFile(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"kb":{"url":"http://root-json/mcp"}}}`)
	putFile(t, filepath.Join(root, config.DirName, config.MCPFile), `{"mcpServers":{"kb":{"url":"http://har-json/mcp"}}}`)

	// Act
	trusted, err := load(t, dir, user, "HARNESS_TRUST_PROJECT=1")
	untrusted, err2 := load(t, dir, user)
	env, err3 := load(t, dir, user, "HARNESS_TRUST_PROJECT=1", `MCP_SERVERS=[{"name":"kb","url":"http://env/mcp"}]`)

	// Assert
	if err != nil || err2 != nil || err3 != nil {
		t.Fatal(err, err2, err3)
	}
	if trusted.MCPServers[0].URL != "http://project-yaml/mcp" || len(trusted.MCPServers) != 1 {
		t.Errorf("project config.yml is the highest file source: %+v", trusted.MCPServers)
	}
	if untrusted.MCPServers[0].URL != "http://user-json/mcp" {
		t.Errorf("an untrusted project contributes nothing: %+v", untrusted.MCPServers)
	}
	if env.MCPServers[0].URL != "http://env/mcp" || env.MCPServers[0].Origin != "MCP_SERVERS" {
		t.Errorf("MCP_SERVERS beats every file: %+v", env.MCPServers)
	}
	var tails []string
	for _, s := range trusted.Layers.MCPSources {
		tails = append(tails, filepath.Base(s))
	}
	if got := strings.Join(tails, ","); got != "config.yml,mcp.json,.mcp.json,mcp.json,config.yml" {
		t.Errorf("sources in priority order (lowest first): %s", got)
	}
}

func TestMCPStdio_ServersAreSkippedWithAReasonUnlessLocalAndOptedIn(t *testing.T) {
	// Arrange: a user-level stdio server (no project approval needed).
	body := `{"mcpServers":{"fs":{"command":"echo"},"web":{"url":"http://w/mcp"}}}`

	// Act
	blocked, err := mcpJSON(t, body)
	allowed, err2 := mcpJSON(t, body, "SANDBOX_BACKEND=local", "HARNESS_MCP_STDIO=true")
	noLocal, err3 := mcpJSON(t, body, "HARNESS_MCP_STDIO=true")

	// Assert
	if err != nil || err2 != nil || err3 != nil {
		t.Fatal(err, err2, err3)
	}
	if len(blocked.MCPServers) != 1 || blocked.MCPServers[0].Name != "web" || len(blocked.Layers.MCPSkipped) != 1 {
		t.Errorf("default: %+v skipped %v", blocked.MCPServers, blocked.Layers.MCPSkipped)
	}
	if len(allowed.MCPServers) != 2 || len(allowed.Layers.MCPSkipped) != 0 {
		t.Errorf("opted in: %+v skipped %v", allowed.MCPServers, allowed.Layers.MCPSkipped)
	}
	if len(noLocal.MCPServers) != 1 {
		t.Errorf("stdio needs the local backend even when opted in: %+v", noLocal.MCPServers)
	}
}

func TestMCPTrust_AnUnreadableTrustFileStopsTheLoad(t *testing.T) {
	// Arrange: a project stdio server needs MCPApproved, which reads trust.json.
	dir, user := repo(t, "", "mcp:\n  allow_stdio: true\n")
	writeProjectStdio(t, dir, "echo")
	writeTrustFile(t, user, `{"mcp": [`, 0o600)

	// Act: the environment trusts the project, so only the approval lookup reads the broken file.
	_, err := load(t, dir, user, "SANDBOX_BACKEND=local", "HARNESS_TRUST_PROJECT=1")

	// Assert
	if err == nil || !strings.Contains(err.Error(), config.TrustFile) {
		t.Fatalf("err %v", err)
	}
}

func TestProviders_ASelectedProviderFoldsItsDefinitionIntoTheSettings(t *testing.T) {
	// Arrange
	body := "provider: lab\nproviders:\n  lab:\n    type: anthropic\n    base_url: https://lab.example/v1\n    models: [m1, m2]\n    default_model: m2\n    session_header: x-lab\n    cache_ttl: 1h\n    api_key_env: LAB_KEY\n"

	// Act
	cfg, err := userCfg(t, body, "LAB_KEY=k")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMProvider != "anthropic" || cfg.LLMProviderName != "lab" || cfg.LLMBaseURL != "https://lab.example/v1" ||
		cfg.LLMDefaultModel != "m2" || cfg.LLMSessionHeader != "x-lab" || cfg.LLMCacheTTL != "1h" || cfg.LLMAPIKey != "k" {
		t.Fatalf("cfg %+v", cfg)
	}
	if strings.Join(cfg.LLMModels, ",") != "m1,m2" {
		t.Errorf("models %v", cfg.LLMModels)
	}
	if cfg.Layers.Sources["LLM_BASE_URL"] != config.SourceUser {
		t.Errorf("provider values report the layer that defined them: %v", cfg.Layers.Sources["LLM_BASE_URL"])
	}
	if cfg.LLMKeySource != "env LAB_KEY" {
		t.Errorf("key source %q", cfg.LLMKeySource)
	}
}

func TestProviders_InvalidDefinitions(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"bad name":            {"providers:\n  -bad:\n    type: openai\n", "providers.-bad: invalid provider name"},
		"entry is a scalar":   {"providers:\n  p: 3\n", "providers.p must be a section"},
		"type not a scalar":   {"providers:\n  p:\n    type: [a]\n", "providers.p.type:"},
		"bad url":             {"providers:\n  p:\n    base_url: \"http://[::1\"\n", "providers.p.base_url:"},
		"api_key_env empty":   {"providers:\n  p:\n    api_key_env: \"\"\n", "providers.p.api_key_env must be the name"},
		"models not a list":   {"providers:\n  p:\n    models: 5\n", "providers.p.models: expected a list"},
		"password":            {"providers:\n  p:\n    password: x\n", "secrets do not belong"},
		"token":               {"providers:\n  p:\n    token: x\n", "secrets do not belong"},
		"secret":              {"providers:\n  p:\n    secret: x\n", "secrets do not belong"},
		"default_model array": {"providers:\n  p:\n    default_model: [a]\n", "providers.p.default_model:"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := userCfg(t, tc.body)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

func TestProviders_TheTypeDefaultsToTheNameForAnthropicAndOpenAIOtherwise(t *testing.T) {
	cases := map[string]struct{ name, wantType string }{
		"named anthropic": {"anthropic", "anthropic"},
		"named other":     {"mine", "openai"},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			body := "provider: " + tc.name + "\nproviders:\n  " + tc.name + ":\n    base_url: https://x.example/v1\n"

			cfg, err := userCfg(t, body)

			if err != nil || cfg.LLMProvider != tc.wantType || cfg.LLMProviderName != tc.name {
				t.Fatalf("err %v provider %q name %q", err, cfg.LLMProvider, cfg.LLMProviderName)
			}
		})
	}
}

func TestProviders_EnvironmentAndProjectValuesBeatTheProviderDefaults(t *testing.T) {
	// Arrange: the provider defines a default model; the project file selects another one the user lists.
	user := "provider: lab\nproviders:\n  lab:\n    base_url: https://lab.example/v1\n    models: [m1, m2]\n    default_model: m1\n"
	dir, userDir := repo(t, "default_model: m2\n", user)

	// Act
	fromProject, err := load(t, dir, userDir)
	fromEnv, err2 := load(t, dir, userDir, "LLM_BASE_URL=http://env/v1", "LLM_DEFAULT_MODEL=m1")

	// Assert
	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if fromProject.LLMDefaultModel != "m2" {
		t.Errorf("what the project selected wins over the provider's default model: %q", fromProject.LLMDefaultModel)
	}
	if fromEnv.LLMBaseURL != "http://env/v1" || fromEnv.LLMDefaultModel != "m1" {
		t.Errorf("env wins: base %q model %q", fromEnv.LLMBaseURL, fromEnv.LLMDefaultModel)
	}
}

func TestProviders_ProjectCannotSelectAProviderTheUserDidNotDefine(t *testing.T) {
	cases := map[string]struct {
		project, user string
		ok            bool
	}{
		"undefined":                    {"provider: evil\n", "", false},
		"built-in openai":              {"provider: openai\n", "", true},
		"built-in anthropic":           {"provider: anthropic\n", "", true},
		"defined by the user":          {"provider: lab\n", "providers:\n  lab:\n    base_url: https://lab.example/v1\n", true},
		"another user provider exists": {"provider: evil\n", "providers:\n  lab:\n    base_url: https://lab.example/v1\n", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir, user := repo(t, tc.project, tc.user)

			_, err := load(t, dir, user)

			if (err == nil) != tc.ok {
				t.Fatalf("err %v, ok expected %v", err, tc.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "is not configured by you") {
				t.Errorf("err %v", err)
			}
		})
	}
}

func TestProviderFlagBeatsTheProjectSelection(t *testing.T) {
	dir, user := repo(t, "provider: evil\n", "")

	cfg, err := config.LoadWith(config.LoadOptions{Dir: dir, UserDir: user, Environ: []string{}, Provider: "anthropic"})

	if err != nil || cfg.LLMProvider != "anthropic" {
		t.Fatalf("err %v provider %v", err, cfg)
	}
}

func TestResolveAPIKey_HintsListEverySourceTriedWhenNothingIsFound(t *testing.T) {
	// Arrange
	body := "provider: lab\nproviders:\n  lab:\n    type: openai\n    api_key_env: LAB_KEY\n"

	// Act
	cfg, err := userCfg(t, body)
	keyErr := cfg.RequireLLMKey()

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if keyErr == nil {
		t.Fatal("no key anywhere must be reported")
	}
	for _, want := range []string{"harness auth login lab", "export LLM_API_KEY", "export LAB_KEY", "export OPENAI_API_KEY"} {
		if !strings.Contains(keyErr.Error(), want) {
			t.Errorf("hint lacks %q: %v", want, keyErr)
		}
	}
}

func TestResolveAPIKey_ConventionalVariableForTheTypeOfTheProvider(t *testing.T) {
	cfg, err := userCfg(t, "provider: anthropic\n", "ANTHROPIC_API_KEY=ak", "OPENAI_API_KEY=ok")
	_, missing := userCfg(t, "provider: anthropic\n", "OPENAI_API_KEY=ok")

	if err != nil || cfg.LLMAPIKey != "ak" || cfg.LLMKeySource != "env ANTHROPIC_API_KEY" {
		t.Fatalf("err %v key %q source %q", err, cfg.LLMAPIKey, cfg.LLMKeySource)
	}
	if missing != nil {
		t.Fatalf("a missing key is not a load error: %v", missing)
	}
}

func TestResolveAPIKey_UnusableAuthFileIsALoadError(t *testing.T) {
	dir, user := repo(t, "", "")
	writeAuthFile(t, user, `{"providers": 5}`, 0o600)

	_, err := load(t, dir, user)

	if err == nil || !strings.Contains(err.Error(), config.AuthFile) {
		t.Fatalf("err %v", err)
	}
}

func TestResolveAPIKey_AuthFileKeyReportsItsFile(t *testing.T) {
	dir, user := repo(t, "", "")
	writeAuthFile(t, user, `{"providers": {"openai": {"api_key": "from-auth-file"}}}`, 0o600)

	cfg, err := load(t, dir, user)

	if err != nil || cfg.LLMAPIKey != "from-auth-file" || cfg.LLMKeySource != "auth.json" || cfg.Layers.AuthFile != config.AuthPath(user) {
		t.Fatalf("err %v key %q source %q auth %q", err, cfg.LLMAPIKey, cfg.LLMKeySource, cfg.Layers.AuthFile)
	}
}

func TestRequireLLMKey_LocalEndpointsNeedNoKey(t *testing.T) {
	for _, base := range []string{"http://localhost:11434/v1", "http://127.0.0.1:8080", "http://[::1]:9/v1"} {
		cfg, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{"LLM_BASE_URL=" + base}})
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.RequireLLMKey(); err != nil {
			t.Errorf("%s: %v", base, err)
		}
	}
	remote, _ := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{"LLM_BASE_URL=https://api.example.com/v1"}})
	if remote.RequireLLMKey() == nil {
		t.Error("a remote endpoint without a key must be refused")
	}
}

func TestMCPCallTimeout_FallsBackToTwoMinutesWhenNotPositive(t *testing.T) {
	for in, want := range map[string]time.Duration{"0": 120 * time.Second, "-5": 120 * time.Second, "30": 30 * time.Second} {
		cfg, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{"MCP_CALL_TIMEOUT_SECONDS=" + in}})
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.MCPCallTimeout(); got != want {
			t.Errorf("MCP_CALL_TIMEOUT_SECONDS=%s: %v, want %v", in, got, want)
		}
	}
}

func TestThreshold_IgnoresValuesOutsideTheOpenUnitInterval(t *testing.T) {
	for in, want := range map[float64]float64{0: 0.75, -0.2: 0.75, 1: 0.75, 1.5: 0.75, 0.6: 0.6} {
		c := &config.Config{}
		c.CompactionThreshold = in
		if got := c.Threshold(); got != want {
			t.Errorf("Threshold with %v = %v, want %v", in, got, want)
		}
	}
}

func TestSkillsPath_ExplicitDirectoryBeatsTheUserDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.HomeEnv, home)
	t.Setenv(config.NoUserDirEnv, "")
	explicit := &config.Config{}
	explicit.SkillsDir = "  /srv/skills  "

	if got := explicit.SkillsPath(); got != "/srv/skills" {
		t.Errorf("explicit: %q", got)
	}
	if got := (&config.Config{}).SkillsPath(); got != filepath.Join(home, config.SkillsFolder) {
		t.Errorf("default: %q", got)
	}
	t.Setenv(config.NoUserDirEnv, "1")
	if got := (&config.Config{}).SkillsPath(); got != "" {
		t.Errorf("without a user directory there is no default skills path: %q", got)
	}
}

func TestNoUserDirEnvValueSemantics(t *testing.T) {
	for val, want := range map[string]bool{"": false, "0": false, "false": false, "No": false, "OFF": false, " ": false, "1": true, "true": true, "yes": true, "x": true} {
		t.Setenv(config.NoUserDirEnv, val)
		if got := config.NoUserDir(); got != want {
			t.Errorf("%s=%q: NoUserDir() = %v, want %v", config.NoUserDirEnv, val, got, want)
		}
	}
}

func TestHomeDirIsTrimmedHarHome(t *testing.T) {
	t.Setenv(config.HomeEnv, "  /custom/har  ")

	if got := config.HomeDir(); got != "/custom/har" {
		t.Fatalf("got %q", got)
	}
}

func TestFindProjectDir(t *testing.T) {
	// Arrange
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	sub := filepath.Join(repoDir, "a", "b")
	for _, d := range []string{filepath.Join(repoDir, ".git"), sub} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(config.HomeEnv, filepath.Join(root, "elsewhere"))

	// Act
	noDirDir, noDirRoot, noDirFound := config.FindProjectDir(sub)
	if err := os.MkdirAll(filepath.Join(repoDir, "a", config.DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, projRoot, found := config.FindProjectDir(sub)

	// Assert
	if noDirFound || noDirDir != "" || noDirRoot != repoDir {
		t.Errorf("without .har the git root is the project root: %q %q %v", noDirDir, noDirRoot, noDirFound)
	}
	if !found || dir != filepath.Join(repoDir, "a", config.DirName) || projRoot != filepath.Join(repoDir, "a") {
		t.Errorf("the nearest .har wins: %q %q %v", dir, projRoot, found)
	}
}

func TestFindProjectDir_NeverTreatsTheUserDirectoryAsAProject(t *testing.T) {
	// Arrange: the user directory ~/.har sits in the home that is also above the project; there is no .git.
	home := t.TempDir()
	userDir := filepath.Join(home, config.DirName)
	work := filepath.Join(home, "work")
	for _, d := range []string{userDir, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(config.HomeEnv, userDir)
	t.Setenv(config.NoUserDirEnv, "")

	// Act
	dir, _, found := config.FindProjectDir(work)

	// Assert
	if found || dir != "" {
		t.Fatalf("the user directory is not a project directory: %q %v", dir, found)
	}
}

func TestModelPriceOptions_ReadsTheProcessEnvironmentAndUserDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.HomeEnv, home)
	t.Setenv(config.NoUserDirEnv, "")
	t.Setenv(config.ModelPricesEnv, "https://prices.example/p.json")

	o, ok := config.ModelPriceOptions()

	if !ok || o.URL != "https://prices.example/p.json" || o.CacheDir != filepath.Join(home, config.CacheFolder) {
		t.Fatalf("ok %v %+v", ok, o)
	}
	t.Setenv(config.ModelPricesEnv, "off")
	if _, ok := config.ModelPriceOptions(); ok {
		t.Fatal("off disables the table")
	}
}

func TestRuntimeAPI_ValidationAndRiskyToolPattern(t *testing.T) {
	cases := map[string]struct{ env, want string }{
		"streams":      {"MAX_STREAMS_PER_USER=0", "MAX_STREAMS_PER_USER must be at least 1"},
		"approval ttl": {"APPROVAL_TTL_HOURS=0", "APPROVAL_TTL_HOURS must be at least 1"},
		"bad pattern":  {"APPROVAL_RISKY_TOOL_PATTERNS=(unclosed", "APPROVAL_RISKY_TOOL_PATTERNS"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{tc.env}})

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
	cfg, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	re := cfg.RiskyToolPattern
	for word, want := range map[string]bool{"create": true, "Delete": true, "writer": true, "read": false, "prelude": false} {
		if re.MatchString(word) != want {
			t.Errorf("default risky pattern on %q: %v, want %v", word, !want, want)
		}
	}
	off, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{"APPROVAL_RISKY_TOOL_PATTERNS=  "}})
	if err != nil || off.RiskyToolPattern != nil {
		t.Fatalf("a blank pattern disables the check: %v %v", err, off.RiskyToolPattern)
	}
}

func TestLoop_RejectsOutOfRangeSettings(t *testing.T) {
	cases := map[string]struct{ env, want string }{
		"stream idle":  {"LLM_STREAM_IDLE_SECONDS=0", "LLM_STREAM_IDLE_SECONDS must be"},
		"call ceiling": {"LLM_CALL_MAX_SECONDS=0", "LLM_CALL_MAX_SECONDS must be"},
		"step retries": {"LLM_STEP_RETRIES=-1", "LLM_STEP_RETRIES must be"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{tc.env}})

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}
