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

func writeAuthFile(t *testing.T, dir, body string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, config.AuthFile)
	if err := os.WriteFile(f, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, mode); err != nil {
		t.Fatal(err)
	}
	return f
}

const authBody = `{"providers": {"opencode": {"api_key": "sk-auth-opencode-1234"}, "openai": {"api_key": "sk-auth-openai-9999"}}}`

func TestUserDirFollowsHarHomeAndNoUserDir(t *testing.T) {
	// Arrange
	home := t.TempDir()
	t.Setenv("HAR_HOME", home)
	t.Setenv("HARNESS_NO_USER_DIR", "")

	// Act / Assert
	if got := config.UserDir(); got != home {
		t.Fatalf("HAR_HOME ignored: %q", got)
	}
	t.Setenv("HARNESS_NO_USER_DIR", "1")
	if got := config.UserDir(); got != "" {
		t.Fatalf("HARNESS_NO_USER_DIR ignored: %q", got)
	}
	if got := config.HomeDir(); got != home {
		t.Fatalf("HomeDir must ignore the switch: %q", got)
	}
	t.Setenv("HAR_HOME", "")
	t.Setenv("HARNESS_NO_USER_DIR", "")
	h, _ := os.UserHomeDir()
	if got := config.UserDir(); got != filepath.Join(h, ".har") {
		t.Fatalf("default: %q", got)
	}
}

func TestAuthFileMustNotBeReadableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	// Arrange
	dir := t.TempDir()
	file := writeAuthFile(t, dir, authBody, 0o644)

	// Act
	_, err := config.ReadAuth(dir)
	_, loadErr := config.LoadWith(config.LoadOptions{Dir: t.TempDir(), UserDir: dir, Environ: []string{}})

	// Assert
	for _, e := range []error{err, loadErr} {
		if e == nil || !strings.Contains(e.Error(), file) || !strings.Contains(e.Error(), "chmod 600") || strings.Contains(e.Error(), "sk-auth") {
			t.Fatalf("error: %v", e)
		}
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := config.ReadAuth(dir)
	if err != nil || keys["opencode"] != "sk-auth-opencode-1234" {
		t.Fatalf("keys %v err %v", keys, err)
	}
}

func TestAnInsecureAuthFileIsNotConsultedWhenAnotherSourceSuppliesTheKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	dir := t.TempDir()
	writeAuthFile(t, dir, authBody, 0o666)

	cfg, err := config.LoadWith(config.LoadOptions{Dir: t.TempDir(), UserDir: dir, Environ: []string{"LLM_API_KEY=from-env"}})

	if err != nil || cfg.LLMAPIKey != "from-env" {
		t.Fatalf("err %v", err)
	}
}

func TestSetAuthKeyWritesAtomicallyWith0600AndKeepsOtherEntries(t *testing.T) {
	// Arrange
	dir := filepath.Join(t.TempDir(), "newhome")

	// Act
	if err := config.SetAuthKey(dir, "a", "key-aaaaaaaaaaaa1111"); err != nil {
		t.Fatal(err)
	}
	if err := config.SetAuthKey(dir, "b", "key-bbbbbbbbbbbb2222"); err != nil {
		t.Fatal(err)
	}
	ok, err := config.RemoveAuthKey(dir, "a")

	// Assert
	if err != nil || !ok {
		t.Fatalf("remove: %v %v", ok, err)
	}
	keys, err := config.ReadAuth(dir)
	if err != nil || len(keys) != 1 || keys["b"] != "key-bbbbbbbbbbbb2222" {
		t.Fatalf("keys %v err %v", keys, err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(dir, config.AuthFile))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %v", st.Mode().Perm())
		}
		ds, _ := os.Stat(dir)
		if ds.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %v", ds.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
	if err := config.SetAuthKey(dir, "bad name", "x"); err == nil {
		t.Fatal("invalid provider name accepted")
	}
}

func TestSetAuthKeyRefusesAnInsecureExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	dir := t.TempDir()
	writeAuthFile(t, dir, authBody, 0o644)

	err := config.SetAuthKey(dir, "x", "key-xxxxxxxxxxxx")

	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("%v", err)
	}
}

func TestMaskKeyShowsOnlyTheLastFour(t *testing.T) {
	if got := config.MaskKey("sk-abcdefghijklmnop1234"); got != "****1234" {
		t.Fatal(got)
	}
	if got := config.MaskKey("short"); got != "****" {
		t.Fatal(got)
	}
}

// keyOf loads with a project file, a user file, an auth.json ("" = none) and the environment.
func keyOf(t *testing.T, project, user, auth string, environ ...string) (*config.Config, error) {
	t.Helper()
	dir, userDir := repo(t, project, user)
	if auth != "" {
		writeAuthFile(t, userDir, auth, 0o600)
	}
	return load(t, dir, userDir, environ...)
}

