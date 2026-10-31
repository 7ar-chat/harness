package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/engine/tools"
)

type fakeSpiller struct {
	path    string
	err     error
	content string
	callID  string
	calls   int
}

func (f *fakeSpiller) SpillOutput(_ context.Context, _ tools.RunContext, id, content string) (string, error) {
	f.calls++
	f.callID, f.content = id, content
	return f.path, f.err
}

const spillFile = "/workspace/.work/tool-output/c1-ab12cd34.txt"

func TestSpillCharsIsNeverAboveTheToolOutputCap(t *testing.T) {
	for name, tc := range map[string]struct {
		spill, cap, want int
	}{
		"defaults":       {0, 0, 20000},
		"custom":         {5000, 0, 5000},
		"cap below":      {0, 8000, 8000},
		"cap above":      {5000, 30000, 5000},
		"both lower cap": {9000, 4000, 4000},
	} {
		t.Run(name, func(t *testing.T) {
			a := &Agent{SpillChars: tc.spill, MaxToolOutputChars: tc.cap}
			if got := a.spillChars(); got != tc.want {
				t.Fatalf("spillChars = %d", got)
			}
		})
	}
}

func TestSpillLeavesAShortOutputAlone(t *testing.T) {
	// Arrange
	st := newState(5)
	sp := &fakeSpiller{path: spillFile}
	st.a.Harness.Spill = sp
	content := strings.Repeat("é", 20000) // exactly the limit, in runes

	// Act
	got := st.spill(context.Background(), call("t", "{}"), content)

	// Assert
	if got != content || sp.calls != 0 {
		t.Fatalf("changed=%v spills=%d", got != content, sp.calls)
	}
}

func TestSpillKeepsTheFullOutputInAFileAndShowsHeadAndTail(t *testing.T) {
	// Arrange
	st := newState(5)
	sp := &fakeSpiller{path: spillFile}
	st.a.Harness.Spill = sp
	content := strings.Repeat("H", 8000) + strings.Repeat("m", 20000) + strings.Repeat("T", 4000)
	tc := call("execute_command", "{}")
	tc.ID = "c1"

	// Act
	got := st.spill(context.Background(), tc, content)

	// Assert
	if sp.content != content || sp.callID != "c1" {
		t.Fatalf("the spiller did not receive the full output of c1 (%d chars, id %q)", len(sp.content), sp.callID)
	}
	if !strings.HasPrefix(got, strings.Repeat("H", 8000)) || !strings.HasSuffix(got, strings.Repeat("T", 4000)) {
		t.Fatal("head and tail not kept")
	}
	if !strings.Contains(got, "20000 of 32000 characters not shown") || !strings.Contains(got, spillFile) {
		t.Fatalf("marker: %q", got[8000:8400])
	}
}

func TestSpillShrinksTheHeadAndTailWhenTheLimitIsSmall(t *testing.T) {
	// Arrange
	st := newState(5)
	st.a.SpillChars = 1000
	st.a.Harness.Spill = &fakeSpiller{path: spillFile}
	content := strings.Repeat("a", 500) + strings.Repeat("b", 2000) + strings.Repeat("c", 500)

	// Act
	got := st.spill(context.Background(), call("t", "{}"), content)

	// Assert
	if !strings.HasPrefix(got, strings.Repeat("a", 500)) || !strings.HasSuffix(got, strings.Repeat("c", 250)) || strings.HasSuffix(got, strings.Repeat("c", 251)) {
		t.Fatalf("shape: head %q… tail …%q", got[:10], got[len(got)-10:])
	}
	if !strings.Contains(got, "2250 of 3000 characters not shown") {
		t.Fatalf("marker count: %q", got[500:700])
	}
}

func TestSpillOfAJSONResultStaysValidJSONWithThePathAdded(t *testing.T) {
	// Arrange
	st := newState(5)
	st.a.Harness.Spill = &fakeSpiller{path: spillFile}
	content := mustJSON(t, map[string]any{"exitCode": 0, "stdout": strings.Repeat("line of output\n", 3000), "stderr": ""})

	// Act
	got := st.spill(context.Background(), call("execute_command", "{}"), content)

	// Assert
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if out["spilled"] != true || out["outputPath"] != spillFile || out["exitCode"] != float64(0) {
		t.Fatalf("fields %v", out)
	}
	if n := utf8.RuneCountInString(got); n > 20000 {
		t.Fatalf("%d runes", n)
	}
}

func TestSpillOfAJSONResultThatHasAnOutputPathUsesSpillPath(t *testing.T) {
	// Arrange
	st := newState(5)
	st.a.Harness.Spill = &fakeSpiller{path: spillFile}
	content := mustJSON(t, map[string]any{"outputPath": "/workspace/outputs/report.txt", "text": strings.Repeat("x", 30000)})

	// Act
	got := st.spill(context.Background(), call("t", "{}"), content)

	// Assert
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if out["outputPath"] != "/workspace/outputs/report.txt" || out["spillPath"] != spillFile {
		t.Fatalf("paths %v %v", out["outputPath"], out["spillPath"])
	}
}

