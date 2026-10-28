---
name: studio-brand
description: Studio's neutral corporate styling for every document the agent produces - fonts available in the sandbox, colour palette (hex codes), chart colours, type sizes, page and slide geometry, title page and footer conventions, French/English date and number formatting - with ready-to-use helpers (scripts/brand.py) for python-docx, python-pptx, openpyxl/xlsxwriter, reportlab and matplotlib. Load it together with the docx, pptx, xlsx, pdf or charts skill whenever you create a new document, deck, workbook, PDF or chart and the user has not supplied their own template or style.
license: Internal (Studio)
---

# Studio brand (neutral corporate defaults)

Use these defaults for NEW deliverables. If the user provides a template or an existing
document, keep THEIR styles instead. No logos or third-party trademarks are part of this
brand: do not draw or invent logos.

**Where files go.** Save every final deliverable in `/workspace/outputs/` (or the path the user asked for).
Never put a deliverable in `/workspace/.work/` and never link a file there (it is scratch space and is not saved).
In your answer, only link files under `/workspace/outputs/` (or files the user asked for).

Helper: `/skills/studio-brand/scripts/brand.py` (import it; stdlib-only at import time).
```python
import sys; sys.path.insert(0, "/skills/studio-brand/scripts"); import brand
```
`python3 /skills/studio-brand/scripts/brand.py --json` prints every constant;
`--swatch /workspace/.work/palette.png` renders the palette.

## 1. Palette

| Name | Hex | Use |
|---|---|---|
| navy | #1B365D | primary: titles, table headers, main series |
| blue | #2E6DA4 | secondary series, links, H3 |
| sky | #8DB8DE | light accent, fills |
| teal | #2A9D8F | accent rule, bullets, highlight |
| amber | #E9A23B | highlight / warning |
| coral | #C8553D | alert, 5th series |
| ink | #1A1F2B | body text |
| slate | #4A5568 | secondary text, captions, subtitles |
| grey | #8C96A3 | footers, axes, muted series |
| rule | #D5DAE1 | borders, separators, gridlines |
| mist | #EEF1F5 | zebra rows, panels, callouts |
| positive / negative | #2E7D32 / #C62828 | gains / losses (always with a sign) |

Chart series order (`brand.CHART_COLORS`): navy, blue, teal, amber, coral, plum #7D5BA6,
grey, olive #8AA05B. Sequential ramp `brand.SEQUENTIAL`, diverging `brand.DIVERGING`.
Text on navy/blue/teal fills is white; on sky/mist/amber it is ink.

**Colours in code**: every library wants its own form (palette name or `RRGGBB` in, worked
example `brand-colours` in `reference/examples.md`):

| Library | Write | Not |
|---|---|---|
| openpyxl | `Font(color=brand.xlsx_color("navy"))`, `PatternFill("solid", fgColor=brand.xlsx_color("mist"))`, `Side(style="thin", color=brand.xlsx_color("rule"))`; shortcuts `cell.fill = brand.xlsx_fill("mist")`, `cell.font = brand.xlsx_font("white", bold=True)` | `brand.hex_()` ("#..." -> "Colors must be aRGB hex values"); a colour string as `cell.fill` |
| python-docx | `run.font.color.rgb = brand.docx_rgb("slate")` | `brand.rgb()` (a tuple -> "must be RGBColor object") |
| python-pptx | `shape.fill.fore_color.rgb = brand.pptx_rgb("navy")`, `run.font.color.rgb = brand.pptx_rgb("white")` | `brand.rgb()` |
| matplotlib, xlsxwriter, reportlab, HTML | `brand.hex_("navy")` -> `"#1B365D"` | |
| Pillow | `brand.rgb("navy")` -> `(27, 54, 93)` | |

## 2. Fonts

| Where | Font | Why |
|---|---|---|
| docx / pptx / xlsx | **Arial** (headings bold, same family) | on every user PC; the sandbox renders it with metric-identical Liberation Sans, so PDFs and previews match Word/PowerPoint |
| code / fixed width | Courier New (Liberation Mono) | |
| matplotlib, Pillow | Liberation Sans (fallback DejaVu Sans, Noto Sans) | installed TTFs |
| reportlab | `brand.reportlab_fonts()` -> "StudioSans" (Liberation Sans TTF) | full Unicode Latin, unlike built-in Helvetica |

Installed families (fc-list): Liberation Sans/Serif/Mono, DejaVu, Noto Sans/Serif (+ many
scripts), Noto Color Emoji, and Carlito/Caladea (metric-compatible with Calibri/Cambria, for
documents users upload). Do not use fonts that are not installed (Segoe UI, Helvetica Neue,
Roboto...): previews substitute them and layouts break.

Sizes (pt): docx title 26, H1 16, H2 13, H3 11.5, body 10.5, table 9.5, caption/footer 8-9.
pptx title slide 40, slide title 28, body 18-22, nothing below 12 (footer and slide number 10), KPI 44. xlsx 10 body,
header 10 bold, sheet title 14.

