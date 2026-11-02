package forge_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/features/factory/forge"
)

// gitSandbox answers the inspect command with the given state and the push
// command with the given output and exit code.
func gitSandbox(inspect string, pushOut string, pushExit int) *recSandbox {
	return &recSandbox{script: func(req sandbox.ExecRequest) (string, int) {
		if strings.Contains(req.Command, " push ") {
			return pushOut, pushExit
		}
		return inspect, 0
	}}
}

func pushEnv(t *testing.T, ti factory.ThreadInfo) (*forge.Checkout, *recSandbox) {
	t.Helper()
	c, _ := checkoutEnv(t, ti)
	return c, gitSandbox("dirty=0\nahead=2\n", "", 0)
}

func TestPushTargetsExactlyTheTaskBranch(t *testing.T) {
	for _, branch := range []string{"har/abc", "factory/9f3c2a", "fix-login.v2", "a/b/c_d"} {
		t.Run(branch, func(t *testing.T) {
			// Arrange
			c, sb := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: branch})
			// Act
			res, err := c.Push(context.Background(), "th1", sb, "main")
			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if res.Branch != branch || res.Ahead != 2 || res.Dirty != 0 {
				t.Fatalf("result = %+v", res)
			}
			push := sb.cmds[len(sb.cmds)-1]
			want := "-C /workspace/repo push --force-with-lease origin HEAD:refs/heads/" + branch
			if !strings.HasSuffix(push.Command, want) {
				t.Fatalf("push command = %q, want it to end with %q", push.Command, want)
			}
			if strings.Count(push.Command, " push ") != 1 {
				t.Fatalf("push command must be one git push: %q", push.Command)
			}
			if push.Env["GH_TOKEN"] != token || push.Env["GIT_TERMINAL_PROMPT"] != "0" {
				t.Fatalf("push env = %v", push.Env)
			}
		})
	}
}

func TestPushTokenIsOnlyInTheEnvOfThePushCommand(t *testing.T) {
	// Arrange
	c, sb := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	// Act
	if _, err := c.Push(context.Background(), "th1", sb); err != nil {
		t.Fatal(err)
	}
	// Assert
	if len(sb.cmds) != 2 {
		t.Fatalf("commands = %d, want inspect then push", len(sb.cmds))
	}
	inText, inEnv, inFiles := tokenSightings(sb)
	if len(inText) > 0 || len(inFiles) > 0 || len(inEnv) != 1 || inEnv[0] != "GH_TOKEN" {
		t.Fatalf("token in text %v, env %v, files %v", inText, inEnv, inFiles)
	}
	if _, has := sb.cmds[0].Env["GH_TOKEN"]; has {
		t.Error("the read-only inspection must not receive the token")
	}
	push := sb.cmds[1]
	for _, want := range []string{"-c credential.helper= -c 'credential.helper=!f()", "--force-with-lease"} {
		if !strings.Contains(push.Command, want) {
			t.Errorf("push command lacks %q: %s", want, push.Command)
		}
	}
	for _, e := range sb.cmds {
		if e.Env["GIT_CONFIG_GLOBAL"] != "/dev/null" || e.Env["GIT_CONFIG_NOSYSTEM"] != "1" {
			t.Errorf("user/system git config not isolated: %v", e.Env)
		}
	}
}

func TestPushRefusesBranchesItMustNeverPush(t *testing.T) {
	tests := []struct {
		name      string
		branch    string
		base      string
		protected []string
		want      string
	}{
		{"the base branch", "main", "main", nil, "base branch main"},
		{"protected by name", "release", "main", []string{"main", "release"}, "protected branch release"},
		{"protected by pattern", "release/1.2", "main", []string{"main", "release/*"}, "protected branch release/1.2"},
		{"main without a configured base", "main", "", []string{"main"}, "protected branch main"},
		{"empty branch", "", "main", nil, "no working branch"},
		{"parent traversal", "har/../main", "main", nil, "not a valid branch"},
		{"space", "har abc", "main", nil, "not a valid branch"},
		{"leading dash", "-f", "main", nil, "not a valid branch"},
		{"shell metacharacter", "x;id", "main", nil, "not a valid branch"},
		{"lock suffix", "har/a.lock", "main", nil, "not a valid branch"},
		{"doubled slash", "har//a", "main", nil, "not a valid branch"},
		{"unsafe base", "har/a", "ma in", nil, "base branch name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			c, sb := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: tc.base, Branch: tc.branch})
			// Act
			_, err := c.Push(context.Background(), "th1", sb, tc.protected...)
			// Assert
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if len(sb.cmds) != 0 {
				t.Fatalf("a command ran: %q", sb.cmds[0].Command)
			}
		})
	}
}

