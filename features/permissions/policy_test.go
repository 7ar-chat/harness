package permissions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/approvals"
)

func TestParseModeAcceptsItsSpellings(t *testing.T) {
	tests := []struct {
		in   string
		want Mode
		bad  bool
	}{
		{"", ModeAsk, false}, {"  ", ModeAsk, false}, {"ASK", ModeAsk, false}, {" Yolo ", ModeYolo, false}, {"yolo-unsafe", ModeYoloUnsafe, false},
		{"yolo_unsafe", ModeYoloUnsafe, false}, {"auto-edit", ModeAutoEdit, false}, {"auto_edit", ModeAutoEdit, false}, {"autoedit", ModeAutoEdit, false},
		{"readonly", ModeReadOnly, false}, {"read-only", ModeReadOnly, false}, {"read_only", ModeReadOnly, false},
		{"reckless", "", true}, {"yolo!", "", true},
	}
	for _, tt := range tests {
		got, err := ParseMode(tt.in)
		if (err != nil) != tt.bad || got != tt.want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q (error %v)", tt.in, got, err, tt.want, tt.bad)
		}
		if tt.bad && !strings.Contains(err.Error(), tt.in) {
			t.Errorf("ParseMode(%q) error %q does not name the input", tt.in, err)
		}
	}
}

func TestParseFileReportsTheOffendingEntry(t *testing.T) {
	// Arrange
	bad := "deny:\n  - \"   \"\n"
	badMode := "mode: reckless\n"
	valid := "mode: auto-edit\nallow:\n  - make*\nask:\n  - make deploy*\ndeny:\n  - rm -rf*\n"

	// Act
	_, _, errEntry := ParseFile([]byte(bad), "perm.yml")
	_, _, errMode := ParseFile([]byte(badMode), "perm.yml")
	mode, rules, err := ParseFile([]byte(valid), "perm.yml")

	// Assert
	if errEntry == nil || !strings.Contains(errEntry.Error(), "perm.yml") || !strings.Contains(errEntry.Error(), "deny") {
		t.Errorf("empty entry: %v", errEntry)
	}
	if errMode == nil || !strings.Contains(errMode.Error(), "perm.yml") || !strings.Contains(errMode.Error(), "reckless") {
		t.Errorf("bad mode: %v", errMode)
	}
	if err != nil || mode != "auto-edit" || len(rules) != 3 {
		t.Fatalf("mode %q rules %v err %v", mode, rules, err)
	}
	if rules[0].Action != Deny || rules[1].Action != Ask || rules[2].Action != Allow {
		t.Errorf("rules are not ordered deny, ask, allow: %v", rules)
	}
	for _, r := range rules {
		if r.Source != "perm.yml" {
			t.Errorf("rule %v lost its source", r)
		}
	}
}

