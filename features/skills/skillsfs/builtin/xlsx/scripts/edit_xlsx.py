#!/usr/bin/env python3
"""Edit an .xlsx with a JSON list of ops (openpyxl underneath): all-or-nothing, with a dry run and
errors that name the fix. Use it instead of hand-written openpyxl code for the usual edits.

Usage:
  python3 edit_xlsx.py BOOK.xlsx --ops ops.json --out NEW.xlsx       # or --inplace
  echo '[{"op":"set_cell","cell":"B2","value":42}]' | python3 edit_xlsx.py BOOK.xlsx --ops - --inplace
  python3 edit_xlsx.py BOOK.xlsx --ops ops.json --dry-run            # the plan, nothing written
  python3 edit_xlsx.py --index                                       # one line per op
  python3 edit_xlsx.py --op format_range                             # one op: signature, fields, example

Every op is {"op": NAME, ...}; "sheet" (default: the first sheet) is the tab name as spelled in the file.
Ops run in order and each sees the previous result (a sheet added or renamed earlier is addressed by its
new name). Every op is validated before the first one is applied; if any op is rejected the file is left
untouched and the failure says which op (index), why (reason), what exists (available) and the closest
spelling (did_you_mean): fix that op and resend the WHOLE list. A workbook with charts, images, pivot
tables, slicers or macros is refused (openpyxl would drop them): use set_cells.py for values and formulas.
Prints {"ok", "out", "applied", "plan"?, "formulas_touched", "next_step"}. Run recalc.py afterwards.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from opserr import OpError, did_you_mean, fail, unknown  # noqa: E402

# name -> (signature, {field: required?}, example)
OPS = {
    "set_cell": ('{cell, value | formula, sheet?, format?}', {"cell": True},
                 {"op": "set_cell", "cell": "B2", "value": 42}),
    "set_range": ('{start, values:[[...]], sheet?}  (a string "=..." is a formula)', {"start": True, "values": True},
                  {"op": "set_range", "start": "A2", "values": [["Jan", 100, "=B2*2"]]}),
    "append_rows": ('{rows:[[...]], sheet?}  (after the last used row)', {"rows": True},
                    {"op": "append_rows", "rows": [["Total", "=SUM(B2:B9)"]]}),
    "clear_range": ('{range, sheet?}', {"range": True}, {"op": "clear_range", "range": "A10:D20"}),
    "format_range": ('{range, sheet?, bold?, italic?, size?, color?, fill?, numFmt?, align?, wrap?, border?}',
                     {"range": True},
                     {"op": "format_range", "range": "A1:D1", "bold": True, "fill": "1F3864", "color": "FFFFFF"}),
    "set_col_width": ('{col | cols:[...], width, sheet?}', {"width": True}, {"op": "set_col_width", "col": "A", "width": 30}),
    "freeze": ('{cell | "none", sheet?}', {"cell": True}, {"op": "freeze", "cell": "A2"}),
    "set_filter": ('{range | "none", sheet?}', {"range": True}, {"op": "set_filter", "range": "A1:D20"}),
    "find_replace": ('{find, replace, sheet?, formulas?}  (text cells; formulas too when formulas:true)',
                     {"find": True, "replace": True}, {"op": "find_replace", "find": "2025", "replace": "2026"}),
    "add_sheet": ('{name, rows?, index?}', {"name": True}, {"op": "add_sheet", "name": "Summary", "rows": [["Item", "Qty"]]}),
    "rename_sheet": ('{sheet, name}', {"sheet": True, "name": True}, {"op": "rename_sheet", "sheet": "Sheet1", "name": "Data"}),
    "delete_sheet": ('{sheet}', {"sheet": True}, {"op": "delete_sheet", "sheet": "Scratch"}),
    "add_chart": ('{type: column|bar|line|pie, series:[range...], categories?, anchor?, title?, sheet?}',
                  {"type": True, "series": True},
                  {"op": "add_chart", "type": "column", "series": ["B1:B13"], "categories": "A2:A13", "anchor": "F2"}),
    "define_name": ('{name, ref}', {"name": True, "ref": True}, {"op": "define_name", "name": "growth", "ref": "Inputs!$B$1"}),
}
FORMULA_OPS = {"set_cell", "set_range", "append_rows", "find_replace", "add_chart"}
FRAGILE = ("charts", "images", "pivot_tables", "slicers", "vba_macros", "external_links", "threaded_comments")
CELL_RE = re.compile(r"^\$?[A-Za-z]{1,3}\$?\d+$")
RANGE_RE = re.compile(r"^\$?[A-Za-z]{1,3}\$?\d+(:\$?[A-Za-z]{1,3}\$?\d+)?$")


def usage_of(name):
    return f"{name} {OPS[name][0]}"


def index_text():
    return "\n".join(f"{n} {sig}" for n, (sig, _, _) in OPS.items())


def op_text(name):
    sig, req, ex = OPS[name]
    return json.dumps({"op": name, "signature": sig, "required": [k for k, v in req.items() if v], "example": ex},
                      indent=2, ensure_ascii=False)


def plan_ops(ops, sheet_names):
    """Validate every op against the state the previous ops leave; returns plan lines. Raises (index, OpError)."""
    names, plan = list(sheet_names), []
    failures = []
    for i, op in enumerate(ops):
        try:
            if not isinstance(op, dict) or "op" not in op:
                raise OpError("op_rejected", "each op is an object with an \"op\" name", usage="edit_xlsx.py --index")
            name = op["op"]
            if name not in OPS:
                raise unknown("op", name, OPS, "unknown_op")
            for field, required in OPS[name][1].items():
                if required and field not in op and not (field == "value" and "formula" in op):
                    if not (name == "set_cell" and "formula" in op) and not (name == "set_col_width" and "col" in op):
                        raise OpError("op_rejected", f"op {name!r}: missing {field!r}", usage=usage_of(name))
            if name == "set_cell" and "value" not in op and "formula" not in op:
                raise OpError("op_rejected", "op 'set_cell': give \"value\" or \"formula\"", usage=usage_of(name))
            if name == "set_col_width" and "col" not in op and "cols" not in op:
                raise OpError("op_rejected", "op 'set_col_width': give \"col\" or \"cols\"", usage=usage_of(name))
            for key in ("cell", "start"):
                if key in op and not (key == "cell" and op[key] == "none" and name == "freeze") \
                        and not CELL_RE.match(str(op[key])):
                    raise OpError("bad_address", f"{key} {op[key]!r} is not a cell address such as B2", usage=usage_of(name))
            if "range" in op and not (op["range"] == "none" and name == "set_filter") and not RANGE_RE.match(str(op["range"])):
                raise OpError("bad_address", f"range {op['range']!r} is not like A1:D20", usage=usage_of(name))
            target = op.get("sheet") if name != "add_sheet" else None
            if name in ("rename_sheet", "delete_sheet"):
                target = op["sheet"]
            if target is None and name not in ("add_sheet", "define_name"):
                target = names[0] if names else None
            if target is not None and target not in names:
                raise OpError("sheet_not_found", f"sheet not found: {target!r}", available=names,
                              did_you_mean=did_you_mean(target, names), usage=usage_of(name))
            if name == "add_sheet":
                if op["name"] in names:
                    raise OpError("op_rejected", f"sheet {op['name']!r} exists", available=names, usage=usage_of(name))
                names.append(op["name"])
            elif name == "rename_sheet":
                names[names.index(op["sheet"])] = op["name"]
            elif name == "delete_sheet":
                if len(names) == 1:
                    raise OpError("op_rejected", "a workbook keeps at least one sheet", usage=usage_of(name))
                names.remove(op["sheet"])
            if name == "add_chart" and op["type"] not in ("column", "bar", "line", "pie"):
                raise OpError("op_rejected", f"chart type {op['type']!r}", available=["column", "bar", "line", "pie"],
                              did_you_mean=did_you_mean(op["type"], ["column", "bar", "line", "pie"]), usage=usage_of(name))
            plan.append(f"[{i}] {name} " + " ".join(f"{k}={json.dumps(v, ensure_ascii=False)[:40]}"
                                                    for k, v in op.items() if k not in ("op", "values", "rows")))
        except OpError as e:
            failures.append(e.record(i, op.get("op") if isinstance(op, dict) else None,
                                     usage=e.extra.pop("usage", None)))
    return plan, failures


def apply_op(wb, op):
    from openpyxl.chart import BarChart, LineChart, PieChart, Reference
    from openpyxl.styles import Alignment, Border, Font, PatternFill, Side
    from openpyxl.utils import get_column_letter, range_boundaries
    from openpyxl.workbook.defined_name import DefinedName

    name = op["op"]
    ws = wb[op["sheet"]] if op.get("sheet") and name not in ("rename_sheet", "delete_sheet", "add_sheet") else wb.worksheets[0]
    if name == "set_cell":
        c = ws[op["cell"]]
        c.value = op["formula"] if "formula" in op and str(op["formula"]).startswith("=") else (
            "=" + op["formula"] if "formula" in op else op["value"])
        if op.get("format"):
            c.number_format = op["format"]
    elif name == "set_range":
        from openpyxl.utils.cell import coordinate_from_string, column_index_from_string
        col, row = coordinate_from_string(op["start"].replace("$", ""))
        c0 = column_index_from_string(col)
        for dr, vals in enumerate(op["values"]):
            for dc, v in enumerate(vals):
                ws.cell(row + dr, c0 + dc, v)
    elif name == "append_rows":
        for r in op["rows"]:
            ws.append(r)
    elif name == "clear_range":
        for row in ws[op["range"]] if ":" in op["range"] else [[ws[op["range"]]]]:
            for c in row:
                c.value = None
    elif name == "format_range":
        side = Side(style="thin", color="999999")
        for row in ws[op["range"]] if ":" in op["range"] else [[ws[op["range"]]]]:
            for c in row:
                if any(k in op for k in ("bold", "italic", "size", "color")):
                    f = c.font
                    c.font = Font(name=f.name, size=op.get("size", f.size), bold=op.get("bold", f.bold),
                                  italic=op.get("italic", f.italic), color=op.get("color", f.color))
                if op.get("fill"):
                    c.fill = PatternFill("solid", fgColor=op["fill"].lstrip("#"))
                if op.get("numFmt"):
                    c.number_format = op["numFmt"]
                if "align" in op or "wrap" in op:
                    c.alignment = Alignment(horizontal=op.get("align", c.alignment.horizontal),
                                            wrap_text=op.get("wrap", c.alignment.wrap_text), vertical=c.alignment.vertical)
                if op.get("border"):
                    c.border = Border(left=side, right=side, top=side, bottom=side)
    elif name == "set_col_width":
        for col in op.get("cols") or [op["col"]]:
            ws.column_dimensions[str(col).upper()].width = op["width"]
    elif name == "freeze":
        ws.freeze_panes = None if op["cell"] == "none" else op["cell"]
    elif name == "set_filter":
        ws.auto_filter.ref = None if op["range"] == "none" else op["range"]
    elif name == "find_replace":
        n = 0
        for row in ws.iter_rows():
            for c in row:
                if isinstance(c.value, str) and op["find"] in c.value and (not c.value.startswith("=") or op.get("formulas")):
                    c.value = c.value.replace(op["find"], op["replace"])
                    n += 1
        if n == 0:
            raise OpError("no_match", f"find_replace: {op['find']!r} matches no {'cell' if not op.get('formulas') else 'text or formula'}"
                          " (formulas are skipped unless \"formulas\": true)")
    elif name == "add_sheet":
        sh = wb.create_sheet(op["name"], op.get("index"))
        for r in op.get("rows") or []:
            sh.append(r)
    elif name == "rename_sheet":
        wb[op["sheet"]].title = op["name"]
    elif name == "delete_sheet":
        del wb[op["sheet"]]
    elif name == "add_chart":
        chart = {"column": BarChart, "bar": BarChart, "line": LineChart, "pie": PieChart}[op["type"]]()
        if op["type"] in ("column", "bar"):
            chart.type = "col" if op["type"] == "column" else "bar"
        if op.get("title"):
            chart.title = op["title"]
        for rng in op["series"]:
            c1, r1, c2, r2 = range_boundaries(rng)
            chart.add_data(Reference(ws, min_col=c1, max_col=c2, min_row=r1, max_row=r2), titles_from_data=True)
        if op.get("categories"):
            c1, r1, c2, r2 = range_boundaries(op["categories"])
            chart.set_categories(Reference(ws, min_col=c1, max_col=c2, min_row=r1, max_row=r2))
        chart.width, chart.height = 16, 8
        ws.add_chart(chart, op.get("anchor", f"{get_column_letter(ws.max_column + 2)}2"))
    elif name == "define_name":
        wb.defined_names[op["name"]] = DefinedName(op["name"], attr_text=op["ref"])


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file", nargs="?")
    ap.add_argument("--ops", help="JSON list of ops (a file, or - for stdin)")
    ap.add_argument("--out")
    ap.add_argument("--inplace", action="store_true")
    ap.add_argument("--dry-run", action="store_true", help="validate and print the plan; write nothing")
    ap.add_argument("--index", action="store_true", help="one line per op")
    ap.add_argument("--op", help="signature, required fields and an example of one op")
    ap.add_argument("--allow-fragile", action="store_true", help="edit although openpyxl may drop charts/images/pivots")
    args = ap.parse_args(argv)
    if args.index:
        print(index_text())
        return 0
    if args.op:
        if args.op not in OPS:
            print(json.dumps({"ok": False, "reason": "unknown_op", "error": f"unknown op {args.op!r}",
                              "available": list(OPS), "did_you_mean": did_you_mean(args.op, OPS)}))
            return 1
        print(op_text(args.op))
        return 0
    if not args.file or not args.ops:
        ap.error("give BOOK.xlsx and --ops FILE")
    if not (args.out or args.inplace or args.dry_run):
        ap.error("give --out NEW.xlsx, --inplace or --dry-run")
    import openpyxl
    sys.path.insert(0, HERE)
    try:
        ops = json.load(sys.stdin if args.ops == "-" else open(args.ops, encoding="utf-8"))
        if isinstance(ops, dict):
            ops = ops.get("ops", [ops])
        from inspect_xlsx import scan_package
        _, fragile = scan_package(args.file)
        if fragile and not args.allow_fragile:
            fail([OpError("fragile_parts", f"the workbook has {', '.join(fragile)}: openpyxl would drop or re-write them",
                          usage="edit the build_xlsx.py spec and rebuild; set_cells.py BOOK.xlsx --inplace --set 'Sheet!B2=42' (values and formulas); "
                                "or --allow-fragile").record()],
                 next_step="a workbook you built with build_xlsx.py: edit its spec (.work/*.json) and rebuild, the spec is the "
                           "source. Any other file: set_cells.py for value/formula edits, or --allow-fragile if losing them is acceptable",
                 file=args.file)
        wb = openpyxl.load_workbook(args.file)
    except (OSError, ValueError, KeyError) as e:
        print(json.dumps({"ok": False, "reason": "unreadable_input", "error": f"{type(e).__name__}: {e}"}))
        return 1
    plan, failures = plan_ops(ops, wb.sheetnames)
    if failures:
        fail(failures, file=args.file)
    if args.dry_run:
        print(json.dumps({"ok": True, "dry_run": True, "file": args.file, "plan": plan}, indent=2, ensure_ascii=False))
        return 0
    for i, op in enumerate(ops):
        try:
            apply_op(wb, op)
        except OpError as e:
            fail([e.record(i, op["op"], usage=usage_of(op["op"]))], file=args.file)
        except Exception as e:  # noqa: BLE001 - an op the validator could not foresee: still all-or-nothing
            fail([{"index": i, "op": op["op"], "reason": "op_rejected", "error": f"{type(e).__name__}: {e}",
                   "usage": usage_of(op["op"])}], file=args.file)
    out = args.file if args.inplace else args.out
    fd, tmp = tempfile.mkstemp(suffix=".xlsx", dir=os.path.dirname(os.path.abspath(out)) or ".")
    os.close(fd)
    wb.save(tmp)
    shutil.move(tmp, out)
    touched = any(op["op"] in FORMULA_OPS for op in ops)
    print(json.dumps({"ok": True, "out": out, "applied": len(ops), "formulas_touched": touched,
                      "next_step": f"python3 /skills/xlsx/scripts/recalc.py {out}" if touched else None},
                     indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SystemExit:
        raise
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
