package executors

import (
	"errors"
	"strings"
	"testing"
)

func TestRenderBuiltins(t *testing.T) {
	cases := []struct {
		name  string
		level Level
		want  []string
		not   []string
	}{
		{ClaudeCode, LevelEdit, []string{`claude -p "$HARNESS_TASK" --output-format stream-json --verbose`, "--permission-mode acceptEdits"}, []string{"{task}", "{mode}"}},
		{ClaudeCode, LevelReadOnly, []string{"--permission-mode plan"}, nil},
		{ClaudeCode, LevelFull, []string{"--dangerously-skip-permissions"}, nil},
		{Codex, LevelEdit, []string{"codex exec --json", "--sandbox workspace-write", `"$HARNESS_TASK"`}, nil},
		{Codex, LevelReadOnly, []string{"--sandbox read-only"}, nil},
		{OpenCode, LevelEdit, []string{`opencode run "$HARNESS_TASK"`}, []string{"{mode}"}},
		{OMP, LevelEdit, []string{`omp -p`, "--approval-mode yolo", `"$HARNESS_TASK"`, "</dev/null"}, []string{"{mode}", "{task}"}},
		{OMP, LevelFull, []string{"--approval-mode yolo"}, nil},
		{OMP, LevelReadOnly, []string{"--tools read,grep,glob", "--approval-mode always-ask"}, []string{"yolo"}},
	}
	for _, c := range cases {
		ex, ok := BuiltinNamed(c.name)
		if !ok {
			t.Fatal(c.name)
		}
		got, err := ex.Render(c.level)
		if err != nil {
			t.Fatalf("%s %s: %v", c.name, c.level, err)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s %s: %q lacks %q", c.name, c.level, got, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%s %s: %q has %q", c.name, c.level, got, n)
			}
		}
	}
	oc, _ := BuiltinNamed(OpenCode)
	if _, err := oc.Render(LevelReadOnly); !errors.Is(err, ErrReadOnlyUnsupported) {
		t.Fatalf("opencode read-only: %v", err)
	}
}

func TestTaskIsNeverInTheCommand(t *testing.T) {
	for _, ex := range Builtins() {
		c, err := ex.Render(LevelEdit)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(c, "{task}") || !strings.Contains(c, "$HARNESS_TASK") {
			t.Errorf("%s: %q", ex.Name, c)
		}
	}
}

