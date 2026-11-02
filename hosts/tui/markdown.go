package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	gtext "github.com/yuin/goldmark/text"
)

var md = goldmark.New(goldmark.WithExtensions(extension.Table, extension.Strikethrough))

// mdr renders one markdown document.
type mdr struct {
	src   []byte
	width int
}

// RenderMarkdown renders markdown: headings, paragraphs, bold/italic/strike,
// inline code, links, bullet and numbered lists (nested), block quotes, rules,
// tables and fenced code blocks with syntax highlighting. An unclosed fence
// (a streaming message) is rendered as code to the end.
func RenderMarkdown(src string, width int) string {
	src = strings.TrimRight(src, "\n")
	if strings.TrimSpace(src) == "" {
		return ""
	}
	if width < 10 {
		width = 10
	}
	b := []byte(src)
	r := &mdr{src: b, width: width}
	doc := md.Parser().Parse(gtext.NewReader(b))
	return plainIfNoColor(strings.Join(r.blocks(doc, width), "\n"))
}

// plainIfNoColor drops the escape sequences of attributes (italic, underline) that
// no theme color controls when colors are off.
func plainIfNoColor(s string) string {
	if colorsOn {
		return s
	}
	return ansi.Strip(s)
}

// blocks renders the block children of n at the given width, separated by a blank line
// (a paragraph directly followed by a list stays tight).
func (r *mdr) blocks(n ast.Node, w int) []string {
	var out []string
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		ls := r.block(c, w)
		if len(ls) == 0 {
			continue
		}
		if len(out) > 0 {
			_, prevPara := c.PreviousSibling().(*ast.Paragraph)
			_, prevTight := c.PreviousSibling().(*ast.TextBlock)
			_, isList := c.(*ast.List)
			if !((prevPara || prevTight) && isList) {
				out = append(out, "")
			}
		}
		out = append(out, ls...)
	}
	return out
}

func (r *mdr) block(n ast.Node, w int) []string {
	switch n := n.(type) {
	case *ast.Heading:
		s := r.inline(n, styleBold)
		if n.Level <= 2 {
			s = styleHead.Render(ansi.Strip(s))
		}
		return strings.Split(wrap(s, w), "\n")
	case *ast.Paragraph, *ast.TextBlock:
		return strings.Split(wrap(r.inline(n, lipgloss.NewStyle()), w), "\n")
	case *ast.ThematicBreak:
		return []string{styleDim.Render(strings.Repeat("─", min(w, 60)))}
	case *ast.FencedCodeBlock:
		lang := string(n.Language(r.src))
		return codeBlock(lang, r.lines(n), w, r.closed(n))
	case *ast.CodeBlock:
		return codeBlock("", r.lines(n), w, true)
	case *ast.HTMLBlock:
		var ls []string
		for _, l := range r.lines(n) {
			ls = append(ls, styleDim.Render(l))
		}
		return ls
	case *ast.Blockquote:
		inner := r.blocks(n, w-2)
		for i, l := range inner {
			inner[i] = styleDim.Render("▎ ") + l
		}
		return inner
	case *ast.List:
		return r.list(n, w)
	case *east.Table:
		return r.table(n, w)
	}
	// unknown container: render children
	return r.blocks(n, w)
}

// lines are the raw source lines of a block node, without the trailing newline.
func (r *mdr) lines(n ast.Node) []string {
	var out []string
	l := n.Lines()
	for i := 0; i < l.Len(); i++ {
		seg := l.At(i)
		out = append(out, strings.TrimRight(string(seg.Value(r.src)), "\r\n"))
	}
	return out
}

func (r *mdr) list(n *ast.List, w int) []string {
	var out []string
	num := n.Start
	if num == 0 {
		num = 1
	}
	for it := n.FirstChild(); it != nil; it = it.NextSibling() {
		marker := "• "
		if n.IsOrdered() {
			marker = fmt.Sprintf("%d. ", num)
			num++
		}
		pad := strings.Repeat(" ", ansi.StringWidth(marker))
		inner := r.blocks(it, max(w-len(pad), 10))
		for i, l := range inner {
			if i == 0 {
				out = append(out, styleDim.Render(marker)+l)
			} else if l == "" {
				out = append(out, "")
			} else {
				out = append(out, pad+l)
			}
		}
		if len(inner) == 0 {
			out = append(out, styleDim.Render(marker))
		}
	}
	return out
}

