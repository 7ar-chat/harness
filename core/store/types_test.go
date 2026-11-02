package store_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

func call(id, name, args string) store.ToolCall {
	return store.ToolCall{ID: id, Type: "function", Function: store.ToolCallFunction{Name: name, Arguments: args}}
}

func assistant(calls ...store.ToolCall) *store.Message {
	return &store.Message{Role: store.RoleAssistant, ToolCalls: calls}
}

func toolResult(callID, errText string) *store.Message {
	return &store.Message{Role: store.RoleTool, ToolCallID: callID, Error: errText}
}

func planArgs(items ...store.PlanItem) string {
	b, _ := json.Marshal(map[string]any{"items": items})
	return string(b)
}

func TestLatestPlanIsTheLastSuccessfulUpdatePlanCall(t *testing.T) {
	// Arrange
	first := store.PlanItem{ID: "1", Title: "write", Status: store.PlanPending}
	second := store.PlanItem{ID: "1", Title: "write", Status: store.PlanCompleted}
	msgs := []*store.Message{
		assistant(call("c1", "update_plan", planArgs(first))), toolResult("c1", ""),
		assistant(call("c2", "update_plan", planArgs(second))), toolResult("c2", ""),
	}

	// Act
	got := store.LatestPlan(msgs)

	// Assert
	if !reflect.DeepEqual(got, []store.PlanItem{second}) {
		t.Fatalf("plan = %+v, want the later one", got)
	}
}

func TestLatestPlanSkipsCallsThatFailedWereNotAnsweredOrHaveABadPlan(t *testing.T) {
	good := store.PlanItem{ID: "1", Title: "ship", Status: store.PlanInProgress}
	base := []*store.Message{assistant(call("ok", "update_plan", planArgs(good))), toolResult("ok", "")}
	cases := []struct {
		name string
		tail []*store.Message
	}{
		{"a failed later call", []*store.Message{assistant(call("bad", "update_plan", planArgs(store.PlanItem{ID: "2", Title: "x", Status: store.PlanPending}))), toolResult("bad", "validation failed")}},
		{"a later call without a result (still running or hidden)", []*store.Message{assistant(call("open", "update_plan", planArgs(store.PlanItem{ID: "2", Title: "x", Status: store.PlanPending})))}},
		{"another tool", []*store.Message{assistant(call("o", "read_file", planArgs(store.PlanItem{ID: "2", Title: "x", Status: store.PlanPending}))), toolResult("o", "")}},
		{"malformed arguments", []*store.Message{assistant(call("m", "update_plan", `{"items": [`)), toolResult("m", "")}},
		{"an unknown status", []*store.Message{assistant(call("s", "update_plan", planArgs(store.PlanItem{ID: "2", Title: "x", Status: "done"}))), toolResult("s", "")}},
		{"an item without a title", []*store.Message{assistant(call("t", "update_plan", planArgs(store.PlanItem{ID: "2", Title: "  ", Status: store.PlanPending}))), toolResult("t", "")}},
		{"an item without an id", []*store.Message{assistant(call("i", "update_plan", planArgs(store.PlanItem{ID: "", Title: "x", Status: store.PlanPending}))), toolResult("i", "")}},
	}
	for _, c := range cases {
		// Act
		got := store.LatestPlan(append(append([]*store.Message{}, base...), c.tail...))

		// Assert
		if !reflect.DeepEqual(got, []store.PlanItem{good}) {
			t.Errorf("%s: plan = %+v, want the earlier valid plan", c.name, got)
		}
	}
}

func TestLatestPlanPicksTheLastCallOfAMessageAndCleansItems(t *testing.T) {
	// Arrange
	older := store.PlanItem{ID: "1", Title: "a", Status: store.PlanPending}
	newer := store.PlanItem{ID: " 2 ", Title: " b\x00 ", Status: store.PlanCompleted}
	msgs := []*store.Message{
		assistant(call("c1", "update_plan", planArgs(older)), call("c2", "update_plan", planArgs(newer))),
		toolResult("c1", ""), toolResult("c2", ""),
	}

	// Act
	got := store.LatestPlan(msgs)

	// Assert
	want := []store.PlanItem{{ID: "2", Title: "b\uFFFD", Status: store.PlanCompleted}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %+v, want %+v (trimmed and sanitized)", got, want)
	}
}

