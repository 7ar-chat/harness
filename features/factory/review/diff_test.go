package review_test

import (
	"reflect"
	"testing"

	"github.com/7ar-chat/harness/features/factory/review"
)

func TestParseDiff(t *testing.T) {
	// Arrange
	diff := `diff --git a/main.go b/main.go
index 1..2 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
 package main
-var a = 1
+var a = 2
+var b = 3
 func main() {}
\ No newline at end of file
diff --git a/old.txt b/new dir/new.txt
similarity index 90%
rename from old.txt
rename to new dir/new.txt
index 3..4 100644
--- a/old.txt
+++ b/new dir/new.txt
@@ -1 +1 @@
-hello
+hello world
diff --git a/logo.png b/logo.png
new file mode 100644
index 0000000..5
Binary files /dev/null and b/logo.png differ
diff --git a/run.sh b/run.sh
old mode 100644
new mode 100755
diff --git a/gone.go b/gone.go
deleted file mode 100644
index 5..0
--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
--- not a header
-package gone
diff --git a/pure.txt b/moved.txt
similarity index 100%
rename from pure.txt
rename to moved.txt
`

	// Act
	got := review.ParseDiff(diff)

	// Assert
	type row struct {
		Path, OldPath          string
		Added, Removed         int
		Binary, Renamed        bool
		New, Deleted, ModeChgd bool
	}
	var rows []row
	for _, f := range got {
		rows = append(rows, row{f.Path, f.OldPath, f.Added, f.Removed, f.Binary, f.Renamed, f.New, f.Deleted, f.ModeChanged})
	}
	want := []row{
		{Path: "main.go", Added: 2, Removed: 1},
		{Path: "new dir/new.txt", OldPath: "old.txt", Added: 1, Removed: 1, Renamed: true},
		{Path: "logo.png", Binary: true, New: true},
		{Path: "run.sh", ModeChgd: true},
		{Path: "gone.go", Removed: 2, Deleted: true},
		{Path: "moved.txt", OldPath: "pure.txt", Renamed: true},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("files =\n%+v\nwant\n%+v", rows, want)
	}
	if m := got[3]; m.OldMode != "100644" || m.NewMode != "100755" {
		t.Errorf("modes = %q -> %q", m.OldMode, m.NewMode)
	}
	if l := got[0].AddedLines; len(l) != 2 || l[0].Line != 2 || l[0].Text != "var a = 2" || l[1].Line != 3 {
		t.Errorf("added lines = %+v", l)
	}
}

func TestParseDiffPlainUnifiedAndEmpty(t *testing.T) {
	// Arrange
	plain := "--- a/x.txt\t2026-01-01\n+++ b/x.txt\t2026-01-02\n@@ -1 +1,2 @@\n a\n+b\n--- a/y.txt\n+++ b/y.txt\n@@ -0,0 +1 @@\n+c\n"

	// Act
	got := review.ParseDiff(plain)

	// Assert
	if len(got) != 2 || got[0].Path != "x.txt" || got[0].Added != 1 || got[1].Path != "y.txt" || got[1].Added != 1 {
		t.Fatalf("files = %+v", got)
	}
	if n := len(review.ParseDiff("")); n != 0 {
		t.Errorf("empty diff gave %d files", n)
	}
}
