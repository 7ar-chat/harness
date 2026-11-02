package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Diff styles of tui.json "diff_style": stacked is the single column of
// RenderDiff; auto switches to the split view (old left, new right) when the
// terminal is at least splitDiffCols wide.
const (
	diffAuto    = "auto"
	diffStacked = "stacked"

	splitDiffCols = 110
	// minSplitCol is the narrowest column worth drawing; below it the stacked view is used.
	minSplitCol = 24
)

func init() {
	register(command{name: "diff", help: "toggle the diff view of edits: split (wide terminals) / stacked", run: (*Model).cmdDiff})
}

// useSplitDiff says whether edit_file diffs are drawn side by side.
func (m *Model) useSplitDiff() bool {
	return m.pf.diffStyle != diffStacked && m.contentWidth() >= splitDiffCols
}

func (m *Model) cmdDiff(string) tea.Cmd {
	if m.pf.diffStyle == diffStacked {
		m.pf.diffStyle = diffAuto
		m.notice("diffs: auto (side by side from %d columns)", splitDiffCols)
	} else {
		m.pf.diffStyle = diffStacked
		m.notice("diffs: stacked")
	}
	return nil
}

// splitRow is one row of the split view: the old line on the left, the new one on the right (0 numbers: none).
type splitRow struct {
	oldNo, newNo int
	oldText      string
	newText      string
	oldSign      string // " " or "-"
	newSign      string // " " or "+"
}

// splitRows pairs the lines of a unified diff (without its "@@" header): a run
// of removed lines faces the run of added lines that follows it, context lines
// face themselves. Line numbers count from 1 within the replacement.
func splitRows(unified []string) []splitRow { return splitRowsFrom(unified, 1) }

// splitRowsFrom is splitRows with the numbers starting at line start.
func splitRowsFrom(unified []string, start int) []splitRow {
	var rows []splitRow
	o, n := start-1, start-1
	for i := 0; i < len(unified); {
		l := unified[i]
		switch {
		case strings.HasPrefix(l, "-") || strings.HasPrefix(l, "+"):
			var del, add []string
			for ; i < len(unified) && strings.HasPrefix(unified[i], "-"); i++ {
				del = append(del, unified[i][1:])
			}
			for ; i < len(unified) && strings.HasPrefix(unified[i], "+"); i++ {
				add = append(add, unified[i][1:])
			}
			for j := 0; j < max(len(del), len(add)); j++ {
				r := splitRow{}
				if j < len(del) {
					o++
					r.oldNo, r.oldText, r.oldSign = o, del[j], "-"
				}
				if j < len(add) {
					n++
					r.newNo, r.newText, r.newSign = n, add[j], "+"
				}
				rows = append(rows, r)
			}
		default:
			o++
			n++
			t := strings.TrimPrefix(l, " ")
			rows = append(rows, splitRow{oldNo: o, newNo: n, oldText: t, newText: t, oldSign: " ", newSign: " "})
			i++
		}
	}
	return rows
}

// renderSplitDiff draws a unified diff (as RenderDiff writes it) in two
// columns, each line wrapped to its column; nil when width is too small for two.
// limit caps the lines drawn.
func renderSplitDiff(diff string, width, limit int) []string {
	ls := strings.Split(diff, "\n")
	if len(ls) == 0 {
		return nil
	}
	const indent, gap = 4, 3 // "    " before the rows, " │ " between the columns
	cw := (width - indent - 2 - gap) / 2
	if cw < minSplitCol {
		return nil
	}
	_, start := diffTarget(ls[0])
	rows := splitRowsFrom(ls[1:], max(start, 1))
	// columns hug the text: the longest line sets the width, so the two sides sit close together
	longest := 0
	for _, r := range rows {
		longest = max(longest, lipgloss.Width(expandTabs(r.oldText)), lipgloss.Width(expandTabs(r.newText)))
	}
	need := longest + len(strconv.Itoa(max(start, 1)+len(rows))) + 4
	if need > cw {
		return nil // a line would wrap inside its half: the stacked view reads better
	}
	cw = max(need, 20)
	lang := diffLang(ls[0])
	numW := 2
	if len(rows) > 0 {
		numW = max(numW, len(strconv.Itoa(max(start, 1)+len(rows))))
	}
	out := []string{"    " + styleHunk.Render(clip(ls[0], width-indent-2))}
	sep := styleDim.Render(" │ ")
	for i, r := range rows {
		if len(out) >= limit {
			out = append(out, styleDim.Render(fmt.Sprintf("    … +%d lines", len(rows)-i)))
			break
		}
		var olo, ohi, nlo, nhi int
		if r.oldSign == "-" && r.newSign == "+" {
			if a, b, c, e, ok := changedSpan(r.oldText, r.newText); ok {
				olo, ohi, nlo, nhi = a, b, c, e
			}
		}
		left := splitCell(lang, r.oldNo, r.oldSign, r.oldText, cw, numW, olo, ohi)
		right := splitCell(lang, r.newNo, r.newSign, r.newText, cw, numW, nlo, nhi)
		for j := 0; j < max(len(left), len(right)); j++ {
			l, rr := padCell("", cw), padCell("", cw)
			if j < len(left) {
				l = left[j]
			}
			if j < len(right) {
				rr = right[j]
			}
			out = append(out, strings.TrimRight("    "+l+sep+rr, " "))
		}
	}
	return out
}

