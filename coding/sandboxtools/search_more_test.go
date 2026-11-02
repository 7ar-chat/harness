package sandboxtools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func TestSearchFilesValidatesItsArguments(t *testing.T) {
	// Arrange
	f := newFixture(t, nil)

	// Act
	_, blank := f.call(context.Background(), "search_files", `{"pattern":"  "}`)
	_, nul := f.call(context.Background(), "search_files", `{"pattern":"x","path":"a\u0000"}`)

	// Assert
	if blank == nil || !strings.Contains(blank.Error(), "pattern is required") || nul == nil || !strings.Contains(nul.Error(), "invalid path") {
		t.Fatalf("blank %v nul %v", blank, nul)
	}
	if len(f.backend.Created()) != 0 {
		t.Fatal("a sandbox was created for an invalid search")
	}
}

func TestSearchFilesScopesToAFileOrDirectoryAndReturnsAnEmptyListWhenNothingMatches(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/src/a.go", "needle\n")
		sb.Put("/workspace/other/b.go", "needle\n")
	})

	// Act
	dir, err1 := f.call(context.Background(), "search_files", `{"pattern":"needle","path":"src"}`)
	file, err2 := f.call(context.Background(), "search_files", `{"pattern":"needle","path":"other/b.go"}`)
	none, err3 := f.call(context.Background(), "search_files", `{"pattern":"absent"}`)

	// Assert
	var d, fl searchResult
	_ = json.Unmarshal([]byte(dir), &d)
	_ = json.Unmarshal([]byte(file), &fl)
	if err1 != nil || len(d.Matches) != 1 || d.Matches[0].Path != "/workspace/src/a.go" {
		t.Errorf("dir: %s %v", dir, err1)
	}
	if err2 != nil || len(fl.Matches) != 1 || fl.Matches[0].Path != "/workspace/other/b.go" {
		t.Errorf("file: %s %v", file, err2)
	}
	if err3 != nil || !strings.Contains(none, `"matches":[]`) || strings.Contains(none, `"truncated":true`) {
		t.Errorf("none: %s %v", none, err3)
	}
}

func TestSearchFilesCapsMaxResultsAtFiveHundredAndCutsLongLines(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/many.txt", strings.Repeat("hit\n", 700))
		sb.Put("/workspace/long.txt", "hit "+strings.Repeat("é", 400)+"\n")
	})

	// Act
	many, err1 := f.call(context.Background(), "search_files", `{"pattern":"^hit$","maxResults":9999}`)
	long, err2 := f.call(context.Background(), "search_files", `{"pattern":"hit é","path":"long.txt"}`)

	// Assert
	var m, l searchResult
	_ = json.Unmarshal([]byte(many), &m)
	_ = json.Unmarshal([]byte(long), &l)
	if err1 != nil || len(m.Matches) != 500 || !m.Truncated {
		t.Errorf("many: %d matches truncated %v %v", len(m.Matches), m.Truncated, err1)
	}
	if err2 != nil || len(l.Matches) != 1 || !strings.HasSuffix(l.Matches[0].Text, "…") || strings.ContainsRune(l.Matches[0].Text, '\uFFFD') || len(l.Matches[0].Text) > 304 {
		t.Errorf("long line: %v %v", l.Matches, err2)
	}
}

func TestSearchFilesHandlesTheStreamOfAnyScript(t *testing.T) {
	event := func(path string, line int, text string) string {
		b, _ := json.Marshal(map[string]any{"type": "match", "data": map[string]any{"path": map[string]any{"text": path}, "line_number": line, "lines": map[string]any{"text": text}}})
		return string(b)
	}
	cases := map[string]struct {
		script  func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error)
		matches int
		err     string
	}{
		"events split across chunks": {func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			e := event("/workspace/a", 1, "one\r\n")
			req.OnStdout(e[:10])
			req.OnStdout(e[10:] + "\n" + `{"type":"begin","data":{}}` + "\n" + "not json\n" + `{"type":"match"`)
			req.OnStdout(`,"data":{"lines":{"text":"two"}}}`) // no trailing newline: flushed at the end
			return sandbox.ExecResult{}, nil
		}, 2, ""},
		"error without matches": {func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			req.OnStderr("regex parse error\n")
			return sandbox.ExecResult{ExitCode: 2}, nil
		}, 0, "search failed: regex parse error"},
		"error without a message": {func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{ExitCode: 3}, nil
		}, 0, "search failed: exit 3"},
		"partial matches survive an error exit": {func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			req.OnStdout(event("/workspace/a", 5, "x") + "\n")
			req.OnStderr("permission denied\n")
			return sandbox.ExecResult{ExitCode: 2}, nil
		}, 1, ""},
		"exec error": {func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{}, errors.New("boom")
		}, 0, "search failed: boom"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			sb := newScripted(map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){"# work:search": c.script})
			ts := toolsOn(t, sb, sandboxtools.Options{})

			// Act
			out, err := ts["search_files"].InvokableRun(context.Background(), `{"pattern":"x"}`)

			// Assert
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err %v, want %q", err, c.err)
				}
				return
			}
			var r searchResult
			_ = json.Unmarshal([]byte(out), &r)
			if err != nil || len(r.Matches) != c.matches {
				t.Fatalf("err %v out %s", err, out)
			}
			if name == "events split across chunks" && (r.Matches[0].Text != "one" || r.Matches[0].Line != 1 || r.Matches[1].Text != "two") {
				t.Fatalf("matches %+v", r.Matches)
			}
		})
	}
}

func TestSearchFilesStopsWhenTheCallIsCancelled(t *testing.T) {
	// Arrange
	ctx, cancel := context.WithCancel(context.Background())
	sb := newScripted(map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){
		"# work:search": func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
			cancel()
			return sandbox.ExecResult{}, context.Canceled
		}})
	ts := toolsOn(t, sb, sandboxtools.Options{})

	// Act
	_, err := ts["search_files"].InvokableRun(ctx, `{"pattern":"x"}`)

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestSearchFilesRetriesOnceOnANewSandboxWithoutDuplicatingMatches(t *testing.T) {
	// Arrange: the first attempt streams a match then finds the sandbox gone.
	calls := 0
	sb := newScripted(map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){
		"# work:search": func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			calls++
			req.OnStdout(`{"type":"match","data":{"path":{"text":"/workspace/a"},"line_number":1,"lines":{"text":"x"}}}` + "\n")
			if calls == 1 {
				return sandbox.ExecResult{}, sandbox.ErrGone
			}
			return sandbox.ExecResult{}, nil
		}})
	ts := toolsOn(t, sb, sandboxtools.Options{})

	// Act
	out, err := ts["search_files"].InvokableRun(context.Background(), `{"pattern":"x"}`)

	// Assert
	var r searchResult
	_ = json.Unmarshal([]byte(out), &r)
	if err != nil || calls != 2 || len(r.Matches) != 1 || r.Truncated {
		t.Fatalf("err %v calls %d out %s", err, calls, out)
	}
}
