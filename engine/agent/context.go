package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/compaction"
)

// Harness v3 §5: in-run masking of old tool results (A-24).
const (
	defaultKeepRecentToolResults = 6
	// maskMinChars: shorter results are kept (a stub would not save anything).
	maskMinChars = 600
)

// unmaskedTools keep their results in the model input: skill instructions, the
// user's answers and plan confirmations stay visible for the whole run.
var unmaskedTools = map[string]bool{"load_skill": true, "ask_user": true, "update_plan": true}

var (
	spillPathRe = regexp.MustCompile(`/workspace/\.work/tool-output/[^\s"'\]:]+`)
	exitCodeRe  = regexp.MustCompile(`"exitCode"\s*:\s*(-?\d+)`)
)

// requestMessages is the model input of a step (harness v3 §5, cache-stable):
// the system prompt (static first, dynamic last), then the history, then the
// run's tail reminder (Harness.Tail), where tool
// results older than the newest KeepRecentToolResults are masked (in batches)
// and images beyond 6 / 6 MiB are stubbed. The stored history is never
// changed, and the masking of a result never changes again once applied, so
// the prompt prefix stays byte-identical between steps until the next batch.
func (st *runState) requestMessages() []*ai.Message {
	out := make([]*ai.Message, 0, len(st.history)+2)
	out = append(out, st.system())
	if st.h.rehydration != nil {
		out = append(out, st.h.rehydration)
	}
	out = capImages(append(out, st.masked(st.history)...))
	if st.h.tail != "" {
		out = append(out, ai.UserMessage(st.h.tail)) // per-run facts after the cacheable prefix
	}
	return out
}

// MaskSetting maps COMPACTION_KEEP_RECENT_TOOL_RESULTS to Harness.KeepRecentToolResults (0: no masking).
func MaskSetting(n int) int {
	if n <= 0 {
		return -1
	}
	return n
}

func (st *runState) keepRecentToolResults() int {
	switch k := st.a.Harness.KeepRecentToolResults; {
	case k < 0:
		return -1
	case k == 0:
		return defaultKeepRecentToolResults
	default:
		return k
	}
}

// maskBatch is how many tool results the masking boundary moves at once in
// the always-on mode (MaskFromFraction < 0): between two moves the masked
// prefix of the request is byte-identical, so the provider's prompt cache keeps hitting.
const maskBatch = 4

// defaultMaskFromFraction: masking starts once the prompt passes half the window.
const defaultMaskFromFraction = 0.5

// MaskFromSetting maps COMPACTION_MASK_FROM_FRACTION to Harness.MaskFromFraction
// (0: mask from the first step, as in v3).
func MaskFromSetting(f float64) float64 {
	if f <= 0 {
		return -1
	}
	return f
}

func (st *runState) maskFrom() float64 {
	switch f := st.a.Harness.MaskFromFraction; {
	case f < 0:
		return -1
	case f == 0 || f >= 1:
		return defaultMaskFromFraction
	default:
		return f
	}
}

// promptTokens estimates the next request: the size the provider reported for
// the previous call plus the messages added since, else an estimate of it all.
func (st *runState) promptTokens() int {
	if n := st.observed(); n > 0 {
		return n
	}
	return st.h.reserved + st.rehydrationTokens() + compaction.EstimateText(st.h.tail) + compaction.EstimateMessages(st.history)
}

// masked returns history with old tool results replaced by stubs (copies).
// Round 3 (A1): masking only starts under context pressure — when the next
// prompt passes MaskFromFraction × window, every result but the newest keep
// is masked in one batch, and stays masked for the rest of the run (by tool
// call id, so a compaction does not move it); below the fraction nothing new
// is masked and the request prefix is byte-identical from step to step. With
// MaskFromFraction < 0 (v3): of the n results the oldest ⌊(n − keep) / batch⌋
// × batch are masked (batch = min(4, keep)).
func (st *runState) masked(history []*ai.Message) []*ai.Message {
	keep := st.keepRecentToolResults()
	if keep < 0 {
		return history
	}
	var results []int
	for i, m := range history {
		if m.Role == ai.RoleTool {
			results = append(results, i)
		}
	}
	var hide func(i int) bool
	if from := st.maskFrom(); from < 0 {
		batch := max(1, min(maskBatch, keep))
		old := 0
		if len(results) > keep {
			old = (len(results) - keep) / batch * batch
		}
		if old == 0 {
			return history
		}
		hide = func(i int) bool { return i < results[old-1]+1 }
	} else {
		if len(results) > keep && float64(st.promptTokens()) >= from*float64(st.h.window) {
			if st.h.maskedCalls == nil {
				st.h.maskedCalls = map[string]bool{}
			}
			for _, i := range results[:len(results)-keep] {
				st.h.maskedCalls[history[i].ToolCallID] = true
			}
		}
		if len(st.h.maskedCalls) == 0 {
			return history
		}
		hide = func(i int) bool { return st.h.maskedCalls[history[i].ToolCallID] }
	}
	out := make([]*ai.Message, len(history))
	copy(out, history)
	for _, i := range results {
		m := out[i]
		if !hide(i) || unmaskedTools[m.ToolName] || utf8.RuneCountInString(m.Content) < maskMinChars {
			continue
		}
		cp := *m
		cp.Content = MaskStub(m.ToolName, m.Content)
		out[i] = &cp
	}
	return out
}

// MaskStub is the model's view of a masked tool result:
// "[<tool> result, <size>, exit <code> — hidden to save context; full output in <outputPath>]"
// (exit and path only when the result reports them). It never suggests
// running the call again: commands are not always idempotent.
func MaskStub(tool, content string) string {
	if tool == "" {
		tool = "tool"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s result, %s", tool, humanSize(len(content)))
	var fields struct {
		ExitCode   *int   `json:"exitCode"`
		OutputPath string `json:"outputPath"`
	}
	_ = json.Unmarshal([]byte(content), &fields)
	if fields.ExitCode != nil {
		fmt.Fprintf(&b, ", exit %d", *fields.ExitCode)
	} else if m := exitCodeRe.FindAllStringSubmatch(content, -1); len(m) > 0 {
		// Not a JSON object any more (a spilled or cut output): the last exitCode field.
		fmt.Fprintf(&b, ", exit %s", m[len(m)-1][1])
	}
	path := fields.OutputPath
	if path == "" {
		path = spillPathRe.FindString(content)
	}
	if path != "" {
		fmt.Fprintf(&b, " — hidden to save context; full output in %s]", path)
	} else {
		b.WriteString(" — hidden to save context]")
	}
	return b.String()
}

func humanSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
