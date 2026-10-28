package bench_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/bench"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/llm/llmtest"
)

func needs(t *testing.T, task bench.Task) {
	t.Helper()
	if m := bench.Missing(task); len(m) > 0 {
		t.Skipf("needs %v on PATH", m)
	}
}

func TestEveryTaskFailsBeforeTheFixAndPassesWithTheReferenceSolution(t *testing.T) {
	for _, task := range bench.Builtin() {
		t.Run(task.Name, func(t *testing.T) {
			// Arrange
			needs(t, task)
			ctx := context.Background()
			before, after := t.TempDir(), t.TempDir()
			if err := bench.Materialize(before, task.Files); err != nil {
				t.Fatal(err)
			}
			if err := bench.Materialize(after, task.Files, task.Solution); err != nil {
				t.Fatal(err)
			}

			// Act
			okBefore, outBefore := bench.Check(ctx, task, before, "")
			okAfter, outAfter := bench.Check(ctx, task, after, task.SolutionAnswer)

			// Assert
			if okBefore {
				t.Errorf("the untouched repository already passes:\n%s", outBefore)
			}
			if !okAfter {
				t.Errorf("the reference solution does not pass:\n%s", outAfter)
			}
		})
	}
}

func TestHiddenTestsCatchAnAgentThatWeakensTheTests(t *testing.T) {
	// Arrange
	task := mustTask(t, "go-fix-bugs")
	needs(t, task)
	dir := t.TempDir()
	_ = bench.Materialize(dir, task.Files, map[string]string{"stats/stats_test.go": "package stats\n\nimport \"testing\"\n\nfunc TestNothing(t *testing.T) {}\n"})

	// Act
	ok, out := bench.Check(context.Background(), task, dir, "")

	// Assert
	if ok {
		t.Fatalf("a repository with emptied tests passed:\n%s", out)
	}
}

func mustTask(t *testing.T, name string) bench.Task {
	t.Helper()
	ts, missing := bench.Select([]string{name})
	if len(missing) > 0 {
		t.Fatalf("no task %s", name)
	}
	return ts[0]
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "scripted")
	t.Setenv("LLM_DEFAULT_MODEL", "scripted")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// scripted writes the files of the task's reference solution with write_file calls, then answers.
func scripted(task bench.Task) llmtest.Turn {
	var chunks []*schema.Message
	i := 0
	for path, content := range task.Solution {
		args := `{"path":` + quote("/workspace/"+path) + `,"content":` + quote(content) + `}`
		chunks = append(chunks, llmtest.Finish(llmtest.ToolCall(i, "w"+string(rune('a'+i)), "write_file", args), "tool_calls"))
		i++
	}
	return llmtest.Turn{Chunks: chunks}
}

func quote(s string) string {
	var b bytes.Buffer
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestRunMeasuresASolvedTask(t *testing.T) {
	// Arrange
	task := mustTask(t, "go-add-feature")
	needs(t, task)
	model := llmtest.New(
		scripted(task),
		llmtest.Turn{Chunks: llmtest.Text("Implemented Slugify.")},
		llmtest.Turn{Chunks: llmtest.Text("A title")},
	)

	// Act
	res := bench.Run(context.Background(), testConfig(t), llmtest.Factory{M: model}, task, bench.Options{})

	// Assert
	if !res.Passed || res.Status != "finished" {
		t.Fatalf("result %+v", res)
	}
	if res.Steps < 2 || res.ToolCalls != 1 || res.Tools["write_file"] != 1 || res.ToolErrors != 0 {
		t.Fatalf("metrics %+v", res)
	}
	if !strings.Contains(res.FinalAnswer, "Implemented Slugify") || res.WallSeconds <= 0 || res.HeapMiB <= 0 {
		t.Fatalf("answer %q wall %v heap %v", res.FinalAnswer, res.WallSeconds, res.HeapMiB)
	}
}

func TestRunFailsATaskTheAgentDidNotSolve(t *testing.T) {
	// Arrange
	task := mustTask(t, "go-add-feature")
	needs(t, task)
	model := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("I could not do it.")}, llmtest.Turn{Chunks: llmtest.Text("A title")})

	// Act
	res := bench.Run(context.Background(), testConfig(t), llmtest.Factory{M: model}, task, bench.Options{})

	// Assert
	if res.Passed || res.Status != "finished" || res.VerifyOutput == "" {
		t.Fatalf("result %+v", res)
	}
}

func TestRunChecksTheAnswerOfAQuestionTask(t *testing.T) {
	// Arrange
	task := mustTask(t, "code-question")
	right := llmtest.New(llmtest.Turn{Chunks: llmtest.Text(task.SolutionAnswer)}, llmtest.Turn{Chunks: llmtest.Text("T")})
	wrong := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("It is in signup.go.")}, llmtest.Turn{Chunks: llmtest.Text("T")})

	// Act
	good := bench.Run(context.Background(), testConfig(t), llmtest.Factory{M: right}, task, bench.Options{})
	bad := bench.Run(context.Background(), testConfig(t), llmtest.Factory{M: wrong}, task, bench.Options{})

	// Assert
	if !good.Passed || bad.Passed {
		t.Fatalf("good %+v bad %+v", good, bad)
	}
}

func TestReportSummaryMarkdownAndCompare(t *testing.T) {
	// Arrange
	a := []bench.Result{
		{Task: "x", Passed: true, Steps: 4, ToolCalls: 3, InputTokens: 1000, CachedTokens: 500, CacheHitPct: 50, OutputTokens: 100, WallSeconds: 10},
		{Task: "y", Passed: false, Steps: 6, InputTokens: 3000, OutputTokens: 200, WallSeconds: 20},
	}
	b := []bench.Result{{Task: "x", Passed: true, Steps: 3, InputTokens: 800, CachedTokens: 600, CacheHitPct: 75, WallSeconds: 8}}
	path := t.TempDir() + "/r.json"

	// Act
	s := bench.Summarize(a)
	var md, cmp bytes.Buffer
	bench.Markdown(&md, a)
	err := bench.WriteJSON(path, "m", a)
	read, rerr := bench.ReadReport(path)
	bench.Compare(&cmp, read, bench.Report{Results: b, Summary: bench.Summarize(b)})

	// Assert
	if s.Runs != 2 || s.Passed != 1 || s.PassRate != 0.5 || s.InputTokens != 4000 || s.CacheHitPct != 12.5 {
		t.Fatalf("summary %+v", s)
	}
	if err != nil || rerr != nil || read.Model != "m" || len(read.Results) != 2 {
		t.Fatalf("json %v %v %+v", err, rerr, read)
	}
	if !strings.Contains(md.String(), "| x | pass |") || !strings.Contains(md.String(), "**1/2**") {
		t.Fatalf("markdown:\n%s", md.String())
	}
	if !strings.Contains(cmp.String(), "4→3") || !strings.Contains(cmp.String(), "1000→800") {
		t.Fatalf("compare:\n%s", cmp.String())
	}
	_ = os.Remove(path)
}
