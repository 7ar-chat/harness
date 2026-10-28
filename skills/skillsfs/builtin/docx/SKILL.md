---
name: docx
description: Create, edit, check and convert Word documents (.docx) - reports, memos, letters, contracts, minutes. Use it to build a new .docx (JSON spec builder with Studio styling, real table of contents, single-line "Title · Page X of Y" footer), to change an existing document while keeping its styles (cross-run find/replace, template filling, sections, tables, images, TOC), to accept/reject or produce tracked changes, and to convert to PDF. To only read or summarise a .docx, call read_file on it; you do not need this skill for that.
license: Internal (Studio)
requires: [studio-brand]
---

# Word documents (.docx)

**FINISH GATE - do this before you say anything about the document to the user.**
1. Check: `python3 /skills/docx/scripts/inspect_docx.py --check /workspace/outputs/FILE.docx`.
   The runtime already runs it after every tool call that changes a file in `/workspace/outputs/` and adds the
   verdict to that call's result (`verification`): when the verdict of your last change is there, do not run it again.
2. If it is `"ok": false` (exit code 2): fix every check with `"ok": false` (its `detail` says how),
   save, and check again. At most 3 rounds.
3. When it is `"ok": true`: call `view_file` on `/workspace/outputs/FILE.docx` once and look at the pages:
   footer, tables, images, TOC, page count, language. Fix what looks wrong (then steps 1-3 again).
   If `view_file` answers that this model cannot view images, skip this step and say so; if it cannot
   render the file, say "could not view: <its reason>".
4. Cross-check the content against the files it comes from:
   `python3 /skills/docx/scripts/cross_check.py /workspace/outputs/FILE.docx SOURCE [SOURCE ...]` lists the
   figures, dates and names that are in none of them. Check each one in the source (`search_files` /
   `read_file`) and fix the wrong ones; also check that nothing the user asked for is missing.
5. Page citations of a PDF ("(p. 12)"): `python3 /skills/pdf/scripts/check_citations.py SOURCE.pdf
   /workspace/outputs/FILE.docx` (load the `pdf` skill if `/skills/pdf` is missing; the runtime also runs it
   with the check of step 1 when the thread has an uploaded PDF). Every claim it lists: use the figure
   printed on that page, cite the page where it is printed, or remove the claim.
6. Never say the document is done, ready or created while `"ok": false`. After 3 rounds, tell
   the user exactly which checks still fail. In your answer, say what you cross-checked against which
   file (and which figures are your own computations).

Only a changed file needs a new check or view: a passing check and a `view_file` after a file's last change stay
valid. Never check or view again a file you did not change since (one `view_file` per file after its last change).

**Files.** Save the final document in `/workspace/outputs/` (or the path the user asked for).
Never put a deliverable in `/workspace/.work/` and never link a file there: it is scratch space
(specs, charts, renders) and is not kept. In your answer, only link files under `/workspace/outputs/`.

**Reading.** `read_file` returns the text of a .docx directly. Use `inspect_docx.py` only when you
need structure: paragraph indices and styles, fields, tracked changes, sections.

Scripts are in `/skills/docx/scripts/`; run them with `python3` (or `bash` for `.sh`) from
`/workspace`. They print JSON; on bad input they exit non-zero with an `error` message.
Studio styling comes from the `studio-brand` skill, installed with this one; `new_document.py` uses it.
Colours in hand-written python-docx: `run.font.color.rgb = brand.docx_rgb("slate")` (not `brand.rgb()`, a tuple).

## 0. Decide

| The user wants | Do |
|---|---|
| A new document | §2 Create (the spec builder first) |
| Changes to an existing document | §3 Edit |
| Tracked changes (list, accept, reject, propose) | §4 |
| A PDF of a document | §5 Convert |
| Always, at the end | the FINISH GATE (§6), then §7 Deliver |

## 1. Structure of an existing document

