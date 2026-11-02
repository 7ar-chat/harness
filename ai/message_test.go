package ai_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
)

func TestConcatMessagesRefusesChunksOfDifferentNamesToolCallIDsAndToolNames(t *testing.T) {
	for name, chunks := range map[string][]*ai.Message{
		"name":         {{Name: "a"}, {Name: "b"}},
		"tool call id": {{ToolCallID: "x"}, {ToolCallID: "y"}},
		"tool name":    {{ToolName: "read"}, {ToolName: "write"}},
		"call type":    {{ToolCalls: []ai.ToolCall{{Index: idx(0), Type: "function"}}}, {ToolCalls: []ai.ToolCall{{Index: idx(0), Type: "custom"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			// Act
			got, err := ai.ConcatMessages(chunks)

			// Assert
			if err == nil {
				t.Fatalf("merged chunks that disagree: %+v", got)
			}
		})
	}
}

func TestConcatMessagesLetsAnEmptyIdentityDeferToTheOneThatIsSet(t *testing.T) {
	// Act
	got, err := ai.ConcatMessages([]*ai.Message{
		{Role: ai.RoleTool, Name: "n"},
		{ToolCallID: "c1", ToolName: "read"},
		{Name: "n", ToolCallID: "c1"},
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != ai.RoleTool || got.Name != "n" || got.ToolCallID != "c1" || got.ToolName != "read" {
		t.Fatalf("merged %+v", got)
	}
}

func TestConcatMessagesPutsWholeCallsBeforeTheStreamedOnesInIndexOrder(t *testing.T) {
	// Arrange
	whole := func(id string) ai.ToolCall {
		return ai.ToolCall{ID: id, Type: "function", Function: ai.FunctionCall{Name: id, Arguments: "{}"}}
	}

	// Act
	got, err := ai.ConcatMessages([]*ai.Message{
		{ToolCalls: []ai.ToolCall{{Index: idx(2), ID: "c", Type: "function", Function: ai.FunctionCall{Name: "c"}}}},
		{ToolCalls: []ai.ToolCall{whole("w1")}},
		{ToolCalls: []ai.ToolCall{{Index: idx(0), ID: "a", Type: "function", Function: ai.FunctionCall{Name: "a"}}}},
		{ToolCalls: []ai.ToolCall{whole("w2")}},
		{ToolCalls: []ai.ToolCall{{Index: idx(2), Function: ai.FunctionCall{Arguments: `{"k":1}`}}}},
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range got.ToolCalls {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "w1,w2,a,c" {
		t.Fatalf("order %v, want whole calls in arrival order, then indexed by index", ids)
	}
	if c := got.ToolCalls[3]; c.Function.Arguments != `{"k":1}` || *c.Index != 2 {
		t.Errorf("joined call %+v", c)
	}
}

func TestConcatMessagesMergesAdjacentTextPartsAndKeepsImagesInPlace(t *testing.T) {
	// Arrange
	img := ai.Part{Type: ai.PartImage, Image: &ai.Image{URL: "u"}}
	text := func(s string) ai.Part { return ai.Part{Type: ai.PartText, Text: s} }

	// Act
	got, err := ai.ConcatMessages([]*ai.Message{
		{Parts: []ai.Part{text("a"), text("b")}},
		{Parts: []ai.Part{img, text("c")}},
		{Parts: []ai.Part{text("d"), img}},
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Parts) != 4 {
		t.Fatalf("parts %+v", got.Parts)
	}
	wantText := []string{"ab", "", "cd", ""}
	for i, p := range got.Parts {
		isImg := i == 1 || i == 3
		if isImg != (p.Type == ai.PartImage) || p.Text != wantText[i] {
			t.Errorf("part %d = %+v", i, p)
		}
	}
	if got, _ := ai.ConcatMessages([]*ai.Message{{Role: ai.RoleUser}}); got.Parts != nil {
		t.Errorf("parts of a message without any: %v", got.Parts)
	}
}

func TestConcatMessagesTakesUsageFromWhicheverChunkHasIt(t *testing.T) {
	// Act
	got, err := ai.ConcatMessages([]*ai.Message{
		{ResponseMeta: &ai.ResponseMeta{FinishReason: "stop"}},
		{ResponseMeta: &ai.ResponseMeta{Usage: &ai.TokenUsage{
			PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7,
			PromptTokenDetails:     ai.PromptTokenDetails{CachedTokens: 1, CacheWriteTokens: 2},
			CompletionTokenDetails: ai.CompletionTokenDetails{ReasoningTokens: 5},
		}}},
		{ResponseMeta: &ai.ResponseMeta{FinishReason: ""}}, // an empty reason does not erase the earlier one
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	u := got.ResponseMeta.Usage
	if got.ResponseMeta.FinishReason != "stop" || u.TotalTokens != 7 || u.PromptTokenDetails.CacheWriteTokens != 2 || u.CompletionTokenDetails.ReasoningTokens != 5 {
		t.Fatalf("meta %+v usage %+v", got.ResponseMeta, u)
	}
}

func TestConcatMessagesNamesTheChunkThatIsNil(t *testing.T) {
	// Act
	_, err := ai.ConcatMessages([]*ai.Message{{Role: ai.RoleAssistant}, {Role: ai.RoleAssistant}, nil})

	// Assert
	if err == nil || !strings.Contains(err.Error(), "index: 2") {
		t.Fatalf("err %v", err)
	}
}

func TestMessageConstructorsSetRoleAndCarryTheirArguments(t *testing.T) {
	// Arrange
	calls := []ai.ToolCall{{ID: "c", Function: ai.FunctionCall{Name: "f"}}}

	// Act
	sys, usr := ai.SystemMessage("s"), ai.UserMessage("u")
	asst := ai.AssistantMessage("a", calls)
	res := ai.ToolMessage("out", "c", ai.WithToolName("f"))
	plain := ai.ToolMessage("out", "c")

	// Assert
	if sys.Role != ai.RoleSystem || sys.Content != "s" || usr.Role != ai.RoleUser || usr.Content != "u" {
		t.Errorf("system %+v user %+v", sys, usr)
	}
	if asst.Role != ai.RoleAssistant || asst.Content != "a" || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "c" {
		t.Errorf("assistant %+v", asst)
	}
	if res.Role != ai.RoleTool || res.Content != "out" || res.ToolCallID != "c" || res.ToolName != "f" || plain.ToolName != "" {
		t.Errorf("tool results %+v %+v", res, plain)
	}
}

func TestApplyCollectsEveryOptionAndSkipsNilOnes(t *testing.T) {
	// Act
	o := ai.Apply(
		ai.WithTemperature(0.5), nil, ai.WithTopP(0.9), ai.WithMaxTokens(100),
		ai.WithReasoningEffort("high"), ai.WithPromptCacheKey("k"), ai.WithToolChoice(ai.ToolChoiceForced, "a", "b"),
	)
	empty := ai.Apply()
	later := ai.Apply(ai.WithMaxTokens(1), ai.WithMaxTokens(2))

	// Assert
	if *o.Temperature != 0.5 || *o.TopP != 0.9 || *o.MaxTokens != 100 || o.ReasoningEffort != "high" || o.PromptCacheKey != "k" {
		t.Errorf("options %+v", o)
	}
	if *o.ToolChoice != ai.ToolChoiceForced || len(o.AllowedToolNames) != 2 || o.AllowedToolNames[1] != "b" {
		t.Errorf("tool choice %v %v", *o.ToolChoice, o.AllowedToolNames)
	}
	if empty.Temperature != nil || empty.TopP != nil || empty.MaxTokens != nil || empty.ToolChoice != nil {
		t.Errorf("no options set but %+v", empty)
	}
	if *later.MaxTokens != 2 {
		t.Errorf("last option wins, got %d", *later.MaxTokens)
	}
}

func TestSendToAClosedStreamReportsItWithoutBlocking(t *testing.T) {
	// Arrange: an unbuffered stream nobody reads.
	sr, sw := ai.Pipe[int](0)
	result := make(chan bool)
	go func() { result <- sw.Send(1, nil) }()

	// Act
	sr.Close()

	// Assert
	if !<-result {
		t.Fatal("a send blocked on a reader that went away did not report closed")
	}
}

func TestSendDeliversAnErrorToTheReaderAndNegativeCapacityIsUnbuffered(t *testing.T) {
	// Arrange
	boom := errors.New("boom")
	sr, sw := ai.Pipe[int](-3)
	go func() {
		defer sw.Close()
		sw.Send(7, nil)
		sw.Send(0, boom)
	}()

	// Act
	v, err1 := sr.Recv()
	_, err2 := sr.Recv()
	sr.Close()

	// Assert
	if v != 7 || err1 != nil || !errors.Is(err2, boom) {
		t.Fatalf("got %d %v %v", v, err1, err2)
	}
}

type ptrArgs struct {
	N int `json:"n"`
}

func TestInferToolAcceptsPointerInputs(t *testing.T) {
	// Arrange
	tl, err := ai.InferTool("double", "d", func(_ context.Context, in *ptrArgs) (int, error) { return in.N * 2, nil })
	if err != nil {
		t.Fatal(err)
	}

	// Act
	out, runErr := tl.InvokableRun(context.Background(), `{"n":21}`)

	// Assert
	if runErr != nil || out != "42" {
		t.Fatalf("out %q err %v", out, runErr)
	}
}

func TestInferToolReportsAResultThatCannotBeEncoded(t *testing.T) {
	// Arrange
	tl, _ := ai.InferTool("bad", "d", func(_ context.Context, _ ptrArgs) (chan int, error) { return make(chan int), nil })

	// Act
	_, err := tl.InvokableRun(context.Background(), `{}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "failed to marshal output") || !strings.Contains(err.Error(), "toolName=bad") {
		t.Fatalf("err %v", err)
	}
}
