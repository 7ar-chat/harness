package compaction_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/compaction"
)

// round is an assistant tool call and its result (a result of size chars).
func round(i, size int) (*ai.Message, *ai.Message) {
	id := fmt.Sprintf("call_%d", i)
	call := ai.AssistantMessage("", []ai.ToolCall{{ID: id, Function: ai.FunctionCall{Name: "read_file", Arguments: fmt.Sprintf(`{"path":"f%d.go"}`, i)}}})
	return call, ai.ToolMessage(strings.Repeat("line of code\n", size/13+1), id, ai.WithToolName("read_file"))
}

// turns is n user turns, each a marked request, one tool round of size chars and an answer.
func turns(n, size int) []*ai.Message {
	var msgs []*ai.Message
	for i := 0; i < n; i++ {
		u := ai.UserMessage(fmt.Sprintf("request %d", i))
		compaction.MarkRequest(u)
		call, result := round(i, size)
		msgs = append(msgs, u, call, result, ai.AssistantMessage(fmt.Sprintf("answer %d", i), nil))
	}
	return msgs
}

func fixedSummary(s string) compaction.Summarizer {
	return func(context.Context, string, string) (string, error) { return s, nil }
}

// requireNoOrphanResult fails when a tool result has no matching call earlier in msgs.
func requireNoOrphanResult(t *testing.T, msgs []*ai.Message) {
	t.Helper()
	calls := map[string]bool{}
	for i, m := range msgs {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = true
		}
		if m.Role == ai.RoleTool && !calls[m.ToolCallID] {
			t.Fatalf("message %d is a result of %s whose call was folded away", i, m.ToolCallID)
		}
	}
}

func TestCompactLeavesAPromptThatFitsAlone(t *testing.T) {
	// Arrange
	msgs := turns(2, 200)
	called := false
	o := compaction.Options{Window: 100000, Previous: "## Goal\nold", Reserved: 50,
		OnStart: func(int) { called = true }, Summarize: fixedSummary("x")}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, o)
	empty, _ := compaction.Compact(context.Background(), nil, compaction.Options{Window: 1, Force: true})

	// Assert
	if err != nil || res.Action != compaction.ActionNone || len(res.Messages) != len(msgs) || res.Summary != "## Goal\nold" {
		t.Fatalf("result %+v err %v", res, err)
	}
	if want := 50 + compaction.EstimateText("## Goal\nold") + compaction.EstimateMessages(msgs); res.TokensBefore != want || res.TokensAfter != want {
		t.Errorf("tokens %d -> %d, want both %d (reserved + summary + messages)", res.TokensBefore, res.TokensAfter, want)
	}
	if called {
		t.Error("OnStart called though the prompt did not change")
	}
	if empty.Action != compaction.ActionNone {
		t.Errorf("an empty conversation was compacted: %+v", empty)
	}
}

func TestCompactTrustsTheObservedPromptSizeOverItsEstimate(t *testing.T) {
	// Arrange: the estimate is far below the threshold, the provider says otherwise.
	msgs := turns(30, 3000)
	o := compaction.Options{Window: 10000, Observed: 9000, Summarize: fixedSummary("## Goal\nS")}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, o)
	o.Observed = 100
	under, _ := compaction.Compact(context.Background(), msgs, o)

	// Assert
	if err != nil || res.TokensBefore != 9000 || res.Action == compaction.ActionNone {
		t.Fatalf("observed 9000 of 10000: %+v err %v", res, err)
	}
	if under.Action != compaction.ActionNone || under.TokensBefore != 100 {
		t.Errorf("observed 100 of 10000: %+v", under)
	}
}

func TestShrinkOnlyCutsToolResultsOlderThanTheLastTwoAssistantTurns(t *testing.T) {
	// Arrange
	msgs := turns(6, 4000)
	before := make([]string, len(msgs))
	for i, m := range msgs {
		before[i] = m.Content
	}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: 8000})

	// Assert
	if err != nil || res.Action != compaction.ActionShrunk || len(res.Messages) != len(msgs) {
		t.Fatalf("result %+v err %v", res.Action, err)
	}
	if res.TokensAfter >= res.TokensBefore {
		t.Errorf("shrinking did not shrink: %d -> %d", res.TokensBefore, res.TokensAfter)
	}
	for i, m := range msgs {
		if m.Content != before[i] {
			t.Fatalf("message %d of the caller's history was edited", i)
		}
	}
	lastTwoAssistants := len(msgs) - 4 // turn 5's call and answer, turn 4's answer: results from 4 on are recent
	for i, m := range res.Messages {
		if m.Role != ai.RoleTool {
			if m != msgs[i] {
				t.Errorf("message %d (%s) was replaced", i, m.Role)
			}
			continue
		}
		short := strings.Contains(m.Content, "characters omitted")
		if i < lastTwoAssistants-2 && !short {
			t.Errorf("old result %d kept whole", i)
		}
		if i >= len(msgs)-4 && short {
			t.Errorf("recent result %d was cut", i)
		}
	}
}