func TestLatestPlanIsNilWhenNoCallWroteOne(t *testing.T) {
	// Arrange / Act / Assert
	if got := store.LatestPlan(nil); got != nil {
		t.Errorf("no messages: %v", got)
	}
	msgs := []*store.Message{{Role: store.RoleUser, Content: "hi"}, assistant(call("c", "update_plan", planArgs()))}
	if got := store.LatestPlan(msgs); got != nil {
		t.Errorf("an unanswered call: %v", got)
	}
	// an empty but valid plan clears the todo list
	cleared := store.LatestPlan([]*store.Message{assistant(call("c", "update_plan", planArgs())), toolResult("c", "")})
	if cleared == nil || len(cleared) != 0 {
		t.Errorf("an empty plan = %#v, want a non-nil empty list", cleared)
	}
}

func TestToolKindNormalizesNamesTheWayApprovalsCompareThem(t *testing.T) {
	cases := map[string]string{
		"execute_command": "executecommand",
		"Execute-Command": "executecommand",
		"  write_file ":   "writefile",
		"WRITE_FILE":      "writefile",
		"already":         "already",
		"":                "",
	}
	for in, want := range cases {
		// Act
		got := store.ToolKind(in)

		// Assert
		if got != want {
			t.Errorf("ToolKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApprovedToolsAfterAddsTheKindOnlyForARunScopedApproval(t *testing.T) {
	paused := []string{"readfile"}
	approve := func(scope, tool string) *store.Approval {
		return &store.Approval{Decision: store.DecisionApprove, Scope: scope, ToolName: tool}
	}
	cases := []struct {
		name string
		a    *store.Approval
		want []string
	}{
		{"no approval", nil, []string{"readfile"}},
		{"run scope adds the normalized kind", approve(store.ApprovalScopeRun, "Execute_Command"), []string{"readfile", "executecommand"}},
		{"run scope, kind already approved", approve(store.ApprovalScopeRun, "read-file"), []string{"readfile"}},
		{"once scope adds nothing", approve(store.ApprovalScopeOnce, "execute_command"), []string{"readfile"}},
		{"no scope is once", approve("", "execute_command"), []string{"readfile"}},
		{"a denial adds nothing", &store.Approval{Decision: store.DecisionDeny, Scope: store.ApprovalScopeRun, ToolName: "execute_command"}, []string{"readfile"}},
		{"a pending approval adds nothing", &store.Approval{Scope: store.ApprovalScopeRun, ToolName: "execute_command"}, []string{"readfile"}},
	}
	for _, c := range cases {
		// Act
		got := store.ApprovedToolsAfter(paused, c.a)

		// Assert
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if !reflect.DeepEqual(paused, []string{"readfile"}) {
		t.Errorf("the paused run's list was modified: %v", paused)
	}
}

func TestApprovedToolsAfterNeverAliasesThePausedList(t *testing.T) {
	// Arrange
	paused := make([]string, 1, 8)
	paused[0] = "readfile"

	// Act
	got := store.ApprovedToolsAfter(paused, &store.Approval{Decision: store.DecisionApprove, Scope: store.ApprovalScopeRun, ToolName: "write"})
	got[0] = "changed"

	// Assert
	if paused[0] != "readfile" || paused[:2][1] != "" {
		t.Fatalf("the result shares memory with the paused list: %v", paused[:2])
	}
}

func TestApprovalPayloadHasWorksRecordShape(t *testing.T) {
	// Arrange
	created := time.Date(2026, 10, 6, 1, 2, 3, 4, time.FixedZone("x", 3600))
	resolved := created.Add(time.Minute)
	pending := &store.Approval{ID: "a1", ThreadID: "t", RunID: "r", ToolCallID: "c", ToolName: "execute_command", ToolInputPreview: "ls", Status: store.ApprovalPending, CreatedAt: created}
	decided := &store.Approval{ID: "a2", Kind: "custom", Status: store.ApprovalResolved, Decision: store.DecisionRespond, Scope: store.ApprovalScopeRun,
		Response: store.ApprovalResponse{SelectedChoice: "yes"}, CreatedAt: created, ResolvedAt: &resolved}

	// Act
	p := pending.Payload()
	d := decided.Payload()

	// Assert
	if p["kind"] != store.ApprovalKindRiskyTool || p["scope"] != store.ApprovalScopeOnce || p["decision"] != nil || p["response"] != nil || p["resolvedAt"] != nil {
		t.Errorf("pending payload = %v", p)
	}
	if p["createdAt"] != "2026-10-06T00:02:03.000000004Z" || p["threadId"] != "t" || p["toolCallId"] != "c" || p["toolInputPreview"] != "ls" {
		t.Errorf("pending payload fields = %v", p)
	}
	if d["kind"] != "custom" || d["scope"] != store.ApprovalScopeRun || d["decision"] != store.DecisionRespond || d["resolvedAt"] != "2026-10-06T00:03:03.000000004Z" {
		t.Errorf("decided payload = %v", d)
	}
	resp := d["response"].(map[string]any)
	if resp["selectedChoice"] != "yes" || resp["responseText"] != nil {
		t.Errorf("response = %v (an empty text must be null, not \"\")", resp)
	}
}

func TestApprovalResultTellsTheModelWhatTheUserDecided(t *testing.T) {
	cases := []struct {
		name     string
		decision string
		resp     store.ApprovalResponse
		want     string
	}{
		{"approve runs the tool", store.DecisionApprove, store.ApprovalResponse{}, ""},
		{"deny", store.DecisionDeny, store.ApprovalResponse{}, store.ApprovalDeniedResult},
		{"unknown decisions deny", "whatever", store.ApprovalResponse{}, store.ApprovalDeniedResult},
		{"respond with both", store.DecisionRespond, store.ApprovalResponse{SelectedChoice: " A ", ResponseText: " more "},
			"The user answered the pending question. Use this answer and continue: selected option: A; free text: more."},
		{"respond with a choice", store.DecisionRespond, store.ApprovalResponse{SelectedChoice: "A"},
			"The user answered the pending question. Use this answer and continue: selected option: A."},
		{"respond with nothing", store.DecisionRespond, store.ApprovalResponse{SelectedChoice: "  "},
			"The user answered the pending question. Use this answer and continue: no extra details provided."},
	}
	for _, c := range cases {
		// Act
		got := store.ApprovalResult(c.decision, c.resp)

		// Assert
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSameDecisionIgnoresWhitespaceButNotTheOutcome(t *testing.T) {
	// Arrange
	a := &store.Approval{Status: store.ApprovalResolved, Decision: store.DecisionRespond, Response: store.ApprovalResponse{SelectedChoice: "A", ResponseText: "text"}}
	pending := &store.Approval{Status: store.ApprovalPending, Decision: store.DecisionRespond}

	cases := []struct {
		name     string
		a        *store.Approval
		decision string
		resp     store.ApprovalResponse
		want     bool
	}{
		{"identical", a, store.DecisionRespond, store.ApprovalResponse{SelectedChoice: "A", ResponseText: "text"}, true},
		{"whitespace only", a, store.DecisionRespond, store.ApprovalResponse{SelectedChoice: " A ", ResponseText: "text\n"}, true},
		{"another decision", a, store.DecisionDeny, store.ApprovalResponse{SelectedChoice: "A", ResponseText: "text"}, false},
		{"another choice", a, store.DecisionRespond, store.ApprovalResponse{SelectedChoice: "B", ResponseText: "text"}, false},
		{"another text", a, store.DecisionRespond, store.ApprovalResponse{SelectedChoice: "A", ResponseText: "other"}, false},
		{"not resolved yet", pending, store.DecisionRespond, store.ApprovalResponse{}, false},
	}
	for _, c := range cases {
		// Act
		got := c.a.SameDecision(c.decision, c.resp)

		// Assert
		if got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMessageAttachmentsDecodesTheMetadataAndIgnoresGarbage(t *testing.T) {
	// Arrange
	withAttachments := &store.Message{Metadata: map[string]any{"attachments": []any{
		map[string]any{"artifactId": "a1", "name": "n.pdf", "path": "/workspace/n.pdf", "sizeBytes": float64(12), "mimeType": "application/pdf", "version": float64(2)},
	}}}

	// Act
	got := store.MessageAttachments(withAttachments)
	none := store.MessageAttachments(&store.Message{})
	garbage := store.MessageAttachments(&store.Message{Metadata: map[string]any{"attachments": "not a list"}})
	unmarshalable := store.MessageAttachments(&store.Message{Metadata: map[string]any{"attachments": make(chan int)}})

	// Assert
	want := []store.Attachment{{ArtifactID: "a1", Name: "n.pdf", Path: "/workspace/n.pdf", SizeBytes: 12, MimeType: "application/pdf", Version: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attachments = %+v", got)
	}
	if none != nil || garbage != nil || unmarshalable != nil {
		t.Errorf("none %v garbage %v unmarshalable %v", none, garbage, unmarshalable)
	}
}

func TestArtifactPathsMarksDeletedFilesAndSkipsEmptyPaths(t *testing.T) {
	// Arrange
	md := map[string]any{"artifacts": []any{
		map[string]any{"path": "/workspace/a.md", "change": store.ChangeCreated},
		map[string]any{"path": "/workspace/b.md", "change": store.ChangeDeleted},
		map[string]any{"path": "", "change": store.ChangeUpdated},
		map[string]any{"path": "/workspace/c.md"},
	}}

	// Act
	got := store.ArtifactPaths(md)

	// Assert
	want := []string{"/workspace/a.md", "/workspace/b.md (deleted)", "/workspace/c.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for name, m := range map[string]map[string]any{"none": nil, "missing key": {"x": 1}, "not a list": {"artifacts": "x"}, "unmarshalable": {"artifacts": make(chan int)}} {
		if store.ArtifactPaths(m) != nil {
			t.Errorf("%s: paths returned", name)
		}
	}
}

func TestMemoryLimitsOf(t *testing.T) {
	// Arrange
	l := store.MemoryLimits{Active: 50, Pending: 5}

	// Act / Assert
	if l.Of(store.MemoryActive) != 50 || l.Of(store.MemoryPending) != 5 || l.Of("deleted") != 0 || l.Of("") != 0 {
		t.Fatalf("limits %d %d %d", l.Of(store.MemoryActive), l.Of(store.MemoryPending), l.Of("deleted"))
	}
	if (store.MemoryLimits{}).Of(store.MemoryActive) != 0 {
		t.Error("the zero value limits something")
	}
}

func TestMemoryEntryCloneIsDeep(t *testing.T) {
	// Arrange
	imp, propImp := 0.5, 0.9
	deleted := time.Unix(100, 0)
	orig := &store.MemoryEntry{
		ID: "m", Tags: []string{"a"}, Importance: &imp, DeletedAt: &deleted,
		Metadata: map[string]any{"trigger": map[string]any{"words": []any{"x", map[string]any{"deep": "y"}}}},
		Proposal: &store.MemoryProposal{Title: "p", Tags: []string{"t"}, Importance: &propImp, Metadata: map[string]any{"k": []any{"v"}}},
	}

	// Act
	cp := orig.Clone()
	cp.Tags[0] = "changed"
	*cp.Importance = 0.1
	*cp.DeletedAt = time.Unix(999, 0)
	cp.Metadata["trigger"].(map[string]any)["words"].([]any)[1].(map[string]any)["deep"] = "changed"
	cp.Proposal.Tags[0] = "changed"
	*cp.Proposal.Importance = 0.1
	cp.Proposal.Metadata["k"].([]any)[0] = "changed"

	// Assert
	if orig.Tags[0] != "a" || *orig.Importance != 0.5 || !orig.DeletedAt.Equal(time.Unix(100, 0)) {
		t.Errorf("scalars shared: %+v", orig)
	}
	if orig.Metadata["trigger"].(map[string]any)["words"].([]any)[1].(map[string]any)["deep"] != "y" {
		t.Error("nested metadata shared")
	}
	if orig.Proposal.Tags[0] != "t" || *orig.Proposal.Importance != 0.9 || orig.Proposal.Metadata["k"].([]any)[0] != "v" {
		t.Errorf("proposal shared: %+v", orig.Proposal)
	}
}

func TestCloneOfNilIsNilAndOfEmptyEntriesIsUsable(t *testing.T) {
	// Arrange
	var e *store.MemoryEntry
	var p *store.MemoryProposal

	// Act
	cp := (&store.MemoryEntry{}).Clone()

	// Assert
	if e.Clone() != nil || p.Clone() != nil {
		t.Error("cloning nil returned a value")
	}
	if cp.Importance != nil || cp.DeletedAt != nil || cp.Proposal != nil || cp.Metadata == nil {
		t.Errorf("clone of the zero entry = %+v", cp)
	}
}

func TestErrorCodeForTerminalStatuses(t *testing.T) {
	cases := []struct{ status, code, want string }{
		{store.StatusFailed, "", "internal"},
		{store.StatusFailed, "model_error", "model_error"},
		{store.StatusInterrupted, "", "interrupted"},
		{store.StatusInterrupted, "max_steps", "max_steps"},
		{store.StatusCompleted, "ignored", ""},
		{store.StatusQueued, "x", ""},
	}
	for _, c := range cases {
		// Act
		got := store.ErrorCodeFor(c.status, c.code)

		// Assert
		if got != c.want {
			t.Errorf("ErrorCodeFor(%q, %q) = %q, want %q", c.status, c.code, got, c.want)
		}
	}
}

func TestIsTerminalIsFalseOnlyForQueuedAndRunning(t *testing.T) {
	for status, want := range map[string]bool{
		store.StatusQueued: false, store.StatusRunning: false,
		store.StatusCompleted: true, store.StatusFailed: true, store.StatusInterrupted: true, "anything else": true,
	} {
		// Act
		got := store.IsTerminal(status)

		// Assert
		if got != want {
			t.Errorf("IsTerminal(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestIsSteerReadsTheMetadataFlag(t *testing.T) {
	cases := []struct {
		name string
		md   map[string]any
		want bool
	}{
		{"flag set", map[string]any{"steer": true}, true},
		{"flag false", map[string]any{"steer": false}, false},
		{"flag of another type", map[string]any{"steer": "true"}, false},
		{"no metadata", nil, false},
	}
	for _, c := range cases {
		// Act
		got := (&store.Message{Metadata: c.md}).IsSteer()

		// Assert
		if got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestRolesAndLabels(t *testing.T) {
	// Arrange / Act / Assert
	for r, want := range map[string]bool{store.RoleOwner: true, store.RoleMember: true, store.RoleViewer: true, "admin": false, "": false} {
		if store.ValidRole(r) != want {
			t.Errorf("ValidRole(%q) = %v", r, !want)
		}
	}
	atLeast := []struct {
		have, want string
		ok         bool
	}{
		{store.RoleOwner, store.RoleMember, true}, {store.RoleMember, store.RoleMember, true}, {store.RoleViewer, store.RoleMember, false},
		{store.RoleMember, store.RoleOwner, false}, {"unknown", store.RoleViewer, false}, {"", "", false}, {store.RoleViewer, store.RoleViewer, true},
	}
	for _, c := range atLeast {
		if store.RoleAtLeast(c.have, c.want) != c.ok {
			t.Errorf("RoleAtLeast(%q, %q) = %v", c.have, c.want, !c.ok)
		}
	}
}

func TestUserLabelAndAvatarPrecedence(t *testing.T) {
	cases := []struct {
		name          string
		u             store.User
		label, avatar string
	}{
		{"login only", store.User{Login: "sub-1"}, "sub-1", ""},
		{"provider name", store.User{Login: "sub-1", Name: "Ada Lovelace", AvatarURL: "https://idp/a.png"}, "Ada Lovelace", "https://idp/a.png"},
		{"display name wins", store.User{Login: "sub-1", Name: "Ada Lovelace", DisplayName: "Ada"}, "Ada", ""},
		{"an uploaded avatar supersedes the provider's", store.User{Login: "a/b", AvatarURL: "https://idp/a.png", AvatarVersion: "v3"}, "a/b", "/api/users/a%2Fb/avatar?v=v3"},
	}
	for _, c := range cases {
		// Act
		label, avatar := c.u.Label(), c.u.Avatar()

		// Assert
		if label != c.label || avatar != c.avatar {
			t.Errorf("%s: label %q avatar %q", c.name, label, avatar)
		}
	}
}

func TestVouchedIsTrueWhenAnyCheckVouches(t *testing.T) {
	// Arrange
	run := &store.Run{ID: "r1"}
	no := func(*store.Run) bool { return false }
	seen := ""
	yes := func(r *store.Run) bool { seen = r.ID; return true }

	cases := []struct {
		name   string
		checks []func(*store.Run) bool
		want   bool
	}{
		{"none", nil, false},
		{"all no", []func(*store.Run) bool{no, no}, false},
		{"a nil check is skipped", []func(*store.Run) bool{nil, no}, false},
		{"one yes", []func(*store.Run) bool{no, nil, yes}, true},
	}
	for _, c := range cases {
		// Act
		got := store.Vouched(run, c.checks)

		// Assert
		if got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	if seen != "r1" {
		t.Error("the check did not receive the run")
	}
}
