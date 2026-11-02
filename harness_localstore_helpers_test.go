package harness_test

import (
	"os"
	"path/filepath"
	"testing"
)

// copyJournal copies the journal as it is on disk right now into dst.
func copyJournal(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(src, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "journal.jsonl"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}