func TestShrinkKeepsSmallResultsAndHeadAndTailOfBigOnes(t *testing.T) {
	// Arrange
	big := "HEAD" + strings.Repeat("x", 5000) + "TAIL"
	var msgs []*ai.Message
	for i, content := range []string{big, "small result"} {
		call, _ := round(i, 0)
		msgs = append(msgs, call, ai.ToolMessage(content, call.ToolCalls[0].ID))
	}
	last, _ := round(9, 0)
	msgs = append(msgs, last, ai.ToolMessage("recent", "call_9"), ai.AssistantMessage("done", nil), ai.AssistantMessage("more", nil))

	// Act
	res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: 1200})

	// Assert
	if err != nil || res.Action != compaction.ActionShrunk {
		t.Fatalf("result %+v err %v", res.Action, err)
	}
	got := res.Messages[1].Content
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") || !strings.Contains(got, "characters omitted") || len([]rune(got)) > compaction.PreviewChars+80 {
		t.Errorf("preview of the big result: %q", got)
	}
	if res.Messages[3].Content != "small result" {
		t.Errorf("a result under the preview size was cut: %q", res.Messages[3].Content)
	}
}

func TestPreviewKeepsAMarkedHeadAndTailWithinTheLimit(t *testing.T) {
	// Act
	short := compaction.Preview("short", 10)
	long := compaction.Preview(strings.Repeat("é", 10)+strings.Repeat("z", 100)+strings.Repeat("ü", 10), 30)

	// Assert
	if short != "short" {
		t.Errorf("text within the limit changed: %q", short)
	}
	if !strings.HasPrefix(long, strings.Repeat("é", 10)) || !strings.HasSuffix(long, strings.Repeat("ü", 10)) || !strings.Contains(long, "[… 90 characters omitted to save context …]") {
		t.Errorf("preview %q", long)
	}
}

func TestAutomaticSummaryKeepsARecentTailAndGetsBackUnderTheThreshold(t *testing.T) {
	// Arrange
	msgs := turns(30, 3000)
	var starts []int
	var gotTranscript string
	o := compaction.Options{Window: 10000, Previous: "## Goal\nbefore",
		OnStart: func(n int) { starts = append(starts, n) },
		Summarize: func(_ context.Context, prev, transcript string) (string, error) {
			gotTranscript = transcript
			if prev != "## Goal\nbefore" {
				t.Errorf("previous summary %q", prev)
			}
			return "## Goal\nnew", nil
		}}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, o)

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized || res.Summary != "## Goal\nnew" {
		t.Fatalf("result %+v err %v", res, err)
	}
	if len(starts) != 1 || starts[0] != res.TokensBefore {
		t.Errorf("OnStart calls %v, want one with the size before (%d)", starts, res.TokensBefore)
	}
	if res.Folded == 0 || res.Folded+len(res.Messages) != len(msgs) {
		t.Errorf("folded %d, kept %d of %d", res.Folded, len(res.Messages), len(msgs))
	}
	if res.Messages[len(res.Messages)-1] != msgs[len(msgs)-1] {
		t.Error("the newest message is not in the tail")
	}
	requireNoOrphanResult(t, res.Messages)
	if limit := int(0.75 * 10000); res.TokensAfter > limit || res.TokensAfter >= res.TokensBefore {
		t.Errorf("tokens %d -> %d, want under %d", res.TokensBefore, res.TokensAfter, limit)
	}
	if !strings.Contains(gotTranscript, "USER: request 0") || strings.Contains(gotTranscript, "USER: request 29") {
		t.Errorf("transcript should hold the folded part only:\n%.300s", gotTranscript)
	}
	if want := compaction.KeepRecent(10000, 0.75); compaction.EstimateMessages(res.Messages) > want+compaction.EstimateMessages(msgs[len(msgs)-4:]) {
		t.Errorf("tail of %d tokens, want about %d", compaction.EstimateMessages(res.Messages), want)
	}
}

