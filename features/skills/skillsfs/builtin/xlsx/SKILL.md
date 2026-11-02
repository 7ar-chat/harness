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

In a Python script you write, take the output path from `sys.argv[1]` and pass it on the command line (`python3 /workspace/.work/build.py /workspace/outputs/book.xlsx`): on some backends `/workspace` exists only in command lines, so a path typed inside the file fails.

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

**Default: JSON spec or CSV -> `build_xlsx.py`** (no code to write; the rules below are applied for you):
```json
{"names": {"growth": "Inputs!$B$2"},
 "sheets": [
  {"name": "Inputs", "header": false, "rows": [["Item", "Value"], ["Growth rate", 0.05]],
   "formats": {"B": "0.0%"}, "inputs": ["B2"], "widths": {"A": 18}},
  {"name": "Plan",
   "rows": [["Month", "Revenue", "Cost", "Margin"],
            ["Jan", 1000, "=B2*0.6", "=B2-C2"],
            ["Feb", "=B2*(1+growth)", "=B3*0.6", "=B3-C3"]],
   "formats": {"B:D": "#,##0"}, "total": {"label": "Total", "cols": ["B", "C", "D"]},
   "charts": [{"type": "column", "title": "Revenue vs cost", "categories": "A2:A3",
               "series": ["B1:B3", "C1:C3"], "anchor": "G2"}]}]}
```
```bash
python3 /skills/xlsx/scripts/build_xlsx.py --spec /workspace/.work/spec.json --out /workspace/outputs/model.xlsx
python3 /skills/xlsx/scripts/build_xlsx.py --csv data.csv --out /workspace/outputs/data.xlsx --decimal ","   # CSV: numbers and ISO dates become real
```
- Sheet keys: `name`, `rows` (row 1 = header unless `"header": false`; `"=B2*C2"` is a live formula, a number is a number),
  `formats` (column letter, `B:D`, header text or a cell range `B2:B3` -> number format), `inputs` (blue font), `total`, `widths`, `freeze`,
  `charts` (`column|bar|line|pie`; a series range starts at its header cell), `coerce`. Top level: `names` (named cells).
- The header is styled, frozen and filtered; widths are fitted; a bad spec lists every problem with the closest key.
  Do not read the script: `--example` prints a full spec, and everything above is the whole format.
- To change a workbook you built this way, edit the spec and rebuild (it is the source); `edit_xlsx.py` refuses a workbook
  with charts. Hand-written openpyxl/xlsxwriter (below) only for what the spec cannot say (conditional formatting,
  validation, tables, images, print setup).

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
2. Look before you read: `inspect_xlsx.py BOOK.xlsx --sheet Data --stats` (used range, cell kinds, header), then only what
   you need: `--range A1:H60 --cols A,C:E --max-rows 50` (a cut dump says which `--range` continues it),
   `--where formula|error|empty|number|text` (just those cells; add `--values` for cached values).
   Then `python3 /skills/xlsx/scripts/inspect_xlsx.py BOOK.xlsx` and read `fragile_parts` / `warnings`
   (`inspect_xlsx.py BOOK.xlsx --formulas [--sheet Data]` lists every formula; `--sheet Data --range A1:H30`
   dumps cells as written, add `--values` for the computed values):
   - none -> the usual edits as ops, all-or-nothing, no code:
     ```bash
     echo '[{"op":"set_cell","sheet":"Data","cell":"B2","value":42},
            {"op":"format_range","sheet":"Data","range":"A1:D1","bold":true,"fill":"1F3864","color":"FFFFFF"},
            {"op":"append_rows","sheet":"Data","rows":[["Total","=SUM(B2:B9)"]]}]' | python3 /skills/xlsx/scripts/edit_xlsx.py BOOK.xlsx --ops - --inplace
     python3 /skills/xlsx/scripts/edit_xlsx.py --index          # the ops: set_cell set_range append_rows clear_range format_range
                                                                # set_col_width freeze set_filter find_replace add_sheet rename_sheet
                                                                # delete_sheet add_chart define_name; --op NAME: one op with an example
     python3 /skills/xlsx/scripts/edit_xlsx.py BOOK.xlsx --ops ops.json --dry-run     # the plan, nothing written
     ```
     A rejected op leaves the file untouched; the failure gives `reason`, `available` (sheet names) and `did_you_mean`:
     fix that op and resend the whole list. Then `recalc.py`. Anything else: openpyxl `load_workbook(path)` (never `data_only=True` for a file you save: it
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
