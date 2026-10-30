package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
)

// repo makes <tmp>/repo (a git root) with the given project config.yml and a
// user directory with the given config.yml ("" = no file).
func repo(t *testing.T, project, user string) (workdir, userDir string) {
	t.Helper()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	userDir = filepath.Join(root, "home", config.DirName)
	for _, d := range []string{filepath.Join(repoDir, ".git"), filepath.Join(repoDir, config.DirName), filepath.Join(repoDir, "pkg", "sub"), userDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		if body != "" {
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(filepath.Join(repoDir, config.DirName, config.ConfigFile), project)
	write(filepath.Join(userDir, config.ConfigFile), user)
	return filepath.Join(repoDir, "pkg", "sub"), userDir
}

func load(t *testing.T, dir, userDir string, environ ...string) (*config.Config, error) {
	t.Helper()
	return config.LoadWith(config.LoadOptions{Dir: dir, UserDir: userDir, Environ: environ})
}

func TestPrecedenceEnvOverProjectOverUserOverDefault(t *testing.T) {
	// Arrange
	dir, user := repo(t,
		"default_model: from-project\n",
		"default_model: from-user\nbase_url: http://user\nprovider: anthropic\nmodels: [a, b, from-project]\nsandbox:\n  backend: local\n")

	// Act
	cfg, err := load(t, dir, user, "LLM_BASE_URL=http://env")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMBaseURL != "http://env" || cfg.LLMDefaultModel != "from-project" || cfg.LLMProvider != "anthropic" || !cfg.LocalSandbox() {
		t.Fatalf("base %q model %q provider %q local %v", cfg.LLMBaseURL, cfg.LLMDefaultModel, cfg.LLMProvider, cfg.LocalSandbox())
	}
	if cfg.AgentMaxSteps != 60 {
		t.Fatalf("default lost: %d", cfg.AgentMaxSteps)
	}
	if got := cfg.Layers.Sources["LLM_BASE_URL"]; got != config.SourceEnv {
		t.Fatalf("base_url source %s", got)
	}
	if got := cfg.Layers.Sources["LLM_DEFAULT_MODEL"]; got != config.SourceProject {
		t.Fatalf("model source %s", got)
	}
	if got := cfg.Layers.Sources["LLM_PROVIDER"]; got != config.SourceUser {
		t.Fatalf("provider source %s", got)
	}
}

func TestProjectDirIsFoundWalkingUpToTheGitRoot(t *testing.T) {
	// Arrange
	dir, user := repo(t, "limits:\n  max_steps: 7\n", "")

	// Act
	cfg, err := load(t, dir, user)

	// Assert
	if err != nil || cfg.AgentMaxSteps != 7 || !strings.HasSuffix(cfg.Layers.ProjectDir, filepath.Join("repo", config.DirName)) {
		t.Fatalf("err %v layers %+v", err, cfg)
	}
}

func TestUnknownKeysAndSecretsAreErrorsNamingTheFile(t *testing.T) {
	// project: the file of the repository; otherwise the user file.
	cases := map[string]struct {
		project bool
		body    string
	}{
		"typo":        {true, "defualt_model: x\n"},
		"nested typo": {false, "sandbox:\n  bakend: local\n"},
		"api_key":     {false, "api_key: sk-123\n"},
		"sandbox key": {false, "sandbox:\n  api_key: sk-123\n"},
		"bad int":     {true, "limits:\n  max_steps: many\n"},
		"bad backend": {false, "sandbox:\n  backend: docker\n"},
		"env secret":  {false, "env:\n  POSTGRES_PASSWORD: x\n"},
		"env unknown": {false, "env:\n  NOT_A_SETTING: x\n"},
		"stdio":       {true, "mcp:\n  allow_stdio: true\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			project, userBody := tc.body, ""
			if !tc.project {
				project, userBody = "", tc.body
			}
			dir, user := repo(t, project, userBody)

			// Act
			_, err := load(t, dir, user)

			// Assert
			if err == nil || !strings.Contains(err.Error(), config.ConfigFile) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestUnknownKeyMessageNamesTheKey(t *testing.T) {
	// Arrange
	dir, user := repo(t, "", "sandbox:\n  bakend: local\n")

	// Act
	_, err := load(t, dir, user)

	// Assert
	if err == nil || !strings.Contains(err.Error(), `"sandbox.bakend"`) {
		t.Fatalf("err %v", err)
	}
}

func TestApiKeyEnvReadsTheNamedVariable(t *testing.T) {
	// Arrange
	dir, user := repo(t, "", "api_key_env: MY_KEY\n")

	// Act
	cfg, err := load(t, dir, user, "MY_KEY=sekret")
	_, missing := load(t, dir, user)

	// Assert
	if err != nil || cfg.LLMAPIKey != "sekret" {
		t.Fatalf("err %v key %q", err, cfg.LLMAPIKey)
	}
	if missing == nil || !strings.Contains(missing.Error(), "MY_KEY") {
		t.Fatalf("missing: %v", missing)
	}
}

func TestPermissionsListsAddUpAcrossFiles(t *testing.T) {
	// Arrange
	dir, user := repo(t, "permissions:\n  mode: readonly\n  deny: [\"Bash(rm*)\"]\n", "permissions:\n  deny: [\"Bash(docker*)\"]\n  allow: [\"Bash(make*)\"]\n")

	// Act
	cfg, err := load(t, dir, user)

	// Assert
	if err != nil || cfg.AgentPermissions != "readonly" {
		t.Fatalf("err %v mode %q", err, cfg.AgentPermissions)
	}
	if cfg.AgentPermissionsDeny != "Bash(docker*)\nBash(rm*)" || cfg.AgentPermissionsAllow != "Bash(make*)" {
		t.Fatalf("deny %q allow %q", cfg.AgentPermissionsDeny, cfg.AgentPermissionsAllow)
	}
}

func TestDumpShowsSourcesAndMasksSecrets(t *testing.T) {
	// Arrange
	dir, user := repo(t, "default_model: m\n", "provider: anthropic\napi_key_env: MY_KEY\nmodels: [m]\n")
	cfg, err := load(t, dir, user, "MY_KEY=sekret", "LLM_LLM_TIMEOUT_SECONDS=1")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	// Act
	cfg.Dump(&out, map[string]string{"-model": "flagged"})

	// Assert
	got := out.String()
	for _, want := range []string{"LLM_DEFAULT_MODEL", "(project file)", "LLM_PROVIDER", "(user file)", "-model", "(flag)", "LLM_API_KEY"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "sekret") {
		t.Fatalf("secret printed:\n%s", got)
	}
}

func TestStdioCannotBeEnabledByTheProjectButByTheUserFile(t *testing.T) {
	// Arrange
	dir, user := repo(t, "", "mcp:\n  allow_stdio: true\n")
	if err := os.WriteFile(filepath.Join(dir, "..", "..", ".mcp.json"), []byte(`{"mcpServers":{"fs":{"command":"echo","args":["hi"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Act
	pendingLocal, errP := load(t, dir, user, "SANDBOX_BACKEND=local", "HARNESS_TRUST_PROJECT=1") // a project's stdio server also needs its approval
	if errP != nil || len(pendingLocal.MCPServers) != 0 || len(pendingLocal.Layers.MCPPending) != 1 {
		t.Fatalf("unapproved project stdio server: %v %+v %v", errP, pendingLocal.MCPServers, pendingLocal.Layers.MCPPending)
	}
	if _, err := config.ApproveMCP(user, pendingLocal.Layers.ProjectRoot, pendingLocal.Layers.MCPPending[0]); err != nil {
		t.Fatal(err)
	}
	local, err1 := load(t, dir, user, "SANDBOX_BACKEND=local", "HARNESS_TRUST_PROJECT=1")
	remote, err2 := load(t, dir, user, "SANDBOX_BACKEND=opensandbox", "OPENSANDBOX_URL=http://x", "HARNESS_TRUST_PROJECT=1")
	noOptIn, err3 := load(t, dir, user, "SANDBOX_BACKEND=local", "HARNESS_MCP_STDIO=0", "HARNESS_TRUST_PROJECT=1")

	// Assert
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatal(err1, err2, err3)
	}
	if len(local.MCPServers) != 1 || local.MCPServers[0].Name != "fs" || !local.MCPServers[0].IsStdio() {
		t.Fatalf("local: %+v", local.MCPServers)
	}
	if len(remote.MCPServers) != 0 || len(remote.Layers.MCPSkipped) != 1 {
		t.Fatalf("remote: %+v %v", remote.MCPServers, remote.Layers.MCPSkipped)
	}
	if len(noOptIn.MCPServers) != 0 {
		t.Fatalf("env HARNESS_MCP_STDIO=0 must win over the user file: %+v", noOptIn.MCPServers)
	}
}

func TestMCPFilesMergeByNameAndExpandHeaders(t *testing.T) {
	// Arrange
	dir, user := repo(t, "mcp:\n  servers:\n    kb:\n      url: http://yaml/mcp\n", "")
	root := filepath.Join(dir, "..", "..")
	writeFile := func(p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(root, ".mcp.json"), `{"mcpServers":{"kb":{"type":"http","url":"http://dot-mcp/mcp"},"docs":{"type":"sse","url":"http://docs/sse","headers":{"Authorization":"Bearer ${DOCS_TOKEN}"}}}}`)

	// Act
	cfg, err := load(t, dir, user, "DOCS_TOKEN=abc", "HARNESS_TRUST_PROJECT=1")
	_, unresolved := load(t, dir, user, "HARNESS_TRUST_PROJECT=1")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]config.MCPServer{}
	for _, s := range cfg.MCPServers {
		byName[s.Name] = s
	}
	if byName["kb"].URL != "http://yaml/mcp" {
		t.Fatalf("config.yml must override .mcp.json: %+v", byName["kb"])
	}
	if d := byName["docs"]; d.Type != "sse" || d.Headers["Authorization"] != "Bearer abc" {
		t.Fatalf("docs %+v", d)
	}
	if unresolved == nil || !strings.Contains(unresolved.Error(), "DOCS_TOKEN") {
		t.Fatalf("unresolved reference must be an error: %v", unresolved)
	}
}
