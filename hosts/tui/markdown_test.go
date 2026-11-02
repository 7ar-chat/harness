package tui

import (
	"strings"
	"testing"
)

func TestMarkdownBlocks(t *testing.T) {
	src := "# Title\n\nSome *em* and **strong** and `code` with [a link](http://x.io).\n\n1. one\n2. two\n   - nested\n\n> quoted\n\n---\n\n| a | b |\n|---|---|\n| 1 | 22 |\n"
	got := RenderMarkdown(src, 40)
	golden(t, "markdown_blocks", got)
	for _, want := range []string{"Title", "1. one", "2. two", "  • nested", "▎ quoted", "(http://x.io)", "┌", "│ a │ b  │"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestMarkdownTableFitsWidth(t *testing.T) {
	src := "| name | description |\n|---|---|\n| x | " + strings.Repeat("word ", 30) + " |\n"
	for _, l := range strings.Split(RenderMarkdown(src, 50), "\n") {
		if w := len([]rune(l)); w > 50 {
			t.Errorf("line of %d columns: %q", w, l)
		}
	}
}

func TestHighlightKeepsLines(t *testing.T) {
	in := []string{"func main() {", "\treturn", "}"}
	if got := highlightLines("go", in); len(got) != 3 {
		t.Fatalf("got %d lines", len(got))
	}
}

func TestBlockCacheReusesAndInvalidates(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	m.blocks = []block{{kind: blockAssistant, text: "hello **x**"}}
	a := m.renderBlock(0, 40)
	if e, ok := m.cache.m[0]; !ok || e.out != a {
		t.Fatalf("block not cached: %+v", m.cache.m)
	}
	m.blocks[0].text += " more"
	if b := m.renderBlock(0, 40); b == a || !strings.Contains(b, "more") {
		t.Fatalf("stale cache: %q", b)
	}
}
