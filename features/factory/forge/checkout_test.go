package forge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/features/factory/forge"
)

func checkoutEnv(t *testing.T, ti factory.ThreadInfo) (*forge.Checkout, *recSandbox) {
	t.Helper()
	st := newStore(newClock())
	if ti.ThreadID != "" {
		if err := st.SetThreadInfo(context.Background(), ti); err != nil {
			t.Fatal(err)
		}
	}
	return &forge.Checkout{Auth: staticAuth{}, Store: st}, &recSandbox{}
}

// tokenSightings reports every place the sandbox saw the token: the command text, the working directory,
// the env of a command, and any file written. allowedEnv are the commands that may carry it in Env only.
func tokenSightings(sb *recSandbox) (inText, inEnv, inFiles []string) {
	for _, c := range sb.cmds {
		if strings.Contains(c.Command, token) || strings.Contains(c.Cwd, token) {
			inText = append(inText, c.Command)
		}
		for k, v := range c.Env {
			if strings.Contains(v, token) {
				inEnv = append(inEnv, k)
			}
		}
	}
	for path, data := range sb.files {
		if strings.Contains(data, token) || strings.Contains(path, token) {
			inFiles = append(inFiles, path)
		}
	}
	return inText, inEnv, inFiles
}

func TestSetupClonesWithAOneShotCredentialAndLeavesNoTokenBehind(t *testing.T) {
	// Arrange
	c, sb := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	// Act
	notice := c.Setup(context.Background(), "th1", sb)
	// Assert
	if !strings.Contains(notice, repo) || !strings.Contains(notice, "/workspace/repo") || !strings.Contains(notice, "har/abc") {
		t.Fatalf("notice = %q", notice)
	}
	if len(sb.cmds) != 1 {
		t.Fatalf("commands = %d, want the single setup command", len(sb.cmds))
	}
	setup := sb.cmds[0]
	inText, inEnv, inFiles := tokenSightings(sb)
	if len(inText) > 0 || len(inFiles) > 0 {
		t.Fatalf("token in command text %v or files %v", inText, inFiles)
	}
	if len(inEnv) != 1 || inEnv[0] != "GH_TOKEN" || setup.Env["GH_TOKEN"] != token {
		t.Fatalf("token must be only in GH_TOKEN of the setup command, found in %v", inEnv)
	}
	for _, want := range []string{"clone --depth", "checkout -B", "Harness Agent", "agent@harness.local",
		`-c credential.helper= -c 'credential.helper=!f() { echo username=x-access-token; echo "password=$GH_TOKEN"; }; f'`} {
		if !strings.Contains(setup.Command, want) {
			t.Errorf("setup script lacks %q", want)
		}
	}
	if setup.Env["BASE_BRANCH"] != "main" || setup.Env["THREAD_BRANCH"] != "har/abc" || setup.Env["REPO_URL"] != "https://github.com/acme/app.git" {
		t.Fatalf("setup env = %v", setup.Env)
	}
}

func TestSetupNeverStoresAHelperOrCredentialInTheRepository(t *testing.T) {
	// Arrange
	c, sb := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	// Act
	c.Setup(context.Background(), "th1", sb)
	// Assert: -c is only ever given to git itself (never stored), nothing writes the config or a file.
	script := sb.cmds[0].Command
	for _, bad := range []string{"git config credential", "git config --global", "credential.helper store", "x-access-token:", "git-credentials", "clone -c", "--config", "umask", "printf"} {
		if strings.Contains(script, bad) {
			t.Errorf("setup script contains %q", bad)
		}
	}
	if strings.Count(script, "git config user.") != 2 || strings.Count(script, "git config ") != 3 {
		t.Errorf("the only config the script may write is user.name and user.email (and it reads the config to guard it):\n%s", script)
	}
	if len(sb.files) != 0 {
		t.Errorf("files written: %v", sb.files)
	}
}

func TestSetupNeverPassesTheTokenToPlainGitCommands(t *testing.T) {
	// Arrange / Act
	c, sb := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	c.Setup(context.Background(), "th1", sb)
	// Assert: the script drops GH_TOKEN from its own environment and hands it back only to the authenticated git calls.
	script := sb.cmds[0].Command
	if !strings.Contains(script, "unset GH_TOKEN") || strings.Count(script, `GH_TOKEN="$TOK"`) != 1 {
		t.Errorf("token scoping missing:\n%s", script)
	}
}

