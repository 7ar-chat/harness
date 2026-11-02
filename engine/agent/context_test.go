package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
)

func TestSettingsMapZeroToDisabledAndKeepPositiveValues(t *testing.T) {
	for n, want := range map[int]int{-3: -1, 0: -1, 1: 1, 6: 6} {
		if got := MaskSetting(n); got != want {
			t.Errorf("MaskSetting(%d) = %d, want %d", n, got, want)
		}
		if got := StepRetriesSetting(n); got != want {
			t.Errorf("StepRetriesSetting(%d) = %d, want %d", n, got, want)
		}
	}
	for f, want := range map[float64]float64{-1: -1, 0: -1, 0.3: 0.3, 1: 1} {
		if got := MaskFromSetting(f); got != want {
			t.Errorf("MaskFromSetting(%v) = %v, want %v", f, got, want)
		}
	}
}

func TestKeepRecentToolResultsDefaultsToSixAndNegativeDisables(t *testing.T) {
	for k, want := range map[int]int{0: 6, 3: 3, -1: -1, -9: -1} {
		st := newState(10)
		st.a.Harness.KeepRecentToolResults = k
		if got := st.keepRecentToolResults(); got != want {
			t.Errorf("keep %d -> %d, want %d", k, got, want)
		}
	}
}

func TestMaskFromDefaultsToHalfTheWindow(t *testing.T) {
	for f, want := range map[float64]float64{0: 0.5, 1: 0.5, 2: 0.5, 0.25: 0.25, -1: -1, -0.5: -1} {
		st := newState(10)
		st.a.Harness.MaskFromFraction = f
		if got := st.maskFrom(); got != want {
			t.Errorf("fraction %v -> %v, want %v", f, got, want)
		}
	}
}

func TestHumanSizeUsesTheLargestUnit(t *testing.T) {
	for n, want := range map[int]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1536: "1.5 KB", 1<<20 - 1: "1024.0 KB", 1 << 20: "1.0 MB", 5 << 20: "5.0 MB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestMaskStubReportsExitCodeAndSpillPathOfAJSONResult(t *testing.T) {
	// Arrange
	content := `{"exitCode":2,"outputPath":"/workspace/.work/tool-output/c1-ab.txt","stdout":"` + strings.Repeat("x", 2000) + `"}`

	// Act
	got := MaskStub("execute_command", content)

	// Assert
	want := "[execute_command result, 2.0 KB, exit 2 — hidden to save context; full output in /workspace/.work/tool-output/c1-ab.txt]"
	if !strings.HasPrefix(got, "[execute_command result, 2.") || !strings.HasSuffix(got, ", exit 2 — hidden to save context; full output in /workspace/.work/tool-output/c1-ab.txt]") {
		t.Fatalf("got %q, want like %q", got, want)
	}
}

func TestMaskStubRecoversExitCodeAndPathFromACutResult(t *testing.T) {
	// A spilled or cut output is no longer a JSON object: the last exitCode and the first spill path are read from the text.
	// Arrange
	content := `{"exitCode": 1, "stdout": "…[… 9000 of 12000 characters not shown. The full output is in /workspace/.work/tool-output/c9-ff.txt: page it …]… "exitCode": 137 tail`

	// Act
	got := MaskStub("", content)

	// Assert
	want := "[tool result, " + humanSize(len(content)) + ", exit 137 — hidden to save context; full output in /workspace/.work/tool-output/c9-ff.txt]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMaskStubWithoutExitOrPathIsJustSizeAndReason(t *testing.T) {
	// Act
	got := MaskStub("search_files", strings.Repeat("hit\n", 300))

	// Assert
	if got != "[search_files result, 1.2 KB — hidden to save context]" {
		t.Fatalf("got %q", got)
	}
}

func toolPair(i int, tool string, size int) []*ai.Message {
	id := fmt.Sprintf("c%d", i)
	return []*ai.Message{
		ai.AssistantMessage("", []ai.ToolCall{{ID: id, Type: "function", Function: ai.FunctionCall{Name: tool, Arguments: "{}"}}}),
		ai.ToolMessage(strings.Repeat("r", size), id, ai.WithToolName(tool)),
	}
}

func historyOf(n int, tool string, size int) []*ai.Message {
	h := []*ai.Message{ai.UserMessage("go")}
	for i := range n {
		h = append(h, toolPair(i, tool, size)...)
	}
	return h
}

func maskedIDs(t *testing.T, in, out []*ai.Message) []string {
	t.Helper()
	if len(in) != len(out) {
		t.Fatalf("length changed: %d -> %d", len(in), len(out))
	}
	var ids []string
	for i := range in {
		if in[i].Role == ai.RoleTool && out[i].Content != in[i].Content {
			ids = append(ids, in[i].ToolCallID)
			if !strings.Contains(out[i].Content, "hidden to save context") {
				t.Errorf("%s changed but is not a stub: %q", in[i].ToolCallID, out[i].Content)
			}
		}
	}
	return ids
}

