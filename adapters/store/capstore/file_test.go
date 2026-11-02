package capstore_test

// Persistence of the file-backed store (capstore.Open). The shared behavioural cases run against it in
// conformance_test.go; these cover what only a file can get wrong.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/capstore"
	"github.com/7ar-chat/harness/core/capabilities"
)

func TestStateSurvivesAReopenIncludingSealedSecrets(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "sub", "capabilities.json")
	clk := newClock()
	f, err := capstore.Open(path, clk.Now)
	must(t, err)
	must(t, f.SeedCatalog(ctx, []capabilities.Entry{entry("alpha")}))
	cat := put(t, f, catalogInstall(capabilities.ScopeProject, "p1", "alpha"))
	custom := put(t, f, customInstall(capabilities.ScopeUser, "bob", "mine"))
	gone := put(t, f, customInstall(capabilities.ScopeUser, "bob", "gone"))
	must(t, f.DeleteInstall(ctx, gone.ID))

	// Act
	g, err := capstore.Open(path, clk.Now)
	must(t, err)

	// Assert
	for _, want := range []*capabilities.Install{cat, custom} {
		got, err := g.GetInstall(ctx, want.ID)
		must(t, err)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("after reopen %+v, want %+v", got, want)
		}
	}
	_, err = g.GetInstall(ctx, gone.ID)
	wantErr(t, "deleted install", err, capabilities.ErrNotFound)
	e, err := g.GetCatalogEntry(ctx, "alpha")
	must(t, err)
	if !e.Builtin || e.URL != entry("alpha").URL {
		t.Fatalf("catalog entry after reopen: %+v", e)
	}
	// the uniqueness rules still hold on the reloaded state
	_, err = g.PutInstall(ctx, customInstall(capabilities.ScopeUser, "bob", "mine"))
	wantErr(t, "name after reopen", err, capabilities.ErrConflict)
	again := put(t, g, catalogInstall(capabilities.ScopeProject, "p1", "alpha"))
	if again.ID != cat.ID {
		t.Fatalf("catalog install after reopen got a new id %q, want %q", again.ID, cat.ID)
	}
}

func TestOpenRefusesACorruptOrForeignFileNamingItAndLeavesItUntouched(t *testing.T) {
	for name, content := range map[string]string{
		"not json":        "{broken",
		"foreign version": `{"version":99,"catalog":[],"installs":[]}`,
		"no install id":   `{"version":1,"installs":[{"scope":"project","scopeId":"p","name":"x"}]}`,
		"unknown scope":   `{"version":1,"installs":[{"id":"1","scope":"team","scopeId":"p","name":"x"}]}`,
		"same name twice": `{"version":1,"installs":[{"id":"1","scope":"user","scopeId":"p","name":"x"},{"id":"2","scope":"user","scopeId":"p","name":"x"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			path := filepath.Join(t.TempDir(), "capabilities.json")
			must(t, os.WriteFile(path, []byte(content), 0o600))

			// Act
			f, err := capstore.Open(path, nil)

			// Assert
			if err == nil || f != nil {
				t.Fatalf("Open = %v, %v; want an error", f, err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q does not name %s", err, path)
			}
			b, rerr := os.ReadFile(path)
			must(t, rerr)
			if string(b) != content {
				t.Fatalf("file was modified: %q", b)
			}
		})
	}
}

func TestAFailedWriteIsReportedAndLeavesTheStoreAsItWas(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	path := filepath.Join(dir, "capabilities.json")
	f, err := capstore.Open(path, nil)
	must(t, err)
	kept := put(t, f, customInstall(capabilities.ScopeProject, "p1", "kept"))
	must(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := os.Create(filepath.Join(dir, "probe")); err == nil {
		t.Skip("directory permissions do not apply (root?)")
	}

	// Act
	_, putErr := f.PutInstall(ctx, customInstall(capabilities.ScopeProject, "p1", "lost"))
	delErr := f.DeleteInstall(ctx, kept.ID)

	// Assert
	if putErr == nil || delErr == nil {
		t.Fatalf("errors = %v, %v; want both reported", putErr, delErr)
	}
	list, err := f.ListInstalls(ctx, capabilities.ScopeProject, "p1")
	must(t, err)
	if len(list) != 1 || list[0].Name != "kept" {
		t.Fatalf("installs = %+v, want memory rolled back to the persisted state", list)
	}
}

func TestSecretsAreNotReadableByOthersAndNoTempFileRemains(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	path := filepath.Join(dir, "capabilities.json")
	f, err := capstore.Open(path, nil)
	must(t, err)

	// Act
	put(t, f, customInstall(capabilities.ScopeProject, "p1", "mine"))

	// Assert
	st, err := os.Stat(path)
	must(t, err)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", st.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	must(t, err)
	if len(entries) != 1 {
		t.Fatalf("directory holds %d files, want only the state file", len(entries))
	}
}

func TestSeedingAnUnchangedCatalogDoesNotRewriteTheFile(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "capabilities.json")
	f, err := capstore.Open(path, nil)
	must(t, err)
	must(t, f.SeedCatalog(ctx, []capabilities.Entry{entry("alpha")}))
	before, err := os.Stat(path)
	must(t, err)

	// Act
	must(t, f.SeedCatalog(ctx, []capabilities.Entry{entry("alpha")}))

	// Assert
	after, err := os.Stat(path)
	must(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("file was replaced by a seed that changed nothing")
	}
}
