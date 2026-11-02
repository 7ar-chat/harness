package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

func userRow(id, text string, at time.Time) *store.Message {
	return &store.Message{ID: id, Role: store.RoleUser, Content: text, CreatedAt: at}
}

func assistantCalling(id, callID, tool, args string) *store.Message {
	return &store.Message{ID: id, Role: store.RoleAssistant, ToolCalls: []store.ToolCall{{ID: callID, Type: "function", Function: store.ToolCallFunction{Name: tool, Arguments: args}}}}
}

func toolRow(callID, content string, md map[string]any) *store.Message {
	return &store.Message{ID: "t-" + callID, Role: store.RoleTool, ToolCallID: callID, Content: content, Metadata: md}
}

func artifactMD(path, change string) map[string]any {
	return map[string]any{"artifacts": []any{map[string]any{"path": path, "change": change}}}
}

func TestTurnMessagesStartAtTheLastUserMessageThatIsNotASteer(t *testing.T) {
	// Arrange
	steer := &store.Message{ID: "s", Role: store.RoleUser, Content: "also B", Metadata: map[string]any{"steer": true}}
	msgs := []*store.Message{userRow("u1", "first", time.Time{}), assistantCalling("a1", "c1", "t", "{}"), userRow("u2", "second", time.Time{}), assistantCalling("a2", "c2", "t", "{}"), steer}

	// Act
	turn := turnMessages(msgs)

	// Assert
	if len(turn) != 3 || turn[0].ID != "u2" {
		t.Fatalf("turn starts at %v (%d messages)", turn[0].ID, len(turn))
	}
	if all := turnMessages(msgs[1:2]); len(all) != 1 {
		t.Fatalf("no user message: %d", len(all))
	}
}

func TestSeedVerificationReplaysChangesChecksAndPendingFilesOfTheTurn(t *testing.T) {
	// Arrange
	docx, xlsx := "/workspace/outputs/r.docx", "/workspace/outputs/b.xlsx"
	st := newState(10)
	turn := []*store.Message{
		userRow("u", "go", time.Time{}),
		assistantCalling("a1", "c1", "execute_command", "{}"),
		toolRow("c1", "built", map[string]any{
			"artifacts": []any{map[string]any{"path": docx, "change": store.ChangeCreated}, map[string]any{"path": xlsx, "change": store.ChangeCreated}},
			"verification": []any{
				map[string]any{"path": docx, "checker": "inspect_docx", "ok": true, "summary": "fine"},
			},
			"verificationPending": []any{xlsx},
		}),
	}

	// Act
	st.seedVerification(turn)

	// Assert
	v := &st.v3.verify
	if checked, ok := v.files[docx].verdict(); !checked || !ok || v.files[docx].last.Summary != "fine" {
		t.Errorf("docx %+v", v.files[docx])
	}
	if d := v.files[xlsx]; !d.pending || d.origin != "c1" || d.checked >= d.changed {
		t.Errorf("xlsx %+v", d)
	}
}

func TestSeedVerificationKeepsTheOrderTheRunRecordedItIn(t *testing.T) {
	// Arrange: the model's own check ran before the file changed again; the runtime's check after it.
	docx := "/workspace/outputs/r.docx"
	st := newState(10)
	md := artifactMD(docx, store.ChangeUpdated)
	md["verification"] = []any{
		map[string]any{"path": docx, "checker": "c", "ok": true, "summary": "model-run ok", "modelRun": true},
		map[string]any{"path": docx, "checker": "c", "ok": false, "summary": "runtime: broken"},
	}

	// Act
	st.seedVerification([]*store.Message{userRow("u", "go", time.Time{}), toolRow("c1", "x", md)})

	// Assert
	d := st.v3.verify.files[docx]
	if checked, ok := d.verdict(); !checked || ok || d.last.Summary != "runtime: broken" || !d.runtime {
		t.Fatalf("%+v", d)
	}
}

func TestSeedVerificationCountsAViewOnlyWhenAModelCallFollowedIt(t *testing.T) {
	for name, tc := range map[string]struct {
		afterView  []*store.Message
		viewError  string
		wantViewed bool
		wantFailed bool
	}{
		"answered after":     {[]*store.Message{{Role: store.RoleAssistant, Content: "looks fine"}}, "", true, false},
		"nothing after":      {nil, "", false, false},
		"only a steer after": {[]*store.Message{{Role: store.RoleUser, Content: "x"}}, "", false, false},
		"view failed":        {[]*store.Message{{Role: store.RoleAssistant, Content: "could not"}}, "Error: render crashed", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			docx := "/workspace/outputs/r.docx"
			st := newState(10)
			view := toolRow("c2", `{"pages":[]}`, nil)
			view.Error = tc.viewError
			turn := []*store.Message{userRow("u", "go", time.Time{}),
				assistantCalling("a1", "c1", "write_file", "{}"), toolRow("c1", "ok", artifactMD(docx, store.ChangeCreated)),
				assistantCalling("a2", "c2", "view_file", fmt.Sprintf(`{"path":%q}`, docx)), view}
			turn = append(turn, tc.afterView...)

			// Act
			st.seedVerification(turn)

			// Assert
			d := st.v3.verify.files[docx]
			if (d.viewed > d.changed) != tc.wantViewed || (d.viewFailed > d.changed) != tc.wantFailed {
				t.Fatalf("%+v", d)
			}
		})
	}
}

