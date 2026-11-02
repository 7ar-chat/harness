package harness_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/7ar-chat/harness"
)

func TestDefaultDirIsTheSessionsFolderOfTheUserDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOCAL_STATE_DIR", "")
	t.Setenv("HAR_HOME", home)
	t.Setenv("HARNESS_NO_USER_DIR", "")
	t.Setenv("XDG_STATE_HOME", "/xdg")

	if got := harness.DefaultLocalStoreDir(); got != filepath.Join(home, "sessions") {
		t.Fatalf("default dir %q", got)
	}
	t.Setenv("LOCAL_STATE_DIR", "/explicit")
	if got := harness.DefaultLocalStoreDir(); got != "/explicit" {
		t.Fatalf("override %q", got)
	}
	t.Setenv("LOCAL_STATE_DIR", "")
	t.Setenv("HARNESS_NO_USER_DIR", "1")
	if got := harness.DefaultLocalStoreDir(); got != filepath.Join("/xdg", "harness") {
		t.Fatalf("hermetic %q", got)
	}
}

func TestDefaultDirFallsBackToTheHomeStateDirectoryWhenTheUserDirectoryIsDisabled(t *testing.T) {
	// Arrange: no explicit dir, no user directory, no XDG state home
	home := t.TempDir()
	t.Setenv("LOCAL_STATE_DIR", "")
	t.Setenv("HARNESS_NO_USER_DIR", "1")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)

	// Act
	got := harness.DefaultLocalStoreDir()

	// Assert
	if want := filepath.Join(home, ".local", "state", "harness"); got != want {
		t.Fatalf("default dir %q, want %q", got, want)
	}
}

func TestOpenLocalStoreWithoutADirectoryUsesTheDefaultOne(t *testing.T) {
	// Arrange
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("LOCAL_STATE_DIR", dir)

	// Act
	st, err := harness.OpenLocalStore("")
	if err != nil {
		t.Fatal(err)
	}
	th, err := st.CreateThread(context.Background(), "alice", "persisted")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := harness.OpenLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()

	// Assert: the thread written through the default directory is found when it is opened explicitly
	got, err := again.GetThread(context.Background(), "alice", th.ID)
	if err != nil || got.Title != "persisted" {
		t.Fatalf("thread %+v err %v", got, err)
	}
}
