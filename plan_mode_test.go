package harness_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/longtest"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
)

// sendWith plays one run of a work-profile harness (sandbox backend, a custom
// side-effecting tool "ping") with the options o and returns the first model request.
func sendWith(t *testing.T, o harness.RunOptions) (*harness.Harness, *llmtest.Model, *store.Run, error) {
	t.Helper()
	model := llmtest.New(
		llmtest.Turn{Chunks: llmtest.Text("a plan")},
		llmtest.Turn{Chunks: llmtest.Text("A title")},
	)
	h, err := harness.New(harness.Options{
		Config: featureConfig(t, nil), Backend: sandboxtest.NewBackend(), Models: llmtest.Factory{M: model},
		Tools: []tools.Provider{longtest.ExtraTool("ping", func(context.Context, string) (string, error) { return "pong", nil })},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()
	th, _ := h.NewThread(ctx, "alice", "")
	o.Model = "gpt-test"
	run, err := h.Send(ctx, "alice", th.ID, "what would you change?", o)
	if err != nil {
		return h, model, nil, err
	}
	var last string
	for ev := range h.Events(ctx, run.ID, 0) {
		last = ev.Type
	}
	if last != agui.RunFinished {
		t.Fatalf("last event %q", last)
	}
	run, _ = h.Store.GetRun(ctx, run.ID)
	return h, model, run, nil
}

func requestTools(m *llmtest.Model) []string {
	var names []string
	for _, ti := range m.Calls()[0].Tools {
		names = append(names, ti.Name)
	}
	return names
}

func TestAPlanRunOffersOnlyReadOnlyToolsAndThePlanPrompt(t *testing.T) {
	// Act
	_, model, run, err := sendWith(t, harness.RunOptions{Mode: harness.ModePlan})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if run.Mode != store.ModePlan {
		t.Fatalf("run record mode = %q", run.Mode)
	}
	names := requestTools(model)
	for _, banned := range []string{"execute_command", "write_file", "edit_file", "job_kill", "ping"} {
		if slices.Contains(names, banned) {
			t.Errorf("plan run offers %s: %v", banned, names)
		}
	}
	for _, want := range []string{"read_file", "search_files", "list_files"} {
		if !slices.Contains(names, want) {
			t.Errorf("plan run lacks %s: %v", want, names)
		}
	}
	var system string
	for _, msg := range model.Calls()[0].Messages {
		system += msg.Content + "\n"
	}
	if !strings.Contains(system, agent.PlanModePrompt) {
		t.Error("the plan prompt section is missing")
	}
}

func TestABuildRunKeepsItsToolsAndHasNoPlanPrompt(t *testing.T) {
	// Act
	_, model, run, err := sendWith(t, harness.RunOptions{})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if run.Mode != store.ModeBuild {
		t.Fatalf("run record mode = %q", run.Mode)
	}
	names := requestTools(model)
	if !slices.Contains(names, "execute_command") || !slices.Contains(names, "ping") {
		t.Errorf("build run lost tools: %v", names)
	}
	for _, msg := range model.Calls()[0].Messages {
		if strings.Contains(msg.Content, agent.PlanModePrompt) {
			t.Error("a build run got the plan prompt")
		}
	}
}

func TestAnUnknownModeIsAnInvalidRequest(t *testing.T) {
	// Act
	_, _, _, err := sendWith(t, harness.RunOptions{Mode: "yolo"})

	// Assert
	if !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)

func TestAttachmentsAreStoredAndListedInTheMessage(t *testing.T) {
	// Act
	h, _, run, err := sendWith(t, harness.RunOptions{Attachments: []harness.Attachment{{Name: "shot", MIME: "image/png", Data: pngBytes}}})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := h.Store.ListMessages(context.Background(), run.ThreadID)
	atts := store.MessageAttachments(msgs[0])
	if len(atts) != 1 || atts[0].Path != "/workspace/shot.png" || atts[0].MimeType != "image/png" {
		t.Fatalf("attachments = %+v", atts)
	}
}

func TestBadAttachmentsAreInvalidRequests(t *testing.T) {
	big := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, harness.MaxAttachmentBytes)...)
	many := make([]harness.Attachment, harness.MaxAttachments+1)
	for i := range many {
		many[i] = harness.Attachment{Name: "a.png", MIME: "image/png", Data: pngBytes}
	}
	cases := map[string][]harness.Attachment{
		"pdf":      {{Name: "a.pdf", MIME: "application/pdf", Data: []byte("%PDF-1.4")}},
		"too big":  {{Name: "a.png", MIME: "image/png", Data: big}},
		"too many": many,
		"empty":    {{Name: "a.png", MIME: "image/png"}},
		"not png":  {{Name: "a.png", MIME: "image/png", Data: []byte("plain text, not an image")}},
	}
	for name, atts := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			_, _, _, err := sendWith(t, harness.RunOptions{Attachments: atts})

			// Assert
			if !errors.Is(err, harness.ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