func TestMaskedInAlwaysOnModeMasksOldResultsInBatchesOfFour(t *testing.T) {
	// Arrange: 6 recent results are kept; between batch boundaries the masked prefix does not move.
	st := newState(50)
	st.a.Harness.MaskFromFraction = -1
	for n, want := range map[int]string{6: "[]", 9: "[]", 10: "[c0 c1 c2 c3]", 13: "[c0 c1 c2 c3]", 14: "[c0 c1 c2 c3 c4 c5 c6 c7]"} {
		h := historyOf(n, "read_file", 1000)

		// Act
		out := st.masked(h)

		// Assert
		if got := fmt.Sprint(maskedIDs(t, h, out)); got != want {
			t.Errorf("%d results: masked %s, want %s", n, got, want)
		}
	}
}

func TestMaskedNeverTouchesTheStoredHistory(t *testing.T) {
	// Arrange
	st := newState(50)
	st.a.Harness.MaskFromFraction = -1
	h := historyOf(12, "read_file", 1000)
	orig := h[2].Content

	// Act
	out := st.masked(h)

	// Assert
	if h[2].Content != orig || out[2].Content == orig {
		t.Fatalf("stored %d chars, model view %d chars", len(h[2].Content), len(out[2].Content))
	}
}

func TestMaskedKeepsShortResultsAndTheToolsWhoseOutputMustStayVisible(t *testing.T) {
	// Arrange
	st := newState(50)
	st.a.Harness.MaskFromFraction = -1
	h := []*ai.Message{ai.UserMessage("go")}
	h = append(h, toolPair(0, "read_file", 599)...)    // shorter than a stub is worth
	h = append(h, toolPair(1, "load_skill", 5000)...)  // skill instructions stay
	h = append(h, toolPair(2, "ask_user", 5000)...)    // the user's answers stay
	h = append(h, toolPair(3, "update_plan", 5000)...) // so do plans
	h = append(h, toolPair(4, "read_file", 5000)...)   // masked
	for i := 5; i < 14; i++ {
		h = append(h, toolPair(i, "read_file", 5000)...)
	}

	// Act
	got := maskedIDs(t, h, st.masked(h))

	// Assert
	if fmt.Sprint(got) != "[c4 c5 c6 c7]" {
		t.Fatalf("masked %v", got)
	}
}

func TestMaskedCountsRunesNotBytesForTheMinimumSize(t *testing.T) {
	// Arrange: 400 runes of 3 bytes each are under the 600-char minimum.
	st := newState(50)
	st.a.Harness.MaskFromFraction = -1
	h := []*ai.Message{ai.UserMessage("go")}
	for i := range 12 {
		id := fmt.Sprintf("c%d", i)
		h = append(h, ai.AssistantMessage("", []ai.ToolCall{{ID: id}}), ai.ToolMessage(strings.Repeat("日", 400), id, ai.WithToolName("read_file")))
	}

	// Act
	got := maskedIDs(t, h, st.masked(h))

	// Assert
	if len(got) != 0 {
		t.Fatalf("masked %v", got)
	}
}

func TestMaskedWithNegativeKeepDisablesMasking(t *testing.T) {
	// Arrange
	st := newState(50)
	st.a.Harness.KeepRecentToolResults = -1
	h := historyOf(30, "read_file", 5000)

	// Act
	out := st.masked(h)

	// Assert
	if &out[0] != &h[0] {
		t.Fatal("history was copied")
	}
	if got := maskedIDs(t, h, out); len(got) != 0 {
		t.Fatalf("masked %v", got)
	}
}

func TestMaskedWaitsForContextPressureAndThenKeepsTheMaskForTheRestOfTheRun(t *testing.T) {
	// Arrange: window 100k tokens, mask from 50 %.
	st := newState(50)
	st.h.window = 100_000
	h := historyOf(10, "read_file", 1000) // ~ 3k tokens: no pressure
	st.history = h

	// Act + Assert: below the fraction nothing is masked.
	if got := maskedIDs(t, h, st.masked(h)); len(got) != 0 {
		t.Fatalf("masked under no pressure: %v", got)
	}

	// The provider reports a 60k-token prompt: all but the newest 6 results are masked at once.
	st.h.lastPrompt, st.h.callAt = 60_000, len(h)
	if got := fmt.Sprint(maskedIDs(t, h, st.masked(h))); got != "[c0 c1 c2 c3]" {
		t.Fatalf("under pressure masked %s", got)
	}

	// Pressure drops (after a compaction): what was masked stays masked, so the prefix stays byte-identical.
	st.h.lastPrompt = 10_000
	h2 := append(append([]*ai.Message(nil), h...), toolPair(10, "read_file", 1000)...)
	st.history = h2
	if got := fmt.Sprint(maskedIDs(t, h2, st.masked(h2))); got != "[c0 c1 c2 c3]" {
		t.Fatalf("masking changed when pressure dropped: %s", got)
	}
}

func TestPromptTokensPrefersTheProvidersCountPlusWhatWasAddedSince(t *testing.T) {
	// Arrange
	st := newState(50)
	st.h.reserved = 1000
	h := historyOf(2, "read_file", 3000)
	st.history = h

	// Act
	estimated := st.promptTokens()
	st.h.lastPrompt, st.h.callAt = 5000, len(h)-2
	observed := st.promptTokens()

	// Assert
	if estimated < 1000+2*3000/4 {
		t.Errorf("estimate %d ignores the history", estimated)
	}
	if observed < 5000+3000/4 || observed > 5000+3000 {
		t.Errorf("observed %d: want the reported 5000 plus the last tool pair", observed)
	}
}
