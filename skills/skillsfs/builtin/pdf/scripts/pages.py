#!/usr/bin/env python3
"""Page operations on PDFs with pypdf: info, merge, split, select/reorder, delete, rotate.
Pages are 1-based; ranges like "1-3,5,8-" (8- = 8 to the end).

Usage:
  python3 pages.py info IN.pdf
  python3 pages.py merge OUT.pdf A.pdf B.pdf C.pdf            # concatenates, keeps bookmarks per file
  python3 pages.py merge OUT.pdf A.pdf B.pdf --bookmarks       # one top-level bookmark per input file
  python3 pages.py split IN.pdf OUTDIR                         # one file per page
  python3 pages.py split IN.pdf OUTDIR --every 5               # chunks of 5 pages
  python3 pages.py split IN.pdf OUTDIR --ranges "1-3,4-10"     # one file per range
  python3 pages.py select IN.pdf OUT.pdf --pages "3,1,2,4-"    # extract and/or reorder
  python3 pages.py delete IN.pdf OUT.pdf --pages "2,5"
  python3 pages.py rotate IN.pdf OUT.pdf --angle 90 [--pages "1-2"]   # clockwise, multiple of 90

Prints JSON ({"out"/"outputs", "pages"}). For damaged PDFs try first: qpdf in.pdf repaired.pdf
"""
from __future__ import annotations

import argparse
import json
import os
import sys


def parse_ranges(spec, n):
    groups = []
    for part in spec.split(","):
        part = part.strip()
        if not part:
            continue
        if "-" in part:
            a, b = part.split("-", 1)
            a, b = int(a or 1), int(b or n)
            groups.append(list(range(a, b + 1)))
        else:
            groups.append([int(part)])
    for g in groups:
        for p in g:
            if not 1 <= p <= n:
                raise SystemExit(f"error: page {p} out of range 1..{n}")
    return groups


def write(writer, out):
    os.makedirs(os.path.dirname(os.path.abspath(out)) or ".", exist_ok=True)
    with open(out, "wb") as fh:
        writer.write(fh)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("info"); p.add_argument("input")
    p = sub.add_parser("merge"); p.add_argument("out"); p.add_argument("inputs", nargs="+")
    p.add_argument("--bookmarks", action="store_true")
    p = sub.add_parser("split"); p.add_argument("input"); p.add_argument("outdir")
    p.add_argument("--every", type=int); p.add_argument("--ranges")
    for name in ("select", "delete"):
        p = sub.add_parser(name); p.add_argument("input"); p.add_argument("out"); p.add_argument("--pages", required=True)
    p = sub.add_parser("rotate"); p.add_argument("input"); p.add_argument("out")
    p.add_argument("--angle", type=int, default=90); p.add_argument("--pages")
    args = ap.parse_args(argv)

    from pypdf import PdfReader, PdfWriter

    if args.cmd == "info":
        r = PdfReader(args.input)
        sizes = []
        for pg in r.pages[:50]:
            w, h = float(pg.mediabox.width), float(pg.mediabox.height)
            sizes.append({"w_pt": round(w), "h_pt": round(h), "rotation": pg.rotation})
        print(json.dumps({"file": args.input, "pages": len(r.pages), "encrypted": r.is_encrypted,
                          "metadata": {k.lstrip("/"): str(v) for k, v in (r.metadata or {}).items()},
                          "form_fields": len(r.get_fields() or {}), "page_sizes": sizes}, indent=2))
        return 0
    if args.cmd == "merge":
        w = PdfWriter()
        for path in args.inputs:
            w.append(path, outline_item=os.path.splitext(os.path.basename(path))[0] if args.bookmarks else None)
        write(w, args.out)
        print(json.dumps({"out": args.out, "pages": len(w.pages), "inputs": len(args.inputs)}))
        return 0
    r = PdfReader(args.input)
    n = len(r.pages)
    if args.cmd == "split":
        os.makedirs(args.outdir, exist_ok=True)
        if args.ranges:
            groups = parse_ranges(args.ranges, n)
        else:
            step = args.every or 1
            groups = [list(range(i, min(i + step, n + 1))) for i in range(1, n + 1, step)]
        base = os.path.splitext(os.path.basename(args.input))[0]
        outputs = []
        for g in groups:
            w = PdfWriter()
            for pnum in g:
                w.add_page(r.pages[pnum - 1])
            name = f"{base}_p{g[0]}" + (f"-{g[-1]}" if len(g) > 1 else "") + ".pdf"
            path = os.path.join(args.outdir, name)
            write(w, path)
            outputs.append({"file": path, "pages": len(g)})
        print(json.dumps({"outputs": outputs}, indent=2))
        return 0
    if args.cmd in ("select", "delete"):
        chosen = [pnum for g in parse_ranges(args.pages, n) for pnum in g]
        order = chosen if args.cmd == "select" else [x for x in range(1, n + 1) if x not in set(chosen)]
        if not order:
            raise SystemExit("error: no page left")
        w = PdfWriter()
        for pnum in order:
            w.add_page(r.pages[pnum - 1])
        write(w, args.out)
        print(json.dumps({"out": args.out, "pages": len(order), "order": order}))
        return 0
    if args.cmd == "rotate":
        if args.angle % 90:
            raise SystemExit("error: --angle must be a multiple of 90")
        targets = {pnum for g in parse_ranges(args.pages, n) for pnum in g} if args.pages else set(range(1, n + 1))
        w = PdfWriter(clone_from=r)
        for i, pg in enumerate(w.pages, 1):
            if i in targets:
                pg.rotate(args.angle)
        write(w, args.out)
        print(json.dumps({"out": args.out, "pages": n, "rotated": sorted(targets)}))
        return 0
    return 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
