package httpapi

import (
	"io/fs"
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
	if files["index.html"] == "" {
		t.Skip("the web UI is not built (make web)")
	}
	return files
}

// uiAssets lists the built files index.html references (their /ui/ paths without the prefix).
func uiAssets(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(readUI(t)["index.html"], -1) {
		out = append(out, strings.TrimPrefix(m[1], "/ui/"))
	}
	return out
}

func TestUIReferencesNoExternalResources(t *testing.T) {
	external := regexp.MustCompile(`(?i)https?://`)
	for name, src := range readUI(t) {
		if !strings.HasSuffix(name, ".html") && !strings.HasSuffix(name, ".css") {
			continue // bundled libraries mention URLs in their JS (docs, SVG namespaces)
		}
		if found := external.FindString(src); found != "" {
			t.Errorf("%s contains %q: the UI must be self-contained", name, found)
		}
	}
}

func TestUIHTMLLoadsOnlyItsOwnBuiltAssets(t *testing.T) {
	files := readUI(t)
	html := files["index.html"]
	assets := uiAssets(t)
	if len(assets) < 2 {
		t.Fatalf("index.html references %v, want a script and a stylesheet", assets)
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if !strings.HasPrefix(m[1], "/ui/") {
			t.Errorf("index.html references %q", m[1])
		}
	}
	for _, a := range assets {
		if _, ok := files[a]; !ok {
			t.Errorf("index.html references %q, which is not built", a)
		}
	}
	if regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]`).MatchString(html) || strings.Contains(html, "<style") || strings.Contains(html, "style=") {
		t.Error("inline script or style is blocked by the CSP")
	}
}

func TestUIStaysSmall(t *testing.T) {
	total := 0
	for _, src := range readUI(t) {
		total += len(src)
	}
	if total > 6<<20 {
		t.Fatalf("the UI is %d bytes; the budget is 6 MB", total)
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
