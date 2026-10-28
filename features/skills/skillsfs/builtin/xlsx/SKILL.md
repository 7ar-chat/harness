---
name: xlsx
description: Create, edit and check Excel workbooks (.xlsx/.xlsm) - financial models, budgets, trackers, data tables, pivot-style summaries, Excel exports. Formulas are written as live Excel formulas (not typed results), recalculated with LibreOffice and checked for #REF!/#DIV/0!/#NAME? errors, typed totals and numbers stored as text. Covers number formats, widths, freeze panes, conditional formatting, validation, Excel tables, native charts, and editing a workbook while keeping its formulas, styles and charts. To only read a workbook or CSV, call read_file (sheet=...) or use pandas; you do not need this skill for that.
---

# Excel workbooks (.xlsx)

**FINISH GATE - do this before you say anything about the workbook to the user.**
1. Run `python3 /skills/xlsx/scripts/recalc.py /workspace/outputs/BOOK.xlsx` once after your last change
   (it recalculates every formula, stores the values in the file and checks it). The runtime also checks
   every workbook a tool call changes (`recalc.py --check-only`) and adds the verdict to that call's result
   (`verification`): do not run `recalc.py` again on a workbook you did not change since.
2. If it prints `"ok": false` (exit code 2): fix every check with `"ok": false` (`errors` lists the
   cells), save, and run the same command again. At most 3 rounds.
3. When it prints `"ok": true`: read a few key results with
   `python3 /skills/xlsx/scripts/inspect_xlsx.py BOOK.xlsx --sheet Summary --range A1:F15 --values`
   and compare them with the source. For a formatted report also call `view_file` on the workbook once
   (if `view_file` answers that this model cannot view images, skip that and say so).
4. Cross-check the data against the files it comes from:
   `python3 /skills/xlsx/scripts/cross_check.py /workspace/outputs/BOOK.xlsx SOURCE [SOURCE ...]` lists the
   figures, dates and names of the workbook that are in none of them. Results of your formulas are
   expected in that list; typed-in data, dates and labels are not: check those in the source and fix them.
5. Never say the workbook is done, ready or created while `"ok": false`. After 3 rounds, tell
   the user exactly which cells are still wrong. In your answer, say what you cross-checked against
   which file.

Only a changed file needs a new check or view: a passing check and a `view_file` after a file's last change stay
valid. Never check or view again a file you did not change since (one `view_file` per file after its last change).

**Files.** Save the final workbook in `/workspace/outputs/` (or the path the user asked for).
Never put a deliverable in `/workspace/.work/` and never link a file there: it is scratch space and
is not kept. In your answer, only link files under `/workspace/outputs/`.

**Reading.** `read_file` returns the cells of a workbook directly (`sheet` for one sheet). For
analysis use pandas: `pd.read_excel(path, sheet_name=None)`; CSV `pd.read_csv(path, sep=None,
engine="python")` (French files: `decimal=","`, `thousands=" "`, `encoding="utf-8-sig"` or `"cp1252"`).
Files written by openpyxl/xlsxwriter have no computed values until `recalc.py` runs (pandas reads NaN, xlsxwriter shows 0).

Scripts are in `/skills/xlsx/scripts/`; run them with `python3` from `/workspace`.

## 0. Decide

| The user wants | Do |
|---|---|
| Analyse data | pandas (above); give numbers with units and the sheet/range they come from |
| A new workbook | §1 Create |
| Changes to an existing workbook | §2 Edit |
| Always, at the end | the FINISH GATE (§3), then §4 Deliver |

## 1. Create

Rules (the user opens the file in Excel and expects a working model):
1. **Formulas, not values**: totals, ratios, growth and lookups are Excel formulas (`=SUM(B2:B13)`,
   `=IFERROR(C2/B2,0)`). Compute in Python only data without a formula meaning (imports, samples).
2. **Numbers as numbers**: write `5100`, never `"5100"` (a number stored as text counts as 0 in
   SUM). xlsxwriter: `write_number` or `Workbook(path, {"strings_to_numbers": True})`.
3. Inputs apart from calculations (an `Inputs` sheet or block, blue font for inputs), referenced
   with absolute refs (`Inputs!$B$2`) or defined names.
4. A number format on every numeric cell (`#,##0`, `0.0%`, `#,##0 "€"`, `dd/mm/yyyy`); dates as
   `datetime.date`, percentages as ratios (0.125 with `0.0%`).
