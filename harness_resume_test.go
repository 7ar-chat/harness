package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/queue"
)

// interruptsOf are the interrupts of the terminal event of a run.
func interruptsOf(t *testing.T, evs []store.Event) []agui.Interrupt {
	t.Helper()
	var ev struct {
		Outcome struct {
			Type       string           `json:"type"`
			Interrupts []agui.Interrupt `json:"interrupts"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(evs[len(evs)-1].Data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Outcome.Type != "interrupt" {
		t.Fatalf("run ended with outcome %q, want interrupt", ev.Outcome.Type)
	}
	return ev.Outcome.Interrupts
}

// askingModel asks one question (ask_user) or runs one command (execute_command); once a tool result is in the
// history it answers "answered: <tool result>".
func askingModel(tool, args string) *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		if last := c.Messages[len(c.Messages)-1]; last.Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("answered: " + last.Content)}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "t1", tool, args), "tool_calls")}}
	})
}

const askArgs = `{"question":"Which database?","options":[{"label":"Postgres"},{"label":"SQLite"}]}`

func TestResumeAnswersAnAskUserQuestionWithTheModeOfTheRequest(t *testing.T) {
	// Arrange
	model := askingModel("ask_user", askArgs)
	h, ctx := facade(t, nil, model, nil)
	th, _ := h.NewThread(ctx, "alice", "")
	asked := exchange(ctx, t, h, "alice", th.ID, "set up storage", harness.RunOptions{})
	intr := interruptsOf(t, waitRun(ctx, t, h, asked))[0]

	// Act
	resumed, err := h.Resume(ctx, "alice", th.ID, harness.ResumeRequest{
		InterruptID: intr.ID, Reason: intr.Reason, Text: "SQLite", Model: "gpt-test", Interactive: true, Mode: harness.ModePlan, ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitRun(ctx, t, h, resumed)

	// Assert: the answer reached the model as the tool result, and the run kept the options
	var answered bool
	for _, c := range model.Calls() {
		if last := c.Messages[len(c.Messages)-1]; last.Role == ai.RoleTool && last.Content == "SQLite" {
			answered = true
		}
	}
	if !answered {
		t.Fatal("the model never saw the answer as the result of ask_user")
	}
	got, _ := h.Store.GetRun(ctx, resumed.ID)
	if got.InteractionMode != store.InteractionInteractive || got.Mode != store.ModePlan || got.ReasoningEffort != "low" || got.Model != "gpt-test" {
		t.Fatalf("resumed run %+v", got)
	}
}

func TestResumeRejectsAnInvalidRequestBeforeTouchingTheThread(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, askingModel("ask_user", askArgs), nil)
	th, _ := h.NewThread(ctx, "alice", "")
	asked := exchange(ctx, t, h, "alice", th.ID, "set up storage", harness.RunOptions{})
	intr := interruptsOf(t, waitRun(ctx, t, h, asked))[0]
	cases := []struct {
		name string
		req  harness.ResumeRequest
	}{
		{"no model", harness.ResumeRequest{InterruptID: intr.ID, Reason: intr.Reason, Text: "x"}},
		{"blank answer", harness.ResumeRequest{InterruptID: intr.ID, Reason: intr.Reason, Text: "  \n", Model: "gpt-test"}},
		{"unknown mode", harness.ResumeRequest{InterruptID: intr.ID, Reason: intr.Reason, Text: "x", Model: "gpt-test", Mode: "turbo"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			run, err := h.Resume(ctx, "alice", th.ID, c.req)

			// Assert
			if run != nil || !errors.Is(err, harness.ErrInvalidRequest) {
				t.Fatalf("run %v err %v", run, err)
			}
		})
	}
	// The question is still open: a valid answer works afterwards.
	if _, err := h.Resume(ctx, "alice", th.ID, harness.ResumeRequest{InterruptID: intr.ID, Reason: intr.Reason, Text: "SQLite", Model: "gpt-test"}); err != nil {
		t.Fatalf("a valid resume after the refused ones: %v", err)
	}
}

func TestResumeOfAnInterruptTheThreadDoesNotHaveFails(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), nil)
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	_, err := h.Resume(ctx, "alice", th.ID, harness.ResumeRequest{InterruptID: "nope", Reason: "ask_user", Text: "x", Model: "gpt-test"})

	// Assert
	if !errors.Is(err, store.ErrNoPendingQuestion) {
		t.Fatalf("err %v", err)
	}
}

func TestResumeReportsAQueueThatRefusesTheRun(t *testing.T) {
	// Arrange: a question is pending on the thread, the queue is down
	h := buildFacade(t, nil, echoModel(), func(o *harness.Options) {
		o.Infra = &harness.Infra{Broker: failingBroker{queue.NewMemory()}}
	})
	ctx := context.Background()
	th, _ := h.NewThread(ctx, "alice", "")
	if err := h.Store.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "i1", ToolCallID: "c1", Question: "?"}); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := h.Resume(ctx, "alice", th.ID, harness.ResumeRequest{InterruptID: "i1", Reason: "ask_user", Text: "yes", Model: "gpt-test"})

	// Assert
	if err == nil || !strings.Contains(err.Error(), "queue down") {
		t.Fatalf("err %v", err)
	}
}

func TestResumeDecidesAToolApproval(t *testing.T) {
	// Arrange: an interactive run is paused before execute_command
	cases := []struct {
		name       string
		req        harness.ResumeRequest
		wantResult string
		wantTools  []string
	}{
		{"deny", harness.ResumeRequest{Decision: store.DecisionDeny}, store.ApprovalDeniedResult, nil},
		{"respond", harness.ResumeRequest{Decision: store.DecisionRespond, Text: "use make instead"}, "free text: use make instead", nil},
		{"approve always", harness.ResumeRequest{Decision: store.DecisionApprove, Always: true}, "", []string{"executecommand"}},
		{"approve once", harness.ResumeRequest{Decision: store.DecisionApprove}, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model := askingModel("execute_command", `{"command":"echo hello"}`)
			h, ctx := facade(t, nil, model, nil)
			th, _ := h.NewThread(ctx, "alice", "")
			paused := exchange(ctx, t, h, "alice", th.ID, "say hello", harness.RunOptions{Interactive: true})
			intr := interruptsOf(t, waitRun(ctx, t, h, paused))[0]
			if intr.Reason != store.ReasonToolApproval {
				t.Fatalf("interrupt %+v", intr)
			}
			req := c.req
			req.InterruptID, req.Reason, req.Model, req.Interactive = intr.ID, intr.Reason, "gpt-test", true

			// Act
			resumed, err := h.Resume(ctx, "alice", th.ID, req)
			if err != nil {
				t.Fatal(err)
			}
			waitRun(ctx, t, h, resumed)

			// Assert
			got, _ := h.Store.GetRun(ctx, resumed.ID)
			if !equalStrings(got.ApprovedTools, c.wantTools) {
				t.Fatalf("approved tools %v, want %v", got.ApprovedTools, c.wantTools)
			}
			a, err := h.Store.GetApproval(ctx, intr.ID)
			if err != nil || a.Decision != c.req.Decision {
				t.Fatalf("approval %+v err %v", a, err)
			}
			if c.wantResult != "" {
				var seen bool
				for _, call := range model.Calls() {
					if last := call.Messages[len(call.Messages)-1]; last.Role == ai.RoleTool && strings.Contains(last.Content, c.wantResult) {
						seen = true
					}
				}
				if !seen {
					t.Fatalf("the model never saw %q as the result of the tool call", c.wantResult)
				}
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSendRefusesAttachmentsAHarnessWithoutArtifactsCannotStore(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), nil)
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	_, err := h.Send(ctx, "alice", th.ID, "look", harness.RunOptions{Model: "gpt-test",
		Attachments: []harness.Attachment{{Name: "a.png", MIME: "image/png", Data: pngBytes}}})

	// Assert
	if !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("err %v", err)
	}
	if msgs := messagesOf(ctx, t, h, th.ID); len(msgs) != 0 {
		t.Fatalf("a refused send left messages %q", contents(msgs))
	}
}

func TestSendStoresAttachmentsAsUploadsWithTheNameOfTheirRealType(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), func(o *harness.Options) {
		o.Features = []harness.Feature{harness.Sandbox(), harness.Plan(), harness.Artifacts()}
	})
	th, _ := h.NewThread(ctx, "alice", "")
	// Act
	run, err := h.Send(ctx, "alice", th.ID, "look at this", harness.RunOptions{Model: "gpt-test",
		Attachments: []harness.Attachment{{Name: "shot.jpg", MIME: "IMAGE/PNG", Data: pngBytes}}})
	if err != nil {
		t.Fatal(err)
	}
	waitRun(ctx, t, h, run)

	// Assert
	msgs := messagesOf(ctx, t, h, th.ID)
	atts := store.MessageAttachments(msgs[0])
	if len(atts) != 1 || atts[0].Name != "shot.png" || atts[0].MimeType != "image/png" || atts[0].SizeBytes != int64(len(pngBytes)) {
		t.Fatalf("attachments %+v", atts)
	}
	arts, err := h.Store.ListArtifacts(ctx, th.ID)
	if err != nil || len(arts) != 1 || arts[0].Name != "shot.png" {
		t.Fatalf("artifacts %+v err %v", arts, err)
	}
}

func TestSendRefusesAnAttachmentWhoseContentIsNotItsDeclaredType(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), func(o *harness.Options) {
		o.Features = []harness.Feature{harness.Sandbox(), harness.Plan(), harness.Artifacts()}
	})
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	_, err := h.Send(ctx, "alice", th.ID, "look", harness.RunOptions{Model: "gpt-test",
		Attachments: []harness.Attachment{{Name: "fake.png", MIME: "image/png", Data: []byte("this is text, not a png")}}})

	// Assert
	if !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("err %v", err)
	}
	if msgs := messagesOf(ctx, t, h, th.ID); len(msgs) != 0 {
		t.Fatalf("a refused send left messages %q", contents(msgs))
	}
}

func TestSendValidatesTheRequestBeforeCreatingARun(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), nil)
	th, _ := h.NewThread(ctx, "alice", "")
	cases := []struct {
		name string
		opts harness.RunOptions
	}{
		{"no model", harness.RunOptions{}},
		{"bad sandbox mode", harness.RunOptions{Model: "gpt-test", Sandbox: "maybe"}},
		{"bad run mode", harness.RunOptions{Model: "gpt-test", Mode: "yolo"}},
		{"too many attachments", harness.RunOptions{Model: "gpt-test", Attachments: make([]harness.Attachment, harness.MaxAttachments+1)}},
		{"not an image", harness.RunOptions{Model: "gpt-test", Attachments: []harness.Attachment{{Name: "a.pdf", MIME: "application/pdf", Data: []byte("x")}}}},
		{"empty attachment", harness.RunOptions{Model: "gpt-test", Attachments: []harness.Attachment{{Name: "a.png", MIME: "image/png"}}}},
		{"huge attachment", harness.RunOptions{Model: "gpt-test", Attachments: []harness.Attachment{{Name: "a.png", MIME: "image/png", Data: make([]byte, harness.MaxAttachmentBytes+1)}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			run, err := h.Send(ctx, "alice", th.ID, "hi", c.opts)

			// Assert
			if run != nil || !errors.Is(err, harness.ErrInvalidRequest) {
				t.Fatalf("run %v err %v", run, err)
			}
		})
	}
	if msgs := messagesOf(ctx, t, h, th.ID); len(msgs) != 0 {
		t.Fatalf("refused sends left messages %q", contents(msgs))
	}
}

func TestSendWithARequiredSandboxFailsWhenTheHarnessHasNone(t *testing.T) {
	// Arrange: no backend configured
	h, ctx := facade(t, nil, echoModel(), func(o *harness.Options) { o.Backend = nil })
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	_, required := h.Send(ctx, "alice", th.ID, "hi", harness.RunOptions{Model: "gpt-test", Sandbox: harness.SandboxOn})
	run, off := h.Send(ctx, "alice", th.ID, "hi", harness.RunOptions{Model: "gpt-test", Sandbox: harness.SandboxOff})

	// Assert
	if !errors.Is(required, harness.ErrNoSandbox) || !errors.Is(required, harness.ErrInvalidRequest) {
		t.Fatalf("required: %v", required)
	}
	if off != nil || run == nil || h.HasSandbox() {
		t.Fatalf("a run without a sandbox is fine: %v (HasSandbox %v)", off, h.HasSandbox())
	}
}