func TestTurnDeliverablesListsTheChangedFilesWithTheirLastVerdictSortedByPath(t *testing.T) {
	// Arrange
	a, b, c := "/workspace/outputs/a.docx", "/workspace/outputs/b.xlsx", "/workspace/outputs/c.md"
	msgs := []*store.Message{
		userRow("old", "yesterday", time.Time{}),
		assistantCalling("a0", "c0", "write_file", "{}"), toolRow("c0", "ok", artifactMD("/workspace/outputs/old.md", store.ChangeCreated)),
		userRow("u", "today", time.Time{}),
		assistantCalling("a1", "c1", "write_file", "{}"),
		toolRow("c1", "ok", map[string]any{
			"artifacts": []any{map[string]any{"path": c, "change": store.ChangeCreated}, map[string]any{"path": b, "change": store.ChangeCreated}, map[string]any{"path": a, "change": store.ChangeCreated}},
			"verification": []any{
				map[string]any{"path": a, "checker": "inspect_docx", "ok": false, "summary": "no title"},
				map[string]any{"path": b, "checker": "inspect_xlsx", "ok": true, "summary": "ok", "failed": true},
			},
		}),
	}

	// Act
	got := TurnDeliverables(msgs)

	// Assert
	if len(got) != 3 || got[0].Path != a || got[1].Path != b || got[2].Path != c {
		t.Fatalf("deliverables %+v", got)
	}
	if got[0].OK == nil || *got[0].OK || got[0].Checker != "inspect_docx" || got[0].Summary != "no title" {
		t.Errorf("a %+v", got[0])
	}
	if got[1].OK != nil || got[1].Checker != "inspect_xlsx" {
		t.Errorf("a checker that could not run gave a verdict: %+v", got[1])
	}
	if got[2].Checker != "" || got[2].OK != nil {
		t.Errorf("c %+v", got[2])
	}
}

func TestDeliverablesSinceCoversTheTurnsStartedAtOrAfterTheGivenTime(t *testing.T) {
	// Arrange
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	msgs := []*store.Message{
		userRow("u1", "turn 1", t0),
		assistantCalling("a1", "c1", "write_file", "{}"), toolRow("c1", "ok", artifactMD("/workspace/outputs/one.md", store.ChangeCreated)),
		userRow("u2", "turn 2", t0.Add(time.Hour)),
		assistantCalling("a2", "c2", "write_file", "{}"), toolRow("c2", "ok", artifactMD("/workspace/outputs/two.md", store.ChangeCreated)),
		userRow("u3", "turn 3", t0.Add(2*time.Hour)),
		assistantCalling("a3", "c3", "write_file", "{}"), toolRow("c3", "ok", artifactMD("/workspace/outputs/three.md", store.ChangeCreated)),
	}
	paths := func(ds []Deliverable) string {
		var out []string
		for _, d := range ds {
			out = append(out, d.Path[len("/workspace/outputs/"):])
		}
		return fmt.Sprint(out)
	}

	// Assert
	if got := paths(DeliverablesSince(msgs, t0.Add(time.Hour))); got != "[three.md two.md]" && got != "[two.md three.md]" {
		t.Errorf("since turn 2: %s", got)
	}
	if got := paths(DeliverablesSince(msgs, t0.Add(time.Hour+time.Minute))); got != "[three.md]" {
		t.Errorf("since after turn 2 started: %s", got)
	}
	if got := paths(DeliverablesSince(msgs, t0.Add(24*time.Hour))); got != "[three.md]" {
		t.Errorf("no turn since: the current turn expected, got %s", got)
	}
}

func TestSeedGuardsRestoresTheStreakErrorsAndFileIntentOfTheTurn(t *testing.T) {
	// Arrange
	st := newState(10)
	failed := toolRow("c2", "Error: boom 1", nil)
	failed.Error = "boom"
	turn := []*store.Message{
		userRow("u", "go", time.Time{}),
		{ID: "a1", Role: store.RoleAssistant, Content: "I will write /workspace/outputs/x.md", ToolCalls: []store.ToolCall{
			{ID: "c1", Function: store.ToolCallFunction{Name: "t", Arguments: `{"a":1}`}},
			{ID: "c2", Function: store.ToolCallFunction{Name: "t", Arguments: `{"a":2}`}},
			{ID: "c3", Function: store.ToolCallFunction{Name: "t", Arguments: `{"a":3}`}},
		}},
		toolRow("c1", "fine", artifactMD("/workspace/data/in.xlsx", store.ChangeUpdated)),
		failed,
	}
	st.g.forceAnswer = true

	// Act
	st.seedGuards(turn)

	// Assert
	g := &st.g
	if len(g.turn) != 2 || g.turn[1].Failed != true || g.turn[0].Name != "t" {
		t.Errorf("turn calls %+v", g.turn)
	}
	if g.failedInRow != 1 || g.errors[errorKey("Error: boom 1")] != 1 {
		t.Errorf("failures %d %v", g.failedInRow, g.errors)
	}
	if !g.asksFile || !g.inPlace {
		t.Errorf("asksFile %v inPlace %v", g.asksFile, g.inPlace)
	}
	if g.forceAnswer {
		t.Error("a new run did not get its tools back")
	}
	if len(g.recent) != 2 {
		t.Errorf("recent %d: the call without a result must not count", len(g.recent))
	}
}