func TestPushRefusesAThreadWithoutAValidRepository(t *testing.T) {
	tests := []struct {
		name string
		ti   factory.ThreadInfo
		id   string
	}{
		{"unknown thread", factory.ThreadInfo{}, "ordinary"},
		{"no repository", factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Branch: "har/a"}, "th1"},
		{"malformed repository", factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: "acme/../evil", Branch: "har/a"}, "th1"},
		{"repository with a URL", factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: "evil.com/x/y", Branch: "har/a"}, "th1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			c, sb := pushEnv(t, tc.ti)
			// Act
			_, err := c.Push(context.Background(), tc.id, sb)
			// Assert
			if err == nil || len(sb.cmds) != 0 {
				t.Fatalf("err = %v, commands = %d", err, len(sb.cmds))
			}
		})
	}
}

func TestPushWithoutATokenDoesNotRunAnything(t *testing.T) {
	// Arrange
	c, sb := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	c.Auth = nil
	// Act
	_, err := c.Push(context.Background(), "th1", sb)
	// Assert
	if err == nil || !strings.Contains(err.Error(), "token") || len(sb.cmds) != 0 {
		t.Fatalf("err = %v, commands = %d", err, len(sb.cmds))
	}
}

func TestPushRefusesWhenThereIsNothingBeyondTheBase(t *testing.T) {
	// Arrange
	c, _ := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	sb := gitSandbox("dirty=3\nahead=0\n", "", 0)
	// Act
	_, err := c.Push(context.Background(), "th1", sb)
	// Assert
	if err == nil || !strings.Contains(err.Error(), "nothing to push: commit your changes first") {
		t.Fatalf("err = %v", err)
	}
	if len(sb.cmds) != 1 || strings.Contains(sb.cmds[0].Command, " push ") {
		t.Fatalf("commands = %d, the push must not run", len(sb.cmds))
	}
}

func TestPushCountsAgainstTheBaseRef(t *testing.T) {
	tests := []struct{ base, want string }{{"main", "origin/main"}, {"", "origin/HEAD"}}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			// Arrange
			c, sb := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: tc.base, Branch: "har/abc"})
			// Act
			if _, err := c.Push(context.Background(), "th1", sb); err != nil {
				t.Fatal(err)
			}
			// Assert
			if got := sb.cmds[0].Env["BASE_REF"]; got != tc.want || !strings.Contains(sb.cmds[0].Command, `rev-list --count "$BASE_REF..HEAD"`) {
				t.Fatalf("BASE_REF = %q, command = %q", got, sb.cmds[0].Command)
			}
		})
	}
}

func TestPushSurfacesUncommittedChanges(t *testing.T) {
	// Arrange
	c, _ := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	sb := gitSandbox("dirty=4\nahead=1\n", "", 0)
	// Act
	res, err := c.Push(context.Background(), "th1", sb)
	// Assert
	if err != nil || res.Dirty != 4 || res.Ahead != 1 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if !strings.Contains(sb.cmds[0].Command, "git status --porcelain") {
		t.Errorf("the inspection must count git status --porcelain lines: %s", sb.cmds[0].Command)
	}
}

