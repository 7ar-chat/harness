#!/usr/bin/env python3
"""Generic checker for data files no other skill covers: a Python file compiles, a JSON file
parses, a CSV file parses with the same number of columns on every non-empty row.

Usage:
  python3 /skills/xlsx/scripts/validate_file.py FILE.csv|FILE.json|FILE.py

Output (the checker contract of harness-v3 §4): {"ok", "file" (absolute path), "checks":
[{"name": "py_compile" | "json" | "csv", "ok", "detail"}], "next_step"?}.
Exit code: 0 = ok, 2 = not ok (fix the file, run again), 1 = usage error, missing file or a file
type it does not check. Work's runtime runs the same checks after a tool call changed such a
file under /workspace/outputs/.
"""
from __future__ import annotations

import csv
import json
import os
import py_compile
import sys
import tempfile

NAMES = {".py": "py_compile", ".json": "json", ".csv": "csv"}


def usage(path, message):
    print(json.dumps({"ok": False, "file": path, "checks": [], "error": message}))
    return 1


def done(path, name, ok, detail, nxt=None):
    out = {"ok": ok, "file": path, "checks": [{"name": name, "ok": ok, "detail": detail}]}
    if nxt:
        out["next_step"] = nxt
    print(json.dumps(out, ensure_ascii=False))
    return 0 if ok else 2


def main(argv=None) -> int:
    args = sys.argv[1:] if argv is None else argv
    if args and args[0] in ("-h", "--help"):
        print(__doc__)
        return 0
    if len(args) != 1:
        print(json.dumps({"ok": False, "error": "usage: validate_file.py FILE (.py, .json or .csv)"}))
        return 1
    p = os.path.abspath(args[0])
    ext = os.path.splitext(p)[1].lower()
    if not os.path.isfile(p):
        return usage(p, f"no such file: {p}")
    name = NAMES.get(ext)
    if name is None:
        return usage(p, f"validate_file.py checks .py, .json and .csv files, not {ext or 'files without extension'}")
    try:
        if ext == ".py":
            with tempfile.TemporaryDirectory() as tmp:  # no __pycache__ next to the deliverable
                py_compile.compile(p, cfile=os.path.join(tmp, "check.pyc"), doraise=True)
            return done(p, name, True, "compiles")
        if ext == ".json":
            with open(p, encoding="utf-8") as f:
                json.load(f)
            return done(p, name, True, "valid JSON")
        with open(p, newline="", encoding="utf-8-sig") as f:
            rows = list(csv.reader(f))
        widths = {len(r) for r in rows if r}
        if len(widths) > 1:
            return done(p, name, False, "rows have different numbers of columns: %s" % sorted(widths),
                        "make every row have the same columns")
        return done(p, name, True, "%d rows" % len(rows))
    except Exception as e:  # noqa: BLE001 - the file is not valid: that is the verdict
        return done(p, name, False, str(e)[:300], "fix the file and check it again")


if __name__ == "__main__":
    sys.exit(main())