// table draws a GFM table with box characters; wide cells are wrapped.
func (r *mdr) table(t *east.Table, w int) []string {
	var rows [][]string
	var header int
	for row := t.FirstChild(); row != nil; row = row.NextSibling() {
		var cells []string
		for c := row.FirstChild(); c != nil; c = c.NextSibling() {
			cells = append(cells, r.inline(c, lipgloss.NewStyle()))
		}
		if _, ok := row.(*east.TableHeader); ok {
			header = len(rows)
		}
		rows = append(rows, cells)
	}
	cols := 0
	for _, row := range rows {
		cols = max(cols, len(row))
	}
	if cols == 0 {
		return nil
	}
	widths := make([]int, cols)
	for _, row := range rows {
		for i, c := range row {
			widths[i] = max(widths[i], ansi.StringWidth(c))
		}
	}
	// shrink the widest column until the table fits
	total := func() int {
		s := 1
		for _, cw := range widths {
			s += cw + 3
		}
		return s
	}
	for total() > w {
		wi := 0
		for i, cw := range widths {
			if cw > widths[wi] {
				wi = i
			}
		}
		if widths[wi] <= 4 {
			break
		}
		widths[wi]--
	}
	line := func(l, m, rr string) string {
		parts := make([]string, cols)
		for i, cw := range widths {
			parts[i] = strings.Repeat("─", cw+2)
		}
		return styleDim.Render(l + strings.Join(parts, m) + rr)
	}
	var out []string
	out = append(out, line("┌", "┬", "┐"))
	for ri, row := range rows {
		cellLines := make([][]string, cols)
		h := 1
		for i := 0; i < cols; i++ {
			c := ""
			if i < len(row) {
				c = row[i]
			}
			if ri == header {
				c = styleBold.Render(ansi.Strip(c))
			}
			cellLines[i] = strings.Split(ansi.Wrap(c, widths[i], ""), "\n")
			h = max(h, len(cellLines[i]))
		}
		for j := 0; j < h; j++ {
			var sb strings.Builder
			sb.WriteString(styleDim.Render("│"))
			for i := 0; i < cols; i++ {
				c := ""
				if j < len(cellLines[i]) {
					c = cellLines[i][j]
				}
				sb.WriteString(" " + padCell(c, widths[i]) + " " + styleDim.Render("│"))
			}
			out = append(out, sb.String())
		}
		if ri == header {
			out = append(out, line("├", "┼", "┤"))
		}
	}
	out = append(out, line("└", "┴", "┘"))
	return out
}

// inline renders the inline children of n with base as the surrounding style.
func (r *mdr) inline(n ast.Node, base lipgloss.Style) string {
	var sb strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		sb.WriteString(r.inlineNode(c, base))
	}
	return sb.String()
}

func (r *mdr) inlineNode(n ast.Node, base lipgloss.Style) string {
	switch n := n.(type) {
	case *ast.Text:
		s := string(n.Segment.Value(r.src))
		if n.HardLineBreak() {
			s += "\n"
		} else if n.SoftLineBreak() {
			s += " "
		}
		return base.Render(s)
	case *ast.String:
		return base.Render(string(n.Value))
	case *ast.CodeSpan:
		var sb strings.Builder
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*ast.Text); ok {
				sb.Write(t.Segment.Value(r.src))
			}
		}
		return styleCode.Render(strings.ReplaceAll(sb.String(), "\n", " "))
	case *ast.Emphasis:
		if n.Level >= 2 {
			return r.inline(n, base.Bold(true))
		}
		return r.inline(n, base.Italic(true))
	case *east.Strikethrough:
		return r.inline(n, base.Strikethrough(true))
	case *ast.Link:
		label := r.inline(n, base.Underline(true))
		url := string(n.Destination)
		if strings.TrimSpace(ansi.Strip(label)) == url {
			return styleLink.Render(url)
		}
		return label + styleDim.Render(" ("+url+")")
	case *ast.AutoLink:
		return styleLink.Render(string(n.URL(r.src)))
	case *ast.Image:
		return styleDim.Render("[image: ") + r.inline(n, base) + styleDim.Render("]")
	case *ast.RawHTML:
		var sb strings.Builder
		for i := 0; i < n.Segments.Len(); i++ {
			seg := n.Segments.At(i)
			sb.Write(seg.Value(r.src))
		}
		return styleDim.Render(sb.String())
	}
	return r.inline(n, base)
}