func TestKeepRecentIsTheTailShareCappedAtHalfTheThreshold(t *testing.T) {
	// Act
	wide := compaction.KeepRecent(100000, 0.75)
	narrow := compaction.KeepRecent(100000, 0.4)

	// Assert
	if wide != 30000 {
		t.Errorf("tail share of the window: %d, want 30000", wide)
	}
	if narrow != 20000 {
		t.Errorf("half of a 0.4 threshold: %d, want 20000", narrow)
	}
}

func TestInvalidThresholdsFallBackToThreeQuarters(t *testing.T) {
	// Arrange: about 80 % of the window.
	msgs := turns(20, 300)
	window := compaction.EstimateMessages(msgs) * 100 / 80

	for _, th := range []float64{0, -1, 1, 1.5} {
		t.Run(fmt.Sprint(th), func(t *testing.T) {
			// Act
			res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: window, Threshold: th, Summarize: fixedSummary("S")})

			// Assert
			if err != nil || res.Action != compaction.ActionSummarized {
				t.Fatalf("threshold %v: an 80 %% prompt ended %q", th, res.Action)
			}
		})
	}
	res, _ := compaction.Compact(context.Background(), msgs, compaction.Options{Window: window, Threshold: 0.9, Summarize: fixedSummary("S")})
	if res.Action != compaction.ActionNone {
		t.Errorf("threshold 0.9 compacted an 80 %% prompt: %s", res.Action)
	}
}

func TestACutInsideATurnKeepsItsRequestAndSteersVerbatim(t *testing.T) {
	// Arrange: one long turn (a request, 40 tool rounds, a steer after round 10).
	req := ai.UserMessage("migrate the whole repository")
	compaction.MarkRequest(req)
	steer := ai.UserMessage("also keep the tests green")
	compaction.MarkSteer(steer)
	msgs := []*ai.Message{req}
	for i := 0; i < 40; i++ {
		call, result := round(i, 3000)
		msgs = append(msgs, call, result)
		if i == 10 {
			msgs = append(msgs, steer)
		}
	}
	var transcript string
	o := compaction.Options{Window: 10000, Summarize: func(_ context.Context, _, tr string) (string, error) {
		transcript = tr
		return "## Goal\nS", nil
	}}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, o)

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized {
		t.Fatalf("result %+v err %v", res.Action, err)
	}
	if res.Messages[0] != req || res.Messages[1] != steer {
		t.Fatalf("kept messages start with %q / %q, want the request then the steer", compaction.Text(res.Messages[0]), compaction.Text(res.Messages[1]))
	}
	if !compaction.IsRequest(res.Messages[0]) || compaction.IsRequest(res.Messages[1]) {
		t.Error("request/steer marks mixed up")
	}
	if res.Folded+len(res.Messages)-2 != len(msgs)-2 {
		t.Errorf("folded %d with %d kept of %d: the carried request and steer count as kept", res.Folded, len(res.Messages), len(msgs))
	}
	requireNoOrphanResult(t, res.Messages)
	if !strings.Contains(transcript, "ends inside the user's latest request") {
		t.Error("the summarizer was not told the cut falls inside a request in progress")
	}
}

func TestACutBetweenTurnsCarriesNothing(t *testing.T) {
	// Arrange: the tail begins at a request, so no request is carried over.
	var msgs []*ai.Message
	msgs = append(msgs, turns(30, 3000)...)
	var transcripts []string
	o := compaction.Options{Window: 10000, Summarize: func(_ context.Context, _, tr string) (string, error) {
		transcripts = append(transcripts, tr)
		return "S", nil
	}}
	// Unmarked history: nothing identifies a request to carry.
	for _, m := range msgs {
		delete(m.Extra, "compaction.user")
	}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, o)

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized {
		t.Fatalf("result %+v err %v", res.Action, err)
	}
	if res.Folded+len(res.Messages) != len(msgs) {
		t.Errorf("folded %d + kept %d != %d", res.Folded, len(res.Messages), len(msgs))
	}
	if strings.Contains(transcripts[0], "latest request") {
		t.Error("in-progress note added without a request to carry")
	}
}