```bash
python3 /skills/docx/scripts/inspect_docx.py IN.docx          # JSON: outline, styles, tables, fields, tracked changes, checks
python3 /skills/docx/scripts/inspect_docx.py IN.docx --text   # "[index] (style) text" per paragraph, then the tables
```
`.doc`/`.odt`/`.rtf`: `soffice --headless --convert-to docx --outdir /workspace/.work IN.doc` first.

## 2. Create

### 2a. Default: JSON spec -> `new_document.py`
```bash
python3 /skills/docx/scripts/new_document.py --example > /workspace/.work/spec.json      # the format
python3 /skills/docx/scripts/new_document.py --spec /workspace/.work/spec.json --out /workspace/outputs/report.docx
```
- Blocks: `heading{text,level}`, `paragraph{text,align}`, `bullets{items}`, `numbered{items}`,
  `table{columns,rows,widths_cm,numeric_cols,caption}`, `image{path,width_cm,caption}`,
  `callout{text,kind}`, `toc{title}`, `page_break`. Inline `**bold**`, `*italic*`, `` `code` ``, `[label](url)`.
- Each block can be `{"type": "heading", "text": "Intro", "level": 1}`, `{"heading": {"text": "Intro"}}`
  or `{"heading": "Intro"}`; a bare string is a paragraph. The same spec builds a PDF with
  `/skills/pdf/scripts/build_pdf.py`. A bad spec stops with the accepted shapes: fix the spec.
- Top level: `locale` (fr|en), `title`, `subtitle`, `author`, `date` (any text: "2026-10-06",
  "14 octobre 2025" are written as a date in the locale, other text as is), `title_page`, `header`,
  `footer`, `landscape`, `template` (a .docx whose styles you inherit), `brand` (default true).
- The footer is ONE paragraph "`footer` · Page X of Y" ("Page X sur Y" in French). Never add page numbers yourself.
- A `toc` block is filled with real entries and page numbers automatically.
- Charts: make a PNG with the `charts` skill in `/workspace/.work/`, then an `image` block
  (`width_cm` 16 = full A4 text width).

### 2b. Custom layouts: python-docx code
Write `/workspace/.work/make_doc.py` and run it. Tested patterns in `reference/python-docx.md`:
fonts (`set_font`), page setup, tables (`set_col_widths`), merged cells, images, sections,
headers/footers, TOC field, hyperlinks, insert after a paragraph. Then:
- footer with page numbers: `python3 /skills/docx/scripts/footer.py OUT.docx --text "Title" --locale en --inplace`
  (or `from footer import set_footer` after `sys.path.insert(0, "/skills/docx/scripts")`);
- table of contents: `python3 /skills/docx/scripts/fill_toc.py OUT.docx --inplace`.

### 2c. docx (npm) alternative
`require("docx")` works (global). Use it for native footnotes or complex numbering:
`reference/docx-js.md`. Give tables `columnWidths`; run `fill_toc.py` on the result if it has a TOC.

### Writing rules
- Real styles: `Heading 1-3` for headings, `List Bullet`/`List Number` for lists, `Caption` for
  captions. Never fake a heading with bold text, never type "1." by hand, never use empty
  paragraphs or spaces for layout (use spacing or a page break).
- One font family; body 10.5-11 pt; A4 portrait, 2-2.5 cm margins, unless the user's template differs.
- Tables: header row, numbers right-aligned with the same decimals, units in the header.
- Write in the user's language; dates and numbers in their locale (fr `6 octobre 2026`, `1 234,5`).

## 3. Edit an existing document (keep its formatting)

1. Copy first: `cp /workspace/in.docx /workspace/outputs/in_v2.docx`; never overwrite the upload.
2. `inspect_docx.py FILE --text` to find paragraph indices, styles and tables.
3. Use the least invasive tool:
   - Text anywhere (body, tables, headers, footers, text boxes, words split across runs):
     ```bash
     python3 /skills/docx/scripts/replace.py FILE.docx --inplace --find "{{client}}" --replace "Contoso"
     python3 /skills/docx/scripts/replace.py FILE.docx --inplace --pairs /workspace/.work/pairs.json   # {"old": "new"}
     ```
     Read `counts` / `not_found`. A miss usually means typographic quotes, a no-break space or another spelling.
   - Structure (insert paragraphs, tables, images; delete sections): python-docx code that inserts
     next to an existing paragraph with the document's own styles (`reference/python-docx.md`
     "Insert after a paragraph"). `doc.add_paragraph()` always appends at the END.
   - Repeating table rows: copy a row with `copy.deepcopy(row._tr)` (reference "Tables").
