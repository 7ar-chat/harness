#!/usr/bin/env python3
"""Edit a .docx by paragraph index with a JSON list of ops (python-docx underneath): insert, rewrite or delete
blocks with the document's OWN styles, all-or-nothing, with a dry run and errors that name the fix.

Usage:
  python3 edit_docx.py IN.docx --ops ops.json --out OUT.docx          # or --inplace
  echo '[{"op":"set_text","index":3,"text":"New **text**"}]' | python3 edit_docx.py IN.docx --ops - --inplace
  python3 edit_docx.py IN.docx --ops ops.json --dry-run               # the plan, nothing written
  python3 edit_docx.py --index                                        # one line per op
  python3 edit_docx.py --op insert_after                              # one op: signature, example

Indexes are those of `inspect_docx.py FILE --text` (python-docx `doc.paragraphs`, tables excluded) AS READ BEFORE
THE BATCH: they do not shift inside one batch, so write all the ops against what you listed. Blocks are the
new_document.py shapes: {"heading": "Intro"}, {"paragraph": "Text with **bold**"}, {"bullets": ["a", "b"]},
{"numbered": [...]}, {"table": {"columns": [...], "rows": [[...]]}}, {"page_break": {}}.
Every op is validated before the first is applied; one rejected op leaves the file untouched and the failure says
which op (index), why (reason), the valid range / styles (available) and the closest spelling (did_you_mean).
For find/replace across split runs use replace.py; tracked changes: tracked_changes.py (these ops are not tracked).
Prints {"ok", "out", "applied", "paragraphs", "next_step"}; then run the FINISH GATE (inspect_docx.py --check).
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

OPS = {
    "insert_after": ('{index, blocks:[...]}  (index -1 = at the start of the document)', {"index": True, "blocks": True},
                     {"op": "insert_after", "index": 4, "blocks": [{"heading": "Risks"}, {"bullets": ["Delay", "Cost"]}]}),
    "replace": ('{range:"A-B" | index, blocks:[...]}  (the paragraphs A..B are replaced by the blocks)',
                {"blocks": True}, {"op": "replace", "range": "6-8", "blocks": [{"paragraph": "Rewritten."}]}),
    "delete": ('{range:"A-B" | index}', {}, {"op": "delete", "range": "10-12"}),
    "set_text": ('{index, text}  (inline **bold**, *italic*, `code`, [label](url); keeps the paragraph style)',
                 {"index": True, "text": True}, {"op": "set_text", "index": 3, "text": "Revenue grew **12%**."}),
    "set_style": ('{index | range, style}', {"style": True}, {"op": "set_style", "index": 5, "style": "Heading 2"}),
}
BLOCK_ALIASES_HELP = "blocks: " + ", ".join(['{"heading": "Intro"}', '{"paragraph": "Text"}', '{"bullets": [..]}',
                                              '{"numbered": [..]}', '{"table": {"columns": [..], "rows": [[..]]}}',
                                              '{"page_break": {}}'])


def usage_of(name):
    return f"{name} {OPS[name][0]}"


def parse_span(op, n):
    """(lo, hi) from index or range 'A-B'; raises OpError when outside 0..n-1."""
    if "range" in op:
        a, _, b = str(op["range"]).partition("-")
        try:
            lo, hi = int(a), int(b or a)
        except ValueError:
            raise OpError("bad_range", f"range {op['range']!r} is not like 6-8", usage=usage_of(op["op"]))
    elif "index" in op:
        lo = hi = op["index"]
        if not isinstance(lo, int):
            raise OpError("bad_range", f"index {lo!r} is not an integer", usage=usage_of(op["op"]))
    else:
        raise OpError("op_rejected", f"op {op['op']!r}: give \"index\" or \"range\"", usage=usage_of(op["op"]))
    if not (0 <= lo <= hi < n):
        raise OpError("out_of_range", f"{lo}-{hi} is outside the document", valid_range=[0, n - 1],
                      usage=usage_of(op["op"]))
    return lo, hi


def plan_ops(ops, doc):
    from blocks import normalize_spec

    from docx.enum.style import WD_STYLE_TYPE

    n = len(doc.paragraphs)
    styles = [s.name for s in doc.styles if s.type == WD_STYLE_TYPE.PARAGRAPH]
    plan, failures, spans = [], [], []
    for i, op in enumerate(ops):
        try:
            if not isinstance(op, dict) or "op" not in op:
                raise OpError("op_rejected", "each op is an object with an \"op\" name", usage="edit_docx.py --index")
            name = op["op"]
            if name not in OPS:
                raise unknown("op", name, OPS, "unknown_op")
            for field, required in OPS[name][1].items():
                if required and field not in op and not (field == "index" and "range" in op):
                    raise OpError("op_rejected", f"op {name!r}: missing {field!r}", usage=usage_of(name))
            span = None
            if name == "insert_after":
                if not (isinstance(op["index"], int) and -1 <= op["index"] < n):
                    raise OpError("out_of_range", f"index {op['index']!r} is outside the document",
                                  valid_range=[-1, n - 1], usage=usage_of(name))
                span = (op["index"], op["index"])
            else:
                span = parse_span(op, n)
            if "blocks" in op:
                try:
                    op["_blocks"] = normalize_spec({"blocks": op["blocks"]}, "edit_docx.py")["blocks"]
                except SystemExit as e:
                    raise OpError("bad_blocks", str(e).split("\n")[0].replace("error: ", ""), usage=BLOCK_ALIASES_HELP)
            if name == "set_style" and op["style"] not in styles:
                raise OpError("style_not_found", f"style not found: {op['style']!r}",
                              available=styles[:30],
                              did_you_mean=did_you_mean(op["style"], styles), usage=usage_of(name))
            if name != "insert_after":
                for j, (a, b, other) in enumerate(spans):
                    if span[0] <= b and a <= span[1] and other in ("delete", "replace", "set_text", "set_style") \
                            and name in ("delete", "replace", "set_text", "set_style"):
                        raise OpError("overlap", f"overlaps op {j} ({other} {a}-{b}): indexes refer to the original document",
                                      usage=usage_of(name))
                spans.append((span[0], span[1], name))
            plan.append(f"[{i}] {name} " + (f"{span[0]}-{span[1]}" if span[0] != span[1] else str(span[0])))
        except OpError as e:
            failures.append(e.record(i, op.get("op") if isinstance(op, dict) else None, usage=e.extra.pop("usage", None)))
    return plan, failures


def build_blocks(doc, blocks):
    """Append the blocks at the end of the body with the document's styles; returns their XML elements."""
    from docx.shared import Cm
    from docx.enum.text import WD_BREAK
    from new_document import add_inline

    body = doc.element.body
    before = list(body)
    for b in blocks:
        kind = b.get("type")
        if kind == "heading":
            doc.add_heading(b["text"], level=int(b.get("level", 1)))
        elif kind == "paragraph":
            add_inline(doc.add_paragraph(style=b.get("style")), b.get("text", ""))
        elif kind in ("bullets", "numbered"):
            for item in b.get("items", []):
                text, level = (item.get("text", ""), int(item.get("level", 0))) if isinstance(item, dict) else (item, 0)
                base = "List Bullet" if kind == "bullets" else "List Number"
                add_inline(doc.add_paragraph(style=base if level == 0 else f"{base} {min(level + 1, 3)}"), text)
        elif kind == "table":
            cols, rows = b.get("columns") or [], b.get("rows") or []
            ncols = len(cols) or (len(rows[0]) if rows else 1)
            table = doc.add_table(rows=(1 if cols else 0) + len(rows), cols=ncols)
            try:
                table.style = doc.styles["Table Grid"]
            except KeyError:
                pass
            for r_i, row in enumerate(([cols] if cols else []) + rows):
                for c_i in range(ncols):
                    v = row[c_i] if c_i < len(row) else ""
                    table.cell(r_i, c_i).text = "" if v is None else str(v)
        elif kind == "page_break":
            doc.add_paragraph().add_run().add_break(WD_BREAK.PAGE)
        else:
            raise OpError("bad_blocks", f"block type {kind!r} is not supported by edit_docx.py "
                                        "(heading, paragraph, bullets, numbered, table, page_break)", usage=BLOCK_ALIASES_HELP)
    made = [e for e in body if e not in before and e.tag.endswith(("}p", "}tbl"))]
    return made