func TestWhenTheTailIsEverythingButTheCarriedRequestNothingIsFolded(t *testing.T) {
	// Arrange: a huge request followed by a few small messages; the provider reports an overflowing prompt.
	req := ai.UserMessage(strings.Repeat("word ", 4000))
	compaction.MarkRequest(req)
	call, result := round(0, 60)
	msgs := []*ai.Message{req, call, result, ai.AssistantMessage("ok", nil)}
	called := false
	o := compaction.Options{Window: 10000, Observed: 9000, OnStart: func(int) { called = true }, Summarize: fixedSummary("S")}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, o)

	// Assert
	if err != nil || res.Action != compaction.ActionNone || res.Folded != 0 || len(res.Messages) != len(msgs) {
		t.Fatalf("result %+v err %v", res, err)
	}
	if called {
		t.Error("OnStart called though nothing changed")
	}
}

func TestWhenTheWholeHistoryFitsTheTailOnlyShrinkingHappens(t *testing.T) {
	// Arrange: the provider says the prompt is over the threshold, yet our estimate says the whole history is a small tail.
	msgs := turns(4, 4000)
	o := compaction.Options{Window: 100000, Observed: 90000, Summarize: func(context.Context, string, string) (string, error) {
		t.Error("summarized a history that fits the tail")
		return "S", nil
	}}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, o)

	// Assert
	if err != nil || res.Action != compaction.ActionShrunk || res.Folded != 0 {
		t.Fatalf("result %+v err %v", res.Action, err)
	}
	if res.TokensBefore != 90000 || res.TokensAfter >= 90000 {
		t.Errorf("tokens %d -> %d", res.TokensBefore, res.TokensAfter)
	}
}

func TestForceKeepsASmallerTailThanAnAutomaticCompaction(t *testing.T) {
	// Arrange
	msgs := turns(30, 3000)
	o := compaction.Options{Window: 1_000_000, Summarize: fixedSummary("S")}

	// Act
	auto, _ := compaction.Compact(context.Background(), msgs, o)
	o.Force = true
	forced, err := compaction.Compact(context.Background(), msgs, o)

	// Assert
	if err != nil || auto.Action != compaction.ActionNone {
		t.Fatalf("auto %+v err %v", auto.Action, err)
	}
	if forced.Action != compaction.ActionSummarized || forced.Folded == 0 {
		t.Fatalf("forced %+v", forced.Action)
	}
	if got, all := compaction.EstimateMessages(forced.Messages), compaction.EstimateMessages(msgs); got > all/2 {
		t.Errorf("forced tail is %d of %d tokens, want about 30 %%", got, all)
	}
	requireNoOrphanResult(t, forced.Messages)
}

func TestManualFoldsEverythingRegardlessOfSize(t *testing.T) {
	// Arrange
	msgs := turns(2, 100)

	// Act
	res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: 1_000_000, Manual: true, Summarize: fixedSummary("S")})

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized || res.Folded != len(msgs) || len(res.Messages) != 0 {
		t.Fatalf("result %+v err %v", res, err)
	}
}

func TestSummarizerFailureDropsTheHeadAndReportsWhy(t *testing.T) {
	boom := errors.New("model down")
	for name, tc := range map[string]struct {
		sum  compaction.Summarizer
		want string
	}{
		"error":   {func(context.Context, string, string) (string, error) { return "", boom }, "model down"},
		"empty":   {fixedSummary(" \n "), "empty summary"},
		"missing": {nil, "no summarizer"},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			msgs := turns(30, 3000)

			// Act
			res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: 10000, Previous: "## Goal\nkept", Summarize: tc.sum})

			// Assert
			if err != nil {
				t.Fatalf("a summarizer failure is not the caller's error: %v", err)
			}
			if res.Action != compaction.ActionDropped || res.Err == nil || !strings.Contains(res.Err.Error(), tc.want) {
				t.Fatalf("action %s err %v", res.Action, res.Err)
			}
			if name == "error" && !errors.Is(res.Err, boom) {
				t.Errorf("cause lost: %v", res.Err)
			}
			if !strings.HasPrefix(res.Summary, "## Goal\nkept") || !strings.Contains(res.Summary, fmt.Sprintf("%d earlier messages were dropped", res.Folded)) {
				t.Errorf("summary %q should keep the previous one and note the drop", res.Summary)
			}
			if res.Through != 0 || res.Folded == 0 || res.Folded+len(res.Messages) != len(msgs) {
				t.Errorf("through %d folded %d kept %d", res.Through, res.Folded, len(res.Messages))
			}
			if res.TokensAfter >= res.TokensBefore {
				t.Errorf("tokens %d -> %d", res.TokensBefore, res.TokensAfter)
			}
		})
	}
}