5. Header row styled and frozen (`ws.freeze_panes = "A2"`), autofilter or Excel table, column
   widths 12-18 for numbers and 30-50 for text, no merged cells inside data, one table per sheet from A1.
6. Use functions LibreOffice 7.4 computes: SUM, SUMIFS, COUNTIFS, AVERAGEIFS, INDEX/MATCH,
   VLOOKUP, IF, IFERROR, ROUND, TEXT, DATE, EOMONTH, NPV/IRR/PMT. Not XLOOKUP, LET, LAMBDA,
   FILTER, UNIQUE, SORT or dynamic arrays (they give `#NAME?` in the check).
- openpyxl (create and edit, the default): `reference/openpyxl.md` (styles, formats, widths,
  tables, conditional formatting, validation, names, charts, images, print setup).
  `cell.fill` takes a PatternFill, not a colour string, and openpyxl colours have no "#".
- xlsxwriter (create only; fast, rich charts): `reference/xlsxwriter.md`.
- pandas: `df.to_excel(writer, sheet_name="Data", index=False)` with `pd.ExcelWriter(path,
  engine="openpyxl")`, then reopen with openpyxl for formats and formulas.
- Pivot tables cannot be created: build a summary sheet with SUMIFS/COUNTIFS and say so.

## 2. Edit an existing workbook

1. Work on a copy in `/workspace/outputs/`; never overwrite the upload.
2. `python3 /skills/xlsx/scripts/inspect_xlsx.py BOOK.xlsx` and read `fragile_parts` / `warnings`
   (`inspect_xlsx.py BOOK.xlsx --formulas [--sheet Data]` lists every formula; `--sheet Data --range A1:H30`
   dumps cells as written, add `--values` for the computed values):
   - none -> openpyxl `load_workbook(path)` (never `data_only=True` for a file you save: it
     replaces every formula by its value). Write values into existing cells; copy styles to new rows.
   - charts, images, pivot tables, slicers, macros or shapes -> openpyxl may drop them. Use:
     ```bash
     python3 /skills/xlsx/scripts/set_cells.py BOOK.xlsx --inplace --set "Data!B5=1250" --set "Data!B20==SUM(B2:B19)"
     python3 /skills/xlsx/scripts/set_cells.py BOOK.xlsx --inplace --json /workspace/.work/edits.json
     ```
3. `insert_rows` does not update formulas, names, charts or conditional formats below: append
   at the end, or rewrite the affected formulas, then run the gate.
4. Keep the workbook's own styles, sheet order, names and formats.

## 3. Verify (the FINISH GATE in detail)

`recalc.py BOOK.xlsx` (`--check-only` to leave the file untouched, `--out COPY.xlsx` to write elsewhere):

| Check | Fails when | Fix |
|---|---|---|
| `opens` | LibreOffice cannot open or recalculate the file | rebuild it; re-run once if it timed out |
| `formula_errors` | a formula gives #DIV/0!, #REF!, #NAME?, #VALUE!, #N/A... | fix the range, `IFERROR`, classic functions (`unsupported_functions`) |
| `live_formulas` | a Total/Sum/Average row holds typed numbers | write `=SUM(...)` / `=AVERAGE(...)` |
| `numbers_as_text` | a number stored as text sits in a range a formula uses | write it as a number |

CSV, JSON and Python deliverables: `python3 /skills/xlsx/scripts/validate_file.py FILE` (checks `csv` even rows, `json` parses, `py_compile`; exit 0 ok, 2 not ok).

## 4. Deliver

Explain the structure in a few lines (sheets, inputs, key formulas, assumptions) and link the file:
`[model.xlsx](workspace-file:///workspace/outputs/model.xlsx)`. Link only files under `/workspace/outputs/`.
Mention limits (for example "pivot table not refreshed: open in Excel and click Refresh").

## Pitfalls
- openpyxl writes formulas in English with `,` separators whatever the user's Excel language.
- Excel tables (`ws.add_table`) need unique, non-empty headers and a unique name without spaces.
- More than 100k rows: xlsxwriter `constant_memory`, or deliver a CSV plus a summary workbook.
- `.xlsm`: keep macros with `load_workbook(path, keep_vba=True)` and save as `.xlsm`, or use `set_cells.py`.
