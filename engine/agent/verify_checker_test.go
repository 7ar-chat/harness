package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
)

func contract(ok bool, file string, checks ...[3]any) string {
	var cs []string
	for _, c := range checks {
		cs = append(cs, fmt.Sprintf(`{"name":%q,"ok":%v,"detail":%q}`, c[0], c[1], c[2]))
	}
	return fmt.Sprintf(`{"ok":%v,"file":%q,"checks":[%s],"next_step":"fix the title"}`, ok, file, strings.Join(cs, ","))
}

func TestParseCheckerOutputReadsTheLastContractObjectOnStdout(t *testing.T) {
	// Arrange
	stdout := "warming up {not json}\n" + contract(false, "/workspace/outputs/./r.docx", [3]any{"fonts", true, ""}, [3]any{"title", false, "missing   title\n slide"}) + "\n"

	// Act
	r, ok := ParseCheckerOutput("inspect_docx", stdout, 2)

	// Assert
	if !ok {
		t.Fatal("contract not found")
	}
	want := CheckResult{Path: "/workspace/outputs/r.docx", Checker: "inspect_docx", OK: false, Summary: "inspect_docx: NOT OK — title: missing title slide", NextStep: "fix the title"}
	if r != want {
		t.Fatalf("got %+v\nwant %+v", r, want)
	}
}

func TestParseCheckerOutputReportsAPassingCheckWithItsCount(t *testing.T) {
	// Act
	r, ok := ParseCheckerOutput("c", contract(true, "/workspace/outputs/a.xlsx", [3]any{"a", true, ""}, [3]any{"b", true, ""}), 0)

	// Assert
	if !ok || !r.OK || r.Failed || r.Summary != "c: ok (2 checks passed)" {
		t.Fatalf("%+v %v", r, ok)
	}
}

