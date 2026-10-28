package compaction_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/compaction"
)

func imageMessage(text string, images int) *schema.Message {
	b64 := "aGVsbG8="
	parts := []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeText, Text: text}}
	for i := 0; i < images; i++ {
		parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &b64, MIMEType: "image/png"}}})
	}
	return &schema.Message{Role: schema.User, UserInputMultiContent: parts}
}

func TestEstimateCountsImagePartsAndTheirText(t *testing.T) {
	// A user message whose text moved into image parts (vision models) is not estimated as empty.
	// Arrange
	text := strings.Repeat("a", 3500) + "\n\n[Attached files: /workspace/uploads/chart.png]"
	plain := schema.UserMessage(text)
	withImages := imageMessage(text, 2)

	// Act
	p, w := compaction.EstimateMessages([]*schema.Message{plain}), compaction.EstimateMessages([]*schema.Message{withImages})

	// Assert
	if p < 800 || w != p+2*compaction.ImageTokens {
		t.Fatalf("plain %d, with 2 images %d", p, w)
	}
}

func TestTranscriptKeepsAttachmentLineAndNotesImages(t *testing.T) {
	// The summarizer sees the text and attachment line of an image message plus an image marker.
	// Arrange
	msgs := []*schema.Message{imageMessage("What is on this chart?\n\n[Attached files: /workspace/uploads/chart.png]", 1)}

	// Act
	got := compaction.Transcript(msgs)

	// Assert
	if !strings.Contains(got, "USER: What is on this chart?") || !strings.Contains(got, "/workspace/uploads/chart.png") ||
		!strings.Contains(got, "[1 image attached]") {
		t.Fatalf("transcript %q", got)
	}
}

func TestSummaryListsArtifactFilesOfSummarizedMessages(t *testing.T) {
	// Files recorded as artifacts (e.g. written by execute_command) survive in the summary's file list.
	// Arrange
	call := schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "execute_command", Arguments: `{"command":"python make.py"}`}}})
	result := schema.ToolMessage("done", "c1")
	compaction.SetFiles(result, []string{"/workspace/report.docx"})
	answer := schema.AssistantMessage("Report ready", nil)
	compaction.SetFiles(answer, []string{"/workspace/old.csv (deleted)"})
	msgs := []*schema.Message{schema.UserMessage("make a report"), call, result, answer, schema.UserMessage("thanks")}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: 100000, Manual: true,
		Summarize: func(context.Context, string, string) (string, error) { return "## Goal\nReport.", nil }})

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized {
		t.Fatalf("result %+v err %v", res, err)
	}
	if !strings.Contains(res.Summary, compaction.FilesHeading+"\n- /workspace/report.docx\n- /workspace/old.csv (deleted)") {
		t.Fatalf("summary %q", res.Summary)
	}
}

func TestSetFilesKeepsPosition(t *testing.T) {
	// Files and position share Extra without overwriting each other.
	// Arrange
	m := schema.ToolMessage("x", "c1")
	m.Extra = map[string]any{"work.attachments": "kept"}

	// Act
	compaction.SetPosition(m, 7)
	compaction.SetFiles(m, []string{"/workspace/a.txt"})
	compaction.SetFiles(m, nil)

	// Assert
	if compaction.Position(m) != 7 || m.Extra["work.attachments"] != "kept" || len(compaction.Files(m)) != 1 {
		t.Fatalf("extra %+v", m.Extra)
	}
}

func TestEstimateCountsCJKRunesAtTheirCalibratedRate(t *testing.T) {
	// Japanese and Chinese text costs about 0.7 token per character (gateway calibration), far more than Latin text per character.
	// Arrange
	ja := strings.Repeat("売上報告書を作成してください。", 100) // 1500 runes
	zh := strings.Repeat("请根据数据生成季度报告。", 100)    // 1200 runes
	latin := strings.Repeat("a", 3500)

	// Act
	gotJa, gotZh, gotLatin := compaction.EstimateText(ja), compaction.EstimateText(zh), compaction.EstimateText(latin)
	gotMsg := compaction.EstimateMessages([]*schema.Message{schema.UserMessage(ja)})

	// Assert
	if gotJa < 950 || gotJa > 1150 || gotZh < 750 || gotZh > 950 || gotLatin < 800 || gotLatin > 950 || gotMsg < gotJa {
		t.Fatalf("ja %d zh %d latin %d message %d", gotJa, gotZh, gotLatin, gotMsg)
	}
}
