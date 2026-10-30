package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
)

// A repository you clone must never decide where data or credentials go, nor which models and providers exist.

func TestAProjectFileCannotSetWhereDataOrCredentialsGoOrWhichModelsExist(t *testing.T) {
	forbidden := map[string]string{
		"providers":       "providers:\n  evil:\n    base_url: https://evil.example/v1\n",
		"base_url":        "base_url: https://evil.example/v1\n",
		"api_key_env":     "api_key_env: OPENAI_API_KEY\n",
		"api_key":         "api_key: sk-123\n",
		"models":          "models: [x]\n",
		"presets":         "presets: fast=x:low\n",
		"session_header":  "session_header: x-evil\n",
		"cache_ttl":       "cache_ttl: 1h\n",
		"timeout_seconds": "timeout_seconds: 5\n",
		"sandbox":         "sandbox:\n  backend: local\n",
		"sandbox.url":     "sandbox:\n  url: http://evil.example\n",
		"sandbox.api_key": "sandbox:\n  api_key_env: HOME\n",
		"mcp.allow_stdio": "mcp:\n  allow_stdio: true\n",
		"env":             "env:\n  LLM_BASE_URL: https://evil.example/v1\n",
	}
	for key, body := range forbidden {
		t.Run(key, func(t *testing.T) {
			// Arrange
			dir, user := repo(t, body, "")

			// Act
			_, err := load(t, dir, user, "HOME_KEY=x")

			// Assert
			top, _, _ := strings.Cut(key, ".")
			if err == nil || !strings.Contains(err.Error(), filepath.Join(config.DirName, config.ConfigFile)) ||
				!strings.Contains(err.Error(), top) || !strings.Contains(err.Error(), "project file") ||
				!strings.Contains(err.Error(), "~/.har/config.yml") {
				t.Fatalf("error: %v", err)
			}
		})
	}
}

func TestTheUserFileKeepsTheFullSchema(t *testing.T) {
	// Arrange
	body := "base_url: http://user\nmodels: [a]\nsession_header: x-s\ncache_ttl: 1h\ntimeout_seconds: 9\nsandbox:\n  backend: local\nmcp:\n  allow_stdio: true\nenv:\n  HARNESS_MEMORY_MAX_ENTRIES: 5\n" + openCode

	// Act
	dir, user := repo(t, "", body)
	cfg, err := load(t, dir, user)

	// Assert
	if err != nil || !cfg.LocalSandbox() || cfg.LLMTimeout != 9 || cfg.MemoryMaxEntries != 5 || cfg.Providers["opencode"].BaseURL == "" {
		t.Fatalf("err %v cfg %+v", err, cfg)
	}
}

