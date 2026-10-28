#!/usr/bin/env python3
"""Summarise an .xlsx/.xlsm, or dump a cell range.

Usage:
  python3 inspect_xlsx.py BOOK.xlsx                         # JSON workbook summary
  python3 inspect_xlsx.py BOOK.xlsx --sheet Data --range A1:H30            # formulas as written (= --formulas)
  python3 inspect_xlsx.py BOOK.xlsx --sheet Data --range A1:H30 --values   # cached values (run recalc.py first)
  python3 inspect_xlsx.py BOOK.xlsx --formulas                            # every formula of every sheet (--sheet: one)

Summary per sheet: dimensions, header row, freeze panes, autofilter, tables, merged cells,
formula count + samples, conditional formats, data validations, number formats, column
widths. Workbook: defined names, and a scan of the package for charts, images, pivot
tables, slicers, macros, external links and comments. openpyxl re-writes or loses some of
these when it re-saves a file, so check "fragile_parts" before editing with openpyxl.
"""
from __future__ import annotations

import argparse
import collections
import json
import re
import sys
import zipfile


def scan_package(path):
    names = zipfile.ZipFile(path).namelist()
    pat = {"charts": r"xl/charts/chart\d+\.xml$", "images": r"xl/media/", "drawings": r"xl/drawings/drawing\d+\.xml$",
           "pivot_tables": r"xl/pivotTables/", "pivot_caches": r"xl/pivotCache/", "slicers": r"xl/slicers?/",
           "vba_macros": r"xl/vbaProject\.bin$", "external_links": r"xl/externalLinks/", "comments": r"xl/comments\d*\.xml$",
           "threaded_comments": r"xl/threadedComments/", "shapes_vml": r"xl/drawings/vmlDrawing", "calc_chain": r"xl/calcChain\.xml$"}
    found = {k: sum(1 for n in names if re.search(v, n)) for k, v in pat.items()}
    fragile = [k for k in ("charts", "images", "pivot_tables", "slicers", "vba_macros", "external_links",
                           "threaded_comments") if found[k]]
    return found, fragile


def jsonable(v):
    if v is None or isinstance(v, (int, float, str, bool)):
        return v
    return str(v)


def dump_range(path, sheet, ref, values):
    import openpyxl

    wb = openpyxl.load_workbook(path, data_only=values)
    ws = wb[sheet] if sheet else wb.active
    rows = []
    for row in ws[ref]:
        rows.append([jsonable(c.value) for c in row])
    return {"sheet": ws.title, "range": ref, "values" if values else "cells": rows}


def list_formulas(path, sheet=None, limit=2000):
    import openpyxl

    wb = openpyxl.load_workbook(path)  # formulas as written
    sheets = [wb[sheet]] if sheet else wb.worksheets
    cells, total = [], 0
    for ws in sheets:
        for row in ws.iter_rows():
            for c in row:
                if c.data_type == "f" or (isinstance(c.value, str) and c.value.startswith("=")):
                    total += 1
                    if len(cells) < limit:
                        cells.append({"sheet": ws.title, "cell": c.coordinate, "formula": str(c.value)})
    return {"file": path, "formulas": cells, "count": total, "truncated": total > len(cells)}


def summary(path):
    import openpyxl

    wb = openpyxl.load_workbook(path)  # formulas as written
    found, fragile = scan_package(path)
    sheets = []
    for ws in wb.worksheets:
        formulas = []
        fcount = 0
        nfmt = collections.Counter()
        for row in ws.iter_rows():
            for c in row:
                if c.data_type == "f" or (isinstance(c.value, str) and c.value.startswith("=")):
                    fcount += 1
                    if len(formulas) < 10:
                        formulas.append({"cell": c.coordinate, "formula": str(c.value)})
                if c.value is not None and c.number_format != "General":
                    nfmt[c.number_format] += 1
        header = [jsonable(c.value) for c in next(ws.iter_rows(min_row=1, max_row=1), [])][:30]
        sheets.append({
            "name": ws.title, "state": ws.sheet_state, "dimensions": ws.dimensions,
            "max_row": ws.max_row, "max_column": ws.max_column, "header_row": header,
            "freeze_panes": ws.freeze_panes, "autofilter": ws.auto_filter.ref,
            "tables": [{"name": t.name, "ref": t.ref} for t in ws.tables.values()],
            "merged_ranges": len(ws.merged_cells.ranges),
            "formulas": fcount, "formula_samples": formulas,
            "conditional_formats": sum(len(r.rules) for r in ws.conditional_formatting),
            "data_validations": len(ws.data_validations.dataValidation),
            "number_formats": dict(nfmt.most_common(8)),
            "column_widths": {k: round(v.width, 1) for k, v in list(ws.column_dimensions.items())[:30] if v.width},
        })
    names = {}
    try:
        for name, dn in wb.defined_names.items():
            names[name] = dn.attr_text
    except AttributeError:  # older openpyxl
        names = {dn.name: dn.attr_text for dn in wb.defined_names.definedName}
    warnings = []
    if fragile:
        warnings.append("package contains " + ", ".join(fragile) + ": openpyxl load+save re-writes charts/images "
                        "(formatting can change) and loses shapes, slicers, form controls, threaded comments and "
                        "macros (unless keep_vba=True). For value/formula edits use set_cells.py, then recalc.py")
    return {"file": path, "sheets": sheets, "defined_names": names, "package": found,
            "fragile_parts": fragile, "warnings": warnings}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file")
    ap.add_argument("--sheet")
    ap.add_argument("--range")
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--values", action="store_true", help="cached values instead of formulas (run recalc.py first)")
    mode.add_argument("--formulas", action="store_true",
                      help="formulas as written (the default with --range); without --range: every formula cell of "
                           "the workbook (or of --sheet), up to 2000")
    args = ap.parse_args(argv)
    try:
        if args.range:
            out = dump_range(args.file, args.sheet, args.range, args.values)
        elif args.formulas:
            out = list_formulas(args.file, args.sheet)
        elif args.values:
            raise ValueError("--values needs --range (e.g. --sheet Summary --range A1:F15 --values)")
        else:
            out = summary(args.file)
    except Exception as exc:  # noqa: BLE001
        print(json.dumps({"error": f"{type(exc).__name__}: {exc}"}))
        return 1
    print(json.dumps(out, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