func TestSetupIsANoopWithoutRepository(t *testing.T) {
	tests := []struct {
		name string
		ti   factory.ThreadInfo
		id   string
	}{
		{"ordinary thread", factory.ThreadInfo{}, "th1"},
		{"thread without repo", factory.ThreadInfo{ThreadID: "th1", TaskID: "t"}, "th1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			c, sb := checkoutEnv(t, tc.ti)
			// Act
			notice := c.Setup(context.Background(), tc.id, sb)
			// Assert
			if notice != "" || len(sb.cmds) != 0 {
				t.Fatalf("notice = %q, commands = %d", notice, len(sb.cmds))
			}
		})
	}
}

func TestSetupRefusesUnsafeOrProtectedNames(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		base   string
	}{
		{"shell metacharacters in the branch", "x; rm -rf /", "main"},
		{"option-like branch", "$(id)", "main"},
		{"unsafe base", "har/a", "main`id`"},
		{"branch is the base", "main", "main"},
		{"no branch", "", "main"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			c, sb := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: tc.base, Branch: tc.branch})
			// Act
			notice := c.Setup(context.Background(), "th1", sb)
			// Assert
			if !strings.Contains(notice, "not checked out") || len(sb.cmds) != 0 {
				t.Fatalf("notice = %q, commands = %d", notice, len(sb.cmds))
			}
		})
	}
}

func TestSetupFailureMessageNeverContainsToken(t *testing.T) {
	// Arrange
	c, sb := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	sb.exit = 128
	sb.out = "fatal: could not read from https://x-access-token:" + token + "@github.com/acme/app.git\n"
	// Act
	notice := c.Setup(context.Background(), "th1", sb)
	// Assert
	if !strings.Contains(notice, "failed") || strings.Contains(notice, token) {
		t.Fatalf("notice = %q", notice)
	}
}

func TestSetupWithoutTokenClonesAnonymously(t *testing.T) {
	// Arrange
	c, sb := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, Branch: "har/abc"})
	c.Auth = nil
	// Act
	notice := c.Setup(context.Background(), "th1", sb)
	// Assert
	if !strings.Contains(notice, "default branch") || len(sb.cmds) != 1 {
		t.Fatalf("notice = %q, commands = %d", notice, len(sb.cmds))
	}
	if _, has := sb.cmds[0].Env["GH_TOKEN"]; has {
		t.Error("no token available: the clone must not carry a GH_TOKEN")
	}
}

func TestSetupHonoursCustomDir(t *testing.T) {
	// Arrange
	c, sb := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	c.Dir = "/workspace/other"
	// Act
	notice := c.Setup(context.Background(), "th1", sb)
	// Assert
	if !strings.Contains(notice, "/workspace/other") || sb.cmds[len(sb.cmds)-1].Env["REPO_DIR"] != "/workspace/other" {
		t.Fatalf("notice = %q", notice)
	}
}

func TestSection(t *testing.T) {
	// Arrange
	st := newStore(newClock())
	_ = st.SetThreadInfo(context.Background(), factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	_ = st.SetThreadInfo(context.Background(), factory.ThreadInfo{ThreadID: "th2", Role: "reviewer"})
	sec := forge.Section(st)
	in := func(id string) agent.Input { return agent.Input{Run: &store.Run{ThreadID: id}} }
	// Act
	got := sec(context.Background(), in("th1"))
	// Assert
	for _, want := range []string{"/workspace/repo", "har/abc", "cannot push", "open_pr", "ci_log", "never commit on main", "protected"} {
		if !strings.Contains(got, want) {
			t.Errorf("section lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "git push") {
		t.Errorf("the section must not tell the agent to push:\n%s", got)
	}
	if s := sec(context.Background(), in("th2")); s != "" {
		t.Errorf("a thread without a repository gets %q", s)
	}
	if s := sec(context.Background(), in("unknown")); s != "" {
		t.Errorf("an ordinary thread gets %q", s)
	}
}
