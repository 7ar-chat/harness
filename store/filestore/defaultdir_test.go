package filestore

import (
	"path/filepath"
	"testing"
)

func TestDefaultDirIsTheSessionsFolderOfTheUserDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOCAL_STATE_DIR", "")
	t.Setenv("HAR_HOME", home)
	t.Setenv("HARNESS_NO_USER_DIR", "")
	t.Setenv("XDG_STATE_HOME", "/xdg")

	if got := DefaultDir(); got != filepath.Join(home, "sessions") {
		t.Fatalf("default dir %q", got)
	}
	t.Setenv("LOCAL_STATE_DIR", "/explicit")
	if got := DefaultDir(); got != "/explicit" {
		t.Fatalf("override %q", got)
	}
	t.Setenv("LOCAL_STATE_DIR", "")
	t.Setenv("HARNESS_NO_USER_DIR", "1")
	if got := DefaultDir(); got != filepath.Join("/xdg", "harness") {
		t.Fatalf("hermetic %q", got)
	}
}