func TestACancelledContextIsTheCallersErrorNotADrop(t *testing.T) {
	// Arrange
	ctx, cancel := context.WithCancel(context.Background())
	o := compaction.Options{Window: 10000, Summarize: func(ctx context.Context, _, _ string) (string, error) {
		cancel()
		return "", ctx.Err()
	}}

	// Act
	res, err := compaction.Compact(ctx, turns(30, 3000), o)

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if res.Action != "" || res.Messages != nil {
		t.Errorf("a cancelled compaction returned %+v", res)
	}
}

func TestASummaryLongerThanTheCapIsTruncated(t *testing.T) {
	// Act
	res, err := compaction.Compact(context.Background(), turns(2, 100), compaction.Options{
		Window: 1_000_000, Manual: true, Summarize: fixedSummary(strings.Repeat("é", 9000))})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Summary, strings.Repeat("é", compaction.SummaryMaxChars)+"\n") || !strings.HasSuffix(res.Summary, "[… summary truncated]") ||
		strings.Count(res.Summary, "é") != compaction.SummaryMaxChars {
		t.Errorf("summary of %d runes", len([]rune(res.Summary)))
	}
}

func TestEachSummarizerCallStaysWithinTheBudgetEvenForOneHugeMessage(t *testing.T) {
	// Arrange
	msgs := []*ai.Message{ai.UserMessage("start"), ai.AssistantMessage(strings.Repeat("padding text ", 20000), nil), ai.UserMessage("end")}
	var sizes []int
	o := compaction.Options{Window: 1_000_000, Manual: true, SummaryBudget: 1,
		Summarize: func(_ context.Context, _, tr string) (string, error) {
			sizes = append(sizes, compaction.EstimateText(tr))
			return "S", nil
		}}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, o)

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized {
		t.Fatalf("result %+v err %v", res.Action, err)
	}
	if len(sizes) < 2 {
		t.Fatalf("one call for %d tokens of history", compaction.EstimateMessages(msgs))
	}
	for i, n := range sizes {
		if n > 600 {
			t.Errorf("call %d got a %d token transcript, the budget floor is 500", i, n)
		}
	}
}

func unmarked(msgs []*ai.Message) []*ai.Message {
	for _, m := range msgs {
		delete(m.Extra, "compaction.user")
	}
	return msgs
}

func TestThroughIsThePositionOfTheLastFoldedMessage(t *testing.T) {
	// Arrange: positions in the caller's log are 10, 20, 30, …; nothing is carried.
	msgs := unmarked(turns(30, 3000))
	for i, m := range msgs {
		compaction.SetPosition(m, int64(i+1)*10)
	}
	compaction.SetPosition(msgs[3], 0) // not recorded: the old position stays

	// Act
	res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: 10000, Summarize: fixedSummary("S")})

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized {
		t.Fatalf("result %+v err %v", res.Action, err)
	}
	if want := int64(res.Folded) * 10; res.Through != want {
		t.Errorf("through %d after folding %d messages, want %d", res.Through, res.Folded, want)
	}
	if first := compaction.Position(res.Messages[0]); first != res.Through+10 {
		t.Errorf("first kept message at %d, through %d", first, res.Through)
	}
}

func TestThroughStopsBelowAKeptMessageThatIsOlderThanAFoldedOne(t *testing.T) {
	// Arrange: a message logged late sits in the head with position 5000, while the tail starts at lower positions.
	msgs := unmarked(turns(30, 3000))
	for i, m := range msgs {
		compaction.SetPosition(m, int64(i+1))
	}
	compaction.SetPosition(msgs[0], 5000)

	// Act
	res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: 10000, Summarize: fixedSummary("S")})

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized {
		t.Fatalf("result %+v err %v", res.Action, err)
	}
	first := compaction.Position(res.Messages[0])
	if res.Through != first-1 {
		t.Errorf("through %d: the summary must not claim the kept message at %d", res.Through, first)
	}
}

func TestACarriedRequestStaysInThePromptButTheSummaryStillCoversIt(t *testing.T) {
	// Arrange
	req := ai.UserMessage("long running task")
	compaction.MarkRequest(req)
	compaction.SetPosition(req, 5)
	msgs := []*ai.Message{req}
	for i := 0; i < 40; i++ {
		call, result := round(i, 3000)
		compaction.SetPosition(call, int64(100+2*i))
		compaction.SetPosition(result, int64(101+2*i))
		msgs = append(msgs, call, result)
	}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: 10000, Summarize: fixedSummary("S")})

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized || res.Messages[0] != req {
		t.Fatalf("result %+v err %v", res.Action, err)
	}
	if want := compaction.Position(res.Messages[1]) - 1; res.Through != want {
		t.Errorf("through %d, want %d (just below the first kept tail message)", res.Through, want)
	}
}

