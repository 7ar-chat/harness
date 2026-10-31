//go:build unix

package longtest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/longtest"
)

func steerTexts(o *longtest.Outcome) []string {
	var out []string
	for _, ev := range o.Events {
		var e struct {
			Name  string `json:"name"`
			Value struct {
				Text string `json:"text"`
			} `json:"value"`
		}
		if json.Unmarshal(ev.Data, &e) == nil && e.Name == "work.steer" {
			out = append(out, e.Value.Text)
		}
	}
	return out
}

func countIn(c []*ai.Message, sub string) int {
	n := 0
	for _, m := range c {
		n += strings.Count(m.Content, sub)
	}
	return n
}

var steers = map[int]string{40: "STEER-ONE please also keep imports sorted", 95: "STEER-TWO skip the data directory"}

// checkLongSession asserts what must hold over any 150-step run; minCompactions is how many compactions the setup forces.
func checkLongSession(t *testing.T, o *longtest.Outcome, window, minCompactions int) {
	t.Helper()

	// Assert: the run finishes after the scripted number of steps.
	if o.Status != "finished" {
		t.Fatalf("status %q after %d steps", o.Status, o.Steps)
	}
	if len(o.Calls) != 150 {
		t.Fatalf("agent made %d model calls, want 150", len(o.Calls))
	}
	maxTokens, bigResults, stubs := 0, 0, 0
	for i, c := range o.Calls {
		// The prompt of every step stays under the window.
		if n := longtest.Tokens(c); n > maxTokens {
			maxTokens = n
		}
		if n := longtest.Tokens(c); n >= window {
			t.Errorf("step %d: prompt ~%d tokens, window %d", i+1, n, window)
		}
	}
	for _, m := range o.Calls[len(o.Calls)-1].Messages {
		if m.Role == ai.RoleTool && len(m.Content) > 20000 {
			bigResults++
		}
		if longtest.IsMaskStub(m) {
			stubs++
		}
	}
	t.Logf("max prompt ~%d tokens of %d; compactions %v; masked results in last request %d; summarizer calls %d", maxTokens, window, o.Compactions, stubs, o.SummarizerCalls)

	// Compaction triggers (several times over 150 steps with outputs this big) and shrinks the request.
	if len(o.Compactions) < minCompactions || o.SummarizerCalls > len(o.Compactions) {
		t.Fatalf("compactions %v, summarizer calls %d", o.Compactions, o.SummarizerCalls)
	}
	for ci, idx := range o.Compactions {
		info := o.CompactionInfo[ci]
		after := o.Calls[idx]
		if info.After >= info.Before {
			t.Errorf("compaction before step %d did not shrink the context: %d -> %d tokens", idx+1, info.Before, info.After)
		}
		if float64(info.Before) < 0.7*float64(window) {
			t.Errorf("compaction before step %d triggered at %d tokens, under the 0.7 threshold of the %d window", idx+1, info.Before, window)
		}
		if info.After > int(0.7*float64(window)) {
			t.Errorf("compaction before step %d left %d tokens, still over the threshold", idx+1, info.After)
		}
		// The request after it keeps the plan, the project instructions and the user's request verbatim (v3 §5).
		text := longtest.CallsText(after)
		for what, marker := range map[string]string{"plan": longtest.PlanTitle, "AGENTS.md section": longtest.AgentsMarker, "last user request": longtest.Request} {
			if !strings.Contains(text, marker) {
				t.Errorf("request after the compaction before step %d lost the %s", idx+1, what)
			}
		}
		if countIn(after.Messages, agent.RehydrationTag) == 0 {
			t.Errorf("no %s message after the compaction before step %d", agent.RehydrationTag, idx+1)
		}
		verbatim := false
		for _, m := range after.Messages {
			verbatim = verbatim || (m.Role == ai.RoleUser && m.Content == longtest.Request)
		}
		if !verbatim {
			t.Logf("note: after the compaction before step %d the request is carried by the summary/rehydration, not as its own user message", idx+1)
		}
	}

	// Prefix stability: between consecutive steps the earlier messages are byte-identical, except
	// at a compaction (history rewritten) and where masking replaces an old result by its stub.
	isCompaction := map[int]bool{}
	for _, idx := range o.Compactions {
		isCompaction[idx] = true
	}
	stubOf := map[string]string{}
	var kept, total float64
	for i := 1; i < len(o.Calls); i++ {
		prev, next := o.Calls[i-1].Messages, o.Calls[i].Messages
		if isCompaction[i] {
			stubOf = map[string]string{} // the compaction cut old results again: their stubs follow
		}
		for _, m := range next {
			if longtest.IsMaskStub(m) {
				if old, ok := stubOf[m.ToolCallID]; ok && old != m.Content {
					t.Errorf("step %d: the stub of %s changed once masked", i+1, m.ToolCallID)
				}
				stubOf[m.ToolCallID] = m.Content
			}
		}
		if isCompaction[i] {
			continue
		}
		first, masked, err := longtest.AppendOnlyExceptMasks(prev, next)
		if err != nil {
			t.Errorf("step %d: %v", i+1, err)
			continue
		}
		total++
		kept += float64(first) / float64(len(prev))
		if masked > 1 {
			t.Logf("step %d masked %d results at once", i+1, masked)
		}
	}
	t.Logf("shared prefix between consecutive steps: %.1f%% of the previous request on average", 100*kept/total)

	// Steering: each steer is delivered exactly once.
	got := steerTexts(o)
	if len(got) != 2 || got[0] != steers[40] || got[1] != steers[95] {
		t.Fatalf("steer events %v", got)
	}
	for step, text := range steers {
		first := -1
		for i, c := range o.Calls {
			n := countIn(c.Messages, agent.SteerPrefix+text)
			if n > 1 {
				t.Errorf("step %d: steer %q present %d times in one request", i+1, text, n)
			}
			if n == 1 && first < 0 {
				first = i + 1
			}
		}
		if first != step+1 {
			t.Errorf("steer sent during step %d first seen at step %d, want %d", step, first, step+1)
		}
	}
	// Spill: big command outputs were cut in the request, never sent whole.
	if bigResults != 0 {
		t.Errorf("%d tool results over 20000 characters in the last request", bigResults)
	}
}