// codeBlock draws a fenced block: a "┌ lang" top, highlighted lines behind a
// "│ " gutter and a "└" bottom.
func codeBlock(lang string, lines []string, w int, closed bool) []string {
	top := "┌"
	if lang != "" {
		top += " " + lang
	}
	out := []string{styleDim.Render(top)}
	hl := highlightLines(lang, lines)
	for _, l := range hl {
		out = append(out, styleDim.Render("│ ")+ansiClip(l, w-2))
	}
	if !closed {
		return out // still streaming
	}
	return append(out, styleDim.Render("└"))
}

// closed says whether a fenced block has its closing fence (false while it streams).
func (r *mdr) closed(n *ast.FencedCodeBlock) bool {
	end := n.Info.Segment.Stop
	if l := n.Lines(); l.Len() > 0 {
		end = l.At(l.Len() - 1).Stop
	} else if n.Info == nil {
		return false
	}
	rest := strings.TrimLeft(string(r.src[min(end, len(r.src)):]), " \t\r\n")
	return strings.HasPrefix(rest, "```") || strings.HasPrefix(rest, "~~~")
}

func ansiClip(s string, w int) string {
	if w <= 0 || ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// colorOn says whether the terminal renders colors (false in tests and pipes).
func colorOn() bool { return colorsOn }

// highlightLines colors code lines with chroma, mapping token classes to the
// theme. lang may be a language name or a file extension; unknown languages
// and terminals without color get the plain code style.
func highlightLines(lang string, lines []string) []string {
	plain := func() []string {
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = styleCode.Render(strings.ReplaceAll(l, "\t", "    "))
		}
		return out
	}
	if !colorOn() || len(lines) == 0 {
		return plain()
	}
	lex := lexerFor(lang)
	if lex == nil {
		lex = lexers.Analyse(strings.Join(lines, "\n"))
	}
	if lex == nil {
		return plain()
	}
	lex = chroma.Coalesce(lex)
	it, err := lex.Tokenise(nil, strings.Join(lines, "\n")+"\n")
	if err != nil {
		return plain()
	}
	var out []string
	var cur strings.Builder
	flush := func() { out = append(out, cur.String()); cur.Reset() }
	for tok := it(); tok != chroma.EOF; tok = it() {
		st := tokenStyle(tok.Type)
		parts := strings.Split(strings.ReplaceAll(tok.Value, "\t", "    "), "\n")
		for i, p := range parts {
			if i > 0 {
				flush()
			}
			if p != "" {
				cur.WriteString(st.Render(p))
			}
		}
	}
	if cur.Len() > 0 {
		flush()
	}
	for len(out) > len(lines) {
		out = out[:len(out)-1]
	}
	return out
}

func lexerFor(lang string) chroma.Lexer {
	lang = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(lang)), ".")
	if lang == "" {
		return nil
	}
	if l := lexers.Get(lang); l != nil {
		return l
	}
	return lexers.Match("x." + lang)
}

// tokenStyle maps a chroma token class to a theme style.
func tokenStyle(t chroma.TokenType) lipgloss.Style {
	switch {
	case t.InCategory(chroma.Comment):
		return styleDim.Italic(true)
	case t.InCategory(chroma.Keyword):
		return styleKw
	case t.InCategory(chroma.LiteralString):
		return styleStr
	case t.InCategory(chroma.LiteralNumber), t == chroma.NameConstant, t == chroma.NameBuiltin:
		return styleNum
	case t == chroma.NameFunction, t == chroma.NameFunctionMagic, t == chroma.NameClass, t == chroma.NameDecorator, t == chroma.NameTag:
		return styleFn
	case t == chroma.GenericDeleted:
		return styleDel
	case t == chroma.GenericInserted:
		return styleAdd
	}
	return lipgloss.NewStyle()
}

// seg is a run of text with one style.
type seg struct {
	text  string
	style lipgloss.Style
}

// highlightSegs tokenizes one line of code for lang (a name or extension).
// Without color or a lexer it is one plain segment.
func highlightSegs(lang, line string) []seg {
	line = strings.ReplaceAll(line, "\t", "    ")
	plain := []seg{{line, lipgloss.NewStyle()}}
	if !colorOn() || lang == "" || line == "" {
		return plain
	}
	lex := lexerFor(lang)
	if lex == nil {
		return plain
	}
	it, err := chroma.Coalesce(lex).Tokenise(nil, line)
	if err != nil {
		return plain
	}
	var out []seg
	for tok := it(); tok != chroma.EOF; tok = it() {
		if v := strings.TrimRight(tok.Value, "\n"); v != "" {
			out = append(out, seg{v, tokenStyle(tok.Type)})
		}
	}
	if len(out) == 0 {
		return plain
	}
	return out
}
