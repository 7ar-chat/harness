package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/prompts"
)

func imagePart(path string, bytes int) ai.Part {
	b64 := base64.StdEncoding.EncodeToString(make([]byte, bytes))
	return ai.Part{Type: ai.PartImage, Image: &ai.Image{Base64Data: b64, MIMEType: "image/png"}, Extra: map[string]any{imagePathKey: path}}
}

func imageMsg(paths ...string) *ai.Message {
	m := &ai.Message{Role: ai.RoleUser, Parts: []ai.Part{{Type: ai.PartText, Text: "pages"}}}
	for _, p := range paths {
		m.Parts = append(m.Parts, imagePart(p, 300))
	}
	return m
}

func imageTexts(m *ai.Message) []string {
	var out []string
	for _, p := range m.Parts {
		switch p.Type {
		case ai.PartText:
			out = append(out, p.Text)
		case ai.PartImage:
			out = append(out, "<image "+p.Extra[imagePathKey].(string)+">")
		}
	}
	return out
}

func TestAddToolImagesQueuesOneUserMessageNamingThePaths(t *testing.T) {
	// Arrange
	st := newState(5)

	// Act
	st.addToolImages(nil)
	st.addToolImages([]tools.Image{{Path: "/workspace/a.png", MIMEType: "image/png", Data: []byte("aaa")}, {Path: "/workspace/b.png", MIMEType: "image/jpeg", Data: []byte("bbb")}})

	// Assert
	if len(st.history) != 1 {
		t.Fatalf("history %d", len(st.history))
	}
	m := st.history[0]
	if m.Role != ai.RoleUser || len(m.Parts) != 3 || !strings.Contains(m.Parts[0].Text, "/workspace/a.png, /workspace/b.png") {
		t.Fatalf("message %+v", m)
	}
	if m.Parts[1].Image.MIMEType != "image/png" || m.Parts[1].Image.Base64Data != base64.StdEncoding.EncodeToString([]byte("aaa")) || m.Parts[2].Extra[imagePathKey] != "/workspace/b.png" {
		t.Fatalf("image parts %+v", m.Parts)
	}
}

func TestCapImagesKeepsTheNewestSixAndStubsTheOthers(t *testing.T) {
	// Arrange
	old := imageMsg("/old1.png", "/old2.png", "/old3.png", "/old4.png")
	recent := imageMsg("/new1.png", "/new2.png", "/new3.png", "/new4.png")
	msgs := []*ai.Message{ai.UserMessage("go"), old, ai.AssistantMessage("hm", nil), recent}

	// Act
	out := capImages(msgs)

	// Assert
	if got := fmt.Sprint(imageTexts(out[3])); got != "[pages <image /new1.png> <image /new2.png> <image /new3.png> <image /new4.png>]" {
		t.Errorf("recent: %s", got)
	}
	want := "[pages [image /old1.png " + notShownStub + "] [image /old2.png " + notShownStub + "] <image /old3.png> <image /old4.png>]"
	if got := fmt.Sprint(imageTexts(out[1])); got != want {
		t.Errorf("old:\n got %s\nwant %s", got, want)
	}
	if len(old.Parts) != 5 || old.Parts[1].Type != ai.PartImage {
		t.Error("the stored message was changed")
	}
	if out[0] != msgs[0] || out[2] != msgs[2] {
		t.Error("messages without images were copied")
	}
}

func TestCapImagesLimitsTheBytesOfARequest(t *testing.T) {
	// Arrange: three 3 MiB images do not fit in 6 MiB together.
	big := func(p string) ai.Part { return imagePart(p, 3<<20) }
	m := &ai.Message{Role: ai.RoleUser, Parts: []ai.Part{big("/a.png"), big("/b.png"), big("/c.png")}}

	// Act
	out := capImages([]*ai.Message{m})

	// Assert
	got := imageTexts(out[0])
	if len(got) != 3 || !strings.HasPrefix(got[0], "[image /a.png") || got[1] != "<image /b.png>" || got[2] != "<image /c.png>" {
		t.Fatalf("%v", got)
	}
}

func TestCapImagesReturnsTheSameSliceWhenNothingIsOverTheLimit(t *testing.T) {
	msgs := []*ai.Message{ai.UserMessage("hi"), imageMsg("/a.png")}
	if out := capImages(msgs); &out[0] != &msgs[0] {
		t.Fatal("copied although nothing changed")
	}
}

func TestImagePathsFallBackToTheAttachmentThePartCameFrom(t *testing.T) {
	// Arrange
	m := &ai.Message{Role: ai.RoleUser, Extra: map[string]any{store.ExtraAttachments: []store.Attachment{{ArtifactID: "art-1", Path: "/workspace/uploads/scan.png"}}},
		Parts: []ai.Part{
			{Type: ai.PartText, Text: "x"},
			{Type: ai.PartImage, Image: &ai.Image{}, Extra: map[string]any{ImageAttachmentKey: "art-1"}},
			{Type: ai.PartImage, Image: &ai.Image{}, Extra: map[string]any{imagePathKey: "/workspace/tool.png", ImageAttachmentKey: "art-1"}},
			{Type: ai.PartImage, Image: &ai.Image{}},
		}}

	// Act
	got := imagePaths(m)

	// Assert
	if fmt.Sprint(got) != "[/workspace/uploads/scan.png /workspace/tool.png ]" {
		t.Fatalf("%q", got)
	}
	if p := imageStub("", "gone"); p.Text != "[image attachment gone]" {
		t.Errorf("unnamed stub %q", p.Text)
	}
	if imageBytes(ai.Part{Type: ai.PartText}) != 0 {
		t.Error("a text part has bytes")
	}
}

