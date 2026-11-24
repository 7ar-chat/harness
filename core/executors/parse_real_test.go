package executors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures under testdata/real are the REAL stdout of the CLIs (claude 2.1.331, codex-cli 0.172.0, omp 19.1.1),
// captured by features/executors/clitest against scripted fake model servers and scrubbed of paths, ids and ports
// (go test ./features/executors/clitest with HARNESS_REAL_CLIS and HARNESS_UPDATE_FIXTURES=1 rewrites them). The
// *-nokey ones are trimmed from a run with no credentials (Codex: against the real API, which answered 401).
func realFixture(t *testing.T, name string) (Parser, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "real", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	parser := map[string]string{"claude": ParserClaudeJSON, "codex": ParserCodexJSONL, "omp": ParserOMPJSON}[strings.SplitN(name, "-", 2)[0]]
	p := NewParser(parser)
	return p, strings.Join(feed(p, string(b)), "\n")
}

func TestRealClaudeEdit(t *testing.T) {
	p, shown := realFixture(t, "claude-edit")
	for _, w := range []string{"I will create hello.txt.", "[tool Write] hello.txt", "Created hello.txt with the greeting.", "[done] success"} {
		if !strings.Contains(shown, w) {
			t.Errorf("progress lacks %q:\n%s", w, shown)
		}
	}
	if strings.Contains(shown, `{"type"`) {
		t.Errorf("raw JSON shown:\n%s", shown)
	}
	s := p.Summary()
	// input 4400 + output 128 + cache read 1000 (the usage of the whole run, as the result line sums it).
	if s.Text != "Created hello.txt with the greeting." || s.Tokens != 5528 || s.USD != 0.02036 || s.Failed {
		t.Fatalf("%+v", s)
	}
}

func TestRealClaudeReadOnlyShowsTheRefusal(t *testing.T) {
	p, shown := realFixture(t, "claude-readonly")
	// A system line whose "message" is a STRING (the old parser failed to decode it and printed the raw JSON).
	if !strings.Contains(shown, "[denied] Write: Cannot write to /workspace/repo/hello.txt while in plan mode.") || strings.Contains(shown, `{"type"`) {
		t.Fatalf("%s", shown)
	}
	if !strings.Contains(shown, "[tool error] Cannot write") {
		t.Errorf("tool error not shown:\n%s", shown)
	}
	s := p.Summary()
	if s.Failed || !strings.Contains(s.Text, "the CLI refused these tool calls: Write /workspace/repo/hello.txt") {
		t.Fatalf("%+v", s)
	}
}

func TestRealClaudeFailures(t *testing.T) {
	for name, want := range map[string]string{"claude-nokey": "Not logged in · Please run /login", "claude-badkey": "Invalid API key · Fix external API key"} {
		p, shown := realFixture(t, name)
		s := p.Summary()
		if !s.Failed || s.Text != want || s.Err != want || s.USD != 0 {
			t.Errorf("%s: %+v", name, s)
		}
		if !strings.Contains(shown, "[failed] "+want) {
			t.Errorf("%s: %s", name, shown)
		}
	}
	// The rejected key is retried by the CLI (api_retry system lines) and shown as such.
	_, shown := realFixture(t, "claude-badkey")
	if !strings.Contains(shown, "[retry 1/1] authentication_failed (HTTP 401)") {
		t.Errorf("retry not shown:\n%s", shown)
	}
}

func TestRealCodexEdit(t *testing.T) {
	p, shown := realFixture(t, "codex-edit")
	for _, w := range []string{"I will create hello.txt.", "[run] printf %s 'hello from the scripted model", "Created hello.txt with the greeting."} {
		if !strings.Contains(shown, w) {
			t.Errorf("progress lacks %q:\n%s", w, shown)
		}
	}
	if strings.Contains(shown, "zsh") || strings.Contains(shown, `{"type"`) {
		t.Errorf("the shell wrapper or raw JSON is shown:\n%s", shown)
	}
	s := p.Summary()
	if s.Text != "Created hello.txt with the greeting." || s.Tokens != 4528 || s.Failed {
		t.Fatalf("%+v", s)
	}
}

// A run that had to reconnect ("error" events) and then completed is a success.
func TestRealCodexTransientErrorsDoNotFailTheRun(t *testing.T) {
	p, shown := realFixture(t, "codex-edit")
	if s := p.Summary(); s.Failed {
		t.Fatalf("%+v\n%s", s, shown)
	}
	q := NewParser(ParserCodexJSONL)
	feed(q, `{"type":"turn.started"}`+"\n"+`{"type":"error","message":"Reconnecting... 2/5 (unexpected status 404)"}`+"\n"+
		`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`+"\n"+`{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":4}}`)
	if s := q.Summary(); s.Failed || s.Text != "ok" || s.Tokens != 7 {
		t.Fatalf("%+v", s)
	}
	// ... while a stream that ends in an error without completing is a failure.
	r := NewParser(ParserCodexJSONL)
	feed(r, `{"type":"error","message":"stream disconnected"}`)
	if s := r.Summary(); !s.Failed || s.Err != "stream disconnected" {
		t.Fatalf("%+v", s)
	}
}