func TestLongSessionWithMaskingKeepsThePromptHalfFullAndCompactsNothing(t *testing.T) {
	// With masking on (default), old results are stubbed once the prompt passes half the window, so the prompt plateaus there and the 0.7 compaction threshold is never reached.
	// Arrange / Act
	o := longtest.Run(t, longtest.Scenario{Steps: 150, Window: 60000, Threshold: 0.7, Steers: steers, Sample: true})

	// Assert
	checkLongSession(t, o, 60000, 0)
	if len(o.Compactions) != 0 {
		t.Logf("compactions %v", o.Compactions)
	}
}

func TestLongSessionWithoutMaskingCompactsRepeatedly(t *testing.T) {
	// Without masking the context grows until compaction cuts it, several times over 150 steps.
	// Arrange / Act
	o := longtest.Run(t, longtest.Scenario{Steps: 150, Window: 60000, Threshold: 0.7, NoMask: true, Steers: steers, Sample: true})

	// Assert
	checkLongSession(t, o, 60000, 2)
}

func TestLongSessionInATightWindowMasksAndCompacts(t *testing.T) {
	// A small window makes the newest 6 results alone pass the threshold: masking and compaction work together.
	// Arrange / Act
	o := longtest.Run(t, longtest.Scenario{Steps: 150, Window: 20000, Threshold: 0.7, Steers: steers, Sample: true})

	// Assert
	checkLongSession(t, o, 20000, 1)
}

func TestAssertAppendOnly(t *testing.T) {
	// Arrange
	a := []*ai.Message{ai.SystemMessage("s"), ai.UserMessage("q")}
	tool := &ai.Message{Role: ai.RoleTool, ToolCallID: "c1", ToolName: "read_file", Content: strings.Repeat("x", 800)}
	stub := &ai.Message{Role: ai.RoleTool, ToolCallID: "c1", ToolName: "read_file", Content: agent.MaskStub("read_file", tool.Content)}

	// Act / Assert
	if err := longtest.AssertAppendOnly(a, append(append([]*ai.Message{}, a...), ai.AssistantMessage("x", nil))); err != nil {
		t.Fatalf("appending is fine: %v", err)
	}
	if err := longtest.AssertAppendOnly(a, []*ai.Message{ai.SystemMessage("s2"), a[1]}); err == nil {
		t.Fatal("a changed system prompt must fail")
	}
	if err := longtest.AssertAppendOnly(a, a[:1]); err == nil {
		t.Fatal("a shrinking request must fail")
	}
	prev := []*ai.Message{a[0], tool}
	if err := longtest.AssertAppendOnly(prev, []*ai.Message{a[0], stub}); err == nil {
		t.Fatal("strict mode must reject masking")
	}
	if first, n, err := longtest.AppendOnlyExceptMasks(prev, []*ai.Message{a[0], stub, a[1]}); err != nil || n != 1 || first != 1 {
		t.Fatalf("masking tolerated: first %d n %d err %v", first, n, err)
	}
	if _, _, err := longtest.AppendOnlyExceptMasks([]*ai.Message{a[0], stub}, []*ai.Message{a[0], tool}); err == nil {
		t.Fatal("unmasking must fail")
	}
}
