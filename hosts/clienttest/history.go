package clienttest

import (
	"testing"

	"github.com/7ar-chat/harness/hosts/client"
)

// extras returns c as client.Extras.
func extras(t *testing.T, c client.Client) client.Extras {
	t.Helper()
	e, ok := c.(client.Extras)
	if !ok {
		t.Fatalf("%T does not implement client.Extras", c)
	}
	return e
}

// converse sends the messages one after the other, waiting for each answer.
func converse(t *testing.T, r *rig, c client.Client, threadID string, texts ...string) {
	t.Helper()
	for _, text := range texts {
		run := mustSend(t, r, c, threadID, text, client.SendOptions{})
		finished(t, stream(t, r, c, run.ID, 0))
		waitIdle(t, r, c, threadID)
	}
}

// contents lists "role:content" of the thread's messages.
func contents(t *testing.T, r *rig, c client.Client, threadID string) []string {
	t.Helper()
	th, err := c.GetThread(r.ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range th.Messages {
		out = append(out, m.Role+":"+m.Content)
	}
	return out
}

// runHistory runs the scenarios of the history operations of client.Extras.
func runHistory(t *testing.T, mk Factory) {
	t.Run("undo and redo", func(t *testing.T) { testUndoRedo(t, mk) })
	t.Run("redo ends with a new message", func(t *testing.T) { testRedoEndsWithNewMessage(t, mk) })
	t.Run("history refused during a run", func(t *testing.T) { testHistoryBusy(t, mk) })
	t.Run("fork", func(t *testing.T) { testFork(t, mk) })
	t.Run("compact", func(t *testing.T) { testCompact(t, mk) })
	t.Run("history ownership", func(t *testing.T) { testHistoryOwnership(t, mk) })
	t.Run("undo restores the files", func(t *testing.T) { testUndoFiles(t, mk) })
}

func equal(a, b []string) bool {
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

func testUndoRedo(t *testing.T, mk Factory) {
	// Arrange
	r := newRig(t, mk, rigOpts{})
	id := newThread(t, r, r.alice)
	converse(t, r, r.alice, id, "first", "second")
	ex := extras(t, r.alice)

	// Act
	u1, err1 := ex.Undo(r.ctx, id)
	after1 := contents(t, r, r.alice, id)
	u2, err2 := ex.Undo(r.ctx, id)
	after2 := contents(t, r, r.alice, id)
	_, err3 := ex.Undo(r.ctx, id)
	r1, errR1 := ex.Redo(r.ctx, id)
	r2, errR2 := ex.Redo(r.ctx, id)
	_, errR3 := ex.Redo(r.ctx, id)
	restored := contents(t, r, r.alice, id)

	// Assert
	if err1 != nil || u1.Prompt != "second" || !u1.CanUndo || !u1.CanRedo || len(u1.Files) != 0 {
		t.Fatalf("first Undo = %+v, %v", u1, err1)
	}
	if !equal(after1, []string{"user:first", "assistant:Hi there"}) {
		t.Fatalf("after the first Undo: %v", after1)
	}
	if err2 != nil || u2.Prompt != "first" || u2.CanUndo || !u2.CanRedo || len(after2) != 0 {
		t.Fatalf("second Undo = %+v, %v; messages %v", u2, err2, after2)
	}
	wantErr(t, "Undo of an empty thread", err3, client.ErrConflict)
	if errR1 != nil || r1.Prompt != "first" || !r1.CanUndo || !r1.CanRedo {
		t.Fatalf("first Redo = %+v, %v", r1, errR1)
	}
	if errR2 != nil || r2.Prompt != "second" || r2.CanRedo {
		t.Fatalf("second Redo = %+v, %v", r2, errR2)
	}
	wantErr(t, "Redo with nothing undone", errR3, client.ErrConflict)
	if want := []string{"user:first", "assistant:Hi there", "user:second", "assistant:Hi there"}; !equal(restored, want) {
		t.Fatalf("after Redo: %v, want %v", restored, want)
	}
}

func testRedoEndsWithNewMessage(t *testing.T, mk Factory) {
	// Arrange
	r := newRig(t, mk, rigOpts{})
	id := newThread(t, r, r.alice)
	converse(t, r, r.alice, id, "first", "second")
	ex := extras(t, r.alice)
	if _, err := ex.Undo(r.ctx, id); err != nil {
		t.Fatal(err)
	}

	// Act
	converse(t, r, r.alice, id, "third")
	_, err := ex.Redo(r.ctx, id)

	// Assert
	wantErr(t, "Redo after a new message", err, client.ErrConflict)
	if want := []string{"user:first", "assistant:Hi there", "user:third", "assistant:Hi there"}; !equal(contents(t, r, r.alice, id), want) {
		t.Fatalf("messages %v", contents(t, r, r.alice, id))
	}
}

func testHistoryBusy(t *testing.T, mk Factory) {
	// Arrange
	model, started, release := blocking()
	defer release()
	r := newRig(t, mk, rigOpts{model: model})
	id := newThread(t, r, r.alice)
	mustSend(t, r, r.alice, id, "work", client.SendOptions{})
	waitFor(t, started)
	ex := extras(t, r.alice)

	// Act
	_, undo := ex.Undo(r.ctx, id)
	_, redo := ex.Redo(r.ctx, id)
	_, compact := ex.Compact(r.ctx, id)
	release()
	waitIdle(t, r, r.alice, id)

	// Assert
	wantErr(t, "Undo during a run", undo, client.ErrConflict)
	wantErr(t, "Redo during a run", redo, client.ErrConflict)
	wantErr(t, "Compact during a run", compact, client.ErrConflict)
}

func testFork(t *testing.T, mk Factory) {
	// Arrange
	r := newRig(t, mk, rigOpts{})
	id := newThread(t, r, r.alice)
	title := "Plan"
	if _, err := r.alice.RenameThread(r.ctx, id, client.ThreadPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	converse(t, r, r.alice, id, "first", "second")
	src, err := r.alice.GetThread(r.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	ex := extras(t, r.alice)

	// Act
	whole, errWhole := ex.Fork(r.ctx, id, "")
	part, errPart := ex.Fork(r.ctx, id, src.Messages[1].ID)
	_, errMissing := ex.Fork(r.ctx, id, "no-such-message")

	// Assert
	if errWhole != nil || whole.ID == id || whole.Title != "Plan (fork)" {
		t.Fatalf("Fork whole = %+v, %v", whole, errWhole)
	}
	if got := contents(t, r, r.alice, whole.ID); len(got) != 4 || got[2] != "user:second" {
		t.Fatalf("whole fork messages %v", got)
	}
	if errPart != nil {
		t.Fatal(errPart)
	}
	if got := contents(t, r, r.alice, part.ID); !equal(got, []string{"user:first", "assistant:Hi there"}) {
		t.Fatalf("partial fork messages %v", got)
	}
	wantErr(t, "Fork at an unknown message", errMissing, client.ErrNotFound)
	if got := contents(t, r, r.alice, id); len(got) != 4 {
		t.Fatalf("the source changed: %v", got)
	}
	// The fork continues on its own.
	converse(t, r, r.alice, part.ID, "third")
	if got := contents(t, r, r.alice, part.ID); len(got) != 4 {
		t.Fatalf("fork after a new message: %v", got)
	}
}

func testCompact(t *testing.T, mk Factory) {
	// Arrange
	r := newRig(t, mk, rigOpts{})
	empty := newThread(t, r, r.alice)
	id := newThread(t, r, r.alice)
	converse(t, r, r.alice, id, "hello")
	ex := extras(t, r.alice)

	// Act
	none, noneErr := ex.Compact(r.ctx, empty)
	res, err := ex.Compact(r.ctx, id)
	th := waitIdle(t, r, r.alice, id)

	// Assert
	if noneErr != nil || none.Compacted {
		t.Fatalf("Compact of an empty thread = %+v, %v: nothing to summarise", none, noneErr)
	}
	if err != nil || !res.Compacted || res.TokensBefore == 0 || res.TokensAfter > res.TokensBefore {
		t.Fatalf("Compact = %+v, %v", res, err)
	}
	if th.ActiveRun != nil || len(th.Messages) != 2 {
		t.Fatalf("the history must stay readable: %+v", th)
	}
}

func testHistoryOwnership(t *testing.T, mk Factory) {
	// Arrange
	r := newRig(t, mk, rigOpts{})
	id := newThread(t, r, r.alice)
	converse(t, r, r.alice, id, "private")
	ex := extras(t, r.bob)

	// Act
	_, compact := ex.Compact(r.ctx, id)
	_, undo := ex.Undo(r.ctx, id)
	_, redo := ex.Redo(r.ctx, id)
	_, fork := ex.Fork(r.ctx, id, "")

	// Assert
	for what, got := range map[string]error{"Compact": compact, "Undo": undo, "Redo": redo, "Fork": fork} {
		wantErr(t, what, got, client.ErrNotFound)
	}
	if got := contents(t, r, r.alice, id); len(got) != 2 {
		t.Fatalf("alice's thread changed: %v", got)
	}
}