func TestRealCodexFailures(t *testing.T) {
	for name, want := range map[string]string{"codex-nokey": "Missing bearer or basic authentication", "codex-badkey": "Incorrect API key provided"} {
		p, shown := realFixture(t, name)
		s := p.Summary()
		if !s.Failed || !strings.Contains(s.Err, want) || !strings.Contains(s.Err, "401") || s.Text != "" {
			t.Errorf("%s: %+v", name, s)
		}
		if !strings.Contains(shown, "[retry] Reconnecting") || !strings.Contains(shown, "[error] unexpected status 401") {
			t.Errorf("%s:\n%s", name, shown)
		}
	}
}

func TestRealCodexReadOnly(t *testing.T) {
	p, _ := realFixture(t, "codex-readonly")
	if s := p.Summary(); s.Failed || s.Text != "Done." {
		t.Fatalf("%+v", s)
	}
}

func TestRealOMPEdit(t *testing.T) {
	p, shown := realFixture(t, "omp-edit")
	for _, w := range []string{"I will create hello.txt.", "[tool write] hello.txt", "Created hello.txt with the greeting."} {
		if !strings.Contains(shown, w) {
			t.Errorf("progress lacks %q:\n%s", w, shown)
		}
	}
	if strings.Contains(shown, `{"type"`) {
		t.Errorf("raw JSON shown:\n%s", shown)
	}
	// totalTokens of the two assistant messages: 2187 + 2341 (omp counts the cached tokens apart from the input).
	s := p.Summary()
	if s.Text != "Created hello.txt with the greeting." || s.Tokens != 4528 || s.Failed || s.USD != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestRealOMPReadOnlyAndFailure(t *testing.T) {
	p, shown := realFixture(t, "omp-readonly")
	if !strings.Contains(shown, "[tool error] Tool bash not found") || p.Summary().Failed {
		t.Fatalf("%s %+v", shown, p.Summary())
	}
	q, shown := realFixture(t, "omp-badkey")
	s := q.Summary()
	if !s.Failed || !strings.HasPrefix(s.Err, "401 Incorrect API key provided") || !strings.Contains(shown, "[error] 401") {
		t.Fatalf("%+v\n%s", s, shown)
	}
}

func TestOversizedJSONLineIsNotFlooded(t *testing.T) {
	for _, name := range []string{ParserClaudeJSON, ParserCodexJSONL, ParserOMPJSON} {
		p := NewParser(name)
		if got := p.Line(`{"type":"assistant","message":{"content":"` + strings.Repeat("x", 5000)); got != "[an oversized event was skipped]" {
			t.Errorf("%s: %q", name, got)
		}
		if got := p.Line("npm warn deprecated something"); got != "npm warn deprecated something" {
			t.Errorf("%s: a plain warning must pass: %q", name, got)
		}
	}
}

func TestParserEdgeCases(t *testing.T) {
	// Claude: a sub-agent's words are shown but are not the answer; a user line with string content is ignored.
	p := NewParser(ParserClaudeJSON)
	feed(p, `{"type":"assistant","message":{"content":[{"type":"text","text":"main answer"}]},"parent_tool_use_id":null}`+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"text","text":"sub-agent chatter"}]},"parent_tool_use_id":"toolu_1"}`+"\n"+
		`{"type":"user","message":{"role":"user","content":"plain"}}`)
	if s := p.Summary(); s.Text != "main answer" {
		t.Fatalf("%+v", s)
	}
	// Claude: error_max_turns result without a result text.
	q := NewParser(ParserClaudeJSON)
	feed(q, `{"type":"result","subtype":"error_max_turns","is_error":false,"total_cost_usd":0.5,"usage":{"input_tokens":1,"output_tokens":1}}`)
	if s := q.Summary(); !s.Failed || s.Err != "error_max_turns" || s.USD != 0.5 {
		t.Fatalf("%+v", s)
	}
	// Codex: a failed command, a file change.
	c := NewParser(ParserCodexJSONL)
	shown := strings.Join(feed(c, `{"type":"item.completed","item":{"type":"command_execution","command":"go test","aggregated_output":"a\nFAIL x","exit_code":2}}`+"\n"+
		`{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"a.go","kind":"update"}]}}`), "\n")
	if !strings.Contains(shown, "[run failed, exit 2] FAIL x") || !strings.Contains(shown, "[edit] update a.go") {
		t.Fatalf("%s", shown)
	}
	// omp: a run aborted before any text.
	o := NewParser(ParserOMPJSON)
	feed(o, `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"aborted","usage":{"totalTokens":5,"cost":{"total":0.25}}}}`)
	if s := o.Summary(); !s.Failed || s.Tokens != 5 || s.USD != 0.25 {
		t.Fatalf("%+v", s)
	}
}
