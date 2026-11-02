#!/usr/bin/env python3
"""Render slide thumbnails (PNG per slide + a numbered contact-sheet grid) for visual QA.
Accepts .docx/.doc/.odt/.rtf/.pptx/.xlsx (converted with LibreOffice first) or .pdf.

Usage:
  python3 thumbnails.py DECK.pptx --outdir /workspace/.work/thumbs --grid /workspace/.work/thumbs/grid.png
  python3 thumbnails.py DECK.pptx --outdir DIR --dpi 110 --first 3 --last 3   (one slide, larger)
  python3 thumbnails.py DECK.pdf --outdir DIR --grid DIR/grid.png

Output (JSON): {"pdf": ..., "pages": N, "images": [...], "grid": path|null}
For the visual check of a deliverable, call view_file on the document itself (it renders and
shows the pages); use this script for custom renders (dpi, page range, contact sheet) and
open a PNG with read_file (vision models see it).
"""
from __future__ import annotations

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import lo  # noqa: E402


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file")
    ap.add_argument("--outdir", default="/workspace/.work/thumbs")
    ap.add_argument("--dpi", type=int, default=60)
    ap.add_argument("--first", type=int)
    ap.add_argument("--last", type=int)
    ap.add_argument("--grid", help="write a contact sheet PNG of the rendered pages")
    ap.add_argument("--cols", type=int, default=3)
    args = ap.parse_args(argv)

    try:
        src = os.path.abspath(args.file)
        os.makedirs(args.outdir, exist_ok=True)
        pdf = src if src.lower().endswith(".pdf") else lo.convert(src, "pdf", args.outdir)
        pages = lo.pdf_page_count(pdf)
        prefix = os.path.splitext(os.path.basename(src))[0]
        for f in os.listdir(args.outdir):  # drop stale renders of the same file
            if f.startswith(prefix + "-") and f.endswith(".png"):
                os.remove(os.path.join(args.outdir, f))
        images = lo.pdf_to_pngs(pdf, args.outdir, args.dpi, args.first, args.last, prefix)
        grid = lo.contact_sheet(images, args.grid, cols=args.cols, label_start=args.first or 1) if args.grid else None
    except lo.OfficeError as exc:
        print(json.dumps({"error": str(exc)}))
        return 1
    print(json.dumps({"pdf": pdf, "pages": pages, "images": images, "grid": grid}, indent=2))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
