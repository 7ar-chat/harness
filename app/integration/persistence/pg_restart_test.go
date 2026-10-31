package persistence_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
)

func TestAConversationWithArtifactVersionsSurvivesARestartOverTheSameDatabase(t *testing.T) {
	// Arrange: process 1 runs a turn whose write_file produces /workspace/notes.txt (version 1).
	db := newDatabase(t)
	p1 := startPGProcess(t, db.open(), processOpts{model: scriptedModel()})
	thread := p1.newThread("alice")
	run1 := p1.send("alice", thread, "Create the notes [write v1]")
	events1 := p1.finishedRun("alice", run1)
	before := p1.thread("alice", thread)
	ctx := context.Background()
	artifacts1, err := p1.H.Artifacts.ListArtifacts(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts1) != 1 || artifacts1[0].Path != notesPath || artifacts1[0].Version != 1 {
		t.Fatalf("artifacts before the restart: %+v", artifacts1)
	}
	p1.stop()

	// Act: process 2 opens a new pool on the same database; its second turn rewrites the file (version 2).
	model2 := scriptedModel()
	p2 := startPGProcess(t, db.open(), processOpts{model: model2})
	after := p2.thread("alice", thread)
	listAfter := p2.threads("alice")
	replay1 := p2.stream("alice", run1)
	run2 := p2.send("alice", thread, "Update the notes [write v2]")
	events2 := p2.finishedRun("alice", run2)

	// Assert: threads, title, messages and the first run's events are intact.
	if len(listAfter) != 1 || listAfter[0].ID != thread || listAfter[0].Title != "A title" {
		t.Fatalf("threads after the restart: %+v", listAfter)
	}
	if !equalJSON(before.Messages, after.Messages) || len(after.Messages) != 4 {
		t.Fatalf("messages changed over the restart:\nbefore %v\nafter  %v", before.Messages, after.Messages)
	}
	// Postgres keeps event payloads as jsonb, which normalises key order and spacing: compare ids and decoded payloads.
	if !sameEvents(replay1, events1) {
		t.Fatalf("replayed events differ:\nbefore %v\nafter  %v", events1, replay1)
	}
	cut := events1[len(events1)/2].ID
	n, _ := strconv.Atoi(cut)
	if resumed := p2.stream("alice", run1, "Last-Event-ID", cut); !sameEvents(resumed, events1[n:]) {
		t.Fatalf("resume after %s: got %v, want %v", cut, resumed, events1[n:])
	}
	if r := p2.run("alice", run1); r.Status != "completed" {
		t.Fatalf("first run after the restart: %+v", r)
	}

	// Assert: the second turn saw the history of the first one, and the second run completed.
	calls := mainCalls(model2)
	if len(calls) != 2 || len(calls[0].Messages) < 5 {
		t.Fatalf("agent calls in process 2: %d", len(calls))
	}
	if got := conversation(calls[0]); len(got) != 5 || got[0].Content != "Create the notes [write v1]" || got[1].CallIDs[0] != "call-v1" ||
		got[2].Role != "tool" || got[3].Content != "Tool finished" || got[4].Content != "Update the notes [write v2]" {
		t.Fatalf("history sent to the model: %+v", got)
	}
	if events2[len(events2)-1].Type() != "RUN_FINISHED" {
		t.Fatalf("second run: %v", types(events2))
	}

	// Assert: the artifact has two versions, the first one is still retrievable byte for byte, and they belong to their runs.
	list, err := p2.H.Artifacts.ListArtifacts(ctx, thread)
	if err != nil || len(list) != 1 {
		t.Fatalf("artifacts after the restart: %+v, %v", list, err)
	}
	a := list[0]
	if a.Path != notesPath || a.Name != "notes.txt" || a.Version != 2 || a.Deleted || a.UserLogin != "alice" || a.RunID != run2 {
		t.Fatalf("artifact = %+v", a)
	}
	v1, info1, err := p2.H.Artifacts.Content(ctx, a, 1)
	if err != nil || string(v1) != notesV1 || info1.Version != 1 || info1.RunID != run1 {
		t.Fatalf("version 1 = %q %+v, %v", v1, info1, err)
	}
	v2, info2, err := p2.H.Artifacts.Content(ctx, a, 0)
	if err != nil || string(v2) != notesV2 || info2.Version != 2 || info2.RunID != run2 {
		t.Fatalf("latest version = %q %+v, %v", v2, info2, err)
	}
	versions, err := p2.H.Artifacts.ListArtifactVersions(ctx, a.ID)
	if err != nil || len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 1 || versions[0].SHA256 == versions[1].SHA256 {
		t.Fatalf("versions = %+v, %v", versions, err)
	}
}

// sameEvents compares two captures of a stream: same ids in the same order and equal JSON payloads.
func sameEvents(a, b []sseEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		var x, y any
		if a[i].ID != b[i].ID || json.Unmarshal([]byte(a[i].Data), &x) != nil || json.Unmarshal([]byte(b[i].Data), &y) != nil || !equalJSON(x, y) {
			return false
		}
	}
	return true
}