func TestLoadNamesTheFileThatCannotBeRead(t *testing.T) {
	// A permissions file that is a directory, or holds a bad mode or entry, fails the load and says which file.
	// Arrange
	dir := t.TempDir()
	asDir := filepath.Join(dir, "dir.yml")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	badMode := filepath.Join(dir, "mode.yml")
	if err := os.WriteFile(badMode, []byte("mode: reckless\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	badEntry := filepath.Join(dir, "entry.yml")
	if err := os.WriteFile(badEntry, []byte("allow:\n  - \"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Act / Assert
	for _, f := range []string{asDir, badMode, badEntry} {
		_, err := Load(Options{Home: dir, Files: []string{f}})
		if err == nil {
			t.Errorf("Load(%s) succeeded", filepath.Base(f))
			continue
		}
		if f != asDir && !strings.Contains(err.Error(), filepath.Base(f)) {
			t.Errorf("Load(%s) error %q does not name the file", filepath.Base(f), err)
		}
	}
}

func TestLoadSkipsMissingExplicitFilesAndHonoursThemWithoutTrust(t *testing.T) {
	// Files given by the caller are its own: allow rules and mode count even for an untrusted project, a missing one is skipped.
	// Arrange
	dir := t.TempDir()
	f := filepath.Join(dir, "mine.yml")
	if err := os.WriteFile(f, []byte("mode: yolo\nallow:\n  - make deploy*\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Act
	p, err := Load(Options{Workspace: dir, Home: dir, Files: []string{filepath.Join(dir, "missing.yml"), f}})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != ModeYolo || len(p.Rules) != 1 || len(p.Ignored) != 0 || len(p.Files) != 1 || p.Files[0] != f {
		t.Fatalf("mode %s rules %v ignored %v files %v", p.Mode, p.Rules, p.Ignored, p.Files)
	}
	if d, _ := p.Decide("execute_command", cmdArgs("make deploy-prod")); d.Action != Allow {
		t.Fatalf("explicit allow = %v", d)
	}
}

func TestLoadReadsTheUserDirectoryWhenNoHomeIsGiven(t *testing.T) {
	// Without Options.Home the user file comes from $HAR_HOME (config.UserDir) and the home directory is os.UserHomeDir.
	// Arrange
	har := t.TempDir()
	t.Setenv(config.HomeEnv, har)
	t.Setenv(config.NoUserDirEnv, "")
	if err := os.WriteFile(filepath.Join(har, config.PermissionsFile), []byte("mode: readonly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)

	// Act
	p, err := Load(Options{Workspace: t.TempDir()})

	// Assert
	if err != nil || p.Mode != ModeReadOnly {
		t.Fatalf("mode %v err %v", p.Mode, err)
	}
	if p.Home != userHome {
		t.Fatalf("home = %q, want %q", p.Home, userHome)
	}
	if d, _ := p.Decide("write_file", pathArgs("~/notes.txt")); d.Action != Deny {
		t.Fatalf("the default home is not used to expand ~: %v", d)
	}
}

func TestLoadIgnoresTheUserDirectoryWhenDisabled(t *testing.T) {
	// Arrange
	har := t.TempDir()
	t.Setenv(config.HomeEnv, har)
	t.Setenv(config.NoUserDirEnv, "1")
	if err := os.WriteFile(filepath.Join(har, config.PermissionsFile), []byte("mode: yolo\ndeny:\n  - ls\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Act
	p, err := Load(Options{Workspace: t.TempDir(), Home: t.TempDir()})

	// Assert
	if err != nil || p.Mode != ModeAsk || len(p.Files) != 0 {
		t.Fatalf("mode %v files %v err %v", p.Mode, p.Files, err)
	}
}

func TestLoadTakesTheModeOfTheEnvironmentBeforeFiles(t *testing.T) {
	// Arrange
	ws, home := writeProjectAndUserPermissions(t, "mode: yolo\n", "mode: auto-edit\n")

	// Act
	env, err := Load(Options{Workspace: ws, Home: home, Mode: "readonly", Trusted: true})
	user, err2 := Load(Options{Workspace: ws, Home: home})

	// Assert
	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if env.Mode != ModeReadOnly {
		t.Errorf("environment mode lost: %s", env.Mode)
	}
	if user.Mode != ModeAutoEdit || len(user.Ignored) != 1 {
		t.Errorf("untrusted project yolo must not loosen the user's auto-edit: mode %s ignored %v", user.Mode, user.Ignored)
	}
	if !strings.Contains(user.Ignored[0], `"auto-edit"`) && !strings.Contains(user.Ignored[0], "auto-edit") {
		t.Errorf("ignored message %q does not name the mode in force", user.Ignored[0])
	}
}

func TestParseRuleEdgeForms(t *testing.T) {
	tests := []struct {
		entry   string
		kind    string
		tool    string
		pattern string
	}{
		{"mcp__github__create*(*title*)", kindTool, "mcp__github__create*", "*title*"},
		{"fs.read(path:*)", kindTool, "fs.read", "path:*"},
		{"Write(src/**)", kindEdit, "", "src/**"},
		{"MultiEdit", kindEdit, "", "*"},
		{"execute_command", kindCommand, "", "*"},
		{"Bash", kindCommand, "", "*"},
		{"bash -c *", kindCommand, "", "bash -c *"},
		{"write_file *", kindCommand, "", "write_file *"},
		{"echo (hi)", kindCommand, "", "echo (hi)"},
		{"weird name(x)", kindCommand, "", "weird name(x)"},
		{"(x)", kindCommand, "", "(x)"},
	}
	for _, tt := range tests {
		r, err := ParseRule(Deny, tt.entry, "src")
		if err != nil || r.Kind != tt.kind || r.Tool != tt.tool || r.Pattern != tt.pattern || r.Action != Deny || r.Source != "src" {
			t.Errorf("ParseRule(%q) = %+v, %v; want kind %s tool %q pattern %q", tt.entry, r, err, tt.kind, tt.tool, tt.pattern)
		}
	}
}

func TestRuleAndDecisionStringsAreReadable(t *testing.T) {
	// The rule text is what the user sees in a decision's reason: it names the action, the tool and the pattern.
	// Arrange
	want := map[string]string{
		"git push*":             "deny: execute_command(git push*)",
		"Edit(**/.env)":         "deny: write_file(**/.env)",
		"mcp__x__send(*hello*)": "deny: mcp__x__send(*hello*)",
	}

	// Act / Assert
	for entry, text := range want {
		if got := mustRule(t, Deny, entry).String(); got != text {
			t.Errorf("rule %q renders as %q, want %q", entry, got, text)
		}
	}
	if d := (Decision{Action: Ask, Reason: "why"}).String(); d != "ask: why" {
		t.Errorf("decision renders as %q", d)
	}
}

func TestDecideReadsArgumentsStrictly(t *testing.T) {
	// A command or path that cannot be read from the arguments asks (never allows), whatever shape the JSON has.
	tests := []struct {
		name, tool, args string
	}{
		{"command missing", "execute_command", `{}`},
		{"command not a string", "execute_command", `{"command":["ls"]}`},
		{"arguments not JSON", "execute_command", `ls -la`},
		{"arguments empty", "execute_command", ``},
		{"path missing", "write_file", `{"content":"x"}`},
		{"path not a string", "edit_file", `{"path":3}`},
		{"arguments not an object", "write_file", `"a.go"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, judged := testPolicy(ModeAsk).Decide(tt.tool, tt.args)
			if !judged || d.Action != Ask || !strings.Contains(d.Reason, "could not be read") {
				t.Fatalf("%s %q = %v (judged %v)", tt.tool, tt.args, d, judged)
			}
		})
	}
	// Alternative spellings of the path key are read, and the first present key that is a string wins.
	for _, key := range []string{"path", "file_path", "filePath"} {
		d, _ := testPolicy(ModeAsk).Decide("write_file", `{"`+key+`":"/etc/hosts"}`)
		if d.Action != Ask || !strings.Contains(d.Reason, "/etc/hosts") {
			t.Errorf("key %s: %v", key, d)
		}
	}
	if d, _ := testPolicy(ModeAsk).Decide("write_file", `{"path":3,"file_path":".env"}`); d.Action != Deny {
		t.Errorf("a non-string path key must fall through to the next spelling: %v", d)
	}
	if d, _ := testPolicy(ModeAsk).Decide("Edit", `{"file_path":"a.go"}`); d.Action != Allow {
		t.Errorf("Work/Claude tool names are edits too: %v", d)
	}
	if d, _ := testPolicy(ModeAsk).Decide("Bash", cmdArgs("git push")); d.Action != Ask {
		t.Errorf("Bash is a command tool: %v", d)
	}
}

func TestMalformedCallsAreJudgedByTheModeToo(t *testing.T) {
	tests := []struct {
		mode Mode
		tool string
		want Action
	}{
		{ModeYolo, "execute_command", Allow},
		{ModeYoloUnsafe, "write_file", Allow},
		{ModeAutoEdit, "write_file", Allow},
		{ModeAutoEdit, "execute_command", Ask},
		{ModeReadOnly, "write_file", Deny},
		{ModeReadOnly, "execute_command", Deny},
	}
	for _, tt := range tests {
		if d, _ := testPolicy(tt.mode).Decide(tt.tool, `{}`); d.Action != tt.want {
			t.Errorf("%s %s with unreadable arguments = %v, want %v", tt.mode, tt.tool, d, tt.want)
		}
	}
}

func TestModeAdjustmentsKeepTheirReasonAndRule(t *testing.T) {
	// Yolo and auto-edit say what they let through; a readonly refusal names why.
	// Arrange
	rule := mustRule(t, Ask, "make deploy*")

	// Act
	yolo, _ := testPolicy(ModeYolo, rule).Decide("execute_command", cmdArgs("make deploy"))
	auto, _ := testPolicy(ModeAutoEdit).Decide("write_file", pathArgs("/etc/hosts"))
	roCmd, _ := testPolicy(ModeReadOnly).Decide("execute_command", cmdArgs("touch f"))
	roEdit, _ := testPolicy(ModeReadOnly).Decide("write_file", pathArgs("a.go"))
	unsafe, _ := testPolicy(ModeYoloUnsafe).Decide("execute_command", cmdArgs("rm -rf /"))

	// Assert
	if yolo.Action != Allow || !strings.HasPrefix(yolo.Reason, "yolo mode: ") || yolo.Rule != rule.String() {
		t.Errorf("yolo: %+v", yolo)
	}
	if auto.Action != Allow || !strings.HasPrefix(auto.Reason, "auto-edit mode: ") || !strings.Contains(auto.Reason, "/etc/hosts") {
		t.Errorf("auto-edit: %+v", auto)
	}
	if roCmd.Action != Deny || !strings.HasPrefix(roCmd.Reason, "readonly mode: ") {
		t.Errorf("readonly command: %+v", roCmd)
	}
	if roEdit.Action != Deny || roEdit.Reason != "readonly mode: files cannot be written" {
		t.Errorf("readonly edit: %+v", roEdit)
	}
	if unsafe.Action != Allow || !strings.Contains(unsafe.Reason, "no permission checks") {
		t.Errorf("yolo-unsafe: %+v", unsafe)
	}
}

func TestOtherToolRulesAreMatchedByNameCaseInsensitivelyAndByArguments(t *testing.T) {
	// Arrange
	p := testPolicy(ModeAsk,
		mustRule(t, Allow, "slack__post*(*)"),
		mustRule(t, Deny, "Slack__post_message(*#general*)"),
		mustRule(t, Ask, "slack__post*(*#ops*)"),
	)

	// Act
	denied, j1 := p.Decide("slack__post_message", `{"channel":"#general"}`)
	asked, j2 := p.Decide("SLACK__POST_MESSAGE", `{"channel":"#ops"}`)
	allowed, j3 := p.Decide("slack__post_message", `{"channel":"#random"}`)
	_, j4 := p.Decide("slack__read_channel", `{"channel":"#random"}`)

	// Assert
	if !j1 || denied.Action != Deny || !strings.Contains(denied.Reason, "rule ") || denied.Rule == "" {
		t.Errorf("deny: %+v judged %v", denied, j1)
	}
	if !j2 || asked.Action != Ask {
		t.Errorf("ask: %+v judged %v", asked, j2)
	}
	if !j3 || allowed.Action != Allow {
		t.Errorf("allow: %+v judged %v", allowed, j3)
	}
	if j4 {
		t.Error("a tool no rule names must stay unjudged")
	}
}

func TestOtherToolsAreAdjustedByMode(t *testing.T) {
	// A rule-judged MCP tool: yolo turns an ask into allow, readonly leaves tools alone, deny survives yolo but not yolo-unsafe.
	// Arrange
	askRule, denyRule := mustRule(t, Ask, "svc__write(*)"), mustRule(t, Deny, "svc__drop(*)")

	// Act
	yolo, _ := testPolicy(ModeYolo, askRule, denyRule).Decide("svc__write", `{}`)
	yoloDeny, _ := testPolicy(ModeYolo, askRule, denyRule).Decide("svc__drop", `{}`)
	unsafeDeny, _ := testPolicy(ModeYoloUnsafe, askRule, denyRule).Decide("svc__drop", `{}`)
	readonly, _ := testPolicy(ModeReadOnly, askRule, denyRule).Decide("svc__write", `{}`)
	autoEdit, _ := testPolicy(ModeAutoEdit, askRule, denyRule).Decide("svc__write", `{}`)

	// Assert
	if yolo.Action != Allow || yoloDeny.Action != Deny || unsafeDeny.Action != Allow || readonly.Action != Ask || autoEdit.Action != Ask {
		t.Fatalf("yolo %v yolo deny %v unsafe deny %v readonly %v auto-edit %v", yolo, yoloDeny, unsafeDeny, readonly, autoEdit)
	}
}

func TestAskToolsNamedByOtherRulesAreStillAskedOncePerRun(t *testing.T) {
	// Arrange
	p := testPolicy(ModeAsk)
	p.AskTools = []string{"fs__*"}

	// Act
	d, judged := p.Decide("FS__read", `{}`)
	other, otherJudged := p.Decide("git__log", `{}`)

	// Assert
	if !judged || d.Action != Ask || !d.Once || !strings.Contains(d.Reason, "FS__read") {
		t.Errorf("ask tool: %+v judged %v", d, judged)
	}
	if otherJudged || other.Action != Allow {
		t.Errorf("a tool outside AskTools is not judged: %+v judged %v", other, otherJudged)
	}
}

func TestPathDecisionsFollowRuleAndProtectionPrecedence(t *testing.T) {
	tests := []struct {
		name  string
		rules []string
		path  string
		want  Action
	}{
		{"ask rule inside the workspace", []string{"ask:write_file(docs/**)"}, "docs/a.md", Ask},
		{"ask rule outranks allow", []string{"allow:write_file(docs/**)", "ask:write_file(docs/**)"}, "docs/a.md", Ask},
		{"allow rule on a plain pattern matches the base name", []string{"deny:write_file(*.lock)"}, "a/b/c.lock", Deny},
		{"deny pattern with a slash matches the workspace-relative path", []string{"deny:write_file(a/b/*)"}, "/workspace/a/b/c", Deny},
		{"deny matches the host path of the workspace", []string{"deny:write_file(/workspace/vendor/**)"}, "/home/u/proj/vendor/x", Deny},
		{"allow outside via the host spelling", []string{"allow:write_file(/etc/app/**)"}, "/etc/app/c", Allow},
		{"ask beats protection when allowed", []string{"allow:write_file(.env)", "ask:write_file(.env)"}, ".env", Ask},
		{"allowed secret", []string{"allow:write_file(.env)"}, ".env", Allow},
		{"rule with glob path judged by its fixed prefix", nil, "src/*.go", Allow},
		{"glob escaping the workspace", nil, "../*.go", Ask},
		{"glob at the root of nothing", nil, "*.go", Allow},
		{"brace glob in /etc", nil, "/etc/{a,b}", Ask},
		{"home expansion with braces", nil, "${HOME}/x", Ask},
		{"HOME alone", nil, "$HOME", Ask},
		{"tilde alone", nil, "~", Ask},
		{"NUL byte", nil, "a\x00b", Ask},
		{"blank path", nil, "  ", Ask},
		{"tmp", nil, "/tmp/x", Allow},
		{"macOS tmp", nil, "/private/tmp/x", Allow},
		{"macOS var folders", nil, "/var/folders/ab/cd/T/x", Allow},
		{"tmp prefix is not tmp", nil, "/tmpfoo/x", Ask},
		{"workspace prefix is not the workspace", nil, "/workspaceX/a", Ask},
		{"host workspace prefix is not the workspace", nil, "/home/u/project/a", Ask},
		{"inside .ssh", nil, "/home/u/.ssh/config", Deny},
		{"inside .aws", nil, "sub/.aws/credentials", Deny},
		{"inside .gnupg", nil, "/home/u/.gnupg/x", Deny},
		{"secret in a nested .git", nil, "a/.git/b", Deny},
		{"key file", nil, "tls/server.key", Deny},
		{"pub key allowed", nil, "keys/id_ed25519.pub", Allow},
		{"npmrc", nil, ".npmrc", Deny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rules []Rule
			for _, r := range tt.rules {
				kind, entry, _ := strings.Cut(r, ":")
				rules = append(rules, mustRule(t, map[string]Action{"allow": Allow, "ask": Ask, "deny": Deny}[kind], entry))
			}
			if d := testPolicy(ModeAsk, rules...).pathDecision(tt.path); d.Action != tt.want {
				t.Fatalf("%q with %v = %v, want %v", tt.path, tt.rules, d, tt.want)
			}
		})
	}
}

func TestHomeExpansionNeedsAKnownHome(t *testing.T) {
	// Without a home directory, ~ and $HOME cannot be resolved, so the path is unknown (asked), never guessed.
	// Arrange
	p := &Policy{Mode: ModeAsk, Workspace: "/home/u/proj"}

	// Act / Assert
	for _, raw := range []string{"~", "~/x", "$HOME", "$HOME/x", "${HOME}", "${HOME}/x"} {
		d := p.pathDecision(raw)
		if d.Action != Ask || !strings.Contains(d.Reason, "depends on a variable") {
			t.Errorf("%q = %v", raw, d)
		}
	}
}

func TestRecursiveDeletesOfTheHomeAreRefusedWhateverTheSpelling(t *testing.T) {
	for _, cmd := range []string{"rm -rf ~", "rm -rf $HOME", "rm -rf ${HOME}", "rm -rf /home/u", "rm -rf /home/u/", "rm -rf /home/u/*", "rm -rf ~/*", "rm -rf /home/u/."} {
		if d := decideCommandAs(ModeAsk, cmd); d.Action != Deny {
			t.Errorf("%q = %v", cmd, d)
		}
	}
	if d := (&Policy{Mode: ModeAsk}).classify("~/x"); d.class != pathUnknown {
		t.Errorf("no home: class %v", d.class)
	}
}

func TestHookLetsTheGateJudgeMCPApprovalToolsAndHandlesMissingInnerGate(t *testing.T) {
	// The approval tool of a project's MCP server is never judged by rules or modes: the interactive gate answers, and without one it is refused.
	// Arrange
	const tool = approvals.TrustMCPToolPrefix + "files"
	ctx := context.Background()
	interactive := &store.Run{InteractionMode: store.InteractionInteractive}
	automatic := &store.Run{InteractionMode: store.InteractionAutomatic}
	yolo, _ := newHook(ModeYoloUnsafe)
	noInner := &Hook{Policy: testPolicy(ModeYolo)}

	// Act
	asks := yolo.NeedsApproval(ctx, interactive, tool, `{}`)
	askAutomatic := yolo.NeedsApproval(ctx, automatic, tool, `{}`)
	risky := yolo.Risky(ctx, automatic, tool, `{}`)
	denyAutomatic := yolo.Denial(ctx, automatic, tool, `{}`)
	denyInteractive := yolo.Denial(ctx, interactive, tool, `{}`)

	// Assert
	if !asks || askAutomatic || !risky {
		t.Fatalf("asks %v asks in automatic %v risky %v", asks, askAutomatic, risky)
	}
	if denyAutomatic != approvals.TrustMCPDeniedResult || denyInteractive != "" {
		t.Fatalf("denial automatic %q interactive %q", denyAutomatic, denyInteractive)
	}
	if noInner.NeedsApproval(ctx, interactive, tool, `{}`) || noInner.Risky(ctx, interactive, tool, `{}`) {
		t.Fatal("without a gate nothing can approve the server")
	}
	if noInner.Denial(ctx, interactive, tool, `{}`) != approvals.TrustMCPDeniedResult {
		t.Fatal("without a gate the MCP server approval must be refused")
	}
}

func TestHookWithoutAnInnerGateDoesNotAskForUnjudgedTools(t *testing.T) {
	// Arrange
	h := &Hook{Policy: testPolicy(ModeAsk)}
	run := &store.Run{ID: "r", InteractionMode: store.InteractionInteractive}

	// Act / Assert
	if h.NeedsApproval(context.Background(), run, "delete_thing", `{}`) || h.Risky(context.Background(), run, "delete_thing", `{}`) {
		t.Fatal("an unjudged tool without a gate was asked")
	}
	if got := h.Denial(context.Background(), run, "delete_thing", `{}`); got != "" {
		t.Fatalf("denial = %q", got)
	}
}

func TestHookRisksUnjudgedToolsLikeTheGate(t *testing.T) {
	// Arrange
	h, _ := newHook(ModeAsk)
	run := &store.Run{ID: "r", InteractionMode: store.InteractionAutomatic}

	// Act
	deleteTool := h.Risky(context.Background(), run, "delete_thing", `{"path":"/x"}`)
	readTool := h.Risky(context.Background(), run, "read_file", `{"path":"/x"}`)

	// Assert
	if !deleteTool || readTool {
		t.Fatalf("delete %v read %v", deleteTool, readTool)
	}
}

func TestHookAsksOnceForToolsMarkedOnce(t *testing.T) {
	// A stdio MCP server's tool asks until one call of that tool kind was approved for the run.
	// Arrange
	h, _ := newHook(ModeAsk)
	h.Policy.AskTools = []string{"fs__*"}

	// Act
	first := h.NeedsApproval(context.Background(), &store.Run{ID: "r"}, "fs__read", `{}`)
	later := h.NeedsApproval(context.Background(), &store.Run{ID: "r", ApprovedTools: []string{approvals.Kind("fs__read")}}, "fs__read", `{}`)
	other := h.NeedsApproval(context.Background(), &store.Run{ID: "r", ApprovedTools: []string{approvals.Kind("fs__read")}}, "fs__write", `{}`)

	// Assert
	if !first || later || !other {
		t.Fatalf("first %v later %v other tool %v", first, later, other)
	}
}

func TestGlobPathMultiStar(t *testing.T) {
	for _, tt := range []struct {
		pat, text string
		want      bool
	}{
		{"a/**/b", "a/b", true}, {"a/**/b", "a/x/y/b", true}, {"a/**", "a/", true}, {"a/**", "a", false}, {"**", "x/y", true}, {"a**b", "a/x/b", true},
		{"?.go", "a.go", true}, {"?.go", "ab.go", false}, {"?.go", "/.go", false}, {"a.b+c", "a.b+c", true}, {"[x]", "[x]", true},
		{"src/*/x", "src/a/x", true}, {"src/*/x", "src/a/b/x", false},
	} {
		if got := globPath(tt.pat, tt.text); got != tt.want {
			t.Errorf("globPath(%q, %q) = %v, want %v", tt.pat, tt.text, got, tt.want)
		}
	}
	for _, tt := range []struct {
		pat, text string
		want      bool
	}{
		{"make*", "make\nall", true}, {"a.c", "abc", false}, {"a|b", "a | b", true}, {"git (a)", "git (a)", true}, {"x?", "xy", true},
	} {
		if got := globCommand(tt.pat, tt.text); got != tt.want {
			t.Errorf("globCommand(%q, %q) = %v, want %v", tt.pat, tt.text, got, tt.want)
		}
	}
}

func TestForkBombsAreRecognisedAcrossSpacing(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{":(){ :|:& };:", true}, {"f(){ f | f & }; f", true}, {"f() { f|f& }; f", true}, {"f(){ f|g& }; f", false}, {"f(){ echo; }; f", false},
		{"ls", false}, {"", false},
	}
	for _, tt := range tests {
		if got := isForkBomb(tt.line); got != tt.want {
			t.Errorf("isForkBomb(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}