func TestImagesSentStubsEveryImageOfTheHistoryOnceItWasInARequest(t *testing.T) {
	// Arrange
	st := newState(5)
	sentMsg := imageMsg("/sent.png")
	st.history = []*ai.Message{sentMsg, imageMsg("/never-sent.png")}

	// Act: the request carried only the first message.
	st.imagesSent([]*ai.Message{sentMsg})

	// Assert
	if got := fmt.Sprint(imageTexts(st.history[0])); got != "[pages [image /sent.png "+shownStub+"]]" {
		t.Errorf("sent: %s", got)
	}
	if got := fmt.Sprint(imageTexts(st.history[1])); got != "[pages [image /never-sent.png "+notShownStub+"]]" {
		t.Errorf("not sent: %s", got)
	}
}

func TestUserMessageListsAttachmentsAfterTheText(t *testing.T) {
	// Arrange
	withAtt := &store.Message{Metadata: map[string]any{"attachments": []any{
		map[string]any{"artifactId": "a1", "path": "/workspace/uploads/a.png"}, map[string]any{"artifactId": "a2", "path": "/workspace/uploads/b.pdf"}}}}

	// Act + Assert
	if m := userMessage(&store.Message{}, "plain"); m.Content != "plain" || m.Extra != nil {
		t.Errorf("plain %+v", m)
	}
	m := userMessage(withAtt, "look at these\n\n")
	if m.Content != "look at these\n\n[Attached files: /workspace/uploads/a.png, /workspace/uploads/b.pdf]" {
		t.Errorf("content %q", m.Content)
	}
	if atts, _ := m.Extra[store.ExtraAttachments].([]store.Attachment); len(atts) != 2 {
		t.Errorf("extra %+v", m.Extra)
	}
	if m := userMessage(withAtt, "  "); m.Content != "[Attached files: /workspace/uploads/a.png, /workspace/uploads/b.pdf]" {
		t.Errorf("text-less message %q", m.Content)
	}
}

type imageAttacher struct{ panics bool }

func (a imageAttacher) AttachImages(_ context.Context, _ *store.Run, h []*ai.Message) []*ai.Message {
	if a.panics {
		panic("attacher bug")
	}
	return append(h, imageMsg("/attached.png"))
}

func TestAttachImagesKeepsTheHistoryWhenTheAttacherPanics(t *testing.T) {
	// Arrange
	h := []*ai.Message{ai.UserMessage("hi")}
	run := &store.Run{ID: "r"}

	// Act
	none := (&Agent{}).attachImages(context.Background(), run, h)
	added := (&Agent{Harness: Harness{Images: imageAttacher{}}}).attachImages(context.Background(), run, h)
	crashed := (&Agent{Harness: Harness{Images: imageAttacher{panics: true}}}).attachImages(context.Background(), run, h)

	// Assert
	if len(none) != 1 || len(added) != 2 || len(crashed) != 1 {
		t.Fatalf("%d %d %d", len(none), len(added), len(crashed))
	}
}

func TestPartKeepsASectionOnlyInVariantsThatIncludeIt(t *testing.T) {
	// Arrange
	sec := func(context.Context, Input) string { return "git rules" }
	section := Part(prompts.Git, sec)

	// Act + Assert
	if got := section(context.Background(), Input{Variant: prompts.Coding}); got != "" {
		t.Errorf("coding variant has no git part: %q", got)
	}
	if got := section(context.Background(), Input{Variant: prompts.Default}); got != "git rules" {
		t.Errorf("default variant: %q", got)
	}
	if got := section(context.Background(), Input{Variant: "no-such-variant"}); got != "git rules" {
		t.Errorf("unknown variants resolve to the default: %q", got)
	}
}

func TestReportUntrustedTaintsTheRunsPromptAndIgnoresOtherContexts(t *testing.T) {
	// Arrange
	r := newRunRig(t, textTurn("ok"))
	r.a.Harness.Sections = []PromptSection{
		nil,
		func(context.Context, Input) string { return "  " },
		func(ctx context.Context, _ Input) string { ReportUntrusted(ctx); return "user skill text" },
	}
	r.a.Harness.Context = []PromptSection{func(context.Context, Input) string { return "goal: ship it" }}
	r.a.Harness.Tail = []PromptSection{func(context.Context, Input) string { return "turn 2 of 5" }}
	ReportUntrusted(context.Background()) // outside a prompt build: harmless

	// Act
	if _, err := r.do("hi"); err != nil {
		t.Fatal(err)
	}

	// Assert
	msgs := r.model.Calls()[0].Messages
	sys := msgs[0].Content
	if !strings.Contains(sys, "user skill text") || !strings.Contains(sys, "goal: ship it") {
		t.Errorf("system prompt lacks the sections: %.200s", sys)
	}
	if tail := msgs[len(msgs)-1]; tail.Content != "<system-reminder>\nturn 2 of 5\n</system-reminder>" {
		t.Errorf("tail %q", tail.Content)
	}
}

func TestPromptVariantComesFromTheModelsConfiguration(t *testing.T) {
	a := &Agent{}
	if a.variantOf("any") != prompts.Default {
		t.Errorf("default %q", a.variantOf("any"))
	}
	a.Harness.PromptVariant = func(m string) string {
		if m == "deepseek-chat" {
			return prompts.DeepSeek
		}
		return "unknown"
	}
	if a.variantOf("deepseek-chat") != prompts.DeepSeek || a.variantOf("other") != prompts.Default {
		t.Errorf("variants %q %q", a.variantOf("deepseek-chat"), a.variantOf("other"))
	}
}