func TestPushFailureIsRedactedAndQuotesGit(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{"rejected", " ! [rejected]        HEAD -> har/abc (stale info)\nerror: failed to push some refs\n", "stale info"},
		{"auth with the token echoed", "remote: Invalid username or password.\nfatal: Authentication failed for 'https://x-access-token:" + token + "@github.com/acme/app.git/' " + token + "\n", "Authentication failed"},
		{"no network", "fatal: unable to access 'https://github.com/acme/app.git/': Could not resolve host: github.com\n", "Could not resolve host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			c, _ := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
			sb := gitSandbox("dirty=0\nahead=1\n", tc.out, 1)
			// Act
			_, err := c.Push(context.Background(), "th1", sb)
			// Assert
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "git push failed") {
				t.Fatalf("err = %v, want git's message %q", err, tc.want)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("the error leaks the token: %v", err)
			}
		})
	}
}

func TestPushFailureMessageIsTailClipped(t *testing.T) {
	// Arrange
	c, _ := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	sb := gitSandbox("dirty=0\nahead=1\n", strings.Repeat("noise line\n", 500)+"the real reason", 1)
	// Act
	_, err := c.Push(context.Background(), "th1", sb)
	// Assert
	if err == nil || !strings.HasSuffix(err.Error(), "the real reason") || len(err.Error()) > 800 {
		t.Fatalf("err = %v (len %d)", err, len(err.Error()))
	}
}

func TestPushInspectionFailuresAreErrorsAndNeverPush(t *testing.T) {
	tests := []struct {
		name    string
		inspect string
		exit    int
		want    string
	}{
		{"checkout unreadable", "fatal: not a git repository", 128, "inspecting the checkout failed: fatal: not a git repository"},
		{"unparseable state", "garbage", 0, "could not read the state"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			c, _ := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
			sb := &recSandbox{out: tc.inspect, exit: tc.exit}
			// Act
			_, err := c.Push(context.Background(), "th1", sb)
			// Assert
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(sb.cmds) != 1 {
				t.Fatalf("commands = %d, only the inspection may run", len(sb.cmds))
			}
		})
	}
}

func TestPushRefusedByTheRepositoryGuardIsAnError(t *testing.T) {
	// Arrange: origin was repointed; the guard in the push command exits 3 before git runs.
	c, _ := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	sb := gitSandbox("dirty=0\nahead=1\n", "origin points at https://evil.example/x.git, not at https://github.com/acme/app.git\n", 3)
	// Act
	_, err := c.Push(context.Background(), "th1", sb)
	// Assert
	if err == nil || !strings.Contains(err.Error(), "origin points at https://evil.example") {
		t.Fatalf("err = %v", err)
	}
}

func TestPushGuardsTheCommandsThatCarryTheToken(t *testing.T) {
	// Arrange
	c, sb := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	c2, sb2 := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	// Act
	if _, err := c.Push(context.Background(), "th1", sb); err != nil {
		t.Fatal(err)
	}
	c2.Setup(context.Background(), "th1", sb2)
	// Assert: both check, in the command that uses the token, where origin points and that no config redirects.
	for name, cmd := range map[string]sandbox.ExecRequest{"push": sb.cmds[1], "setup": sb2.cmds[0]} {
		for _, want := range []string{`git remote get-url --push origin`, `"$EXPECT_URL"`, "url|credential", "exit 3"} {
			if !strings.Contains(cmd.Command, want) {
				t.Errorf("%s: guard lacks %q", name, want)
			}
		}
		if cmd.Env["EXPECT_URL"] != "https://github.com/acme/app.git" {
			t.Errorf("%s: EXPECT_URL = %q", name, cmd.Env["EXPECT_URL"])
		}
	}
}

type failingAuth struct{}

func (failingAuth) Token(context.Context, string) (string, error) {
	return "", errors.New("mint failed for " + token)
}

// execErrSandbox fails the n-th Exec (1-based) with an infrastructure error carrying the token.
type execErrSandbox struct {
	recSandbox
	failAt int
}

func (s *execErrSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	res, err := s.recSandbox.Exec(ctx, req)
	if len(s.cmds) == s.failAt {
		return res, errors.New("sandbox gone while using " + token)
	}
	return res, err
}

func TestPushWithAFailingTokenSourceRunsNothing(t *testing.T) {
	// Arrange
	c, sb := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	c.Auth = failingAuth{}
	// Act
	_, err := c.Push(context.Background(), "th1", sb)
	// Assert
	if err == nil || len(sb.cmds) != 0 || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v, commands = %d", err, len(sb.cmds))
	}
}