def apply_all(doc, ops):
    from new_document import add_inline

    anchors = [p._p for p in doc.paragraphs]           # original numbering, fixed before any op runs
    for op in ops:
        name = op["op"]
        if name == "insert_after":
            made = build_blocks(doc, op["_blocks"])
            ref = anchors[op["index"]] if op["index"] >= 0 else None
            for el in made:
                if ref is None:
                    doc.element.body.insert(0, el)
                    ref = el
                else:
                    ref.addnext(el)
                    ref = el
        elif name in ("replace", "delete"):
            lo, hi = parse_span(op, len(anchors))
            els = anchors[lo:hi + 1]
            if name == "replace":
                made = build_blocks(doc, op["_blocks"])
                ref = els[-1]
                for el in made:
                    ref.addnext(el)
                    ref = el
            for el in els:
                el.getparent().remove(el)
        elif name == "set_text":
            from docx.text.paragraph import Paragraph
            p = Paragraph(anchors[op["index"]], doc._body)
            style = p.style
            for r in list(p.runs):
                r._r.getparent().remove(r._r)
            add_inline(p, op["text"])
            p.style = style
        elif name == "set_style":
            from docx.text.paragraph import Paragraph
            lo, hi = parse_span(op, len(anchors))
            for el in anchors[lo:hi + 1]:
                Paragraph(el, doc._body).style = op["style"]


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file", nargs="?")
    ap.add_argument("--ops", help="JSON list of ops (a file, or - for stdin)")
    ap.add_argument("--out")
    ap.add_argument("--inplace", action="store_true")
    ap.add_argument("--dry-run", action="store_true", help="validate and print the plan; write nothing")
    ap.add_argument("--index", action="store_true", help="one line per op")
    ap.add_argument("--op", help="signature and an example of one op")
    args = ap.parse_args(argv)
    if args.index:
        print("\n".join(f"{n} {sig}" for n, (sig, _, _) in OPS.items()))
        return 0
    if args.op:
        if args.op not in OPS:
            print(json.dumps({"ok": False, "reason": "unknown_op", "error": f"unknown op {args.op!r}",
                              "available": list(OPS), "did_you_mean": did_you_mean(args.op, OPS)}))
            return 1
        sig, req, ex = OPS[args.op]
        print(json.dumps({"op": args.op, "signature": sig, "example": ex}, indent=2, ensure_ascii=False))
        return 0
    if not args.file or not args.ops:
        ap.error("give IN.docx and --ops FILE")
    if not (args.out or args.inplace or args.dry_run):
        ap.error("give --out NEW.docx, --inplace or --dry-run")
    import docx
    try:
        ops = json.load(sys.stdin if args.ops == "-" else open(args.ops, encoding="utf-8"))
        if isinstance(ops, dict):
            ops = ops.get("ops", [ops])
        doc = docx.Document(args.file)
    except Exception as e:  # noqa: BLE001
        print(json.dumps({"ok": False, "reason": "unreadable_input", "error": f"{type(e).__name__}: {e}"}))
        return 1
    plan, failures = plan_ops(ops, doc)
    if failures:
        fail(failures, file=args.file)
    if args.dry_run:
        print(json.dumps({"ok": True, "dry_run": True, "file": args.file, "paragraphs": len(doc.paragraphs), "plan": plan},
                         indent=2, ensure_ascii=False))
        return 0
    try:
        apply_all(doc, ops)
    except OpError as e:
        fail([e.record(usage=e.extra.pop("usage", None))], file=args.file)
    out = args.file if args.inplace else args.out
    fd, tmp = tempfile.mkstemp(suffix=".docx", dir=os.path.dirname(os.path.abspath(out)) or ".")
    os.close(fd)
    doc.save(tmp)
    shutil.move(tmp, out)
    print(json.dumps({"ok": True, "out": out, "applied": len(ops), "paragraphs": len(doc.paragraphs),
                      "next_step": f"python3 /skills/docx/scripts/inspect_docx.py --check {out}"}, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SystemExit:
        raise
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
