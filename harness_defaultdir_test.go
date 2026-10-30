package harness_test

import (
	"path/filepath"
	"testing"

	"github.com/GhaziBenDahmane/harness"
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