func TestSummaryAppendsTheFilesWrittenKeepingEarlierOnesAndDroppingACopiedList(t *testing.T) {
	// Arrange
	write := func(id, name, args string) []*ai.Message {
		return []*ai.Message{
			ai.AssistantMessage("", []ai.ToolCall{{ID: id, Function: ai.FunctionCall{Name: name, Arguments: args}}}),
			ai.ToolMessage("ok", id),
		}
	}
	var msgs []*ai.Message
	msgs = append(msgs, ai.UserMessage("go"))
	msgs = append(msgs, write("1", "write_file", `{"path":"/w/new.go"}`)...)
	msgs = append(msgs, write("2", "edit_file", `{"path":" /w/old.go "}`)...)
	msgs = append(msgs, write("3", "write_file", `{"path":"/w/new.go"}`)...) // duplicate
	msgs = append(msgs, write("4", "read_file", `{"path":"/w/read.go"}`)...) // not a writer
	msgs = append(msgs, write("5", "write_file", `not json`)...)             // ignored
	msgs = append(msgs, write("6", "write_file", `{"path":"  "}`)...)        // blank
	previous := "## Goal\nold\n\n" + compaction.DefaultFilesHeading + "\n- /w/old.go\n- /w/first.go\n\nTrailing paragraph"
	copied := "## Goal\nnew\n\n" + compaction.DefaultFilesHeading + "\n- /w/stale.go"

	// Act
	res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: 100000, Manual: true, Previous: previous, Summarize: fixedSummary(copied)})

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized {
		t.Fatalf("result %+v err %v", res.Action, err)
	}
	want := "## Goal\nnew\n\n" + compaction.DefaultFilesHeading + "\n- /w/old.go\n- /w/first.go\n- /w/new.go"
	if res.Summary != want {
		t.Errorf("summary\n got %q\nwant %q", res.Summary, want)
	}
}

func TestFileToolsAndHeadingAreConfigurable(t *testing.T) {
	// Arrange
	msgs := []*ai.Message{
		ai.UserMessage("go"),
		ai.AssistantMessage("", []ai.ToolCall{
			{ID: "1", Function: ai.FunctionCall{Name: "save", Arguments: `{"path":"/w/saved.txt"}`}},
			{ID: "2", Function: ai.FunctionCall{Name: "write_file", Arguments: `{"path":"/w/default.txt"}`}},
		}),
	}
	run := func(o compaction.Options) string {
		o.Window, o.Manual, o.Summarize = 100000, true, fixedSummary("S")
		res, err := compaction.Compact(context.Background(), msgs, o)
		if err != nil {
			t.Fatal(err)
		}
		return res.Summary
	}

	// Act
	custom := run(compaction.Options{FileTools: []string{"save"}, FilesHeading: "Fichiers:"})
	again := run(compaction.Options{FileTools: []string{"save"}, FilesHeading: "Fichiers:", Previous: custom})
	none := run(compaction.Options{FileTools: []string{}})

	// Assert
	if custom != "S\n\nFichiers:\n- /w/saved.txt" {
		t.Errorf("custom tools: %q", custom)
	}
	if again != custom {
		t.Errorf("compacting again duplicated the list: %q", again)
	}
	if none != "S" {
		t.Errorf("an empty FileTools list still listed files: %q", none)
	}
}

func TestOnlyTheNewestFortyFilesAreListed(t *testing.T) {
	// Arrange
	var calls []ai.ToolCall
	for i := 0; i < 55; i++ {
		calls = append(calls, ai.ToolCall{ID: fmt.Sprint(i), Function: ai.FunctionCall{Name: "write_file", Arguments: fmt.Sprintf(`{"path":"/w/f%02d"}`, i)}})
	}
	msgs := []*ai.Message{ai.UserMessage("go"), ai.AssistantMessage("", calls)}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: 100000, Manual: true, Summarize: fixedSummary("S")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(res.Summary, "\n- ") != 40 || strings.Contains(res.Summary, "/w/f14\n") || !strings.Contains(res.Summary, "- /w/f15\n") || !strings.HasSuffix(res.Summary, "- /w/f54") {
		t.Errorf("summary %q", res.Summary)
	}
}

