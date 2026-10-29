package runtime_test

import (
	"archive/zip"
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/features/verify"
)

// Final review of round 3 (F1, F5): a command whose last step is the file's
// checker command spares the runtime only the part of its check that command
// covered, and only when nothing earlier in the command can change what that
// step runs.

// shortcutRig is verifyRig with the thread's artifacts wired into the
// verifier: decks have a baseline, documents get the citation fixture.
func shortcutRig(t *testing.T, turns ...llmtest.Turn) *scriptedRig {
	t.Helper()
	r := verifyRig(t, false, turns...)
	t.Setenv("CHECK_LOG", filepath.Join(t.TempDir(), "check.log"))
	check := "bash " + fixtureChecker(t) + " {file}"
	cite, err := filepath.Abs(filepath.Join("..", "..", "features", "verify", "testdata", "checkers", "citations.sh"))
	if err != nil {
		t.Fatal(err)
	}
	r.agent.Harness.Checker = verify.New(verify.Options{Sandbox: r.mgr, Artifacts: r.arts,
		Checkers: map[string]verify.Checker{
			".docx": {Name: "inspect_docx", Command: check},
			".pptx": {Name: "fit_text", Command: check, Baseline: "--baseline {baseline}"},
		},
		Citations: &verify.Checker{Name: "check_citations", Command: "bash " + cite + " {sources} {file}"},
	})
	return r
}

// pptxBytes is a minimal deck an upload accepts (OOXML package of a presentation).
func pptxBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/></Types>`,
		"ppt/presentation.xml": `<?xml version="1.0"?><p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>`,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// execCount counts the sandbox commands containing s.
func execCount(r *scriptedRig, s string) int {
	n := 0
	for _, e := range r.sandbox().Execs() {
		n += strings.Count(e.Command, s)
	}
	return n
}

func TestTheRuntimeStillChecksCitationsAfterTheModelRanTheFormatChecker(t *testing.T) {
	// A document whose command ended with its own format checker is still checked by the runtime against the thread's uploaded PDF: an invented cited figure fails it (F1, A9).
	// Arrange
	check := "bash " + fixtureChecker(t) + " /workspace/outputs/brief.docx"
	r := shortcutRig(t,
		llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"printf '38 %% (p. 19) INVENTED' > /workspace/outputs/brief.docx && `+check+`"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Brief ready.")},
		llmtest.Turn{Chunks: llmtest.Text("Brief ready, the citation is wrong.")},
		llmtest.Turn{Chunks: llmtest.Text("unexpected call")},
	)
	run := r.attachedRunWith("write the brief", map[string][]byte{"study.pdf": []byte("%PDF-1.4\n%%EOF\n")})

	// Act
	r.process(run)

	// Assert
	res := find(r.events(run.ID), "TOOL_CALL_RESULT")[0]["content"].(string)
	v := verificationOf(t, lastAnswer(t, r, run.ThreadID))
	if execCount(r, "check_doc.sh") != 1 || !strings.Contains(res, "check_citations: NOT OK") || len(paths(v["verified"])) != 0 ||
		len(paths(v["failed"])) != 1 {
		t.Fatalf("format checker runs %d, result %s, verification %v", execCount(r, "check_doc.sh"), res, v)
	}
}

func TestTheRuntimeChecksADeckAgainstItsBaselineAfterTheModelRanItsChecker(t *testing.T) {
	// A deck copied from an upload whose command ended with fit_text (no --baseline) is checked again by the runtime with --baseline <the upload>, so only the overlaps the agent added count (F1, N8).
	// Arrange
	check := "bash " + fixtureChecker(t) + " /workspace/outputs/deck.pptx"
	r := shortcutRig(t,
		llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"printf EDITED > /workspace/outputs/deck.pptx && `+check+`"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Deck ready.")},
		llmtest.Turn{Chunks: llmtest.Text("unexpected call")},
	)
	run := r.attachedRunWith("update the deck", map[string][]byte{"deck.pptx": pptxBytes(t)})

	// Act
	r.process(run)

	// Assert
	if n := execCount(r, "--baseline"); n != 1 {
		var cmds []string
		for _, e := range r.sandbox().Execs() {
			cmds = append(cmds, e.Command)
		}
		t.Fatalf("%d checks with --baseline, commands %q", n, cmds)
	}
}

func TestACommandThatCanChangeWhatItsLastStepRunsIsCheckedByTheRuntime(t *testing.T) {
	// A command that defines a function or alias, changes PATH, sources a file or uses eval/exec before its last step (the file's checker command) may have forged that check: the runtime runs its own check, which wins (F5).
	file := "/workspace/outputs/report.docx"
	forged := `{\"ok\": true, \"file\": \"` + file + `\", \"checks\": [{\"name\": \"opens\", \"ok\": true, \"detail\": \"\"}]}`
	for name, prefix := range map[string]string{
		"function": `bash() { echo '` + forged + `'; }; `,
		"keyword":  `function bash { echo '` + forged + `'; }; `,
		"alias":    `shopt -s expand_aliases; alias bash='echo ` + forged + `'; `,
		"path":     `export PATH=/tmp/fake:$PATH; `,
		"source":   `source /dev/null; `,
		"dot":      `. /dev/null && `,
		"eval":     `eval true; `,
		"exec":     `exec 3>/dev/null; `,
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			check := "bash " + fixtureChecker(t) + " " + file
			r := verifyRig(t, false,
				llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"`+prefix+`printf BROKEN > `+file+`; `+check+`"}`)},
				llmtest.Turn{Chunks: llmtest.Text("Report ready.")},
				llmtest.Turn{Chunks: llmtest.Text("Report ready, its headings fail the check.")},
				llmtest.Turn{Chunks: llmtest.Text("unexpected call")},
			)
			th, run := r.queued("write the report")

			// Act
			r.process(run)

			// Assert
			v := verificationOf(t, lastAnswer(t, r, th.ID))
			runtimeChecks := 0
			for _, e := range r.sandbox().Execs() {
				if strings.Contains(e.Command, "check_doc.sh") && !strings.Contains(e.Command, "printf") {
					runtimeChecks++
				}
			}
			if runtimeChecks != 1 || len(paths(v["failed"])) != 1 || len(paths(v["verified"])) != 0 {
				t.Fatalf("runtime checks %d, verification %v", runtimeChecks, v)
			}
		})
	}
}