4. Do not change fonts, styles, margins or numbering the user did not ask to change.
5. Headings changed and there is a TOC: `fill_toc.py FILE --inplace`.
Say what you cannot do: python-docx does not edit embedded charts/OLE objects, SmartArt or equations.

## 4. Tracked changes

```bash
python3 /skills/docx/scripts/tracked_changes.py IN.docx --list
python3 /skills/docx/scripts/tracked_changes.py IN.docx --accept-all --out /workspace/outputs/clean.docx
python3 /skills/docx/scripts/tracked_changes.py IN.docx --reject-all --out /workspace/outputs/original.docx
python3 /skills/docx/scripts/tracked_changes.py IN.docx --accept-all --strip-comments --out /workspace/outputs/final.docx
python3 /skills/docx/scripts/replace.py IN.docx --out /workspace/outputs/proposal.docx --find "old" --replace "new" --track --author "Studio"
```
Edits made with python-docx code are NOT tracked; say so if the user expects tracked changes.

## 5. Convert

```bash
bash /skills/docx/scripts/to_pdf.sh /workspace/outputs/report.docx /workspace/outputs      # prints {"pdf", "pages"}
pandoc IN.md -o /workspace/outputs/OUT.docx --reference-doc=/workspace/.work/template.docx   # Markdown -> docx with a template's styles
```
Keep a template you only borrow styles from in `/workspace/.work/` (or use the user's file where it is),
not in `/workspace/outputs/`. Check the PDF with `python3 /skills/pdf/scripts/extract.py --check` if the pdf skill is loaded.

## 6. Verify (the FINISH GATE in detail)

`inspect_docx.py --check FILE` runs these checks; fix a failing one like this:

| Check | Fails when | Fix |
|---|---|---|
| `opens` | the file is not a valid .docx | rebuild it |
| `content` | the body is empty | add the content |
| `headings` | more than 500 words and no Heading style | use heading blocks / `doc.add_heading` |
| `empty_paragraphs` | 3+ empty paragraphs in a row | delete them; use spacing or a page break |
| `footer` | page number glued to text, two page numbers, numbers typed as text | `footer.py FILE --text "Title" --inplace` |
| `toc` | a TOC field shows no entries or "update the table" | `fill_toc.py FILE --inplace` |
| `table_widths` | no column grid, columns < 0.5 cm, table wider than the text | `widths_cm` / `set_col_widths` / `columnWidths` |
| `images` | an image is larger than the text area | give a width that fits (`width_cm` <= 16 on A4) |
| `placeholders` | `{{...}}`, Lorem ipsum, TODO, TBD, FIXME, `[Insert ...]` left | replace them with real content |

For edits, also compare before and after (`inspect_docx.py --text` on both) and confirm only the
intended changes happened.

## 7. Deliver

Reply with a short summary (what the document contains, anything you could not do or verify) and
link each file: `[report.docx](workspace-file:///workspace/outputs/report.docx)`. Link only files
that exist under `/workspace/outputs/`. Do not paste the document into the chat.

## Pitfalls
- `font.name` alone does not change Heading/Title fonts: their theme attributes win. Use `set_font`
  (`reference/python-docx.md`), which also removes the theme attributes.
- Column widths set only on cells are ignored by LibreOffice and the PDF export: use `set_col_widths` (sets the grid).
- `paragraph.text = "..."` deletes the runs (formatting, images, fields) of that paragraph; prefer `replace.py`.
- Images: always give a width (`Cm(16)` max on A4 portrait with 2.2 cm margins).
- `doc.tables` lists top-level tables only; nested tables are `cell.tables`.
- Fonts not installed are substituted and change the page count: use Arial, Times New Roman,
  Courier New, Calibri or Cambria.