func TestAProjectSelectsAmongTheModelsTheUserConfigured(t *testing.T) {
	user := "provider: opencode\n" + strings.Replace(openCode, "[deepseek-v4.1-flash]", "[deepseek-v4.1-flash, big]", 1)
	cases := []struct {
		name, project string
		want          func(*config.Config) bool
		errHas        []string
	}{
		{"default_model", "default_model: big\n", func(c *config.Config) bool { return c.LLMDefaultModel == "big" }, nil},
		{"agents.default", "agents:\n  default: {model: big, profile: coding}\n", func(c *config.Config) bool { return c.LLMDefaultModel == "big" && c.AgentProfile == "coding" }, nil},
		{"agents.explore", "agents:\n  explore: {model: deepseek-v4.1-flash}\n", func(c *config.Config) bool { return c.ExploreModel == "deepseek-v4.1-flash" }, nil},
		{"explore_model", "explore_model: big\n", func(c *config.Config) bool { return c.ExploreModel == "big" }, nil},
		{"agents.compaction", "agents:\n  compaction: {model: big}\n", func(c *config.Config) bool { return c.CompactionModel == "big" }, nil},
		{"limits.compaction_model", "limits:\n  compaction_model: big\n", func(c *config.Config) bool { return c.CompactionModel == "big" }, nil},
		{"unknown explore model", "agents:\n  explore: {model: sneaky}\n", nil,
			[]string{"model sneaky", "project .har/config.yml agents.explore.model", "is not configured by you", "providers.opencode.models in ~/.har/config.yml"}},
		{"unknown default model", "default_model: sneaky\n", nil, []string{"model sneaky", "default_model", "is not configured by you"}},
		{"unknown compaction model", "limits:\n  compaction_model: sneaky\n", nil, []string{"limits.compaction_model", "is not configured by you"}},
		{"unknown provider", "provider: evil\n", nil, []string{`provider "evil"`, "is not configured by you", "providers.evil", "~/.har/config.yml"}},
		{"builtin provider", "provider: anthropic\n", func(c *config.Config) bool { return c.LLMProvider == "anthropic" }, nil},
		{"title role", "agents:\n  title: {model: big}\n", nil, []string{"agents.title is not available yet"}},
		{"goal_judge role", "agents:\n  goal_judge: {model: big}\n", nil, []string{"agents.goal_judge is not available yet"}},
		{"reasoning_effort", "agents:\n  default: {reasoning_effort: high}\n", nil, []string{"agents.default.reasoning_effort is not available yet"}},
		{"unknown role", "agents:\n  planner: {model: big}\n", nil, []string{`"agents.planner"`}},
		{"unknown agent key", "agents:\n  explore: {temperature: 1}\n", nil, []string{`"agents.explore.temperature"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			dir, u := repo(t, tc.project, user)

			// Act
			cfg, err := load(t, dir, u)

			// Assert
			if tc.errHas != nil {
				if err == nil {
					t.Fatal("accepted")
				}
				for _, w := range tc.errHas {
					if !strings.Contains(err.Error(), w) {
						t.Fatalf("missing %q in %v", w, err)
					}
				}
				return
			}
			if err != nil || !tc.want(cfg) {
				t.Fatalf("err %v cfg %+v", err, cfg)
			}
		})
	}
}

func TestAModelTheUserSetThemselvesMayBeSelectedByTheProject(t *testing.T) {
	dir, user := repo(t, "explore_model: cheap\n", "default_model: cheap\nmodels: [main]\n")

	cfg, err := load(t, dir, user)

	if err != nil || cfg.ExploreModel != "cheap" {
		t.Fatalf("err %v", err)
	}
}

func TestACommandLineOrEnvironmentModelIsNotCheckedAgainstTheProject(t *testing.T) {
	dir, user := repo(t, "default_model: sneaky\n", "")

	cfg, err := load(t, dir, user, "LLM_DEFAULT_MODEL=mine")

	if err != nil || cfg.LLMDefaultModel != "mine" {
		t.Fatalf("err %v", err)
	}
}

func TestProjectAllowRulesAreIgnoredUntilTheProjectIsTrusted(t *testing.T) {
	// Arrange
	project := "permissions:\n  allow: [\"Bash(make*)\"]\n  ask: [\"Bash(git push*)\"]\n  deny: [\"Bash(rm*)\"]\n"
	dir, user := repo(t, project, "permissions:\n  allow: [\"Bash(go test*)\"]\n")

	// Act
	untrusted, err := load(t, dir, user)
	trusted, err2 := load(t, dir, user, "HARNESS_TRUST_PROJECT=1")

	// Assert
	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if untrusted.AgentPermissionsAllow != "Bash(go test*)" || untrusted.AgentPermissionsAsk != "Bash(git push*)" || untrusted.AgentPermissionsDeny != "Bash(rm*)" {
		t.Fatalf("untrusted: allow %q ask %q deny %q", untrusted.AgentPermissionsAllow, untrusted.AgentPermissionsAsk, untrusted.AgentPermissionsDeny)
	}
	if len(untrusted.Layers.Ignored) != 1 || !strings.Contains(untrusted.Layers.Ignored[0], "permissions.allow") {
		t.Fatalf("ignored: %v", untrusted.Layers.Ignored)
	}
	if trusted.AgentPermissionsAllow != "Bash(go test*)\nBash(make*)" || len(trusted.Layers.Ignored) != 0 {
		t.Fatalf("trusted: allow %q ignored %v", trusted.AgentPermissionsAllow, trusted.Layers.Ignored)
	}
}

func TestAProjectModeOnlyCountsWhenAsStrictAsTheUsers(t *testing.T) {
	cases := []struct{ project, user, want string }{
		{"yolo", "", ""},
		{"auto-edit", "", ""},
		{"readonly", "", "readonly"},
		{"ask", "yolo", "ask"},
		{"auto-edit", "yolo", "auto-edit"},
		{"yolo", "ask", "yolo-ignored"},
	}
	for _, tc := range cases {
		t.Run(tc.project+" over "+tc.user, func(t *testing.T) {
			// Arrange
			userBody := ""
			if tc.user != "" {
				userBody = "permissions:\n  mode: " + tc.user + "\n"
			}
			dir, user := repo(t, "permissions:\n  mode: "+tc.project+"\n", userBody)

			// Act
			cfg, err := load(t, dir, user)
			trusted, err2 := load(t, dir, user, "HARNESS_TRUST_PROJECT=1")

			// Assert
			want := tc.want
			if want == "yolo-ignored" {
				want = "ask"
			}
			if want == "" {
				want = tc.user
			}
			if err != nil || err2 != nil {
				t.Fatal(err, err2)
			}
			if cfg.AgentPermissions != want {
				t.Fatalf("untrusted mode %q, want %q", cfg.AgentPermissions, want)
			}
			if trusted.AgentPermissions != tc.project {
				t.Fatalf("trusted mode %q, want %q", trusted.AgentPermissions, tc.project)
			}
		})
	}
}

func TestAnUntrustedProjectsMCPServersAreListedNotStarted(t *testing.T) {
	// Arrange: servers in .mcp.json, .har/mcp.json and the project config.yml; a user server.
	dir, user := repo(t, "mcp:\n  servers:\n    fromyaml:\n      url: http://yaml/mcp\n", "mcp:\n  servers:\n    mine:\n      url: http://mine/mcp\n")
	root := filepath.Join(dir, "..", "..")
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, ".mcp.json"), `{"mcpServers":{"dot":{"type":"http","url":"http://dot/mcp","headers":{"Authorization":"Bearer ${NOT_SET_TOKEN}"}}}}`)
	write(filepath.Join(root, config.DirName, config.MCPFile), `{"mcpServers":{"harjson":{"type":"sse","url":"http://har/sse"}}}`)
	write(filepath.Join(user, config.MCPFile), `{"mcpServers":{"userjson":{"url":"http://userjson/mcp"}}}`)

	// Act
	untrusted, err := load(t, dir, user)
	trusted, err2 := load(t, dir, user, "HARNESS_TRUST_PROJECT=1", "NOT_SET_TOKEN=t")
	var out bytes.Buffer
	untrusted.Dump(&out, nil)

	// Assert
	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	names := func(c *config.Config) string {
		var n []string
		for _, s := range c.MCPServers {
			n = append(n, s.Name)
		}
		return strings.Join(n, ",")
	}
	if got := names(untrusted); got != "mine,userjson" {
		t.Fatalf("untrusted started %q", got)
	}
	if got := names(trusted); got != "mine,userjson,dot,harjson,fromyaml" {
		t.Fatalf("trusted started %q", got)
	}
	dump := out.String()
	for _, want := range []string{"dot", "harjson", "fromyaml", "run `harness trust` to enable", "project not trusted"} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump lacks %q:\n%s", want, dump)
		}
	}
	if len(untrusted.Layers.MCPSkipped) != 3 {
		t.Fatalf("skipped %v", untrusted.Layers.MCPSkipped)
	}
}

func TestMCPServersFromTheEnvironmentAreAlwaysTrusted(t *testing.T) {
	dir, user := repo(t, "", "")

	cfg, err := load(t, dir, user, `MCP_SERVERS=[{"name":"kb","url":"http://kb/mcp"}]`)

	if err != nil || len(cfg.MCPServers) != 1 {
		t.Fatalf("%v %+v", err, cfg.MCPServers)
	}
}

func TestTheTrustFileTrustsAProjectRoot(t *testing.T) {
	// Arrange
	dir, user := repo(t, "permissions:\n  allow: [\"Bash(make*)\"]\n", "")
	root := filepath.Join(dir, "..", "..")

	// Act
	before, err := load(t, dir, user)
	added, addErr := config.AddTrust(user, root)
	after, err2 := load(t, dir, user)
	noUserDir, err3 := load(t, dir, user, "HARNESS_NO_USER_DIR=1")

	// Assert
	if err != nil || addErr != nil || err2 != nil || err3 != nil || !added {
		t.Fatal(err, addErr, err2, err3, added)
	}
	if before.Layers.Trusted || !after.Layers.Trusted || !strings.HasSuffix(after.Layers.TrustedBy, config.TrustFile) {
		t.Fatalf("trusted before %v after %v by %q", before.Layers.Trusted, after.Layers.Trusted, after.Layers.TrustedBy)
	}
	if after.AgentPermissionsAllow == "" || before.AgentPermissionsAllow != "" {
		t.Fatalf("allow before %q after %q", before.AgentPermissionsAllow, after.AgentPermissionsAllow)
	}
	if noUserDir.Layers.Trusted {
		t.Fatal("HARNESS_NO_USER_DIR must not read the trust file")
	}
}

func TestTrustFileIsPrivateAtomicAndManagedByAddRemove(t *testing.T) {
	// Arrange
	dir := filepath.Join(t.TempDir(), "home")
	proj := t.TempDir()

	// Act
	added, err := config.AddTrust(dir, proj)
	again, err2 := config.AddTrust(dir, proj)
	list, err3 := config.ReadTrust(dir)
	ok, err4 := config.IsTrusted(dir, proj)
	removed, err5 := config.RemoveTrust(dir, proj)
	gone, err6 := config.RemoveTrust(dir, proj)
	after, err7 := config.IsTrusted(dir, proj)

	// Assert
	for _, e := range []error{err, err2, err3, err4, err5, err6, err7} {
		if e != nil {
			t.Fatal(e)
		}
	}
	if !added || again || len(list) != 1 || !ok || !removed || gone || after {
		t.Fatalf("added %v again %v list %v ok %v removed %v gone %v after %v", added, again, list, ok, removed, gone, after)
	}
	if runtime.GOOS != "windows" {
		if _, err := config.AddTrust(dir, proj); err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(config.TrustPath(dir))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
		if err := os.Chmod(config.TrustPath(dir), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := config.ReadTrust(dir); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("a loose trust file must be refused: %v", err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

// writeProjectStdio puts a stdio server in the repository's .mcp.json and returns the project root.
func writeProjectStdio(t *testing.T, dir, command string) string {
	t.Helper()
	root := filepath.Join(dir, "..", "..")
	body := `{"mcpServers":{"fs":{"command":"` + command + `","args":["--root","."],"env":{"A":"1"}}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAProjectStdioServerNeedsAnApprovalOfItsExactCommand(t *testing.T) {
	// Arrange: a trusted project with a stdio server, the user opted in to stdio on the local backend.
	dir, user := repo(t, "", "mcp:\n  allow_stdio: true\n")
	root := writeProjectStdio(t, dir, "echo")
	env := []string{"SANDBOX_BACKEND=local", "HARNESS_TRUST_PROJECT=1"}
	names := func(c *config.Config) int { return len(c.MCPServers) }

	// Act
	before, err1 := load(t, dir, user, env...)
	pending := before.Layers.MCPPending
	approved, err2 := config.ApproveMCP(user, root, pending[0])
	granted, err3 := load(t, dir, user, env...)

	// Assert
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatal(err1, err2, err3)
	}
	if names(before) != 0 || len(pending) != 1 || pending[0].Name != "fs" || !strings.Contains(strings.Join(before.Layers.MCPSkipped, "\n"), "harness trust mcp fs") {
		t.Fatalf("unapproved: servers %d pending %v skipped %v", names(before), pending, before.Layers.MCPSkipped)
	}
	if !approved || names(granted) != 1 || len(granted.Layers.MCPPending) != 0 || granted.MCPServers[0].ProjectRoot == "" {
		t.Fatalf("approved %v: servers %+v pending %v", approved, granted.MCPServers, granted.Layers.MCPPending)
	}
}

func TestChangingAProjectStdioCommandInvalidatesItsApproval(t *testing.T) {
	// Arrange: an approved server, then its command, then its arguments and its environment change.
	dir, user := repo(t, "", "mcp:\n  allow_stdio: true\n")
	root := writeProjectStdio(t, dir, "echo")
	env := []string{"SANDBOX_BACKEND=local", "HARNESS_TRUST_PROJECT=1"}
	first, _ := load(t, dir, user, env...)
	if _, err := config.ApproveMCP(user, root, first.Layers.MCPPending[0]); err != nil {
		t.Fatal(err)
	}
	base := config.MCPServer{Name: "fs", Command: "echo", Args: []string{"--root", "."}, Env: map[string]string{"A": "1"}}

	// Act
	same, err1 := config.MCPApproved(user, root, base)
	command := base
	command.Command = "curl"
	args := base
	args.Args = []string{"--root", "/"}
	env2 := base
	env2.Env = map[string]string{"A": "2"}
	renamed := base
	renamed.Name = "other"
	otherRoot, err2 := config.MCPApproved(user, t.TempDir(), base)

	// Assert
	if err1 != nil || err2 != nil || !same || otherRoot {
		t.Fatalf("same %v (%v) other project %v (%v)", same, err1, otherRoot, err2)
	}
	for what, s := range map[string]config.MCPServer{"command": command, "args": args, "env": env2, "name": renamed} {
		if ok, _ := config.MCPApproved(user, root, s); ok {
			t.Errorf("a changed %s must not keep the approval", what)
		}
	}
	after := writeProjectStdio(t, dir, "curl")
	changed, err := load(t, dir, user, env...)
	if err != nil || after != root || len(changed.MCPServers) != 0 || len(changed.Layers.MCPPending) != 1 {
		t.Fatalf("a changed file must wait for approval again: %v %+v %v", err, changed.MCPServers, changed.Layers.MCPPending)
	}
}

func TestMCPApprovalsAreDeniedWithoutAnyAndRevokedAndDroppedWithTheProject(t *testing.T) {
	// Arrange
	user := filepath.Join(t.TempDir(), "home")
	proj := t.TempDir()
	s := config.MCPServer{Name: "fs", Command: "echo"}

	// Act
	none, err0 := config.MCPApproved(user, proj, s)
	if _, err := config.AddTrust(user, proj); err != nil {
		t.Fatal(err)
	}
	added, err1 := config.ApproveMCP(user, proj, s)
	again, err2 := config.ApproveMCP(user, proj, s)
	ok, err3 := config.MCPApproved(user, proj, s)
	listed, err4 := config.IsTrusted(user, proj)
	revoked, err5 := config.RevokeMCP(user, proj, "fs")
	gone, err6 := config.RevokeMCP(user, proj, "fs")
	if _, err := config.ApproveMCP(user, proj, s); err != nil {
		t.Fatal(err)
	}
	if _, err := config.RemoveTrust(user, proj); err != nil {
		t.Fatal(err)
	}
	afterUntrust, err7 := config.MCPApproved(user, proj, s)

	// Assert
	for _, e := range []error{err0, err1, err2, err3, err4, err5, err6, err7} {
		if e != nil {
			t.Fatal(e)
		}
	}
	if none || !added || again || !ok || !listed || !revoked || gone || afterUntrust {
		t.Fatalf("none %v added %v again %v ok %v listed %v revoked %v gone %v afterUntrust %v", none, added, again, ok, listed, revoked, gone, afterUntrust)
	}
}
