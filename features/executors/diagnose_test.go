package executors

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/executors"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
)

func TestFailedRunExplainsAMissingKeyAndBooksItUnpriced(t *testing.T) {
	// Arrange
	sb := &fakeSandbox{}
	sb.handlers = append([]func(sandbox.ExecRequest) (string, string, int, bool, bool){
		func(r sandbox.ExecRequest) (string, string, int, bool, bool) {
			if strings.Contains(r.Command, "opencode run") {
				return "", "Error: 401 Unauthorized: invalid api key\n", 1, false, true
			}
			return "", "", 0, false, false
		}}, gitHandlers()...)
	st := &fakeStore{task: &factory.Task{ID: "tk"}}
	st.info = &factory.ThreadInfo{ThreadID: "t", TaskID: "tk", Login: "a"}

	// Act
	res, err := newProvider(sb, st, nil).Run(context.Background(), executors.Request{Login: "a", ThreadID: "t", Executor: "opencode", Task: "x"}, nil)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Problem, "add your key in Account > Executors") || !strings.Contains(res.Problem, "OPENAI_API_KEY") {
		t.Fatalf("problem = %q", res.Problem)
	}
	if !strings.Contains(executors.Render(res), "problem: ") {
		t.Fatal("the report must carry the problem")
	}
	var found bool
	for _, e := range st.events {
		if e.Kind == factory.EventExecutor {
			found = true
			if e.Data["unpriced"] != true || !strings.Contains(e.Text, "cost not reported by this CLI") || e.Data["problem"] == nil {
				t.Fatalf("event = %+v", e)
			}
		}
	}
	if !found || len(st.spends) != 1 || st.spends[0].Unpriced != 1 {
		t.Fatalf("events %+v spends %+v", st.events, st.spends)
	}
}

func TestDiagnoseMissingCLI(t *testing.T) {
	ex := executors.Executor{Name: "omp", Binary: "omp", CredentialEnv: []string{"OPENAI_API_KEY"}}
	if got := Diagnose(ex, "bash: omp: command not found"); !strings.Contains(got, "not installed") {
		t.Fatalf("got %q", got)
	}
	if got := Diagnose(ex, "bash: other: command not found"); got != "" {
		t.Fatalf("an unrelated missing command is not the CLI: %q", got)
	}
	if got := Diagnose(ex, "compile error"); got != "" {
		t.Fatalf("got %q", got)
	}
}