func TestParseCheckerOutputExitCodeOverridesTheVerdict(t *testing.T) {
	passing := contract(true, "/workspace/outputs/a.xlsx", [3]any{"a", true, ""})
	for name, tc := range map[string]struct {
		exit       int
		ok, failed bool
		summary    string
	}{
		"exit 0 trusts the output":        {0, true, false, "c: ok (1 checks passed)"},
		"exit 2 means not ok":             {2, false, false, "c: ok (1 checks passed)"},
		"exit 1 means it could not run":   {1, false, true, "c could not run (exit 1)"},
		"exit 127 means it could not run": {127, false, true, "c could not run (exit 127)"},
	} {
		t.Run(name, func(t *testing.T) {
			// Act
			r, ok := ParseCheckerOutput("c", passing, tc.exit)

			// Assert
			if !ok || r.OK != tc.ok || r.Failed != tc.failed || r.Summary != tc.summary {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestParseCheckerOutputIgnoresOutputsThatAreNotTheContract(t *testing.T) {
	for name, stdout := range map[string]string{
		"empty":           "",
		"plain text":      "all good",
		"no ok field":     `{"checks":[],"file":"/a"}`,
		"no checks field": `{"ok":true,"file":"/a"}`,
		"unterminated":    `{"ok":true,"checks":[`,
	} {
		t.Run(name, func(t *testing.T) {
			if r, ok := ParseCheckerOutput("c", stdout, 0); ok {
				t.Fatalf("found %+v", r)
			}
		})
	}
}

func TestParseCheckerOutputFindsTheContractAfterNestedBraces(t *testing.T) {
	// The last "{" belongs to a nested object of the contract: the search backs up to the outer object.
	stdout := `log line {"x":1}` + "\n" + `{"ok":true,"file":"/workspace/outputs/a.md","checks":[{"name":"n","ok":true,"detail":""}],"meta":{"k":1}}`

	// Act
	r, ok := ParseCheckerOutput("c", stdout, 0)

	// Assert
	if !ok || r.Path != "/workspace/outputs/a.md" {
		t.Fatalf("%+v %v", r, ok)
	}
}

func execResult(stdout string, exit int) string {
	return mustJSONv(map[string]any{"stdout": stdout, "exitCode": exit})
}

func mustJSONv(v any) string {
	s, _ := marshalNoEscape(v)
	return s
}

func execCall(command string) ai.ToolCall {
	return call("execute_command", mustJSONv(map[string]any{"command": command}))
}

func TestModelRanCheckerCountsOnlyAStrictCheckerScriptRun(t *testing.T) {
	// Arrange
	file := "/workspace/outputs/r.docx"
	good := contract(true, file, [3]any{"a", true, ""})
	for name, tc := range map[string]struct {
		call    ai.ToolCall
		content string
		want    bool
	}{
		"python script":           {execCall("python3 /skills/docx/inspect_docx.py " + file), execResult(good, 0), true},
		"shell script":            {execCall("bash /skills/docx/check.sh " + file), execResult(good, 0), true},
		"quoted script":           {execCall(`python3 "/skills/docx/inspect_docx.py" '` + file + `'`), execResult(good, 0), true},
		"cat of an old report":    {execCall("cat /workspace/report.json"), execResult(good, 0), false},
		"another tool":            {call("read_file", `{}`), execResult(good, 0), false},
		"no exit code":            {execCall("python3 x.py"), mustJSONv(map[string]any{"stdout": good}), false},
		"result is not json":      {execCall("python3 x.py"), "Error: boom", false},
		"stdout without contract": {execCall("python3 x.py"), execResult("hello", 0), false},
		"empty checks":            {execCall("python3 x.py"), execResult(`{"ok":true,"file":"`+file+`","checks":[]}`, 0), false},
		"file not a deliverable":  {execCall("python3 x.py"), execResult(contract(true, "/workspace/data/in.docx", [3]any{"a", true, ""}), 0), false},
		"bad arguments":           {call("execute_command", "not json"), execResult(good, 0), false},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			v := &verifyState{}
			v.file(file).changed = 1

			// Act
			r, ok := v.modelRanChecker(tc.call, tc.content)

			// Assert
			if ok != tc.want {
				t.Fatalf("ok = %v (%+v)", ok, r)
			}
			if ok && (r.Path != file || r.Checker == "" || !r.OK) {
				t.Fatalf("result %+v", r)
			}
		})
	}
}

func TestModelRanCheckerNamesTheCheckerAfterTheScript(t *testing.T) {
	// Arrange
	file := "/workspace/outputs/r.docx"
	v := &verifyState{}
	v.file(file).changed = 1

	// Act
	r, ok := v.modelRanChecker(execCall("cd /workspace && python3 /skills/docx/inspect_docx.py "+file), execResult(contract(true, file, [3]any{"a", true, ""}), 0))

	// Assert
	if !ok || r.Checker != "inspect_docx" {
		t.Fatalf("%+v %v", r, ok)
	}
}

func TestTrackedResolvesAReportedFileAgainstTheTurnsDeliverables(t *testing.T) {
	// Arrange
	v := &verifyState{}
	v.file("/workspace/outputs/a/report.docx").changed = 1
	v.file("/workspace/outputs/b/report.docx").changed = 1
	v.file("/workspace/outputs/deck.pptx").changed = 1
	v.file("/workspace/outputs/unchanged.md") // seen but not changed in this turn

	for file, want := range map[string]string{
		"/workspace/outputs/deck.pptx":       "/workspace/outputs/deck.pptx",
		"/workspace/outputs/../outputs/x.md": "/workspace/outputs/x.md",
		"/workspace/data/in.docx":            "",
		"outputs/deck.pptx":                  "/workspace/outputs/deck.pptx",
		"deck.pptx":                          "/workspace/outputs/deck.pptx",
		"a/report.docx":                      "/workspace/outputs/a/report.docx",
		"report.docx":                        "", // ambiguous: two files end with it
		"unchanged.md":                       "",
		"missing.md":                         "",
		"  ":                                 "",
		"":                                   "",
	} {
		if got := v.tracked(file); got != want {
			t.Errorf("tracked(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestCheckedLastRequiresTheFilesOwnCheckerCommandToBeTheLastStep(t *testing.T) {
	// Arrange
	file := "/workspace/outputs/r.docx"
	cmd := "python3 /skills/docx/inspect_docx.py /workspace/outputs/r.docx"
	st := newState(5)
	st.a.Harness.Checker = &fakeChecker{commands: map[string]string{file: cmd}}
	run := func(args any) ai.ToolCall { return call("execute_command", mustJSONv(args)) }
	for name, tc := range map[string]struct {
		tc   ai.ToolCall
		want bool
	}{
		"exactly the command":    {run(map[string]any{"command": cmd}), true},
		"after a build step":     {run(map[string]any{"command": "python3 build.py && " + cmd}), true},
		"after a semicolon":      {run(map[string]any{"command": "make; " + cmd}), true},
		"words in another order": {run(map[string]any{"command": "python3 /workspace/outputs/r.docx /skills/docx/inspect_docx.py"}), true},
		"quoted arguments":       {run(map[string]any{"command": `python3 "/skills/docx/inspect_docx.py" '/workspace/outputs/r.docx'`}), true},
		"checker is not last":    {run(map[string]any{"command": cmd + " && python3 rewrite.py"}), false},
		"piped":                  {run(map[string]any{"command": cmd + " | head"}), false},
		"redirected":             {run(map[string]any{"command": cmd + " > /dev/null"}), false},
		"background flag":        {run(map[string]any{"command": cmd, "background": true}), false},
		"function redefinition":  {run(map[string]any{"command": "python3() { true; }; " + cmd}), false},
		"PATH changed":           {run(map[string]any{"command": "PATH=/tmp:$PATH; " + cmd}), false},
		"script named twice":     {run(map[string]any{"command": "cp /tmp/fake.py /skills/docx/inspect_docx.py && " + cmd}), false},
		"interpreter by path":    {run(map[string]any{"command": "/tmp/python3 /skills/docx/inspect_docx.py /workspace/outputs/r.docx"}), false},
		"another file":           {run(map[string]any{"command": "python3 /skills/docx/inspect_docx.py /workspace/outputs/other.docx"}), false},
		"another tool":           {call("write_file", `{}`), false},
		"not json":               {call("execute_command", "x"), false},
		"empty command":          {run(map[string]any{"command": ""}), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := st.checkedLast(tc.tc, file); got != tc.want {
				t.Fatalf("checkedLast = %v", got)
			}
		})
	}
}

func TestCheckedLastIsFalseWhenTheCheckerHasNoCommand(t *testing.T) {
	st := newState(5)
	st.a.Harness.Checker = &fakeChecker{}
	if st.checkedLast(execCall("python3 x.py /workspace/outputs/r.docx"), "/workspace/outputs/r.docx") {
		t.Fatal("no command to compare with, yet the check counted")
	}
	st.a.Harness.Checker = nil
	if st.checkedLast(execCall("python3 x.py"), "/workspace/outputs/r.docx") {
		t.Fatal("no checker, yet the check counted")
	}
}

func TestShellWordsSplitsOnBlanksAndRemovesQuotes(t *testing.T) {
	for in, want := range map[string]string{
		`a b   c`:                    "[a b c]",
		"a\tb":                       "[a b]",
		`python3 "my file.py" 'x y'`: "[python3 my file.py x y]",
		`a "" b`:                     "[a  b]",
		`say "it's"`:                 "[say it's]",
		``:                           "[]",
	} {
		if got := fmt.Sprint(shellWords(in)); got != want {
			t.Errorf("shellWords(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSameWordsComparesAsMultisetsOfAtLeastTwoWords(t *testing.T) {
	if !sameWords([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("order should not matter")
	}
	for name, pair := range map[string][2][]string{
		"different":        {{"a", "b"}, {"a", "c"}},
		"length":           {{"a", "b"}, {"a", "b", "c"}},
		"empty":            {{}, {}},
		"single word":      {{"a"}, {"a"}},
		"empty first word": {{"", "x"}, {"", "x"}},
		"duplicates":       {{"a", "a", "b"}, {"a", "b", "b"}},
	} {
		if sameWords(pair[0], pair[1]) {
			t.Errorf("%s: words matched", name)
		}
	}
}

func TestUnsafeShellFlagsWhatCanRedirectALaterStep(t *testing.T) {
	for cmd, want := range map[string]bool{
		"python3 check.py f":                 false,
		"echo path; python3 check.py":        false,
		"export FOO=bar && python3 check.py": false,
		"alias python3=true":                 true,
		"eval \"$X\"":                        true,
		"source ./env.sh":                    true,
		". ./env.sh":                         true,
		"foo() { true; }":                    true,
		"PATH=/tmp:$PATH python3 x.py":       true,
		"PYTHONPATH=/tmp python3 x.py":       true,
		"export BASH_ENV=/tmp/x":             true,
		"echo import os > sitecustomize.py":  true,
		"trap 'x' EXIT":                      true,
		"cat /etc/passwd":                    false,
		"python3 my-eval-tool.py":            false,
	} {
		if got := unsafeShell(cmd); got != want {
			t.Errorf("unsafeShell(%q) = %v, want %v", cmd, got, want)
		}
	}
}
