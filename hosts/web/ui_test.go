package httpserver

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readUI(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := fs.WalkDir(UI(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(UI(), p)
		files[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"index.html", "app.js", "app.css"} {
		if files[want] == "" {
			t.Fatalf("the UI lacks %s", want)
		}
	}
	return files
}

func TestUIReferencesNoExternalResources(t *testing.T) {
	// Arrange
	external := regexp.MustCompile(`(?i)(https?:)?//[a-z0-9.-]+\.[a-z]{2,}|@import|<iframe|\beval\(|new Function\(|\.innerHTML|document\.write|<script>[^<]|\son[a-z]+="`)

	for name, src := range readUI(t) {
		// Act
		found := external.FindString(src)

		// Assert
		if found != "" {
			t.Errorf("%s contains %q: the UI must be self-contained and free of inline code", name, found)
		}
	}
}

func TestUIHTMLLoadsOnlyItsOwnScriptAndStylesheet(t *testing.T) {
	html := readUI(t)["index.html"]
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if !strings.HasPrefix(m[1], "/ui/") {
			t.Errorf("index.html references %q", m[1])
		}
	}
	if strings.Contains(html, "style=") {
		t.Error("inline style attributes are blocked by the CSP")
	}
}

func TestUIStaysSmall(t *testing.T) {
	total := 0
	for _, src := range readUI(t) {
		total += strings.Count(src, "\n") + 1
	}
	if total > 600 {
		t.Fatalf("the UI is %d lines; the budget is 600", total)
	}
}

func TestUIScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	path := filepath.Join(t.TempDir(), "app.js")
	if err := os.WriteFile(path, []byte(readUI(t)["app.js"]), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}

func TestDispositionCannotBreakOutOfTheHeader(t *testing.T) {
	for _, name := range []string{"a\"b.txt", "a;b", "line\r\nX-Evil: 1", "../../x", "", "..", "naïve é.txt"} {
		got := dispositionFor(name)
		if !strings.HasPrefix(got, "attachment") || strings.ContainsAny(got, "\r\n") || strings.Contains(got, "/") {
			t.Errorf("dispositionFor(%q) = %q", name, got)
		}
	}
}