const openCode = "providers:\n  opencode:\n    type: openai\n    base_url: https://opencode.ai/zen/go/v1\n    session_header: x-opencode-session\n    models: [deepseek-v4.1-flash]\n    default_model: deepseek-v4.1-flash\n    api_key_env: OPENCODE_KEY\n"

func TestAPIKeyResolutionOrder(t *testing.T) {
	const auth = `{"providers": {"opencode": {"api_key": "k3"}}}`
	cases := []struct {
		name    string
		environ []string
		auth    string
		want    string
		source  string
	}{
		{"LLM_API_KEY alone", []string{"LLM_API_KEY=k1"}, "", "k1", "env LLM_API_KEY"},
		{"api_key_env alone", []string{"OPENCODE_KEY=k2"}, "", "k2", "env OPENCODE_KEY"},
		{"auth.json alone", nil, auth, "k3", "auth.json"},
		{"all together: LLM_API_KEY first", []string{"LLM_API_KEY=k1", "OPENCODE_KEY=k2", "OPENAI_API_KEY=k4"}, auth, "k1", "env LLM_API_KEY"},
		{"without LLM_API_KEY: api_key_env", []string{"OPENCODE_KEY=k2", "OPENAI_API_KEY=k4"}, auth, "k2", "env OPENCODE_KEY"},
		{"without api_key_env: auth.json", []string{"OPENAI_API_KEY=k4"}, auth, "k3", "auth.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := keyOf(t, "provider: opencode\n", openCode, tc.auth, tc.environ...)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LLMAPIKey != tc.want || cfg.LLMKeySource != tc.source {
				t.Fatalf("key %q from %q, want %q from %q", cfg.LLMAPIKey, cfg.LLMKeySource, tc.want, tc.source)
			}
		})
	}
}

func TestConventionalKeysByTypeForTheBuiltInProviders(t *testing.T) {
	both := []string{"OPENAI_API_KEY=oa", "ANTHROPIC_API_KEY=an"}

	openai, err1 := keyOf(t, "", "provider: openai\n", "", both...)
	anthropic, err2 := keyOf(t, "", "provider: anthropic\n", "", both...)

	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if openai.LLMAPIKey != "oa" || openai.LLMKeySource != "env OPENAI_API_KEY" || anthropic.LLMAPIKey != "an" || anthropic.LLMKeySource != "env ANTHROPIC_API_KEY" {
		t.Fatalf("%q %q / %q %q", openai.LLMAPIKey, openai.LLMKeySource, anthropic.LLMAPIKey, anthropic.LLMKeySource)
	}
}

func TestAuthJSONEntryNamedLikeABuiltInType(t *testing.T) {
	cfg, err := keyOf(t, "", "", `{"providers": {"openai": {"api_key": "from-auth"}}}`, "OPENAI_API_KEY=conv")
	if err != nil || cfg.LLMAPIKey != "from-auth" || cfg.LLMKeySource != "auth.json" {
		t.Fatalf("%v %q", err, cfg.LLMKeySource)
	}
}

func TestANamedProviderWithItsOwnBaseURLNeverReceivesTheConventionalKey(t *testing.T) {
	cfg, err := keyOf(t, "provider: opencode\n", openCode, "", "OPENAI_API_KEY=openai-only")
	if err != nil || cfg.LLMAPIKey != "" {
		t.Fatalf("%v", err)
	}
	e := cfg.RequireLLMKey()
	if e == nil || !strings.Contains(e.Error(), "harness auth login opencode") || !strings.Contains(e.Error(), "OPENCODE_KEY") {
		t.Fatalf("hint: %v", e)
	}
}

func TestRequireLLMKeyAcceptsLocalEndpointsAndSetKeys(t *testing.T) {
	cfg, _ := keyOf(t, "", "provider: openai\nbase_url: http://localhost:11434/v1\n", "")
	if err := cfg.RequireLLMKey(); err != nil {
		t.Fatal(err)
	}
	cfg, _ = keyOf(t, "", "", "", "LLM_API_KEY=x")
	if err := cfg.RequireLLMKey(); err != nil {
		t.Fatal(err)
	}
	cfg, _ = keyOf(t, "", "", "")
	if err := cfg.RequireLLMKey(); err == nil || !strings.Contains(err.Error(), "harness auth login openai") {
		t.Fatalf("%v", err)
	}
}

