---
name: pdf
description: Create, manipulate and check PDF files - extract text or tables (to CSV/Excel), merge, split, reorder, delete or rotate pages, fill PDF forms (AcroForm), add a watermark or stamp, produce a PDF report, letter or export (JSON spec builder sharing the docx spec format, reportlab, or Word/HTML/Markdown converted to PDF), and check a PDF deliverable (text layer, fonts embedded, glyphs). OCR of scanned PDFs is not available. To only read or summarise a PDF, call read_file on it (pages="1-3"); you do not need this skill for that.
license: Internal (Studio)
requires: [studio-brand]
---

# PDF

**FINISH GATE - do this before you say anything about a PDF you produced.**
1. Check: `python3 /skills/pdf/scripts/extract.py --check /workspace/outputs/FILE.pdf`.
   The runtime already runs it after every tool call that changes a file in `/workspace/outputs/` and adds the
   verdict to that call's result (`verification`): when the verdict of your last change is there, do not run it again.
2. If it is `"ok": false` (exit code 2): fix every check with `"ok": false` (its `detail` says
   how), rebuild the PDF, and check again. At most 3 rounds.
3. When it is `"ok": true`: call `view_file` on `/workspace/outputs/FILE.pdf` once (6 pages per call,
   `pages: "7-12"` for more) and look: cut tables, overlapping text, blank pages, wrong rotation.
   If `view_file` answers that this model cannot view images, skip this step and say so; if it cannot
   render the file, say "could not view: <its reason>".
4. Cross-check the content against the files it comes from:
   `python3 /skills/pdf/scripts/cross_check.py /workspace/outputs/FILE.pdf SOURCE [SOURCE ...]` lists the
   figures, dates and names that are in none of them; check each one in the source and fix the wrong ones.
5. Never say the PDF is done or ready while `"ok": false`. After 3 rounds, tell the user exactly
   which checks still fail. For merges and splits, also check the page counts add up (`pages.py info`).
   In your answer, say what you cross-checked against which file.

**CITATION GATE - whenever you cite pages of a PDF ("(p. 12)") in a deliverable or in your answer.**
1. `python3 /skills/pdf/scripts/check_citations.py SOURCE.pdf DELIVERABLE` (`.docx`, `.pptx`, `.md`, `.pdf`...;
   for your chat answer, write it to `/workspace/.work/answer.md` first). It checks that every figure and
   quote before a "(p. N)" is printed on page N (exit 0 ok, 2 not ok). The runtime also runs it on every
   .docx/.pptx/.pdf you change when the thread has an uploaded PDF.
2. Every claim it lists: read that page (`read_file` with `pages`), then use the figure printed there,
   cite the page where it is printed, or remove the claim. Never cite a page for a figure you did not
   read on it. Check again until `"ok": true`.
3. In your answer, say that the citations were checked against the PDF.

Only a changed file needs a new check or view: a passing check and a `view_file` after a file's last change stay
valid. Never check or view again a file you did not change since (one `view_file` per file after its last change).

**Files.** Save every deliverable in `/workspace/outputs/` (or the path the user asked for).
Never put a deliverable in `/workspace/.work/` and never link a file there: it is scratch space and
is not kept. In your answer, only link files under `/workspace/outputs/`.

**Reading.** `read_file` returns the text of a PDF directly (`pages: "1-3,7"`). Use `extract.py`
for tables, word positions, layout-preserving text or the scanned-page flags.

Tools: pdfplumber, pypdf, qpdf, poppler (`pdftotext`, `pdfinfo`), reportlab, LibreOffice, pandoc.
No OCR (tesseract is not installed). Scripts: `/skills/pdf/scripts/` (run with `python3` from `/workspace`).

## 0. Decide

| The user wants | Do |
|---|---|
| Tables or data out of a PDF | §1 |
| Merge / split / reorder / rotate / delete pages | §2 |
| Fill a form | §3 |
| Watermark / stamp | §4 |
| A new PDF | §5 |
| Always, at the end | the FINISH GATE (§6), then §7 Deliver |

## 1. Extract

```bash
python3 /skills/pdf/scripts/extract.py IN.pdf --tables --pages 3-5 --csv-dir /workspace/outputs/tables   # one CSV per table
python3 /skills/pdf/scripts/extract.py IN.pdf --format markdown        # text + tables per page
python3 /skills/pdf/scripts/extract.py IN.pdf --layout --pages 2       # keep columns aligned
python3 /skills/pdf/scripts/extract.py IN.pdf --words --pages 1        # words + coordinates
python3 /skills/pdf/scripts/extract.py IN.pdf                          # JSON: metadata, text per page, scanned flags
```
- Tables: check the rows (merged headers, wrapped cells come out as `None` or multi-line); clean
  with pandas before exporting to Excel (`xlsx` skill).
- Pages flagged `"scanned": true` have no text layer and OCR is unavailable: tell the user.
- Encrypted: `--password PW` or `qpdf --decrypt --password=PW in.pdf out.pdf`. Damaged: `qpdf in.pdf /workspace/.work/repaired.pdf`.

## 2. Page operations

