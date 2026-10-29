package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Diff styles of tui.json "diff_style": stacked is the single column of
// RenderDiff; auto switches to the split view (old left, new right) when the
// terminal is at least splitDiffCols wide.
const (
	diffAuto    = "auto"
	diffStacked = "stacked"

	splitDiffCols = 120
	// minSplitCol is the narrowest column worth drawing; below it the stacked view is used.
	minSplitCol = 24
)

func init() {
	register(command{name: "diff", help: "toggle the diff view of edits: split (wide terminals) / stacked", run: (*Model).cmdDiff})
}

// useSplitDiff says whether edit_file diffs are drawn side by side.
func (m *Model) useSplitDiff() bool {
	return m.pf.diffStyle != diffStacked && m.width >= splitDiffCols
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
func splitRows(unified []string) []splitRow {
	var rows []splitRow
	o, n := 0, 0
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
	rows := splitRows(ls[1:])
	numW := 2
	if len(rows) > 0 {
		numW = max(numW, len(strconv.Itoa(len(rows))))
	}
	out := []string{"    " + styleHunk.Render(clip(ls[0], width-indent-2))}
	sep := styleDim.Render(" │ ")
	for i, r := range rows {
		if len(out) >= limit {
			out = append(out, styleDim.Render(fmt.Sprintf("    … +%d lines", len(rows)-i)))
			break
		}
		left := splitCell(r.oldNo, r.oldSign, r.oldText, cw, numW, styleDel)
		right := splitCell(r.newNo, r.newSign, r.newText, cw, numW, styleAdd)
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

// splitCell draws one side of a row: the line number, the sign and the text,
// wrapped to the column; a missing line (number 0) is blank.
func splitCell(no int, sign, text string, cw, numW int, st lipgloss.Style) []string {
	if no == 0 {
		return []string{padCell("", cw)}
	}
	tw := max(cw-numW-3, 1)
	body := strings.Split(ansi.Wrap(strings.ReplaceAll(strings.TrimRight(text, " \r"), "\t", "    "), tw, ""), "\n")
	textStyle := st
	if sign == " " {
		textStyle = lipgloss.NewStyle()
	}
	out := make([]string, len(body))
	for j, b := range body {
		num, sg := strings.Repeat(" ", numW), " "
		if j == 0 {
			num, sg = fmt.Sprintf("%*d", numW, no), sign
		}
		cell := styleDim.Render(num+" ") + textStyle.Render(sg+" "+b)
		out[j] = padCell(cell, cw)
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