func TestNamedProviderSelectionAndProjectOverride(t *testing.T) {
	project := "provider: opencode\ndefault_model: project-model\n"

	cfg, err := keyOf(t, project, strings.Replace(openCode, "[deepseek-v4.1-flash]", "[deepseek-v4.1-flash, project-model]", 1), "")

	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers["opencode"]
	if cfg.LLMProviderName != "opencode" || cfg.LLMProvider != "openai" || cfg.LLMBaseURL != "https://opencode.ai/zen/go/v1" ||
		cfg.LLMDefaultModel != "project-model" || cfg.LLMSessionHeader != "x-opencode-session" || p.APIKeyEnv != "OPENCODE_KEY" {
		t.Fatalf("name %q type %q base %q model %q hdr %q p %+v", cfg.LLMProviderName, cfg.LLMProvider, cfg.LLMBaseURL, cfg.LLMDefaultModel, cfg.LLMSessionHeader, p)
	}
	if len(cfg.LLMModels) != 2 || cfg.LLMModels[0] != "deepseek-v4.1-flash" || cfg.LLMModels[1] != "project-model" {
		t.Fatalf("models %v", cfg.LLMModels)
	}
	if cfg.Layers.Sources["LLM_DEFAULT_MODEL"] != config.SourceProject {
		t.Fatalf("the project's selection must win over the provider's default: %v", cfg.Layers.Sources)
	}
}

func TestProviderFlagAndEnvSelection(t *testing.T) {
	dir, user := repo(t, "", "provider: opencode\n"+openCode+"  claude:\n    type: anthropic\n    default_model: c1\n")

	byEnv, err := load(t, dir, user, "LLM_PROVIDER=claude")
	byFlag, err2 := config.LoadWith(config.LoadOptions{Dir: dir, UserDir: user, Environ: []string{"LLM_PROVIDER=claude"}, Provider: "opencode"})
	_, err3 := load(t, dir, user, "LLM_PROVIDER=nope")

	if err != nil || err2 != nil || byEnv.LLMProvider != "anthropic" || byFlag.LLMProviderName != "opencode" {
		t.Fatalf("%v %v", err, err2)
	}
	if err3 == nil || !strings.Contains(err3.Error(), "nope") {
		t.Fatalf("unknown provider: %v", err3)
	}
}

func TestEnvStillBeatsAProviderDefinition(t *testing.T) {
	cfg, err := keyOf(t, "provider: opencode\n", openCode, "", "LLM_BASE_URL=http://env", "LLM_DEFAULT_MODEL=envmodel")
	if err != nil || cfg.LLMBaseURL != "http://env" || cfg.LLMDefaultModel != "envmodel" {
		t.Fatalf("%v %q %q", err, cfg.LLMBaseURL, cfg.LLMDefaultModel)
	}
}

func TestProvidersRejectPlainKeysAndUnknownKeys(t *testing.T) {
	for body, want := range map[string]string{
		"providers:\n  p:\n    api_key: sk-secret\n": "secrets do not belong",
		"providers:\n  p:\n    colour: red\n":        `unknown key "providers.p.colour"`,
		"providers:\n  p:\n    type: gemini\n":       "openai or anthropic",
	} {
		_, err := keyOf(t, "", body, "")
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "sk-secret") {
			t.Fatalf("%q: %v", body, err)
		}
	}
	if _, err := keyOf(t, "api_key: sk-x\n", "", ""); err == nil {
		t.Fatal("flat api_key accepted")
	}
}

func TestHarnessNoUserDirIgnoresAuthAndUserConfig(t *testing.T) {
	dir, user := repo(t, "", "default_model: from-user\n")
	writeAuthFile(t, user, `{"providers": {"openai": {"api_key": "from-auth"}}}`, 0o600)

	with, err := load(t, dir, user)
	without, err2 := load(t, dir, user, "HARNESS_NO_USER_DIR=1")

	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if with.LLMAPIKey != "from-auth" || with.LLMDefaultModel != "from-user" {
		t.Fatalf("%q", with.LLMDefaultModel)
	}
	if without.LLMAPIKey != "" || without.LLMDefaultModel == "from-user" || without.Layers.UserDir != "" {
		t.Fatalf("user dir read: %q", without.LLMDefaultModel)
	}
}

func TestHarHomeIsWhereLoadReadsTheUserFiles(t *testing.T) {
	home := t.TempDir()
	writeAuthFile(t, home, authBody, 0o600)
	if err := os.WriteFile(filepath.Join(home, config.ConfigFile), []byte("default_model: hh\nprovider: opencode\n"+openCode), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAR_HOME", home)
	t.Setenv("HARNESS_NO_USER_DIR", "")

	cfg, err := config.LoadWith(config.LoadOptions{Dir: t.TempDir()})

	if err != nil || cfg.LLMAPIKey != "sk-auth-opencode-1234" || cfg.Layers.UserDir != home {
		t.Fatalf("%v %q", err, cfg.Layers.UserDir)
	}
}

func TestDumpMasksTheKeyAndNamesItsSource(t *testing.T) {
	cfg, err := keyOf(t, "provider: opencode\n", openCode, `{"providers": {"opencode": {"api_key": "sk-auth-opencode-1234"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	cfg.Dump(&out, nil)

	got := out.String()
	if strings.Contains(got, "sk-auth") || !strings.Contains(got, "LLM_API_KEY") || !strings.Contains(got, "(auth.json)") || !strings.Contains(got, "****") {
		t.Fatalf("dump:\n%s", got)
	}
}