func TestPushSandboxErrorsAreRedacted(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(strconv.Itoa(failAt), func(t *testing.T) {
			// Arrange
			c, _ := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
			sb := &execErrSandbox{recSandbox: *gitSandbox("dirty=0\nahead=1\n", "", 0), failAt: failAt}
			// Act
			_, err := c.Push(context.Background(), "th1", sb)
			// Assert
			if err == nil || strings.Contains(err.Error(), token) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestPushFailureWithoutOutputStillSaysItFailed(t *testing.T) {
	// Arrange
	c, _ := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	sb := gitSandbox("dirty=0\nahead=1\n", "", 128)
	// Act
	_, err := c.Push(context.Background(), "th1", sb)
	// Assert
	if err == nil || err.Error() != "git push failed (exit 128)" {
		t.Fatalf("err = %v", err)
	}
}

func TestPushQuotesAnUnusualCheckoutDirectory(t *testing.T) {
	// Arrange
	c, sb := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	c.Dir = "/workspace/my repo's"
	// Act
	_, err := c.Push(context.Background(), "th1", sb)
	// Assert
	if err != nil || !strings.Contains(sb.cmds[1].Command, `-C '/workspace/my repo'\''s' push`) || sb.cmds[1].Env["REPO_DIR"] != c.Dir {
		t.Fatalf("err = %v, command = %q", err, sb.cmds[len(sb.cmds)-1].Command)
	}
}

func TestSetupSandboxErrorIsRedacted(t *testing.T) {
	// Arrange
	c, _ := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	sb := &execErrSandbox{failAt: 1}
	// Act
	notice := c.Setup(context.Background(), "th1", sb)
	// Assert
	if !strings.Contains(notice, "failed") || strings.Contains(notice, token) {
		t.Fatalf("notice = %q", notice)
	}
}

func TestSetupWithAFailingTokenSourceClonesAnonymously(t *testing.T) {
	// Arrange
	c, sb := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	c.Auth = failingAuth{}
	// Act
	notice := c.Setup(context.Background(), "th1", sb)
	// Assert
	if !strings.Contains(notice, "checked out") || len(sb.cmds) != 1 || sb.cmds[0].Env["GH_TOKEN"] != "" {
		t.Fatalf("notice = %q, commands = %+v", notice, sb.cmds)
	}
}

func TestCheckoutSectionUsesItsOwnDirectory(t *testing.T) {
	// Arrange
	c, _ := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
	c.Dir = "/workspace/other"
	// Act
	got := c.Section()(context.Background(), agent.Input{Run: &store.Run{ThreadID: "th1"}})
	// Assert
	if !strings.Contains(got, "/workspace/other") || strings.Contains(got, "git push") {
		t.Fatalf("section = %s", got)
	}
}

func TestPushIgnoresMalformedStateLinesButNeedsBoth(t *testing.T) {
	tests := []struct {
		name    string
		inspect string
		wantErr string
	}{
		{"non-numeric ahead", "dirty=0\nahead=many\n", "could not read the state"},
		{"negative dirty", "dirty=-1\nahead=1\n", "could not read the state"},
		{"noise around valid lines", "warning: something\ndirty=1\nstray\nahead=2\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			c, _ := pushEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
			sb := gitSandbox(tc.inspect, "", 0)
			// Act
			res, err := c.Push(context.Background(), "th1", sb)
			// Assert
			if tc.wantErr == "" {
				if err != nil || res.Dirty != 1 || res.Ahead != 2 {
					t.Fatalf("res = %+v, err = %v", res, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestSetupAndPushRefuseAMalformedRepository(t *testing.T) {
	// Arrange
	c, sb := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Repo: "acme/../evil", BaseBranch: "main", Branch: "har/abc"})
	// Act
	notice := c.Setup(context.Background(), "th1", sb)
	// Assert
	if !strings.Contains(notice, "not checked out") || len(sb.cmds) != 0 {
		t.Fatalf("notice = %q, commands = %d", notice, len(sb.cmds))
	}
}
