package compaction_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/compaction"
)

func imageMessage(text string, images int) *ai.Message {
	parts := []ai.Part{{Type: ai.PartText, Text: text}}
	for i := 0; i < images; i++ {
		parts = append(parts, ai.Part{Type: ai.PartImage,
			Image: &ai.Image{Base64Data: "aGVsbG8=", MIMEType: "image/png"}})
	}
	return &ai.Message{Role: ai.RoleUser, Parts: parts}
}

func TestEstimateCountsImagePartsAndTheirText(t *testing.T) {
	// A user message whose text moved into image parts (vision models) is not estimated as empty.
	// Arrange
	text := strings.Repeat("a", 3500) + "\n\n[Attached files: /workspace/uploads/chart.png]"
	plain := ai.UserMessage(text)
	withImages := imageMessage(text, 2)

	// Act
	p, w := compaction.EstimateMessages([]*ai.Message{plain}), compaction.EstimateMessages([]*ai.Message{withImages})

	// Assert
	if p < 800 || w != p+2*compaction.ImageTokens {
		t.Fatalf("plain %d, with 2 images %d", p, w)
	}
}

func TestTranscriptKeepsAttachmentLineAndNotesImages(t *testing.T) {
	// The summarizer sees the text and attachment line of an image message plus an image marker.
	// Arrange
	msgs := []*ai.Message{imageMessage("What is on this chart?\n\n[Attached files: /workspace/uploads/chart.png]", 1)}

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
	call := ai.AssistantMessage("", []ai.ToolCall{{ID: "c1", Function: ai.FunctionCall{Name: "execute_command", Arguments: `{"command":"python make.py"}`}}})
	result := ai.ToolMessage("done", "c1")
	compaction.SetFiles(result, []string{"/workspace/report.docx"})
	answer := ai.AssistantMessage("Report ready", nil)
	compaction.SetFiles(answer, []string{"/workspace/old.csv (deleted)"})
	msgs := []*ai.Message{ai.UserMessage("make a report"), call, result, answer, ai.UserMessage("thanks")}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, compaction.Options{Window: 100000, Manual: true,
		Summarize: func(context.Context, string, string) (string, error) { return "## Goal\nReport.", nil }})

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized {
		t.Fatalf("result %+v err %v", res, err)
	}
	if !strings.Contains(res.Summary, compaction.DefaultFilesHeading+"\n- /workspace/report.docx\n- /workspace/old.csv (deleted)") {
		t.Fatalf("summary %q", res.Summary)
	}
}

func TestSetFilesKeepsPosition(t *testing.T) {
	// Files and position share Extra without overwriting each other.
	// Arrange
	m := ai.ToolMessage("x", "c1")
	m.Extra = map[string]any{"app.attachments": "kept"}

	// Act
	compaction.SetPosition(m, 7)
	compaction.SetFiles(m, []string{"/workspace/a.txt"})
	compaction.SetFiles(m, nil)

	// Assert
	if compaction.Position(m) != 7 || m.Extra["app.attachments"] != "kept" || len(compaction.Files(m)) != 1 {
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
	gotMsg := compaction.EstimateMessages([]*ai.Message{ai.UserMessage(ja)})

	// Assert
	if gotJa < 950 || gotJa > 1150 || gotZh < 750 || gotZh > 950 || gotLatin < 800 || gotLatin > 950 || gotMsg < gotJa {
		t.Fatalf("ja %d zh %d latin %d message %d", gotJa, gotZh, gotLatin, gotMsg)
	}
}