## 3. Layout conventions

- **Word/PDF**: A4 portrait, 2.2 cm margins (text width 16.6 cm). Title page: title (26 pt navy
  bold), subtitle (14 pt slate), navy rule, "Préparé par : X" / "Prepared by: X", date in
  long format, page break. Footer on every page: ONE right-aligned paragraph "Title · Page x
  sur y" / "Title · Page x of y", 8 pt grey, thin rule above (`docx_footer` replaces whatever
  the footer held; never add a second page number). Optional header text right.
- **Slides**: 16:9 (13.333 x 7.5 in), 0.6 in margins, title band at top (28 pt navy bold,
  left-aligned), short teal accent rule under the title, footer text left + slide number
  right (10 pt grey) on content slides; title slide with a navy band on the left; section
  slides navy background with white title.
- **Workbooks**: header row navy fill + white bold text, frozen below the header, autofilter,
  zebra optional, numbers right-aligned with formats from `brand.XLSX_FORMATS`.
- **Charts**: left-aligned bold title stating the message, slate subtitle with the unit, light
  horizontal gridlines only, no top/right spines, source line bottom-left in grey, 200 dpi.
- Classification labels (C1-C4) are applied by Studio on download: do not type them yourself.

## 4. Numbers and dates

| | French (fr) | English (en) |
|---|---|---|
| number | `1 234 567,89` (no-break space) | `1,234,567.89` |
| percent | `12,5 %` | `12.5%` |
| currency | `1 234,50 €` | `€1,234.50` |
| long date | `6 octobre 2026`, `1er mars 2026` | `6 October 2026` |
| short date | `06/10/2026` | `06/10/2026` (or ISO `2026-10-06` for international readers) |

`brand.format_number(x, decimals, locale)`, `format_percent(ratio, decimals, locale)`,
`format_currency(x, "EUR", decimals, locale)`, `format_date(date, locale, "long"|"short"|"month"|"iso")`.
Dates written as text: `brand.parse_date("14 octobre 2025")` -> `date(2025, 10, 14)` (ISO, `14/10/2025`,
French/English month names; `None` if it is not a full date, never an error); `brand.date_label(text, locale)`
writes a parsed date in the locale and keeps any other text ("Automne 2025") as is.
In Excel store raw numbers + a number format (Excel localises the display); never write
formatted strings into numeric cells. Use the user's language for labels (`brand.LABELS`).

## 5. Apply it

| Target | Call |
|---|---|
| python-docx | colours `brand.docx_rgb("slate")`; `brand.docx_apply(doc, "fr")` (A4, margins, Normal/Heading/Title/Caption/List styles, language); `brand.docx_title_block(doc, title, subtitle, author, date, "fr")`; `brand.docx_footer(doc, "Doc title", "fr", header_text=None)` (one paragraph "Doc title · Page X sur Y"); `brand.docx_style_table(table, numeric_cols=[2])` |
| python-pptx | colours `brand.pptx_rgb("navy")`; `prs = brand.pptx_new()` (16:9 + theme fonts/colours) or `brand.pptx_apply_theme(prs)`; `brand.pptx_style_text(tf, size, "ink")`; `brand.pptx_footer(slide, prs, "text", number)`; `brand.pptx_style_table(table, numeric_cols=[1])` |
| openpyxl | `brand.xlsx_header(ws)`; `brand.xlsx_body_font(ws)`; `brand.xlsx_autofit(ws)`; `cell.number_format = brand.XLSX_FORMATS["eur"]`; colours `brand.xlsx_color("navy")`, `brand.xlsx_fill("mist")`, `brand.xlsx_font("navy", bold=True)` |
| xlsxwriter | `fmts = brand.xlsxwriter_formats(workbook)` -> header, text, integer, number, percent, eur, date, title |
| matplotlib | `brand.mpl_apply()`; sizes `brand.FIGURE_SIZES["doc"]`; tick format `ax.yaxis.set_major_formatter(brand.mpl_number_formatter("percent", 0, "fr"))` |
| reportlab | `font = brand.reportlab_fonts()`; colours `HexColor(brand.hex_("navy"))` |

The docx skill's `new_document.py`, the pptx skill's `build_deck.py`, the pdf skill's
`build_pdf.py` and the charts skill apply all of this automatically when this skill is
installed. Worked examples: `reference/examples.md` (tested).

## 6. Verify

The brand has no checker of its own: run the FINISH GATE of the document skill (`inspect_docx.py
--check`, `fit_text.py`, `recalc.py`, `extract.py --check`, `make_chart.py --check`), then call
`view_file` on the deliverable once after its last change and check one font family, palette colours only, consistent
title/footer placement, numbers and dates in the user's locale.

## 7. Deliver

The brand is applied through the document skills; deliver their files as usual, e.g.
`[note.docx](workspace-file:///workspace/outputs/note.docx)`. If the user asks for the palette
itself, render `brand.py --swatch /workspace/outputs/palette.png` and link it.