// splitCell draws one side of a row: the line number, the sign and the text
// (syntax-colored, tinted), wrapped to the column; a missing line (number 0) is blank.
func splitCell(lang string, no int, sign, text string, cw, numW, hlo, hhi int) []string {
	if no == 0 {
		return []string{padCell("", cw)}
	}
	tw := max(cw-numW-3, 1)
	styled := diffText(lang, sign, text, hlo, hhi)
	body := strings.Split(ansi.Wrap(styled, tw, ""), "\n")
	signSt := lipgloss.NewStyle()
	switch sign {
	case "+":
		signSt = styleAdd
	case "-":
		signSt = styleDel
	}
	out := make([]string, len(body))
	for j, b := range body {
		num, sg := strings.Repeat(" ", numW), " "
		if j == 0 {
			num, sg = fmt.Sprintf("%*d", numW, no), sign
		}
		cell := styleDim.Render(num+" ") + signSt.Render(sg+" ") + b
		out[j] = padCell(fillTo(cell, sign, cw), cw)
	}
	return out
}

// padCell pads s with spaces to w columns (s may hold ANSI sequences).
func padCell(s string, w int) string {
	if pad := w - ansi.StringWidth(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// diffLang is the syntax-highlighting language of a diff: the extension of the
// path in its "@@ path @@" header.
func diffLang(header string) string {
	p, _ := diffTarget(header)
	if i := strings.LastIndex(p, "."); i >= 0 {
		return p[i+1:]
	}
	return ""
}

// changedSpan is the rune range [lo,hi) of a and b that differs once their
// common prefix and suffix are dropped; ok is false when that is nothing or
// everything (a whole-line change needs no word highlight).
func changedSpan(a, b string) (alo, ahi, blo, bhi int, ok bool) {
	ra, rb := []rune(expandTabs(a)), []rune(expandTabs(b))
	pre := 0
	for pre < len(ra) && pre < len(rb) && ra[pre] == rb[pre] {
		pre++
	}
	suf := 0
	for suf < len(ra)-pre && suf < len(rb)-pre && ra[len(ra)-1-suf] == rb[len(rb)-1-suf] {
		suf++
	}
	if pre+suf == 0 || (pre+suf == len(ra) && pre+suf == len(rb)) {
		return 0, 0, 0, 0, false
	}
	// unrelated lines share only indentation: highlighting their "changed words" misleads
	if len(strings.TrimSpace(string(ra[:pre])))+len(strings.TrimSpace(string(ra[len(ra)-suf:]))) < min(len(strings.TrimSpace(string(ra))), len(strings.TrimSpace(string(rb))))/2 {
		return 0, 0, 0, 0, false
	}
	return pre, len(ra) - suf, pre, len(rb) - suf, true
}

// diffText styles the text of a changed line ("+" or "-"; " " for context):
// syntax colors for lang, the line tint as background and the [hlo,hhi) runes
// (the changed words) on the stronger tint. It is plain text without color.
func diffText(lang, sign, text string, hlo, hhi int) string {
	text = expandTabs(strings.TrimRight(text, " \r"))
	bg, hi := "", ""
	switch sign {
	case "+":
		bg, hi = diffTint.add, diffTint.addHi
	case "-":
		bg, hi = diffTint.del, diffTint.delHi
	}
	var sb strings.Builder
	pos := 0
	put := func(s string, st lipgloss.Style, from int) {
		// split s (starting at rune offset from) at the changed-word range
		rs := []rune(s)
		for len(rs) > 0 {
			in := from >= hlo && from < hhi
			n := len(rs)
			if hhi > hlo {
				switch {
				case from < hlo:
					n = min(n, hlo-from)
				case from < hhi:
					n = min(n, hhi-from)
				}
			}
			cell := st
			switch {
			case in && hi != "":
				cell = cell.Background(lipgloss.Color(hi))
			case bg != "":
				cell = cell.Background(lipgloss.Color(bg))
			}
			if in && hi == "" && bg == "" && sign != " " {
				cell = cell.Underline(true)
			}
			sb.WriteString(cell.Render(string(rs[:n])))
			rs, from = rs[n:], from+n
		}
	}
	for _, sg := range highlightSegs(lang, text) {
		st := sg.style
		if sign == "+" && diffTint.add == "" {
			st = styleAdd
		} else if sign == "-" && diffTint.del == "" {
			st = styleDel
		}
		put(sg.text, st, pos)
		pos += len([]rune(sg.text))
	}
	return plainIfNoColor(sb.String())
}

// diffFill is the background color for padding a changed line to the full width.
func diffFill(sign string) string {
	switch sign {
	case "+":
		return diffTint.add
	case "-":
		return diffTint.del
	}
	return ""
}

// fillTo pads s with spaces (tinted for the sign) to w columns.
func fillTo(s, sign string, w int) string {
	bg := diffFill(sign)
	pad := w - ansi.StringWidth(s)
	if bg == "" || pad <= 0 {
		return s
	}
	return s + lipgloss.NewStyle().Background(lipgloss.Color(bg)).Render(strings.Repeat(" ", pad))
}

// stackedDiff draws a unified diff in one column: a removed run, then the added
// run, each line syntax-colored and tinted, with the changed words of paired
// lines highlighted.
func stackedDiff(ls []string, width, limit int) []string {
	var out []string
	lang := ""
	if len(ls) > 0 && strings.HasPrefix(ls[0], "@@") {
		lang = diffLang(ls[0])
	}
	tw := max(width-6, 10)
	_, start := diffTarget(firstLine(ls))
	o, n := start-1, start-1
	numW := 0
	if start > 0 {
		numW = len(strconv.Itoa(start+len(ls))) + 1 // number and a space
		tw -= numW
	}
	emit := func(sign, text string, lo, hi int) {
		gutter := ""
		if start > 0 {
			no := 0
			switch sign {
			case "-":
				o++
				no = o
			case "+":
				n++
				no = n
			default:
				o++
				n++
				no = n
			}
			gutter = styleDim.Render(fmt.Sprintf("%*d ", numW-1, no))
		}
		body := ansiClip(diffText(lang, sign, text, lo, hi), tw-1)
		line := sign
		if sign == " " {
			line = " "
		}
		st := lipgloss.NewStyle()
		switch sign {
		case "+":
			st = styleAdd
		case "-":
			st = styleDel
		}
		out = append(out, "    "+gutter+fillTo(st.Render(line)+body, sign, tw))
	}
	for i := 0; i < len(ls); {
		if len(out) >= limit {
			out = append(out, styleDim.Render(fmt.Sprintf("    … +%d lines", len(ls)-i)))
			break
		}
		l := ls[i]
		switch {
		case strings.HasPrefix(l, "@@"):
			out = append(out, "    "+styleHunk.Render(clip(l, tw+numW)))
			i++
		case strings.HasPrefix(l, "-") || strings.HasPrefix(l, "+"):
			var del, add []string
			for ; i < len(ls) && strings.HasPrefix(ls[i], "-"); i++ {
				del = append(del, ls[i][1:])
			}
			for ; i < len(ls) && strings.HasPrefix(ls[i], "+"); i++ {
				add = append(add, ls[i][1:])
			}
			dh, ah := make([][2]int, len(del)), make([][2]int, len(add))
			for j := 0; j < min(len(del), len(add)); j++ {
				if alo, ahi, blo, bhi, ok := changedSpan(del[j], add[j]); ok {
					dh[j], ah[j] = [2]int{alo, ahi}, [2]int{blo, bhi}
				}
			}
			for j, t := range del {
				emit("-", t, dh[j][0], dh[j][1])
			}
			for j, t := range add {
				emit("+", t, ah[j][0], ah[j][1])
			}
		default:
			emit(" ", strings.TrimPrefix(l, " "), 0, 0)
			i++
		}
	}
	return out
}

func expandTabs(s string) string { return strings.ReplaceAll(s, "\t", "    ") }

// padCellStyled pads s with spaces drawn in st to w columns.
func padCellStyled(s string, w int, st lipgloss.Style) string {
	if pad := w - ansi.StringWidth(s); pad > 0 {
		return s + st.Render(strings.Repeat(" ", pad))
	}
	return s
}

// diffTarget is the path and the 1-based first line of a "@@ path @@" or
// "@@ path:12 @@" header (start 0: the line is unknown).
func diffTarget(header string) (path string, start int) {
	p := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(header, "@@"), "@@"))
	if i := strings.LastIndex(p, ":"); i > 0 {
		if n, err := strconv.Atoi(p[i+1:]); err == nil && n > 0 {
			return p[:i], n
		}
	}
	return p, 0
}

func firstLine(ls []string) string {
	if len(ls) > 0 {
		return ls[0]
	}
	return ""
}