```bash
python3 /skills/pdf/scripts/pages.py info IN.pdf
python3 /skills/pdf/scripts/pages.py merge /workspace/outputs/all.pdf a.pdf b.pdf c.pdf [--bookmarks]
python3 /skills/pdf/scripts/pages.py split IN.pdf /workspace/outputs/parts [--every 5 | --ranges "1-3,4-9"]
python3 /skills/pdf/scripts/pages.py select IN.pdf OUT.pdf --pages "1,3-5,2"
python3 /skills/pdf/scripts/pages.py delete IN.pdf OUT.pdf --pages "2,7"
python3 /skills/pdf/scripts/pages.py rotate IN.pdf OUT.pdf --angle 90 --pages "2"
```
Office files: convert first (`soffice --headless --convert-to pdf --outdir /workspace/.work file.docx`).

## 3. Fill a form

```bash
python3 /skills/pdf/scripts/fill_form.py FORM.pdf --list                     # names, types, options, pages
python3 /skills/pdf/scripts/fill_form.py FORM.pdf --template > /workspace/.work/values.json
python3 /skills/pdf/scripts/fill_form.py FORM.pdf --values /workspace/.work/values.json --out /workspace/outputs/filled.pdf [--flatten]
```
- Exit 2 with `mismatches`/`unknown`: a field name or option is wrong; fix values.json and rerun.
- Map the user's data with the field tooltips (`--list`) and nearby words (`extract.py --words`).
  Never invent personal data, signatures or dates: ask.
- `"count": 0`: a flat form; overlay text at coordinates (`reference/forms.md` "Flat forms").

## 4. Watermark / stamp

```bash
python3 /skills/pdf/scripts/watermark.py IN.pdf OUT.pdf --text "DRAFT"
python3 /skills/pdf/scripts/watermark.py IN.pdf OUT.pdf --text "Copie pour M. X" --position footer
python3 /skills/pdf/scripts/watermark.py IN.pdf OUT.pdf --overlay stamp.png --position top-right --width-pt 90
```
Do not add confidentiality classifications yourself: Studio labels downloaded files.

## 5. Create a PDF

1. **Report-like document**: JSON spec -> `build_pdf.py` (Studio styling, embedded fonts, page x of y,
   bookmarks, TOC). It takes exactly the docx builder's spec, shorthand included
   (`{"heading": "Intro"}`, `{"paragraph": "..."}`, a bare string), so one spec gives both formats.
   ```bash
   python3 /skills/pdf/scripts/build_pdf.py --example > /workspace/.work/spec.json
   python3 /skills/pdf/scripts/build_pdf.py --spec /workspace/.work/spec.json --out /workspace/outputs/report.pdf
   ```
   Blocks: `heading`, `paragraph`, `bullets`, `numbered`, `table{columns,rows,widths_cm,numeric_cols,caption}`,
   `image{path,width_cm,caption}`, `callout`, `toc`, `page_break`.
2. **The user also wants an editable Word file**: build the .docx with the `docx` skill, then
   `bash /skills/docx/scripts/to_pdf.sh report.docx /workspace/outputs`.
3. **Markdown / HTML**: `pandoc in.md -o /workspace/.work/in.docx`, then LibreOffice to PDF (pandoc's
   own PDF output needs LaTeX, not installed). Simple HTML: `soffice --headless --convert-to pdf in.html`.
4. **Custom layout** (letters, certificates, invoices): reportlab, `reference/generate.md` (embedded
   fonts with `register_fonts()`, tables, images, "Page x of y" with `numbered_canvas`); fillable
   fields in `reference/forms.md`.
Charts: a PNG from the `charts` skill as an `image` block.

## 6. Verify (the FINISH GATE in detail)

`extract.py --check FILE.pdf` runs these checks (first 100 pages):

| Check | Fails when | Fix |
|---|---|---|
| `opens` | pypdf/pdfplumber cannot open the file | rebuild it (or `qpdf` repair) |
| `pages` | no page | rebuild it |
| `text` | no text layer at all, or a blank page | build from text; remove the blank page / fix the page break |
| `fonts_embedded` | a font used for text is not embedded (the 14 standard PDF fonts are accepted) | use a TrueType font (`build_pdf.py`, `register_fonts()`, `brand.reportlab_fonts()`) |
| `glyphs` | characters without a glyph (U+FFFD, notdef: shown as boxes) | a font that covers the text (Liberation Sans / DejaVu Sans) |

Forms: `fill_form.py OUT.pdf --list` shows the values you set.

## 7. Deliver

Link the result: `[report.pdf](workspace-file:///workspace/outputs/report.pdf)`; for extractions,
give the key findings in the answer and link the CSV/XLSX files. Link only files under `/workspace/outputs/`.

## Pitfalls
- pdfplumber interleaves multi-column text: use `extract.py --layout` or crop (`page.crop(...)`, `reference/forms.md`).
- Page numbers in viewers can differ from printed page labels: say which you use.
- reportlab built-in fonts (Helvetica, Times) only cover Western European text and are not embedded:
  register a TTF for anything else.
- reportlab coordinates start bottom-left, in points (1 cm = 28.35 pt; A4 = 595 x 842 pt).
- Form fields from several PDFs can collide on names when merged: flatten before merging.
