//go:build unix

package clienttest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitIn runs git in dir with a fixed identity.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@localhost"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func testUndoFiles(t *testing.T, mk Factory) {
	// Arrange: a git workspace; the model writes out.md during the run.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q")
	if err := os.WriteFile(filepath.Join(ws, "keep.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, ws, "add", ".")
	gitIn(t, ws, "commit", "-q", "-m", "init")
	r := newRig(t, mk, rigOpts{local: true, workspace: ws, model: scripted("write_file", `{"path":"out.md","content":"hello files"}`)})
	id := newThread(t, r, r.alice)
	converse(t, r, r.alice, id, "write it")
	out := filepath.Join(ws, "out.md")
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("the run did not write the file: %v", err)
	}
	ex := extras(t, r.alice)

	// Act
	undone, undoErr := ex.Undo(r.ctx, id)
	_, goneErr := os.Stat(out)
	redone, redoErr := ex.Redo(r.ctx, id)
	back, backErr := os.ReadFile(out)

	// Assert
	if undoErr != nil || undone.Prompt != "write it" || len(undone.Files) != 1 || undone.Files[0] != "out.md" {
		t.Fatalf("Undo = %+v, %v", undone, undoErr)
	}
	if !os.IsNotExist(goneErr) {
		t.Fatalf("out.md still there after Undo: %v", goneErr)
	}
	if redoErr != nil || redone.Prompt != "write it" || len(redone.Files) != 1 || redone.Files[0] != "out.md" {
		t.Fatalf("Redo = %+v, %v", redone, redoErr)
	}
	if backErr != nil || string(back) != "hello files" {
		t.Fatalf("out.md after Redo = %q, %v", back, backErr)
	}
	if kept, err := os.ReadFile(filepath.Join(ws, "keep.txt")); err != nil || string(kept) != "kept" {
		t.Fatalf("keep.txt = %q, %v", kept, err)
	}
}