func TestMarksAndPositionsShareExtraWithoutOverwritingIt(t *testing.T) {
	// Arrange
	m := ai.UserMessage("x")
	m.Extra = map[string]any{"app": "kept"}
	plain := ai.UserMessage("y")

	// Act
	compaction.MarkSteer(m)
	steerIsRequest := compaction.IsRequest(m)
	compaction.MarkRequest(m)
	compaction.SetPosition(plain, 0)

	// Assert
	if steerIsRequest || !compaction.IsRequest(m) || m.Extra["app"] != "kept" {
		t.Errorf("extra %+v", m.Extra)
	}
	if compaction.IsRequest(plain) || plain.Extra != nil || compaction.Position(plain) != 0 || compaction.Files(plain) != nil {
		t.Errorf("an unmarked message has state: %+v", plain.Extra)
	}
}

func TestTextJoinsContentAndTextPartsAndImagesCountSeparately(t *testing.T) {
	// Arrange
	m := &ai.Message{Role: ai.RoleUser, Content: "lead", Parts: []ai.Part{
		{Type: ai.PartText, Text: "one"}, {Type: ai.PartText}, {Type: ai.PartImage, Image: &ai.Image{URL: "u"}}, {Type: ai.PartText, Text: "two"}}}
	noLead := &ai.Message{Role: ai.RoleUser, Parts: []ai.Part{{Type: ai.PartText, Text: "only"}}}

	// Act / Assert
	if got := compaction.Text(m); got != "lead\none\ntwo" {
		t.Errorf("text %q", got)
	}
	if got := compaction.Text(noLead); got != "only" {
		t.Errorf("text %q", got)
	}
	if compaction.Images(m) != 1 || compaction.Images(ai.UserMessage("x")) != 0 {
		t.Errorf("images %d", compaction.Images(m))
	}
}

func TestTranscriptRendersEachKindOfMessageForTheSummarizer(t *testing.T) {
	// Arrange
	longArgs := `{"content":"` + strings.Repeat("a", 1000) + `"}`
	msgs := []*ai.Message{
		ai.SystemMessage("never shown"),
		imageMessage("look", 3),
		ai.AssistantMessage("   ", []ai.ToolCall{{ID: "c1", Function: ai.FunctionCall{Name: "write_file", Arguments: longArgs}}}),
		ai.ToolMessage(strings.Repeat("r", 3000), "c1"),
		ai.ToolMessage("orphan output", "gone", ai.WithToolName("fallback_tool")),
		ai.AssistantMessage("final words", nil),
	}

	// Act
	got := compaction.Transcript(msgs)

	// Assert
	for _, want := range []string{
		"USER: look [3 images attached]",
		"TOOL CALL write_file: {\"content\":\"aaa",
		"TOOL RESULT write_file:\n<untrusted_tool_output>\nrrr",
		"TOOL RESULT fallback_tool:\n<untrusted_tool_output>\norphan output\n</untrusted_tool_output>",
		"ASSISTANT: final words",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"never shown", "ASSISTANT:    ", strings.Repeat("a", 700), strings.Repeat("r", 1300)} {
		if strings.Contains(got, bad) {
			t.Errorf("transcript contains %.40q…", bad)
		}
	}
	if !strings.Contains(got, "…\n\n") || !strings.Contains(got, "characters omitted") {
		t.Errorf("clipped arguments and results are not marked:\n%.400s", got)
	}
}

func TestTranscriptWithinLeavesOutTheOldestMessagesAndSaysHowMany(t *testing.T) {
	// Arrange
	var msgs []*ai.Message
	for i := 0; i < 50; i++ {
		msgs = append(msgs, ai.UserMessage(fmt.Sprintf("message-%02d %s", i, strings.Repeat("filler words ", 40))))
	}

	// Act
	got := compaction.TranscriptWithin(msgs, 1000)

	// Assert
	if n := compaction.EstimateText(got); n > 1000 {
		t.Errorf("%d tokens for a 1000 token budget", n)
	}
	kept := strings.Count(got, "USER: message-")
	var omitted int
	if _, err := fmt.Sscanf(got, "[… %d earlier messages omitted to fit the summarizer …]", &omitted); err != nil {
		t.Fatalf("no omission marker: %.100s", got)
	}
	if kept == 0 || kept+omitted != 50 || !strings.Contains(got, "message-49") || strings.Contains(got, "message-00") {
		t.Errorf("kept %d, omitted %d", kept, omitted)
	}
	if all := compaction.TranscriptWithin(msgs[:2], 0); strings.Contains(all, "omitted") || strings.Count(all, "USER:") != 2 {
		t.Errorf("a transcript that fits was cut: %.100s", all)
	}
}