func TestLevelFor(t *testing.T) {
	for _, c := range []struct {
		policy string
		ro     bool
		want   Level
	}{{"", false, LevelEdit}, {"supervised", false, LevelEdit}, {"sandboxed", false, LevelEdit}, {"autonomous", false, LevelFull},
		{"autonomous", true, LevelReadOnly}, {"sandboxed", true, LevelReadOnly}} {
		if got := LevelFor(c.policy, c.ro); got != c.want {
			t.Errorf("%v: %s", c, got)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := Executor{Name: "my-cli", ProjectID: "p", Command: "mycli {task}", Binary: "mycli", CredentialEnv: []string{"MY_KEY", "MY_KEY", " "}}
	if err := ok.Validate(); err != nil || ok.Kind != KindCustom || len(ok.CredentialEnv) != 1 {
		t.Fatalf("%v %+v", err, ok)
	}
	bad := map[string]func(*Executor){
		"name":      func(e *Executor) { e.Name = "Bad Name" },
		"builtin":   func(e *Executor) { e.Name = "codex" },
		"project":   func(e *Executor) { e.ProjectID = "" },
		"no task":   func(e *Executor) { e.Command = "mycli" },
		"binary":    func(e *Executor) { e.Binary = "my cli; rm" },
		"parser":    func(e *Executor) { e.Parser = "xml" },
		"env path":  func(e *Executor) { e.CredentialEnv = []string{"PATH"} },
		"env ld":    func(e *Executor) { e.CredentialEnv = []string{"LD_PRELOAD"} },
		"env harn":  func(e *Executor) { e.CredentialEnv = []string{"HARNESS_TASK"} },
		"env shape": func(e *Executor) { e.CredentialEnv = []string{"lower"} },
		"level":     func(e *Executor) { e.ModeFlags = map[Level]string{"root": "x"} },
		"nul":       func(e *Executor) { e.Command = "mycli {task}\x00" },
	}
	for name, mut := range bad {
		e := Executor{Name: "my-cli", ProjectID: "p", Command: "mycli {task}", Binary: "mycli"}
		mut(&e)
		if err := e.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

// Recorded (trimmed) outputs of the CLIs.
const claudeOut = `{"type":"system","subtype":"init","cwd":"/workspace/repo","tools":["Bash","Edit"]}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Reading the code."},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/workspace/repo/main.go"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"package main"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"result","subtype":"success","is_error":false,"duration_ms":12000,"num_turns":3,"result":"All done: added the flag.","total_cost_usd":0.1234,"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":1000,"cache_creation_input_tokens":10}}
`

func feed(p Parser, out string) (shown []string) {
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if s := p.Line(l); s != "" {
			shown = append(shown, s)
		}
	}
	return shown
}

func TestClaudeParser(t *testing.T) {
	p := NewParser(ParserClaudeJSON)
	shown := strings.Join(feed(p, claudeOut), "\n")
	for _, w := range []string{"Reading the code.", "[tool Read] /workspace/repo/main.go", "[tool Bash] go test ./...", "[done] success"} {
		if !strings.Contains(shown, w) {
			t.Errorf("progress lacks %q: %q", w, shown)
		}
	}
	s := p.Summary()
	if s.Text != "All done: added the flag." || s.USD != 0.1234 || s.Tokens != 1160 || s.Failed {
		t.Fatalf("%+v", s)
	}
}

func TestClaudeParserErrorAndNoResult(t *testing.T) {
	p := NewParser(ParserClaudeJSON)
	feed(p, `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"","total_cost_usd":0.5,"usage":{"input_tokens":1,"output_tokens":1}}`)
	if s := p.Summary(); !s.Failed || s.Err != "error_max_turns" || s.USD != 0.5 {
		t.Fatalf("%+v", s)
	}
	// A run killed before its result: the last assistant text stands in.
	q := NewParser(ParserClaudeJSON)
	feed(q, `{"type":"assistant","message":{"content":[{"type":"text","text":"half way"}]}}`+"\nwarning: not json")
	if s := q.Summary(); s.Text != "half way" {
		t.Fatalf("%+v", s)
	}
}

const codexOut = `{"type":"thread.started","thread_id":"th1"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"go test ./...","status":"in_progress"}}
{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"go test ./...","aggregated_output":"ok","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"i2","type":"agent_message","text":"Implemented the change and the tests pass."}}
{"type":"turn.completed","usage":{"input_tokens":2000,"cached_input_tokens":1500,"output_tokens":400}}
`

func TestCodexParser(t *testing.T) {
	p := NewParser(ParserCodexJSONL)
	shown := strings.Join(feed(p, codexOut), "\n")
	if !strings.Contains(shown, "[run] go test ./...") || !strings.Contains(shown, "Implemented the change") {
		t.Fatalf("%q", shown)
	}
	s := p.Summary()
	if s.Text != "Implemented the change and the tests pass." || s.Tokens != 2400 || s.Failed || s.USD != 0 {
		t.Fatalf("%+v", s)
	}
	q := NewParser(ParserCodexJSONL)
	feed(q, `{"type":"turn.failed","error":{"message":"rate limited"}}`)
	if s := q.Summary(); !s.Failed || s.Err != "rate limited" {
		t.Fatalf("%+v", s)
	}
}

func TestTextParserKeepsTheTail(t *testing.T) {
	p := NewParser("")
	for i := 0; i < 500; i++ {
		p.Line("line " + strings.Repeat("x", 100))
	}
	p.Line("the end")
	s := p.Summary()
	if !strings.HasSuffix(s.Text, "the end") || len([]rune(s.Text)) > maxSummary+1 {
		t.Fatalf("%d", len(s.Text))
	}
}

func TestRedact(t *testing.T) {
	in := "key sk-ant-api03-abcdefghijklmnopqrstuvwxyz Authorization: Bearer abc.def.ghi x-api-key: zzzzzzzzzz url https://u:hunter22@host/x and my-custom-secret-value ANTHROPIC_API_KEY=plainvalue123 password=\"p4ss\""
	out := Redact(in, "my-custom-secret-value")
	for _, leak := range []string{"abcdefghijklmnopqrstuvwxyz", "abc.def.ghi", "zzzzzzzzzz", "hunter22", "my-custom-secret-value", "plainvalue123", "p4ss"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked in %q", leak, out)
		}
	}
	if Redact("an ordinary line", "ab") != "an ordinary line" {
		t.Fatal("short secrets must be ignored")
	}
}

func TestRender(t *testing.T) {
	r := &Result{Executor: "codex", Level: LevelEdit, Summary: "done", Changed: []string{"a.go"}, DiffStat: "1 file changed", Tokens: 10, USD: 0.01}
	out := Render(r)
	for _, w := range []string{"exit code 0", "usage: 10 tokens", "done", "a.go", "1 file changed"} {
		if !strings.Contains(out, w) {
			t.Errorf("lacks %q: %s", w, out)
		}
	}
	r.ExitCode = 3
	if !strings.HasPrefix(Render(r), "EXECUTOR FAILED") {
		t.Fatal("failure header")
	}
}