func TestSpillOfAJSONResultWithoutLargeStringsGetsAPreview(t *testing.T) {
	// Arrange: a long array has nothing to cut in place.
	st := newState(5)
	st.a.Harness.Spill = &fakeSpiller{path: spillFile}
	rows := make([]map[string]int, 4000)
	for i := range rows {
		rows[i] = map[string]int{"n": i}
	}
	content := mustJSON(t, map[string]any{"rows": rows})

	// Act
	got := st.spill(context.Background(), call("t", "{}"), content)

	// Assert
	var out struct {
		Spilled    bool   `json:"spilled"`
		OutputPath string `json:"outputPath"`
		Preview    string `json:"preview"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("preview is not valid JSON: %v", err)
	}
	if !out.Spilled || out.OutputPath != spillFile || !strings.HasPrefix(out.Preview, `{"rows":[{"n":0}`) {
		t.Fatalf("%+v", out)
	}
	if utf8.RuneCountInString(got) > 20000 {
		t.Fatalf("%d runes", utf8.RuneCountInString(got))
	}
}

func TestSpillCutsWithANoteWhenThereIsNoSandboxToKeepTheOutput(t *testing.T) {
	for name, sp := range map[string]tools.OutputSpiller{
		"no spiller":         nil,
		"no running sandbox": &fakeSpiller{err: tools.ErrNoSpill},
		"write failed":       &fakeSpiller{err: errors.New("disk full")},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			st := newState(5)
			st.a.Harness.Spill = sp
			content := strings.Repeat("a", 10000) + strings.Repeat("z", 30000)

			// Act
			got := st.spill(context.Background(), call("t", "{}"), content)

			// Assert
			if utf8.RuneCountInString(got) > 20000 {
				t.Fatalf("%d runes", utf8.RuneCountInString(got))
			}
			if !strings.HasPrefix(got, "aaa") || !strings.HasSuffix(got, "zzz") || !strings.Contains(got, "the full output could not be kept") {
				t.Fatalf("not head + note + tail: %q…%q", got[:5], got[len(got)-5:])
			}
		})
	}
}

func TestCutWithNoteKeepsAJSONObjectValidAndMarksItTruncated(t *testing.T) {
	// Arrange
	content := mustJSON(t, map[string]any{"exitCode": 1, "stdout": strings.Repeat("x", 30000)})

	// Act
	got := cutWithNote(content, 5000)

	// Assert
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if out["truncated"] != true || out["exitCode"] != float64(1) || utf8.RuneCountInString(got) > 5000 {
		t.Fatalf("%v (%d runes)", out, utf8.RuneCountInString(got))
	}
}

func TestCutWithNoteOfAnUncuttableJSONObjectFallsBackToAPlainCut(t *testing.T) {
	// Arrange: many small strings, none worth cutting.
	m := map[string]string{}
	for i := range 400 {
		m[string(rune('a'+i%26))+strings.Repeat("k", i%7)+string(rune('A'+i/26))] = strings.Repeat("v", 100)
	}
	content := mustJSON(t, m)

	// Act
	got := cutWithNote(content, 6000)

	// Assert
	if utf8.RuneCountInString(got) > 6000 || !strings.Contains(got, "characters not shown") {
		t.Fatalf("%d runes: %q", utf8.RuneCountInString(got), got[:60])
	}
}

func TestToolImagesAreQueuedByTheEmitterAndDrainedOnce(t *testing.T) {
	// Arrange
	st := newState(5)
	st.a.Harness.Vision = func(m string) bool { return m == "m" }
	em := &toolEmitter{st: st}

	// Act
	em.AttachImage(tools.Image{Path: "/workspace/a.png", Data: []byte("png")})
	em.AttachImage(tools.Image{Path: "/workspace/empty.png"}) // no data: nothing to show
	first, second := st.takeToolImages(), st.takeToolImages()

	// Assert
	if !em.CanViewImages() {
		t.Error("a vision model cannot view images")
	}
	st.in.Run.Model = "text-only"
	if em.CanViewImages() {
		t.Error("a text-only model can view images")
	}
	if len(first) != 1 || first[0].Path != "/workspace/a.png" || len(second) != 0 {
		t.Fatalf("first %+v second %+v", first, second)
	}
}

func TestCanViewImagesIsFalseWithoutAVisionPredicate(t *testing.T) {
	em := &toolEmitter{st: newState(5)}
	if em.CanViewImages() {
		t.Fatal("no Vision hook, yet images are viewable")
	}
}

type roTool struct {
	name string
	ro   bool
}

func (r roTool) Info(context.Context) (*ai.ToolInfo, error) { return &ai.ToolInfo{Name: r.name}, nil }
func (r roTool) ReadOnly() bool                             { return r.ro }