func TestTranscriptWithinClipsANewestMessageThatAloneIsTooBig(t *testing.T) {
	// Arrange
	msgs := []*ai.Message{ai.UserMessage("old"), ai.UserMessage("HEAD " + strings.Repeat("big ", 20000) + " TAIL")}

	// Act
	got := compaction.TranscriptWithin(msgs, 400)

	// Assert
	if n := compaction.EstimateText(got); n > 400 {
		t.Errorf("%d tokens for a 400 token budget", n)
	}
	if !strings.HasPrefix(got, "[… 1 earlier messages omitted") || !strings.Contains(got, "USER: HEAD") || !strings.HasSuffix(got, "TAIL") || !strings.Contains(got, "characters omitted") {
		t.Errorf("transcript %.300s … %.60s", got, got[max(0, len(got)-60):])
	}
}

func TestTranscriptDefaultCapKeepsTheNewestHundredTwentyThousandCharacters(t *testing.T) {
	// Arrange
	var msgs []*ai.Message
	for i := 0; i < 400; i++ {
		msgs = append(msgs, ai.UserMessage(fmt.Sprintf("m%03d %s", i, strings.Repeat("z", 995))))
	}

	// Act
	got := compaction.Transcript(msgs)

	// Assert
	if len(got) > 125_000 || len(got) < 100_000 || !strings.Contains(got, "m399") || strings.Contains(got, "m000") || !strings.HasPrefix(got, "[… ") {
		t.Errorf("transcript of %d bytes", len(got))
	}
}

func TestEstimateToolsGrowsWithTheDefinitions(t *testing.T) {
	// Arrange
	small := []*ai.ToolInfo{{Name: "ls", Desc: "list"}}
	big := []*ai.ToolInfo{{Name: "ls", Desc: strings.Repeat("list the files in a directory ", 50)}, {Name: "cat", Desc: "read"}}

	// Act
	none, one, many := compaction.EstimateTools(nil), compaction.EstimateTools(small), compaction.EstimateTools(big)

	// Assert
	if !(none < one && one+300 < many) {
		t.Errorf("estimates %d < %d < %d", none, one, many)
	}
}

func TestEstimateTreatsControlCharactersAndUnicodeSpacesAsSeparators(t *testing.T) {
	// Act
	control := compaction.EstimateText("a\x01b")
	nbsp := compaction.EstimateText("a\u00a0b")
	nbspRun := compaction.EstimateText("a\u00a0\u00a0\u00a0b")
	plain := compaction.EstimateText("a b")
	run := compaction.EstimateText("a   b")

	// Assert
	if control != 2 || nbsp != 2 || plain != 2 {
		t.Errorf("single separators cost nothing: control %d nbsp %d plain %d", control, nbsp, plain)
	}
	if nbspRun != 3 || run != 3 {
		t.Errorf("a run of separators costs one token: nbsp run %d, spaces %d", nbspRun, run)
	}
}

func TestEstimateRatesDifferByScript(t *testing.T) {
	// Act
	cyrillic := compaction.EstimateText(strings.Repeat("п", 400))
	greek := compaction.EstimateText(strings.Repeat("α", 245))
	arabic := compaction.EstimateText(strings.Repeat("ب", 250))
	digits := compaction.EstimateText(strings.Repeat("7", 300))
	punct := compaction.EstimateText(strings.Repeat("#", 300))
	emoji := compaction.EstimateText(strings.Repeat("😀", 100))

	// Assert
	for name, tc := range map[string][3]int{
		"cyrillic 4 letters per token":  {cyrillic, 95, 105},
		"greek 2.45 letters per token":  {greek, 95, 105},
		"other alphabets 2.5 per token": {arabic, 95, 105},
		"digits 3 per token":            {digits, 95, 105},
		"punctuation 3 per token":       {punct, 95, 105},
		"emoji 1.9 tokens each":         {emoji, 185, 195},
	} {
		if tc[0] < tc[1] || tc[0] > tc[2] {
			t.Errorf("%s: %d, want %d–%d", name, tc[0], tc[1], tc[2])
		}
	}
}
